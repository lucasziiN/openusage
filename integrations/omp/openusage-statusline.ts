import { execFile } from "node:child_process";
import { stripVTControlCharacters } from "node:util";
import { type Component, visibleWidth, wrapTextWithAnsi } from "@oh-my-pi/pi-tui";
import type { Theme } from "@oh-my-pi/pi-tui/theme";

type OmpExtensionContext = {
  ui: {
    readonly theme: Theme;
    // Only present on OMP builds carrying the below-footer patch; stock
    // releases (e.g. after `omp update`) omit it, so always feature-detect.
    getNativeFooter?(width: number): readonly string[];
    setStatus(key: string, text: string | undefined): void;
    setWidget(
      key: string,
      content: string[] | ((_ui: unknown, theme: Theme) => Component) | undefined,
      options: { placement: WidgetPlacement },
    ): void;
  };
  models: { list(): Array<{ name: string; id: string; provider: string }> };
  sessionManager: {
    getSessionId(): string;
    getEntries(): Array<{
      type: string;
      message?: {
        role: string;
        model?: string;
        provider?: string;
        usage?: { cost?: { total?: number }; input?: number; output?: number };
      };
    }>;
  };
  setInterval(callback: () => void, delayMs: number): unknown;
};

type WidgetPlacement = "belowFooter" | "belowEditor";

type OmpExtensionAPI = {
  on(
    event: "session_start" | "session_switch" | "session_shutdown" | "message_end",
    listener: (_event: unknown, context: OmpExtensionContext) => void,
  ): void;
};

type QuotaSummary = {
  codex: string;
  claude: string;
  codex5hPct?: number;
  claude5hPct?: number;
};

type BlockDetails = {
  costUSD: number;
  secondsLeft: number;
  burnUSDHour: number;
};

type RenderedOmpStatus = {
  status: string;
  quotas: string[];
  color: boolean;
};

type SessionModelCost = {
  provider: string;
  model: string;
  costUSD?: number;
};

const STATUS_KEY = "openusage-quota";
const REFRESH_INTERVAL_MS = 60_000;
const COST_REFRESH_INTERVAL_MS = 5 * 60_000;
const COMMAND_TIMEOUT_MS = 15_000;
const COST_TIMEOUT_MS = 45_000;

function readQuotaSummary(): Promise<QuotaSummary | undefined> {
  const { promise, resolve } = Promise.withResolvers<QuotaSummary | undefined>();
  execFile(
    "openusage", ["omp-statusline", "--json"],
    { encoding: "utf8", maxBuffer: 4 * 1024, timeout: COMMAND_TIMEOUT_MS, windowsHide: true },
    (error, stdout) => {
      if (error) {
        resolve(undefined);
        return;
      }
      try {
        const summary = JSON.parse(stdout) as QuotaSummary;
        resolve(typeof summary.codex === "string" && typeof summary.claude === "string" ? summary : undefined);
      } catch {
        resolve(undefined);
      }
    },
  );
  return promise;
}

function readBlockDetails(): Promise<BlockDetails | undefined> {
  const { promise, resolve } = Promise.withResolvers<BlockDetails | undefined>();
  execFile(
    "openusage", ["omp-statusline", "costs"],
    { encoding: "utf8", maxBuffer: 4 * 1024, timeout: COST_TIMEOUT_MS, windowsHide: true },
    (error, stdout) => {
      if (error) {
        resolve(undefined);
        return;
      }
      try {
        const block = JSON.parse(stdout) as BlockDetails;
        resolve(
          Number.isFinite(block.costUSD) && block.costUSD >= 0 &&
          Number.isFinite(block.secondsLeft) && block.secondsLeft > 0 &&
          Number.isFinite(block.burnUSDHour) && block.burnUSDHour >= 0
            ? block
            : undefined,
        );
      } catch {
        resolve(undefined);
      }
    },
  );
  return promise;
}

function localDate(now: Date): string {
  const year = now.getFullYear();
  const month = String(now.getMonth() + 1).padStart(2, "0");
  const day = String(now.getDate()).padStart(2, "0");
  return `${year}-${month}-${day}`;
}

function dailyReportCost(stdout: string): number | undefined {
  try {
    const response: unknown = JSON.parse(stdout);
    if (typeof response !== "object" || response === null) {
      return undefined;
    }

    const report = response as {
      kind?: unknown;
      rows?: unknown;
      totals?: { cost_usd?: unknown; total_tokens?: unknown };
    };
    const cost = report.totals?.cost_usd;
    const tokens = report.totals?.total_tokens;
    if (
      report.kind !== "daily" ||
      !Array.isArray(report.rows) ||
      typeof cost !== "number" ||
      !Number.isFinite(cost) ||
      cost < 0 ||
      typeof tokens !== "number" ||
      !Number.isFinite(tokens) ||
      tokens < 0 ||
      (tokens > 0 && cost === 0)
    ) {
      return undefined;
    }
    return cost;
  } catch {
    return undefined;
  }
}

function readDailyCost(provider: string, date: string): Promise<number | undefined> {
  const { promise, resolve } = Promise.withResolvers<number | undefined>();
  execFile(
    "openusage",
    [
      "daily", "--provider", provider,
      "--source", "direct", "--offline",
      "--since", date, "--until", date, "--json",
    ],
    {
      encoding: "utf8",
      maxBuffer: 32 * 1024,
      timeout: COST_TIMEOUT_MS,
      windowsHide: true,
    },
    (error, stdout) => {
      resolve(error ? undefined : dailyReportCost(stdout));
    },
  );
  return promise;
}

function sessionRecordedCosts(context: OmpExtensionContext): {
  sessionCostUSD?: number;
  modelCosts: SessionModelCost[];
} {
  const models = new Map<string, { provider: string; model: string; costUSD: number; priced: boolean }>();
  const names = new Map(context.models.list().map((model) => [
    `${model.provider}/${model.id}`, model.name,
  ]));
  let totalCostUSD = 0;
  let complete = true;
  let turns = 0;
  for (const entry of context.sessionManager.getEntries()) {
    const message = entry.message;
    if (entry.type !== "message" || message?.role !== "assistant" || !message.usage) {
      continue;
    }
    turns++;
    const provider = message.provider ?? "";
    const modelID = message.model ?? "Unknown model";
    const key = `${provider}/${modelID}`;
    let model = models.get(key);
    if (!model) {
      model = { provider, model: names.get(key) || modelID, costUSD: 0, priced: true };
      models.set(key, model);
    }
    const amount = message.usage.cost?.total;
    if (typeof amount !== "number" || !Number.isFinite(amount) || amount < 0) {
      model.priced = false;
      complete = false;
      continue;
    }
    model.costUSD += amount;
    totalCostUSD += amount;
  }
  return {
    sessionCostUSD: complete && turns > 0 ? totalCostUSD : undefined,
    modelCosts: Array.from(models.values(), (model) => ({
      provider: model.provider,
      model: model.model,
      costUSD: model.priced ? model.costUSD : undefined,
    })),
  };
}

function renderStatusLine(
  values: Record<string, unknown>,
  done: (rendered: RenderedOmpStatus | undefined) => void,
): void {
  const command = execFile(
    "openusage", ["omp-statusline", "--render", "--json"],
    { encoding: "utf8", maxBuffer: 4 * 1024, timeout: COMMAND_TIMEOUT_MS, windowsHide: true },
    (error, stdout) => {
      if (error) {
        done(undefined);
        return;
      }
      try {
        const rendered = JSON.parse(stdout) as RenderedOmpStatus;
        if (typeof rendered.status === "string" &&
            typeof rendered.color === "boolean" &&
            Array.isArray(rendered.quotas) &&
            rendered.quotas.every((quota) => typeof quota === "string")) {
          done(rendered);
          return;
        }
      } catch {
        // Invalid output is not safe to show in the footer.
      }
      done(undefined);
    },
  );
  command.stdin?.write(JSON.stringify(values));
  command.stdin?.end();
}

// Stock OMP silently treats unknown placements as "aboveEditor"; "belowEditor"
// keeps the widget next to the footer until a patched build is reinstalled.
function widgetPlacement(ui: OmpExtensionContext["ui"]): WidgetPlacement {
  return typeof ui.getNativeFooter === "function" ? "belowFooter" : "belowEditor";
}

function createStatusWidget(
  rendered: RenderedOmpStatus,
  ui: OmpExtensionContext["ui"],
): Component & { readonly footerColumnWidths: readonly number[] } {
  const rows = rendered.status ? [rendered.status, ...rendered.quotas] : rendered.quotas;
  const fields = rows.map((row, index) => {
    const plain = stripVTControlCharacters(row);
    const isCostRow = rendered.status !== "" && index === 0;
    const blocks = (isCostRow || plain.startsWith("🐙 ") ? plain.replace(/ [|/] /g, " · ") : plain)
      .trim().split(" · ");
    // Icons occupy the native brand column; text starts under its model column.
    const icon = /^(💰|🔥|🐙) (.*)$/u.exec(blocks[0]);
    if (icon) {
      blocks.splice(0, 1, icon[1], icon[2]);
    } else {
      blocks.unshift("");
    }
    let kind: "cost" | "provider" | "models" = "models";
    if (isCostRow) {
      kind = "cost";
    } else if (plain.startsWith("🐙 ")) {
      kind = "provider";
    }
    return { blocks, kind };
  });
  const footerColumnWidths: number[] = [];
  for (const row of fields) {
    row.blocks.slice(0, -1).forEach((cell, index) => {
      footerColumnWidths[index] = Math.max(footerColumnWidths[index] ?? 0, visibleWidth(cell));
    });
  }

  return {
    footerColumnWidths,
    invalidate() {},
    render(width: number): string[] {
      // This runs inside OMP's frame loop, where a throw takes down the whole
      // session; drop the widget for this frame instead.
      try {
        return renderRows(width);
      } catch {
        return [];
      }
    },
  };

  function renderRows(width: number): string[] {
    if (width <= 2) {
      return [];
    }
    const theme = ui.theme;
    const nativeRow = typeof ui.getNativeFooter === "function"
      ? ui.getNativeFooter(width).map(stripVTControlCharacters).find((line) => line.includes(" · "))
      : undefined;
    const columns = [1];
    if (nativeRow) {
      for (const separator of nativeRow.matchAll(/ · /g)) {
        const start = visibleWidth(nativeRow.slice(0, separator.index)) + 3;
        if (start < width - 1) {
          columns.push(start);
        }
      }
    } else {
      // Stock OMP: no native footer to align with, so lay out on our own widths.
      for (const columnWidth of footerColumnWidths) {
        const start = columns[columns.length - 1] + columnWidth + 3;
        if (start >= width - 1) {
          break;
        }
        columns.push(start);
      }
    }
    const separatorText = rendered.color ? theme.fg("statusLineSep", " · ") : " · ";
    const output: string[] = [];

    for (const row of fields) {
      let rowColumns = columns;
      if (row.kind === "cost") {
        const nativeColumnWrapsCost = row.blocks.some(
          (cell, index) => index + 1 < columns.length &&
            visibleWidth(cell) > columns[index + 1] - columns[index] - 3,
        );
        if (nativeColumnWrapsCost) {
          const compactColumns = [columns[0], columns[1] ?? 5];
          for (let index = 2; index < row.blocks.length; index++) {
            compactColumns.push(
              compactColumns[index - 1] + visibleWidth(row.blocks[index - 1]) + 3,
            );
          }
          const lastIndex = row.blocks.length - 1;
          const compactWidth = compactColumns[lastIndex] + visibleWidth(row.blocks[lastIndex]);
          // Keep cost blocks intact at half-screen width if there is room
          // to tighten their gaps without changing the native status row.
          if (compactWidth < width - 1) {
            rowColumns = compactColumns;
          }
        }
      }
      // When OMP drops columns on a narrow terminal, retain the remaining fields
      // on continuation lines in the last column rather than hiding them.
      const cells = row.blocks.slice(0, rowColumns.length);
      if (row.blocks.length > rowColumns.length) {
        cells[rowColumns.length - 1] = row.blocks.slice(rowColumns.length - 1).filter(Boolean).join("\n");
      }
      const wrapped = cells.map((cell, index) => {
        let styled = cell;
        if (rendered.color) {
          const textColor = cell === "n/a" ? "muted"
            : row.kind === "models" ? "statusLineModel" : "statusLineContext";
          // Style each run separately: an amount's reset must not erase the
          // theme colour of the label or countdown that follows it.
          styled = cell.split(/(\$[\d,.]+|\d+(?:\.\d+)?%)/g).map((part, index) =>
            theme.fg(index % 2 === 1 ? "statusLinePath" : textColor, part),
          ).join("");
          if (row.kind === "provider" && index === 1) {
            styled = theme.bold(theme.fg("warning", cell));
          }
        }
        const end = index + 1 < cells.length ? rowColumns[index + 1] - 3 : width - 1;
        return wrapTextWithAnsi(styled, Math.max(1, end - rowColumns[index]));
      });
      const height = Math.max(...wrapped.map((cell) => cell.length));
      for (let lineIndex = 0; lineIndex < height; lineIndex++) {
        let line = " ";
        for (let column = 0; column < wrapped.length; column++) {
          if (column > 0) {
            const gap = Math.max(0, rowColumns[column] - 3 - visibleWidth(line));
            line += " ".repeat(gap) + (lineIndex === 0 ? separatorText : "   ");
          }
          line += wrapped[column][lineIndex] ?? "";
        }
        output.push(line);
      }
    }
    return output;
  }
}

export default function registerOpenUsageStatusline(pi: OmpExtensionAPI): void {
  let activeContext: OmpExtensionContext | undefined;
  let quota: QuotaSummary | undefined;
  let block: BlockDetails | undefined;
  let dailyCost: number | undefined;
  let costDate = "";
  let lastCostRefreshAt = 0;
  let lastBlockRefreshAt = 0;
  let quotaRefreshInProgress = false;
  let blockRefreshInProgress = false;
  let costRefreshInProgress = false;
  let timerStarted = false;
  let renderVersion = 0;

  const publish = (): void => {
    const context = activeContext;
    if (!context) {
      return;
    }
    const session = sessionRecordedCosts(context);
    const values = {
      sessionCostUSD: session.sessionCostUSD,
      modelCosts: session.modelCosts,
      todayCostUSD: dailyCost,
      block,
      chatgptQuota: quota?.codex,
      claudeQuota: quota?.claude,
    };
    const version = ++renderVersion;
    renderStatusLine(values, (rendered) => {
      if (activeContext !== context || renderVersion !== version) {
        return;
      }
      if (!rendered) {
        context.ui.setStatus(STATUS_KEY, "OpenUsage unavailable");
        context.ui.setWidget(STATUS_KEY, undefined, { placement: widgetPlacement(context.ui) });
        return;
      }
      // One widget preserves the requested cost → provider → models order.
      // OMP's native status line and its configuration stay untouched.
      const hasContent = rendered.status !== "" || rendered.quotas.length > 0;
      // Flag a stock `omp update` instead of silently degrading the layout.
      context.ui.setStatus(
        STATUS_KEY,
        widgetPlacement(context.ui) === "belowFooter" ? undefined : "OpenUsage: stock OMP, run rebuild-omp-patched.sh",
      );
      context.ui.setWidget(
        STATUS_KEY,
        hasContent ? () => createStatusWidget(rendered, context.ui) : undefined,
        { placement: widgetPlacement(context.ui) },
      );
    });
  };

  const refresh = (): void => {
    if (!activeContext) {
      return;
    }
    const now = new Date();
    const today = localDate(now);
    if (costDate !== today) {
      costDate = today;
      dailyCost = undefined;
      lastCostRefreshAt = 0;
      publish();
    }

    if (!quotaRefreshInProgress) {
      quotaRefreshInProgress = true;
      void readQuotaSummary().then((result) => {
        quota = result;
        quotaRefreshInProgress = false;
        publish();
      });
    }

    if (!blockRefreshInProgress && now.getTime() - lastBlockRefreshAt >= REFRESH_INTERVAL_MS) {
      blockRefreshInProgress = true;
      lastBlockRefreshAt = now.getTime();
      void readBlockDetails().then((result) => {
        block = result;
        blockRefreshInProgress = false;
        publish();
      });
    }

    if (!costRefreshInProgress && now.getTime() - lastCostRefreshAt >= COST_REFRESH_INTERVAL_MS) {
      costRefreshInProgress = true;
      lastCostRefreshAt = now.getTime();
      void Promise.all([
        readDailyCost("codex", today),
        readDailyCost("claude_code", today),
        readDailyCost("pi", today),
      ]).then(([codexCost, claudeCost, ompCost]) => {
        costRefreshInProgress = false;
        if (costDate !== today) {
          return;
        }
        if (codexCost !== undefined && claudeCost !== undefined && ompCost !== undefined) {
          dailyCost = codexCost + claudeCost + ompCost;
        } else if (ompCost !== undefined) {
          dailyCost = ompCost;
        } else {
          dailyCost = undefined;
        }
        publish();
      });
    }
  };

  const attach = (_event: unknown, context: OmpExtensionContext): void => {
    activeContext = context;
    context.ui.setStatus(STATUS_KEY, "OpenUsage loading...");
    context.ui.setWidget(STATUS_KEY, undefined, { placement: widgetPlacement(context.ui) });
    publish();
    refresh();
    if (!timerStarted) {
      timerStarted = true;
      context.setInterval(refresh, REFRESH_INTERVAL_MS);
    }
  };

  pi.on("session_start", attach);
  pi.on("session_switch", attach);
  pi.on("message_end", (_event, context) => {
    if (context.sessionManager.getSessionId() === activeContext?.sessionManager.getSessionId()) {
      activeContext = context;
      publish();
    }
  });
  pi.on("session_shutdown", () => {
    activeContext?.ui.setWidget(STATUS_KEY, undefined, { placement: "belowFooter" });
    activeContext?.ui.setStatus(STATUS_KEY, undefined);
    activeContext = undefined;
    renderVersion++;
  });
}
