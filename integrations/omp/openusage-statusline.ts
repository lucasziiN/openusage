import { execFile } from "node:child_process";
import { type FileHandle, open, readdir } from "node:fs/promises";
import { join } from "node:path";
import { stripVTControlCharacters } from "node:util";
import {
  type Component,
  matchesKey,
  routeSgrMouseInput,
  truncateToWidth,
  visibleWidth,
  wrapTextWithAnsi,
} from "@oh-my-pi/pi-tui";
import type { Theme, ThemeColor } from "@oh-my-pi/pi-tui/theme";

// ---------------------------------------------------------------------------
// The slice of OMP's extension API this file uses
// ---------------------------------------------------------------------------

type WidgetPlacement = "belowFooter" | "belowEditor";

type OmpTui = {
  requestRender(): void;
  readonly terminal?: { readonly rows: number };
};

type OmpUI = {
  readonly theme: Theme;
  // Only present on OMP builds carrying the below-footer patch; stock
  // releases (e.g. after `omp update`) omit it, so always feature-detect.
  getNativeFooter?(width: number): readonly string[];
  setStatus(key: string, text: string | undefined): void;
  // Prints a line into the transcript: "info" dim, "warning" yellow. Alerts
  // never use "error": it also clears pending input and stops the spinner.
  notify(message: string, type?: "info" | "warning"): void;
  setWidget(
    key: string,
    content: string[] | ((_ui: unknown, theme: Theme) => Component) | undefined,
    options: { placement: WidgetPlacement },
  ): void;
  custom<T>(
    factory: (tui: OmpTui, theme: Theme, keybindings: unknown, done: (result: T) => void) => Component,
    options?: { overlay?: boolean; overlayOptions?: Record<string, unknown> },
  ): Promise<T>;
};

type SessionHeader = { timestamp: string; parentSession?: string };

type OmpExtensionContext = {
  // False for headless sessions (task/eval subagents, print mode), whose UI is a no-op.
  hasUI: boolean;
  ui: OmpUI;
  models: { list(): Array<{ name: string; id: string; provider: string }> };
  sessionManager: {
    getSessionId(): string;
    // Subagent transcripts (`<id>.jsonl`, nested under `<parent>/`) live here.
    getArtifactsDir(): string | null;
    getEntries(): SessionEntry[];
    // `parentSession` marks a fork, branch or clone, created at `timestamp`
    // with a copy of the parent's entries (original timestamps kept).
    getHeader?(): SessionHeader | null;
  };
  setInterval(callback: () => void, delayMs: number): unknown;
};

type InputEvent = { text: string; source: "interactive" | "rpc" | "extension" };

type MessageEndEvent = { message?: { role?: string } };

type SessionListener = (_event: unknown, context: OmpExtensionContext) => void;

type OmpExtensionAPI = {
  on(event: "session_start" | "session_switch" | "session_branch" | "session_shutdown", listener: SessionListener): void;
  on(event: "message_end", listener: (event: MessageEndEvent, context: OmpExtensionContext) => void): void;
  // Runs before OMP's built-in slash commands; `handled` consumes the input.
  on(
    event: "input",
    listener: (event: InputEvent, context: OmpExtensionContext) => { handled: true } | undefined,
  ): void;
  // Session event bus; task/eval subagent frames arrive on `task:subagent:*`.
  events: { on(channel: string, handler: (data: unknown) => void): () => void };
};

type Usage = { cost?: { total?: number }; input?: number; output?: number };

// Assistant turns carry usage on `message`; `model_usage` entries record side
// calls (auto-thinking, judges) with provider, model and usage at the top level.
type SessionEntry = {
  type: string;
  id?: string;
  timestamp?: string;
  provider?: string;
  model?: string;
  usage?: Usage;
  message?: { role: string; model?: string; provider?: string; usage?: Usage };
};

// ---------------------------------------------------------------------------
// Data from `openusage omp-statusline`
// ---------------------------------------------------------------------------

type QuotaStatus = "ok" | "warning" | "exhausted";

type QuotaWindow = {
  label: string; // "5h", "7d", or "7d Fable" for a tier/model bucket
  durationMs?: number;
  scoped?: boolean;
  usedFraction: number; // mean across accounts; above 1 is overage
  resetsAt?: number; // Unix ms
  status: QuotaStatus;
  accounts: number;
  // Linear pace from when OMP measured the window (single account only):
  // usage expected at the reset, and when it runs out if that comes first.
  projectedFraction?: number;
  exhaustsAt?: number; // Unix ms
};

// Claude's paid overage budget: money, not a rate-limit window.
type ExtraUsage = { usedUSD: number; limitUSD?: number; usedFraction?: number };

type ProviderQuota = {
  key: string; // "chatgpt", "claude", or the OMP provider id
  name: string;
  plan?: string;
  accounts: number;
  fetchedAt?: number; // Unix ms of OMP's oldest report
  resets?: number; // saved rate-limit resets
  usableResets?: number;
  resetsExpireAt?: number; // soonest expiry of a usable saved reset
  unreported?: number; // accounts OMP has no usage data for
  disabled?: number; // sign-ins OMP disabled (e.g. an expired grant)
  extra?: ExtraUsage;
  windows: QuotaWindow[];
};

type QuotaSummary = { schema: number; providers: ProviderQuota[]; error?: string };

type BlockDetails = {
  costUSD: number;
  burnUSDHour: number;
  projectedUSD: number;
  startedAt: number; // Unix ms
  endsAt: number; // Unix ms
  lastActivityAt: number; // Unix ms
};

type DayCost = { date: string; costUSD: number; turns: number; unpriced?: number };

// `subscription` is the quota key ("chatgpt", "claude") a model bills against.
type CostShare = { provider?: string; subscription?: string; name: string; todayUSD: number; weekUSD: number };

type WindowSpend = { key: string; costUSD: number; turns: number };

type SessionSpend = {
  id: string;
  title: string;
  project: string;
  costUSD: number; // last 7 days, subagents included
  todayUSD: number;
  subagentUSD: number;
  turns: number;
  lastActivityAt: number;
};

type TokenTotals = { input: number; output: number; cacheRead: number; cacheWrite: number };

// Recorded local Pi/OMP spend. History fields come only with `--days`.
type LocalCosts = {
  block?: BlockDetails;
  todayUSD?: number;
  lastHourUSD?: number;
  windows?: WindowSpend[];
  days?: DayCost[];
  hours?: number[]; // today's spend per local clock hour, index 0 = 00:00
  models?: CostShare[];
  projects?: CostShare[];
  sessions?: SessionSpend[];
  tokens?: { today: TokenTotals; week: TokenTotals };
  monthUSD?: number;
  monthProjectedUSD?: number;
};

// Aggregated from OMP's hourly usage snapshots (`omp usage --history`).
type HistoryWindow = {
  label: string;
  scoped?: boolean;
  limitHits: number;
  peakFraction?: number;
  daily: Array<{ date: string; peakFraction?: number }>;
};

type LimitHistory = { days: number; providers: Array<{ key: string; name: string; windows: HistoryWindow[] }> };

type CellStatus = "warning" | "exhausted" | "stale";

type QuotaDisplay = "used" | "left";

type RenderedOmpStatus = {
  status: string;
  quotas: string[];
  // Cells that need attention, keyed by their exact text.
  cellStatus?: Record<string, CellStatus>;
  color: boolean;
  quotaDisplay: QuotaDisplay;
  providerStatus: boolean;
};

type RenderResult =
  | { kind: "ok"; rendered: RenderedOmpStatus }
  | { kind: "mismatch" }
  | { kind: "failed" };

type IncidentLevel = "minor" | "major" | "critical" | "maintenance";

type ProviderIncident = { indicator: IncidentLevel; title: string };

type QuotaAlert = { level: "warning" | "info"; message: string };

// Today's API-equivalent cost of the other local CLIs; undefined when unknown.
type ToolCosts = { codex?: number; claude?: number };

// ---------------------------------------------------------------------------
// Session costs (main transcript + subagent transcripts)
// ---------------------------------------------------------------------------

type TranscriptTurn = { key: string; provider: string; modelID: string; costUSD?: number; atMs?: number };

type TranscriptCursor = {
  // File identity; OMP rewrites transcripts through a temp file and a rename.
  ino?: bigint;
  offset: number;
  // Bytes after the last newline: a line still being appended.
  pending: Buffer;
  // Keyed by entry id so a rewritten transcript never counts a turn twice.
  turns: Map<string, TranscriptTurn>;
};

type SubagentUsageTracker = {
  // Resolves whether the subagent totals changed.
  scan(): Promise<boolean>;
  turns(): Iterable<TranscriptTurn>;
};

type ModelSpend = {
  provider: string;
  name: string;
  costUSD: number;
  priced: boolean; // false when any of its turns lacks a recorded price
  turns: number;
};

type SessionCosts = {
  // Undefined when any turn lacks a recorded price: a partial sum would read as the total.
  costUSD?: number;
  turns: number;
  subagentCostUSD: number;
  subagentTurns: number;
  models: ModelSpend[]; // main agent's models first
  // Turns a fork copied from its parent session: spent (and counted) there.
  inheritedCostUSD: number;
  inheritedTurns: number;
};

const STATUS_KEY = "openusage-quota";
// The JSON schema this extension and `openusage omp-statusline` exchange;
// matches ompExtensionSchema in cmd/openusage/omp_statusline.go.
const OPENUSAGE_SCHEMA = 3;
const SCHEMA_MISMATCH_STATUS = "OpenUsage: openusage.exe and this extension are different builds; reinstall both, restart OMP";
const REFRESH_INTERVAL_MS = 60_000;
// Timers drift; a tick a few seconds early must not skip a whole cycle.
const REFRESH_SLACK_MS = 5_000;
const TOOL_COST_REFRESH_INTERVAL_MS = 5 * 60_000;
// Quota measured longer ago than this is labelled with its age (the Go
// renderer uses the same threshold for statusline cells).
const QUOTA_STALE_AFTER_MS = 15 * 60_000;
// A provider missing from a check keeps its last reading for this long.
const QUOTA_KEEP_MS = 60 * 60_000;
// `omp usage` alone may use 25 s: OMP's per-credential budget plus startup.
const QUOTA_TIMEOUT_MS = 35_000;
const COMMAND_TIMEOUT_MS = 15_000;
const COST_TIMEOUT_MS = 45_000;
// Forcing `omp usage invalidate` hits the vendors' rate-limited endpoints.
const FRESH_REFRESH_MIN_INTERVAL_MS = 60_000;
const STATUS_REFRESH_INTERVAL_MS = 5 * 60_000;
const STATUS_TIMEOUT_MS = 6_000;
// OMP persists a finished turn on a promise chain right after message_end;
// a short delay lets it land and merges bursts of tool results.
const PUBLISH_DEBOUNCE_MS = 150;
const SUBAGENT_SCAN_DEBOUNCE_MS = 500;
const DASHBOARD_HISTORY_DAYS = 14;
const LIMIT_HISTORY_DAYS = 7;
const LIMIT_HISTORY_REFRESH_MS = 30 * 60_000;
const DASHBOARD_TICK_MS = 10_000;
// Progress is coalesced (~150 ms) and lifecycle covers start/finish/abort; the
// raw `task:subagent:event` stream fires per token and would only add churn.
const SUBAGENT_CHANNELS = ["task:subagent:progress", "task:subagent:lifecycle"];

// Statuspage feeds, and the components OMP's traffic to each subscription uses.
const STATUS_PAGES: Record<string, { url: string; components: string[] }> = {
  claude: {
    url: "https://status.claude.com/api/v2/summary.json",
    components: ["Claude Code", "Claude API (api.anthropic.com)"],
  },
  chatgpt: {
    url: "https://status.openai.com/api/v2/summary.json",
    components: ["Codex API", "CLI"],
  },
};

// Subagent sessions rebind this module's factory in-process, so module state is
// shared: headless instances ping the UI instance whenever a subagent at any
// depth finishes an assistant message.
const subagentUsageListeners = new Set<() => void>();

function signalSubagentUsage(): void {
  for (const listener of subagentUsageListeners) {
    listener();
  }
}

// ---------------------------------------------------------------------------
// openusage process boundary
// ---------------------------------------------------------------------------

function runOpenUsage(
  args: string[],
  options: { timeoutMs: number; maxBuffer: number; stdin?: string },
): Promise<string | undefined> {
  const { promise, resolve } = Promise.withResolvers<string | undefined>();
  const child = execFile(
    "openusage",
    args,
    { encoding: "utf8", maxBuffer: options.maxBuffer, timeout: options.timeoutMs, windowsHide: true },
    (error, stdout) => resolve(error ? undefined : stdout),
  );
  if (options.stdin !== undefined) {
    // A missing or crashed binary closes stdin early; the exec callback reports it.
    child.stdin?.on("error", () => {});
    child.stdin?.end(options.stdin);
  }
  return promise;
}

function parseJSONObject(text: string | undefined): Record<string, unknown> | undefined {
  if (text === undefined) {
    return undefined;
  }
  try {
    const value: unknown = JSON.parse(text);
    return typeof value === "object" && value !== null ? value as Record<string, unknown> : undefined;
  } catch {
    return undefined;
  }
}

function isAmount(value: unknown): value is number {
  return typeof value === "number" && Number.isFinite(value) && value >= 0;
}

// Resolves undefined only when openusage could not run or printed no JSON
// object; a response from a differently versioned build keeps its `schema`
// so the caller can say so.
async function readQuotaSummary(fresh: boolean): Promise<QuotaSummary | undefined> {
  const args = ["omp-statusline", "--json"];
  if (fresh) {
    args.push("--fresh");
  }
  const summary = parseJSONObject(await runOpenUsage(args, { timeoutMs: QUOTA_TIMEOUT_MS, maxBuffer: 256 * 1024 }));
  if (!summary) {
    return undefined;
  }
  const providers = (Array.isArray(summary.providers) ? summary.providers : []).filter((provider): provider is ProviderQuota =>
    typeof provider === "object" && provider !== null &&
    typeof provider.key === "string" && typeof provider.name === "string" && Array.isArray(provider.windows),
  ).map((provider) => ({
    ...provider,
    windows: provider.windows.filter((window) =>
      typeof window.label === "string" && isAmount(window.usedFraction),
    ),
  }));
  return {
    schema: typeof summary.schema === "number" ? summary.schema : 0,
    providers,
    error: typeof summary.error === "string" ? summary.error : undefined,
  };
}

async function readLocalCosts(
  historyDays: number,
  windows: ReadonlyArray<{ key: string; since: number }>,
): Promise<LocalCosts | undefined> {
  const args = ["omp-statusline", "costs"];
  if (historyDays > 0) {
    args.push("--days", String(historyDays));
  }
  for (const window of windows) {
    args.push("--window", `${window.key}=${Math.round(window.since)}`);
  }
  const costs = parseJSONObject(await runOpenUsage(args, { timeoutMs: COST_TIMEOUT_MS, maxBuffer: 2 * 1024 * 1024 }));
  if (!costs || costs.schema !== OPENUSAGE_SCHEMA) {
    return undefined;
  }
  const block = costs.block as Partial<BlockDetails> | undefined;
  const tokens = costs.tokens as { today?: TokenTotals; week?: TokenTotals } | undefined;
  return {
    block: block && isAmount(block.costUSD) && isAmount(block.burnUSDHour) && isAmount(block.projectedUSD) &&
        isAmount(block.startedAt) && isAmount(block.endsAt) && isAmount(block.lastActivityAt)
      ? block as BlockDetails
      : undefined,
    todayUSD: isAmount(costs.todayUSD) ? costs.todayUSD : undefined,
    lastHourUSD: isAmount(costs.lastHourUSD) ? costs.lastHourUSD : undefined,
    windows: Array.isArray(costs.windows) ? costs.windows as WindowSpend[] : undefined,
    days: Array.isArray(costs.days) ? costs.days as DayCost[] : undefined,
    hours: Array.isArray(costs.hours) && costs.hours.length === 24 && costs.hours.every(isAmount)
      ? costs.hours
      : undefined,
    models: Array.isArray(costs.models) ? costs.models as CostShare[] : undefined,
    projects: Array.isArray(costs.projects) ? costs.projects as CostShare[] : undefined,
    sessions: Array.isArray(costs.sessions) ? costs.sessions as SessionSpend[] : undefined,
    tokens: tokens?.today && tokens.week ? { today: tokens.today, week: tokens.week } : undefined,
    monthUSD: isAmount(costs.monthUSD) ? costs.monthUSD : undefined,
    monthProjectedUSD: isAmount(costs.monthProjectedUSD) ? costs.monthProjectedUSD : undefined,
  };
}

async function readLimitHistory(days: number): Promise<LimitHistory | undefined> {
  const history = parseJSONObject(await runOpenUsage(
    ["omp-statusline", "history", "--days", String(days)],
    { timeoutMs: QUOTA_TIMEOUT_MS, maxBuffer: 1024 * 1024 },
  ));
  if (!history || history.schema !== OPENUSAGE_SCHEMA || !Array.isArray(history.providers)) {
    return undefined;
  }
  return { days: typeof history.days === "number" ? history.days : days, providers: history.providers as LimitHistory["providers"] };
}

// The alert rules live in Go (`omp-statusline alerts`); this side only keeps
// the opaque state between checks and shows what comes back.
async function evaluateAlerts(summary: QuotaSummary, state: unknown): Promise<{ alerts: QuotaAlert[]; state: unknown } | undefined> {
  const result = parseJSONObject(await runOpenUsage(
    ["omp-statusline", "alerts"],
    { timeoutMs: COMMAND_TIMEOUT_MS, maxBuffer: 256 * 1024, stdin: JSON.stringify({ summary, state: state ?? null }) },
  ));
  if (!result || result.schema !== OPENUSAGE_SCHEMA || !Array.isArray(result.alerts)) {
    return undefined;
  }
  const alerts = result.alerts.filter((alert): alert is QuotaAlert =>
    typeof alert === "object" && alert !== null && typeof alert.message === "string" &&
    (alert.level === "warning" || alert.level === "info"),
  );
  return { alerts, state: result.state };
}

function localDate(now: Date): string {
  const year = now.getFullYear();
  const month = String(now.getMonth() + 1).padStart(2, "0");
  const day = String(now.getDate()).padStart(2, "0");
  return `${year}-${month}-${day}`;
}

// A daily report's total, or undefined when unusable: tokens without any
// price would read as a free day.
function dailyReportCost(stdout: string | undefined): number | undefined {
  const report = parseJSONObject(stdout);
  const totals = report?.totals as { cost_usd?: unknown; total_tokens?: unknown } | undefined;
  const cost = totals?.cost_usd;
  const tokens = totals?.total_tokens;
  if (report?.kind !== "daily" || !Array.isArray(report.rows) || !isAmount(cost) || !isAmount(tokens) ||
      (tokens > 0 && cost === 0)) {
    return undefined;
  }
  return cost;
}

async function readDailyCost(provider: string, date: string): Promise<number | undefined> {
  return dailyReportCost(await runOpenUsage(
    [
      "daily", "--provider", provider,
      "--source", "direct", "--offline",
      "--since", date, "--until", date, "--json",
    ],
    { timeoutMs: COST_TIMEOUT_MS, maxBuffer: 256 * 1024 },
  ));
}

async function renderStatusLine(values: Record<string, unknown>): Promise<RenderResult> {
  const rendered = parseJSONObject(await runOpenUsage(
    ["omp-statusline", "--render", "--json"],
    { timeoutMs: COMMAND_TIMEOUT_MS, maxBuffer: 256 * 1024, stdin: JSON.stringify(values) },
  ));
  if (!rendered) {
    return { kind: "failed" };
  }
  if (rendered.schema !== OPENUSAGE_SCHEMA) {
    return { kind: "mismatch" };
  }
  // Invalid output is not safe to show in the footer.
  if (typeof rendered.status !== "string" || typeof rendered.color !== "boolean" ||
      !Array.isArray(rendered.quotas) || !rendered.quotas.every((quota) => typeof quota === "string")) {
    return { kind: "failed" };
  }
  return {
    kind: "ok",
    rendered: {
      status: rendered.status,
      quotas: rendered.quotas,
      cellStatus: rendered.cellStatus as RenderedOmpStatus["cellStatus"],
      color: rendered.color,
      quotaDisplay: rendered.quotaDisplay === "left" ? "left" : "used",
      providerStatus: rendered.providerStatus !== false,
    },
  };
}

// ---------------------------------------------------------------------------
// Provider status pages
// ---------------------------------------------------------------------------

type StatuspageSummary = {
  components?: Array<{ name?: string; status?: string }>;
  incidents?: Array<{ name?: string; status?: string; impact?: string; components?: Array<{ name?: string }> }>;
};

const COMPONENT_LEVELS: Record<string, IncidentLevel> = {
  degraded_performance: "minor",
  partial_outage: "major",
  major_outage: "critical",
  under_maintenance: "maintenance",
};

const IMPACT_LEVELS: Record<string, IncidentLevel> = {
  minor: "minor",
  major: "major",
  critical: "critical",
  maintenance: "maintenance",
};

const LEVEL_RANK: Record<IncidentLevel, number> = { maintenance: 1, minor: 2, major: 3, critical: 4 };

// The worst open incident or degraded component that OMP's traffic to this
// subscription depends on: null when all of them are operational, undefined
// when the page could not be read.
async function readProviderStatus(page: { url: string; components: string[] }): Promise<ProviderIncident | null | undefined> {
  let summary: StatuspageSummary;
  try {
    const response = await fetch(page.url, {
      signal: AbortSignal.timeout(STATUS_TIMEOUT_MS),
      headers: { accept: "application/json" },
    });
    if (!response.ok) {
      return undefined;
    }
    summary = await response.json() as StatuspageSummary;
  } catch {
    return undefined;
  }
  let worst: ProviderIncident | null = null;
  const consider = (level: IncidentLevel | undefined, title: string): void => {
    if (level && (!worst || LEVEL_RANK[level] > LEVEL_RANK[worst.indicator])) {
      worst = {
        indicator: level,
        title: stripVTControlCharacters(title).replace(/[\u0000-\u001f\u007f]/g, " ").replace(/\s+/g, " ").trim().slice(0, 80),
      };
    }
  };
  for (const incident of summary.incidents ?? []) {
    if (incident.status === "resolved" || incident.status === "postmortem") {
      continue;
    }
    const level = IMPACT_LEVELS[incident.impact ?? ""];
    const components = incident.components ?? [];
    const affected = components.some((component) => page.components.includes(component.name ?? ""));
    // An incident that names no components counts only when it is severe.
    if (affected || (components.length === 0 && (level === "major" || level === "critical"))) {
      consider(level, incident.name ?? "Incident");
    }
  }
  for (const component of summary.components ?? []) {
    if (component.name && page.components.includes(component.name)) {
      consider(COMPONENT_LEVELS[component.status ?? ""], `${component.name}: ${(component.status ?? "").replaceAll("_", " ")}`);
    }
  }
  return worst;
}

// ---------------------------------------------------------------------------
// Session costs
// ---------------------------------------------------------------------------

// usage.cost.total is priced by OMP with the model that produced the turn, so a
// subagent's turns carry its own model's price, never the parent's.
function usageTurn(entry: SessionEntry): TranscriptTurn | undefined {
  let source: { provider?: string; model?: string; usage?: Usage } | undefined;
  if (entry.type === "message" && entry.message?.role === "assistant") {
    source = entry.message;
  } else if (entry.type === "model_usage") {
    source = entry;
  }
  if (!source?.usage) {
    return undefined;
  }
  const provider = source.provider ?? "";
  const modelID = source.model ?? "Unknown model";
  const amount = source.usage.cost?.total;
  const atMs = entry.timestamp === undefined ? Number.NaN : Date.parse(entry.timestamp);
  return {
    key: `${provider}/${modelID}`,
    provider,
    modelID,
    costUSD: isAmount(amount) ? amount : undefined,
    atMs: Number.isFinite(atMs) ? atMs : undefined,
  };
}

function modelNames(context: OmpExtensionContext): Map<string, string> {
  return new Map(context.models.list().map((model) => [`${model.provider}/${model.id}`, model.name]));
}

function sessionCosts(
  context: OmpExtensionContext,
  names: ReadonlyMap<string, string>,
  subagentTurns: Iterable<TranscriptTurn>,
): SessionCosts {
  const models = new Map<string, ModelSpend>();
  const costs: SessionCosts = {
    turns: 0, subagentCostUSD: 0, subagentTurns: 0, models: [], inheritedCostUSD: 0, inheritedTurns: 0,
  };
  // A fork or continuation starts with a copy of its parent's history (and
  // subagent transcripts), each turn keeping its original timestamp. That
  // money was spent, and is counted, in the parent session.
  const header = context.sessionManager.getHeader?.();
  const forkedAtMs = header?.parentSession ? Date.parse(header.timestamp) : Number.NaN;
  let total = 0;
  let complete = true;
  const add = (turn: TranscriptTurn, subagent: boolean): void => {
    if (turn.atMs !== undefined && turn.atMs < forkedAtMs) {
      costs.inheritedTurns++;
      costs.inheritedCostUSD += turn.costUSD ?? 0;
      return;
    }
    let model = models.get(turn.key);
    if (!model) {
      model = {
        provider: turn.provider,
        name: names.get(turn.key) || turn.modelID,
        costUSD: 0,
        priced: true,
        turns: 0,
      };
      models.set(turn.key, model);
    }
    model.turns++;
    costs.turns++;
    if (subagent) {
      costs.subagentTurns++;
    }
    if (turn.costUSD === undefined) {
      model.priced = false;
      complete = false;
      return;
    }
    model.costUSD += turn.costUSD;
    total += turn.costUSD;
    if (subagent) {
      costs.subagentCostUSD += turn.costUSD;
    }
  };
  // Main turns first keeps the main agent's models leading the model row.
  for (const entry of context.sessionManager.getEntries()) {
    const turn = usageTurn(entry);
    if (turn) {
      add(turn, false);
    }
  }
  for (const turn of subagentTurns) {
    add(turn, true);
  }
  // A new session has spent nothing yet: $0.00, not unknown.
  costs.costUSD = complete ? total : undefined;
  costs.models = [...models.values()];
  return costs;
}

async function listTranscripts(dir: string): Promise<string[]> {
  let entries;
  try {
    entries = await readdir(dir, { withFileTypes: true });
  } catch {
    return [];
  }
  const files: string[] = [];
  for (const entry of entries) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) {
      files.push(...await listTranscripts(path));
    } else if (entry.isFile() && entry.name.endsWith(".jsonl")) {
      files.push(path);
    }
  }
  return files;
}

// Whether the bytes consumed so far are still the file's prefix at the
// boundary we resume from: the last complete line ends in a newline at the
// same place and the partial line after it is unchanged. An in-place
// rewrite that kept the size would otherwise resume mid-entry.
async function resumesCleanly(handle: FileHandle, cursor: TranscriptCursor): Promise<boolean> {
  const boundary = cursor.offset - cursor.pending.length;
  if (boundary === 0 && cursor.pending.length === 0) {
    return true;
  }
  const start = Math.max(0, boundary - 1);
  const probe = Buffer.alloc(cursor.offset - start);
  const { bytesRead } = await handle.read(probe, 0, probe.length, start);
  if (bytesRead !== probe.length) {
    return false;
  }
  const newlineOK = boundary === 0 || probe[0] === 0x0a;
  return newlineOK && probe.subarray(boundary === 0 ? 0 : 1).equals(cursor.pending);
}

function resetCursor(cursor: TranscriptCursor): boolean {
  const hadTurns = cursor.turns.size > 0;
  cursor.offset = 0;
  cursor.pending = Buffer.alloc(0);
  cursor.turns.clear();
  return hadTurns;
}

// Reads only the bytes appended since the last scan; returns whether the
// counted turns changed. A replaced or rewritten file is recounted from the
// start. A read error keeps the cursor for the next scan.
async function advanceTranscript(file: string, cursor: TranscriptCursor): Promise<boolean> {
  let handle: FileHandle | undefined;
  let changed = false;
  try {
    handle = await open(file, "r");
    const stat = await handle.stat({ bigint: true });
    const size = Number(stat.size);
    const replaced = cursor.ino !== undefined && stat.ino !== cursor.ino;
    cursor.ino = stat.ino;
    if (replaced || size < cursor.offset || !await resumesCleanly(handle, cursor)) {
      changed = resetCursor(cursor);
    }
    if (size === cursor.offset) {
      return changed;
    }
    const appended = Buffer.alloc(size - cursor.offset);
    const { bytesRead } = await handle.read(appended, 0, appended.length, cursor.offset);
    cursor.offset += bytesRead;
    const data = Buffer.concat([cursor.pending, appended.subarray(0, bytesRead)]);
    const lastNewline = data.lastIndexOf(0x0a);
    cursor.pending = Buffer.from(data.subarray(lastNewline + 1));
    for (const line of data.subarray(0, Math.max(lastNewline, 0)).toString("utf8").split("\n")) {
      // Cheap prefilter: tool results dominate transcript bytes.
      if (!line.includes('"assistant"') && !line.includes('"model_usage"')) {
        continue;
      }
      let entry: SessionEntry;
      try {
        entry = JSON.parse(line) as SessionEntry;
      } catch {
        continue;
      }
      const turn = usageTurn(entry);
      if (turn) {
        cursor.turns.set(entry.id ?? `@${cursor.turns.size}`, turn);
        changed = true;
      }
    }
    return changed;
  } catch {
    return changed;
  } finally {
    await handle?.close();
  }
}

// Totals every subagent transcript (task, eval agent(), workpool, advisor —
// nested ones included) under the main session's artifacts directory. Killed
// agents keep their transcript, so cancelled work stays counted.
function createSubagentUsageTracker(dir: string): SubagentUsageTracker {
  const cursors = new Map<string, TranscriptCursor>();
  return {
    async scan() {
      const files = await listTranscripts(dir);
      const present = new Set(files);
      let changed = false;
      for (const [file, cursor] of cursors) {
        if (!present.has(file)) {
          changed ||= cursor.turns.size > 0;
          cursors.delete(file);
        }
      }
      for (const file of files) {
        let cursor = cursors.get(file);
        if (!cursor) {
          cursor = { offset: 0, pending: Buffer.alloc(0), turns: new Map() };
          cursors.set(file, cursor);
        }
        changed = await advanceTranscript(file, cursor) || changed;
      }
      return changed;
    },
    *turns() {
      for (const cursor of cursors.values()) {
        yield* cursor.turns.values();
      }
    },
  };
}

// ---------------------------------------------------------------------------
// Below-footer statusline widget
// ---------------------------------------------------------------------------

type StatusRow = { blocks: string[]; kind: "cost" | "provider" | "models" };

// Stock OMP silently treats unknown placements as "aboveEditor"; "belowEditor"
// keeps the widget next to the footer until a patched build is reinstalled.
function widgetPlacement(ui: OmpUI): WidgetPlacement {
  return typeof ui.getNativeFooter === "function" ? "belowFooter" : "belowEditor";
}

// Splits the rendered rows into cells: icons occupy the native brand column,
// and text starts under its model column.
function statusRows(rendered: RenderedOmpStatus): StatusRow[] {
  const rows = rendered.status ? [rendered.status, ...rendered.quotas] : rendered.quotas;
  return rows.map((row, index) => {
    const plain = stripVTControlCharacters(row);
    const isCostRow = rendered.status !== "" && index === 0;
    const isProviderRow = plain.startsWith("🐙 ");
    const blocks = (isCostRow || isProviderRow ? plain.replace(/ [|/] /g, " · ") : plain)
      .trim().split(" · ");
    const icon = /^(💰|🔥|🐙) (.*)$/u.exec(blocks[0]);
    if (icon) {
      blocks.splice(0, 1, icon[1], icon[2]);
    } else {
      blocks.unshift("");
    }
    return { blocks, kind: isCostRow ? "cost" : isProviderRow ? "provider" : "models" };
  });
}

// Widest non-final cell per column: the width a cell needs so the next
// column's separator lines up across rows (and with OMP's native footer).
function statusColumnWidths(rows: readonly StatusRow[]): number[] {
  const widths: number[] = [];
  for (const row of rows) {
    row.blocks.slice(0, -1).forEach((cell, index) => {
      widths[index] = Math.max(widths[index] ?? 0, visibleWidth(cell));
    });
  }
  return widths;
}

// Column starts that fit every row on one line: the native footer's own
// separators when they leave room, else OpenUsage's column widths starting
// under the native model column, else the native columns with wrapping.
function statusColumnStarts(
  rows: readonly StatusRow[],
  columnWidths: readonly number[],
  nativeRow: string | undefined,
  width: number,
): number[] {
  const fits = (starts: readonly number[]): boolean => rows.every((row) => {
    if (row.blocks.length > starts.length) {
      return false;
    }
    return row.blocks.every((cell, index) => {
      const end = index + 1 < row.blocks.length ? starts[index + 1] - 3 : width - 1;
      return starts[index] + visibleWidth(cell) <= end;
    });
  });

  let native: number[] | undefined;
  if (nativeRow) {
    native = [1];
    for (const separator of nativeRow.matchAll(/ · /g)) {
      const start = visibleWidth(nativeRow.slice(0, separator.index)) + 3;
      if (start < width - 1) {
        native.push(start);
      }
    }
    if (fits(native)) {
      return native;
    }
  }
  const own = [1];
  const firstText = Math.max(native?.[1] ?? 0, 1 + (columnWidths[0] ?? 0) + 3);
  for (let index = 1; index <= columnWidths.length; index++) {
    own.push(index === 1 ? firstText : own[index - 1] + columnWidths[index - 1] + 3);
  }
  if (fits(own)) {
    return own;
  }
  return native ?? own.filter((start) => start < width - 1);
}

function createStatusWidget(
  rendered: RenderedOmpStatus,
  ui: OmpUI,
): Component & { readonly footerColumnWidths: readonly number[] } {
  const rows = statusRows(rendered);
  const footerColumnWidths = statusColumnWidths(rows);

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

  function styleCell(cell: string, row: StatusRow, index: number): string {
    const theme = ui.theme;
    if (!rendered.color) {
      return cell;
    }
    if (row.kind === "provider" && index === 1) {
      return theme.bold(theme.fg("warning", cell));
    }
    if (cell === "n/a") {
      return theme.fg("muted", cell);
    }
    const pressure = rendered.cellStatus?.[cell];
    if (pressure === "stale") {
      return theme.fg("muted", cell);
    }
    // Incident and sign-in cells are words, not amounts: colour them whole.
    if (pressure && !/\d%/.test(cell)) {
      return theme.fg(pressure === "exhausted" ? "error" : "warning", cell);
    }
    const textColor: ThemeColor = row.kind === "models" ? "statusLineModel" : "statusLineContext";
    const amountColor: ThemeColor = pressure === "exhausted" ? "error" : pressure === "warning" ? "warning" : "statusLinePath";
    // Style each run separately: an amount's reset must not erase the theme
    // colour of the label or countdown that follows it.
    return cell.split(/(\$[\d,.]+|\d+(?:\.\d+)?%)/g).map((part, partIndex) =>
      theme.fg(partIndex % 2 === 1 ? amountColor : textColor, part),
    ).join("");
  }

  function renderRows(width: number): string[] {
    if (width <= 2) {
      return [];
    }
    const nativeRow = typeof ui.getNativeFooter === "function"
      ? ui.getNativeFooter(width).map(stripVTControlCharacters).find((line) => line.includes(" · "))
      : undefined;
    const columns = statusColumnStarts(rows, footerColumnWidths, nativeRow, width);
    const separatorText = rendered.color ? ui.theme.fg("statusLineSep", " · ") : " · ";
    const output: string[] = [];

    for (const row of rows) {
      // When nothing fits on one line, retain the remaining fields on
      // continuation lines in the last column rather than hiding them.
      const cells = row.blocks.slice(0, columns.length);
      if (row.blocks.length > columns.length) {
        cells[columns.length - 1] = row.blocks.slice(columns.length - 1).filter(Boolean).join("\n");
      }
      const wrapped = cells.map((cell, index) => {
        const end = index + 1 < cells.length ? columns[index + 1] - 3 : width - 1;
        const styled = cell.split("\n").map((part) => styleCell(part, row, index)).join("\n");
        return wrapTextWithAnsi(styled, Math.max(1, end - columns[index]));
      });
      const height = Math.max(...wrapped.map((cell) => cell.length));
      for (let lineIndex = 0; lineIndex < height; lineIndex++) {
        let line = " ";
        for (let column = 0; column < wrapped.length; column++) {
          if (column > 0) {
            const gap = Math.max(0, columns[column] - 3 - visibleWidth(line));
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

// ---------------------------------------------------------------------------
// /usage dashboard: formatting rules (pure)
// ---------------------------------------------------------------------------

type Tone = "ok" | "warning" | "error" | "dim";

type Pace = { text: string; tone: Tone };

const WEEKDAYS = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const EIGHTH_BLOCKS = ["", "▏", "▎", "▍", "▌", "▋", "▊", "▉"];
const SPARK_BLOCKS = ["▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"];
const DAY_MS = 24 * 60 * 60_000;

const usd = new Intl.NumberFormat("en-US", { style: "currency", currency: "USD" });
const compact = new Intl.NumberFormat("en-US", { notation: "compact", maximumFractionDigits: 1 });

// 37m, 5h12m, 6d14h — the same shape as the statusline countdowns.
function shortDuration(durationMs: number): string {
  const minutes = Math.max(1, Math.ceil(durationMs / 60_000));
  if (minutes < 60) {
    return `${minutes}m`;
  }
  const hours = Math.floor(minutes / 60);
  if (hours < 24) {
    return minutes % 60 === 0 ? `${hours}h` : `${hours}h${minutes % 60}m`;
  }
  const days = Math.floor(hours / 24);
  return hours % 24 === 0 ? `${days}d` : `${days}d${hours % 24}h`;
}

function elapsedLabel(sinceMs: number, now: number): string {
  const seconds = Math.max(0, Math.round((now - sinceMs) / 1000));
  if (seconds < 45) {
    return "just now";
  }
  return `${shortDuration(seconds * 1000)} ago`;
}

// "14:05" today, "Sat 09:40" within the coming week.
function clockLabel(atMs: number, now: number): string {
  const at = new Date(atMs);
  const time = `${String(at.getHours()).padStart(2, "0")}:${String(at.getMinutes()).padStart(2, "0")}`;
  return localDate(at) === localDate(new Date(now)) ? time : `${WEEKDAYS[at.getDay()]} ${time}`;
}

function usedPercent(usedFraction: number): number {
  const percent = Math.round(usedFraction * 100);
  // Never claim a limit is reached before it is.
  return percent >= 100 && usedFraction < 1 ? 99 : Math.min(percent, 999);
}

function runsOutBeforeReset(window: QuotaWindow): window is QuotaWindow & { exhaustsAt: number; resetsAt: number } {
  return window.exhaustsAt !== undefined && window.resetsAt !== undefined &&
    window.exhaustsAt < window.resetsAt && window.usedFraction < 1;
}

// The projection itself is openusage's (ompPace), made from the moment OMP
// measured the window; only the countdown to it is computed here. A 7d
// window that lasts also gets the share it can still spend per day.
function paceFor(window: QuotaWindow, now: number): Pace | undefined {
  if (window.resetsAt !== undefined && window.resetsAt <= now) {
    return { text: "window has reset", tone: "dim" };
  }
  if (window.usedFraction >= 1) {
    return { text: "limit reached", tone: "error" };
  }
  if (runsOutBeforeReset(window)) {
    return { text: `at this pace, runs out in ~${shortDuration(Math.max(0, window.exhaustsAt - now))}`, tone: "warning" };
  }
  const parts: string[] = [];
  let tone: Tone = "ok";
  if (window.projectedFraction !== undefined) {
    parts.push(`on pace for ~${Math.round(window.projectedFraction * 100)}% by reset`);
    tone = window.projectedFraction >= 0.9 ? "warning" : "ok";
  }
  // Example: 18% used with 6.6 days left leaves 82% / 6.6 ≈ 12% per day.
  const daysLeft = window.resetsAt === undefined ? 0 : (window.resetsAt - now) / DAY_MS;
  if (daysLeft >= 1) {
    parts.push(`budget ≈${Math.floor((100 - usedPercent(window.usedFraction)) / daysLeft)}%/day until reset`);
  }
  return parts.length > 0 ? { text: parts.join(" · "), tone } : undefined;
}

// Fraction of the window already elapsed, for the bar's pace marker.
function elapsedFraction(window: QuotaWindow, now: number): number | undefined {
  if (!window.durationMs || window.resetsAt === undefined || window.resetsAt <= now) {
    return undefined;
  }
  const fraction = 1 - (window.resetsAt - now) / window.durationMs;
  return fraction >= 0 && fraction <= 1 ? fraction : undefined;
}

// Share of input served from the prompt cache. OMP counts `input` without
// the cached part, so input + cacheRead + cacheWrite is all input.
function cacheShare(tokens: TokenTotals): number | undefined {
  const input = tokens.input + tokens.cacheRead + tokens.cacheWrite;
  return input > 0 ? tokens.cacheRead / input : undefined;
}

function sparkline(values: ReadonlyArray<number | undefined>): string {
  return values.map((value) => value === undefined
    ? "·"
    : SPARK_BLOCKS[Math.min(7, Math.max(0, Math.ceil(Math.min(value, 1) * 8) - 1))]).join("");
}

// ---------------------------------------------------------------------------
// /usage dashboard: rendering
// ---------------------------------------------------------------------------

type Style = {
  fg(color: ThemeColor, text: string): string;
  bold(text: string): string;
};

type DashboardData = {
  now: number;
  quota?: QuotaSummary;
  quotaError?: string;
  quotaLoading: boolean;
  costs?: LocalCosts;
  history?: LocalCosts;
  historyLoading: boolean;
  limitHistory?: LimitHistory;
  toolCosts: ToolCosts;
  toolCostsLoading: boolean;
  // The statusline's "today est": OMP plus the other CLIs with a usable report.
  todayUSD?: number;
  session?: SessionCosts;
  names: ReadonlyMap<string, string>;
  statuses: Readonly<Record<string, ProviderIncident>>;
  quotaDisplay: QuotaDisplay;
};

type DashboardSource = {
  // Bumped whenever any displayed data changes.
  version(): number;
  snapshot(): DashboardData;
  refresh(): void;
  subscribe(listener: () => void): () => void;
  readonly color: boolean;
};

const TONE_COLORS: Record<Tone, ThemeColor> = { ok: "success", warning: "warning", error: "error", dim: "dim" };
const QUOTA_TONES: Record<QuotaStatus, Tone> = { ok: "ok", warning: "warning", exhausted: "error" };
const INCIDENT_COLORS: Record<IncidentLevel, ThemeColor> = {
  maintenance: "accent",
  minor: "warning",
  major: "error",
  critical: "error",
};

function padEndTo(text: string, width: number): string {
  return text + " ".repeat(Math.max(0, width - visibleWidth(text)));
}

function padStartTo(text: string, width: number): string {
  return " ".repeat(Math.max(0, width - visibleWidth(text))) + text;
}

// A horizontal bar with eighth-block precision. `track` shades the unfilled
// part so the bar reads as a share of 100%; `marker` (0..1) draws a thin line
// at that position, e.g. how much of a quota window has elapsed.
function meter(
  style: Style,
  fraction: number,
  width: number,
  color: ThemeColor,
  options: { track: boolean; marker?: number },
): string {
  const eighths = Math.round(Math.min(Math.max(fraction, 0), 1) * width * 8);
  const markerCell = options.marker === undefined ? -1 : Math.min(width - 1, Math.floor(options.marker * width));
  const runs: Array<{ color: ThemeColor; text: string }> = [];
  for (let cell = 0; cell < width; cell++) {
    const filled = Math.min(8, Math.max(0, eighths - cell * 8));
    let run: { color: ThemeColor; text: string };
    if (cell === markerCell) {
      run = { color: "text", text: "│" };
    } else if (filled > 0) {
      run = { color, text: filled === 8 ? "█" : EIGHTH_BLOCKS[filled] };
    } else {
      run = { color: "dim", text: options.track ? "░" : " " };
    }
    const last = runs.at(-1);
    if (last?.color === run.color) {
      last.text += run.text;
    } else {
      runs.push(run);
    }
  }
  return runs.map((run) => style.fg(run.color, run.text)).join("");
}

function sectionHeading(style: Style, title: string, note?: string): string {
  const heading = style.bold(style.fg("accent", title));
  return note ? `${heading}  ${style.fg("dim", note)}` : heading;
}

function quotaPercentText(window: QuotaWindow, display: QuotaDisplay): string {
  const used = usedPercent(window.usedFraction);
  return display === "left" ? `${Math.max(0, 100 - used)}% left` : `${used}%`;
}

// "last 7 days: 5h ▁▃█▅▂█▇ limit hit 3× · 7d ▂▃▅▆▇██ peak 94%"
function limitHistoryLine(history: LimitHistory | undefined, key: string): string | undefined {
  const provider = history?.providers.find((candidate) => candidate.key === key);
  const windows = provider?.windows.filter((window) => !window.scoped) ?? [];
  if (windows.length === 0) {
    return undefined;
  }
  const parts = windows.map((window) => {
    const spark = sparkline(window.daily.map((day) => day.peakFraction));
    const summary = window.limitHits > 0
      ? `limit hit ${window.limitHits}×`
      : `peak ${window.peakFraction === undefined ? "n/a" : `${usedPercent(window.peakFraction)}%`}`;
    return `${window.label} ${spark} ${summary}`;
  });
  return `last ${history?.days ?? LIMIT_HISTORY_DAYS} days: ${parts.join(" · ")}`;
}

function limitsSection(data: DashboardData, style: Style, width: number): string[] {
  const quota = data.quota;
  const fetchedAt = quota?.providers.map((provider) => provider.fetchedAt ?? 0).filter((at) => at > 0) ?? [];
  const notes = fetchedAt.length > 0 ? [`checked ${elapsedLabel(Math.min(...fetchedAt), data.now)}`] : [];
  if (quota && data.quotaError) {
    // The last good reading is still shown; say that it could not be renewed.
    notes.push("latest check failed");
  }
  const lines = [sectionHeading(style, "Subscription limits", notes.join(" · ") || undefined)];
  if (!quota || quota.providers.length === 0) {
    const message = data.quotaLoading ? "Checking limits…" : data.quotaError ?? "OMP reported no subscription limits.";
    lines.push(`  ${style.fg("dim", message)}`);
    return lines;
  }

  const windowSpend = new Map((data.history?.windows ?? []).map((spend) => [spend.key, spend]));
  const spendText = (provider: ProviderQuota, window: QuotaWindow): string => {
    const spend = windowSpend.get(`${provider.key}/${window.label}`);
    return spend ? `${usd.format(spend.costUSD)} this window` : "";
  };
  const resetText = (window: QuotaWindow): string => {
    if (window.resetsAt === undefined) {
      return "";
    }
    if (window.resetsAt <= data.now) {
      return "reset since check";
    }
    return `resets in ${shortDuration(window.resetsAt - data.now)} (${clockLabel(window.resetsAt, data.now)})`;
  };
  const rows = quota.providers.flatMap((provider) => provider.windows.map((window) => ({ provider, window })));
  const labelWidth = Math.max(3, ...rows.map(({ window }) => visibleWidth(window.label)));
  const percentWidth = Math.max(4, ...rows.map(({ window }) => visibleWidth(quotaPercentText(window, data.quotaDisplay))));
  const resetWidth = Math.max(0, ...rows.map(({ window }) => visibleWidth(resetText(window))));
  const spendWidth = Math.max(0, ...rows.map(({ provider, window }) => visibleWidth(spendText(provider, window))));
  // label, bar, percentage, reset and spend on one line; the pace note joins
  // them when there is room and otherwise sits on the line below.
  const fixedWidth = 4 + labelWidth + 2 + 2 + percentWidth + 2 + resetWidth + (spendWidth > 0 ? 2 + spendWidth : 0);
  const inlinePace = width - fixedWidth - 12 >= 40;
  const barWidth = Math.max(8, Math.min(36, width - fixedWidth - (inlinePace ? 42 : 0)));

  for (const provider of quota.providers) {
    const stale = provider.fetchedAt !== undefined && data.now - provider.fetchedAt > QUOTA_STALE_AFTER_MS;
    lines.push("");
    let header = `  ${style.bold(style.fg("warning", provider.name))}`;
    if (provider.plan) {
      header += style.fg("dim", ` · ${provider.plan}`);
    }
    if (provider.accounts > 1) {
      header += style.fg("dim", ` · ${provider.accounts} accounts pooled`);
    }
    if (provider.resets) {
      const usable = provider.usableResets ?? provider.resets;
      header += style.fg(usable > 0 ? "success" : "warning",
        ` · ✦ ${provider.resets} saved reset${provider.resets === 1 ? "" : "s"}`);
      const details: string[] = [];
      if (usable !== provider.resets) {
        details.push(`${usable} usable now`);
      }
      if (provider.resetsExpireAt !== undefined && provider.resetsExpireAt > data.now) {
        details.push(`next expires in ${shortDuration(provider.resetsExpireAt - data.now)}`);
      }
      details.push("/usage reset");
      header += style.fg("dim", ` (${details.join(", ")})`);
    }
    if (stale && provider.fetchedAt !== undefined) {
      header += style.fg("warning", ` · as of ${elapsedLabel(provider.fetchedAt, data.now)}`);
    }
    lines.push(header);

    const notices: Array<{ color: ThemeColor; text: string }> = [];
    const incident = data.statuses[provider.key];
    if (incident) {
      notices.push({ color: INCIDENT_COLORS[incident.indicator], text: `⚠ ${incident.title}` });
    }
    if (provider.disabled) {
      notices.push({ color: "warning", text: "sign-in disabled — run /login" });
    }
    if (provider.unreported) {
      notices.push({
        color: "dim",
        text: `${provider.unreported} account${provider.unreported === 1 ? "" : "s"} reported no usage data`,
      });
    }
    if (provider.extra) {
      const extra = provider.extra;
      const text = extra.limitUSD !== undefined
        ? `extra usage ${usd.format(extra.usedUSD)} of ${usd.format(extra.limitUSD)}` +
          (extra.usedFraction !== undefined ? ` (${usedPercent(extra.usedFraction)}%)` : "")
        : `extra usage ${usd.format(extra.usedUSD)} spent, no cap`;
      notices.push({ color: "dim", text });
    }
    for (const notice of notices) {
      lines.push(`    ${style.fg(notice.color, truncateToWidth(notice.text, Math.max(10, width - 4)))}`);
    }
    if (provider.windows.length === 0 && notices.length === 0) {
      lines.push(`    ${style.fg("dim", "no limit windows reported")}`);
    }

    for (const window of provider.windows) {
      const reset = window.resetsAt !== undefined && window.resetsAt <= data.now;
      let tone: Tone = QUOTA_TONES[window.status] ?? "ok";
      if (reset || stale) {
        tone = "dim";
      } else if (tone === "ok" && runsOutBeforeReset(window)) {
        tone = "warning";
      }
      const percent = padStartTo(quotaPercentText(window, data.quotaDisplay), percentWidth);
      let line = `    ${style.fg(window.scoped ? "muted" : "text", padEndTo(window.label, labelWidth))}  ` +
        `${meter(style, window.usedFraction, barWidth, TONE_COLORS[tone], { track: true, marker: elapsedFraction(window, data.now) })}  ` +
        `${style.bold(style.fg(TONE_COLORS[tone], percent))}  ${style.fg("dim", padEndTo(resetText(window), resetWidth))}`;
      if (spendWidth > 0) {
        line += `  ${style.fg("statusLinePath", padEndTo(spendText(provider, window), spendWidth))}`;
      }
      // Stale numbers cannot support a projection.
      const pace = stale ? undefined : paceFor(window, data.now);
      if (pace && inlinePace) {
        line += `  ${style.fg(TONE_COLORS[pace.tone], pace.text)}`;
      }
      lines.push(line.trimEnd());
      if (pace && !inlinePace) {
        lines.push(`    ${" ".repeat(labelWidth + 2)}${style.fg(TONE_COLORS[pace.tone], pace.text)}`);
      }
    }
    const history = limitHistoryLine(data.limitHistory, provider.key);
    if (history) {
      lines.push(`    ${style.fg("dim", truncateToWidth(history, Math.max(10, width - 4)))}`);
    }
  }
  lines.push("", `  ${style.fg("dim", "│ marks elapsed window time; fill beyond it is faster than an even pace.")}`);
  return lines;
}

function spendSection(data: DashboardData, style: Style): string[] {
  const lines = [sectionHeading(style, "Spend", "API-equivalent estimates, not your subscription bill")];
  const rows: Array<{ label: string; amount: string; detail: string }> = [];

  const session = data.session;
  if (session) {
    const priced = [...session.models].filter((model) => model.priced).sort((a, b) => b.costUSD - a.costUSD);
    const parts = priced.slice(0, 3).map((model) => `${model.name} ${usd.format(model.costUSD)}`);
    if (session.models.length > 3) {
      parts.push(`+${session.models.length - 3} more`);
    }
    if (session.subagentTurns > 0) {
      parts.push(`subagents ${usd.format(session.subagentCostUSD)} of it`);
    }
    if (session.costUSD === undefined) {
      parts.unshift("some turns have no recorded price");
    }
    if (session.turns === 0) {
      parts.unshift("no turns yet");
    }
    if (session.inheritedTurns > 0) {
      parts.push(`excludes ${usd.format(session.inheritedCostUSD)} carried over from the parent session`);
    }
    rows.push({
      label: "This session",
      amount: session.costUSD === undefined ? "n/a" : usd.format(session.costUSD),
      detail: parts.join(" · "),
    });
  }

  const omp = data.costs?.todayUSD;
  const [codexText, claudeText] = [data.toolCosts.codex, data.toolCosts.claude].map((value) =>
    value !== undefined ? usd.format(value) : data.toolCostsLoading ? "…" : "n/a",
  );
  rows.push({
    label: "Today",
    amount: data.todayUSD === undefined ? "n/a" : usd.format(data.todayUSD),
    detail: `OMP ${omp === undefined ? "n/a" : usd.format(omp)} · Codex CLI ${codexText} · Claude Code ${claudeText}`,
  });

  if (data.costs?.lastHourUSD !== undefined) {
    rows.push({ label: "Last hour", amount: usd.format(data.costs.lastHourUSD), detail: "OMP, last 60 minutes" });
  }

  const block = data.costs?.block;
  if (block && block.endsAt > data.now) {
    const details = [
      `since ${clockLabel(block.startedAt, data.now)}`,
      `last turn ${elapsedLabel(block.lastActivityAt, data.now)}`,
      `${shortDuration(block.endsAt - data.now)} left`,
    ];
    if (block.burnUSDHour > 0) {
      let burn = `🔥 ${usd.format(block.burnUSDHour)}/hr`;
      if (block.projectedUSD > block.costUSD) {
        burn += ` → ~${usd.format(block.projectedUSD)} by ${clockLabel(block.endsAt, data.now)}`;
      }
      details.push(burn);
    }
    rows.push({ label: "5h block", amount: usd.format(block.costUSD), detail: details.join(" · ") });
  } else {
    rows.push({ label: "5h block", amount: "—", detail: "no active OMP activity block" });
  }

  const month = data.history?.monthUSD;
  if (month !== undefined) {
    const end = new Date(new Date(data.now).getFullYear(), new Date(data.now).getMonth() + 1, 0);
    const projected = data.history?.monthProjectedUSD;
    rows.push({
      label: "This month",
      amount: usd.format(month),
      detail: projected !== undefined
        ? `OMP · on pace for ~${usd.format(projected)} by ${MONTHS[end.getMonth()]} ${end.getDate()}`
        : "OMP",
    });
  }

  const amountWidth = Math.max(...rows.map((row) => visibleWidth(row.amount)));
  for (const row of rows) {
    lines.push(`    ${padEndTo(row.label, 13)}${style.bold(style.fg("statusLinePath", padStartTo(row.amount, amountWidth)))}` +
      `  ${style.fg("dim", row.detail)}`);
  }

  const tokens = data.history?.tokens;
  if (tokens) {
    const describe = (label: string, totals: TokenTotals): string => {
      const total = totals.input + totals.output + totals.cacheRead + totals.cacheWrite;
      const share = cacheShare(totals);
      return `${label} ${compact.format(total)}` + (share === undefined ? "" : ` (${Math.round(share * 100)}% of input from cache)`);
    };
    lines.push(`    ${padEndTo("Tokens", 13)}${style.fg("dim", `${describe("today", tokens.today)} · ${describe("7 days", tokens.week)}`)}`);
  }
  return lines;
}

function dailySection(data: DashboardData, style: Style, width: number): string[] {
  const history = data.history?.days;
  if (!history || history.length === 0) {
    return [
      sectionHeading(style, "Daily spend · OMP"),
      `  ${style.fg("dim", data.historyLoading ? `Loading ${DASHBOARD_HISTORY_DAYS}-day history…` : "History unavailable.")}`,
    ];
  }
  const total = history.reduce((sum, day) => sum + day.costUSD, 0);
  const week = history.slice(-7).reduce((sum, day) => sum + day.costUSD, 0);
  const activeDays = history.filter((day) => day.turns > 0).length;
  const totals = [`last 7 days ${usd.format(week)}`];
  if (usd.format(total) !== usd.format(week)) {
    totals.push(`last ${history.length} days ${usd.format(total)}`);
  }
  if (activeDays > 0) {
    totals.push(`${usd.format(total / activeDays)} per active day`);
  }
  const lines = [sectionHeading(style, "Daily spend · OMP", totals.join(" · "))];
  // Days before the first recorded turn carry no information; keep a week.
  const firstActive = history.findIndex((day) => day.turns > 0);
  const days = history.slice(Math.min(firstActive < 0 ? history.length : firstActive, history.length - 7));
  const today = localDate(new Date(data.now));
  const amounts = days.map((day) => `${day.unpriced ? "≥" : ""}${usd.format(day.costUSD)}`);
  const notes = days.map((day) =>
    [day.turns > 0 ? `${day.turns.toLocaleString("en-US")} turns` : "", day.date === today ? "today" : ""]
      .filter(Boolean).join(" · "),
  );
  const amountWidth = Math.max(...amounts.map((amount) => visibleWidth(amount)));
  const notesWidth = Math.max(...notes.map((note) => visibleWidth(note)));
  const barWidth = Math.max(8, Math.min(60, width - 4 - 6 - 2 - 2 - amountWidth - 2 - notesWidth));
  const busiest = Math.max(...days.map((day) => day.costUSD));
  days.forEach((day, index) => {
    const date = new Date(`${day.date}T12:00:00`);
    const isToday = day.date === today;
    const label = `${WEEKDAYS[date.getDay()]} ${String(date.getDate()).padStart(2, "0")}`;
    const fraction = busiest > 0 ? day.costUSD / busiest : 0;
    lines.push(
      `    ${isToday ? style.bold(style.fg("accent", label)) : style.fg("muted", label)}  ` +
      `${meter(style, fraction, barWidth, isToday ? "accent" : "statusLineModel", { track: false })}  ` +
      `${style.fg(day.costUSD > 0 ? "statusLinePath" : "dim", padStartTo(amounts[index], amountWidth))}  ` +
      `${style.fg(isToday ? "accent" : "dim", notes[index])}`.trimEnd(),
    );
  });
  const hours = data.history?.hours;
  if (hours && hours.some((cost) => cost > 0)) {
    lines.push("", ...hourlyLines(hours, data.now, style));
  }
  return lines;
}

// Today's OMP spend per clock hour, two cells per hour and aligned under the
// daily bars, with a time axis beneath. Hours still to come stay blank.
function hourlyLines(hours: number[], now: number, style: Style): string[] {
  const peak = Math.max(...hours);
  const currentHour = new Date(now).getHours();
  let bars = "";
  let axis = "";
  hours.forEach((cost, hour) => {
    const level = Math.min(7, Math.max(0, Math.ceil(cost / peak * 8) - 1));
    const cell = hour > currentHour || cost <= 0 ? "  " : SPARK_BLOCKS[level].repeat(2);
    bars += style.fg(hour === currentHour ? "accent" : "statusLineModel", cell);
    axis += hour % 6 === 0 ? String(hour).padStart(2, "0") : "  ";
  });
  const busiest = `busiest ${String(hours.indexOf(peak)).padStart(2, "0")}:00, ${usd.format(peak)}`;
  return [
    `    ${style.fg("muted", "Hourly")}  ${bars}  ${style.fg("dim", busiest)}`,
    `            ${style.fg("dim", axis.trimEnd())}`,
  ];
}

function breakdownSection(data: DashboardData, style: Style, width: number): string[] {
  const history = data.history;
  if (!history) {
    return [];
  }
  const table = (title: string, shares: CostShare[] | undefined, nameOf: (share: CostShare) => string): string[] => {
    const top = (shares ?? []).slice(0, 6);
    if (top.length === 0) {
      return [];
    }
    const names = top.map(nameOf);
    const nameWidth = Math.min(30, Math.max(...names.map((name) => visibleWidth(name)), visibleWidth(title)));
    const todayCells = top.map((share) => usd.format(share.todayUSD));
    const weekCells = top.map((share) => usd.format(share.weekUSD));
    const todayWidth = Math.max(5, ...todayCells.map((cell) => visibleWidth(cell)));
    const weekWidth = Math.max(6, ...weekCells.map((cell) => visibleWidth(cell)));
    const weekTotal = (shares ?? []).reduce((sum, share) => sum + share.weekUSD, 0);
    const barWidth = Math.max(0, Math.min(20, width - 4 - nameWidth - 2 - todayWidth - 2 - weekWidth - 2 - 5));
    const lines = [
      `  ${style.bold(style.fg("accent", padEndTo(title, nameWidth + 2)))}  ` +
      `${style.fg("dim", padStartTo("today", todayWidth))}  ${style.fg("dim", padStartTo("7 days", weekWidth))}`,
    ];
    top.forEach((share, index) => {
      const fraction = weekTotal > 0 ? share.weekUSD / weekTotal : 0;
      const bar = barWidth >= 4
        ? `  ${meter(style, fraction, barWidth, "statusLineModel", { track: false })} ${style.fg("dim", `${Math.round(fraction * 100)}%`)}`
        : "";
      lines.push(
        `    ${padEndTo(truncateToWidth(names[index], nameWidth), nameWidth)}  ` +
        `${style.fg(share.todayUSD > 0 ? "statusLinePath" : "dim", padStartTo(todayCells[index], todayWidth))}  ` +
        `${style.fg("statusLinePath", padStartTo(weekCells[index], weekWidth))}${bar}`,
      );
    });
    return lines;
  };
  const models = table("Top models", history.models, (share) =>
    data.names.get(`${share.provider}/${share.name}`) ?? share.name);
  const projects = table("Top projects", history.projects, (share) => share.name);
  if (models.length === 0 && projects.length === 0) {
    return [];
  }
  return [
    sectionHeading(style, "Where the OMP spend went"),
    ...models,
    ...(models.length > 0 && projects.length > 0 ? [""] : []),
    ...projects,
  ];
}

function sessionsSection(data: DashboardData, style: Style, width: number): string[] {
  const sessions = (data.history?.sessions ?? []).slice(0, 8);
  if (sessions.length === 0) {
    return [];
  }
  const titles = sessions.map((session) => session.title || `(untitled ${session.id.slice(0, 8)})`);
  const weekCells = sessions.map((session) => usd.format(session.costUSD));
  const todayCells = sessions.map((session) => session.todayUSD > 0 ? usd.format(session.todayUSD) : "—");
  const weekWidth = Math.max(6, ...weekCells.map((cell) => visibleWidth(cell)));
  const todayWidth = Math.max(5, ...todayCells.map((cell) => visibleWidth(cell)));
  const details = sessions.map((session) => [
    session.project,
    session.subagentUSD > 0 ? `subagents ${usd.format(session.subagentUSD)}` : "",
    `last active ${elapsedLabel(session.lastActivityAt, data.now)}`,
  ].filter(Boolean).join(" · "));
  const titleWidth = Math.max(12, Math.min(44, width - 4 - 2 - weekWidth - 2 - todayWidth - 2 - 30));
  const lines = [
    `${sectionHeading(style, "Top sessions", "last 7 days, subagents included")}`,
    `    ${padEndTo("", titleWidth)}  ${style.fg("dim", padStartTo("7 days", weekWidth))}  ${style.fg("dim", padStartTo("today", todayWidth))}`,
  ];
  sessions.forEach((_session, index) => {
    lines.push(
      `    ${padEndTo(truncateToWidth(titles[index], titleWidth), titleWidth)}  ` +
      `${style.fg("statusLinePath", padStartTo(weekCells[index], weekWidth))}  ` +
      `${style.fg(todayCells[index] === "—" ? "dim" : "statusLinePath", padStartTo(todayCells[index], todayWidth))}  ` +
      `${style.fg("dim", details[index])}`,
    );
  });
  return lines;
}

function dashboardContent(data: DashboardData, style: Style, width: number): string[] {
  const sections = [
    limitsSection(data, style, width),
    spendSection(data, style),
    dailySection(data, style, width),
    breakdownSection(data, style, width),
    sessionsSection(data, style, width),
  ].filter((section) => section.length > 0);
  return sections.flatMap((section, index) => index === 0 ? section : ["", ...section]);
}

function createUsageDashboard(
  tui: OmpTui,
  theme: Theme,
  source: DashboardSource,
  close: () => void,
): Component & { dispose(): void } {
  const style: Style = source.color
    ? { fg: (color, text) => theme.fg(color, text), bold: (text) => theme.bold(text) }
    : { fg: (_color, text) => text, bold: (text) => text };
  let scroll = 0;
  let viewportRows = 10;
  // Mouse motion alone makes OMP re-render the overlay many times a second;
  // content is rebuilt only when the data, the width or the clock tick moves.
  let contentCache: { key: string; lines: string[]; data: DashboardData } | undefined;
  const unsubscribe = source.subscribe(() => tui.requestRender());
  // Keeps countdowns and pace notes current while the dashboard stays open.
  const ticker = setInterval(() => tui.requestRender(), DASHBOARD_TICK_MS);
  ticker.unref?.();
  let disposed = false;

  const dispose = (): void => {
    if (disposed) {
      return;
    }
    disposed = true;
    clearInterval(ticker);
    unsubscribe();
  };

  const scrollBy = (rows: number): void => {
    scroll = Math.max(0, scroll + rows);
    tui.requestRender();
  };

  return {
    invalidate() {
      contentCache = undefined;
    },
    dispose,
    render(width: number): string[] {
      try {
        return renderFrame(width);
      } catch {
        // A throw here would take down OMP's frame loop.
        return [];
      }
    },
    handleInput(data: string): void {
      if (routeSgrMouseInput(data, (event) => {
        if (event.wheel === null) {
          return false;
        }
        scrollBy(event.wheel * 3);
        return true;
      })) {
        return;
      }
      if (matchesKey(data, "escape") || matchesKey(data, "q")) {
        dispose();
        close();
      } else if (matchesKey(data, "r")) {
        source.refresh();
        tui.requestRender();
      } else if (matchesKey(data, "up") || matchesKey(data, "k")) {
        scrollBy(-1);
      } else if (matchesKey(data, "down") || matchesKey(data, "j")) {
        scrollBy(1);
      } else if (matchesKey(data, "pageUp")) {
        scrollBy(-viewportRows);
      } else if (matchesKey(data, "pageDown") || matchesKey(data, "space")) {
        scrollBy(viewportRows);
      } else if (matchesKey(data, "home")) {
        scroll = 0;
        tui.requestRender();
      } else if (matchesKey(data, "end")) {
        scrollBy(Number.MAX_SAFE_INTEGER);
      }
    },
  };

  function content(inner: number): { lines: string[]; data: DashboardData } {
    const key = `${inner}:${source.version()}:${Math.floor(Date.now() / DASHBOARD_TICK_MS)}`;
    if (contentCache?.key !== key) {
      const data = source.snapshot();
      contentCache = { key, lines: dashboardContent(data, style, inner), data };
    }
    return contentCache;
  }

  function renderFrame(width: number): string[] {
    const height = Math.max(12, tui.terminal?.rows ?? process.stdout.rows ?? 40);
    const inner = Math.max(20, width - 4);
    const { lines: body, data } = content(inner);
    viewportRows = height - 4;
    const maxScroll = Math.max(0, body.length - viewportRows);
    scroll = Math.min(scroll, maxScroll);

    const box = theme.boxRound;
    const border = (text: string): string => style.fg("border", text);
    const loading = data.quotaLoading || data.historyLoading || data.toolCostsLoading;
    const title = ` ${style.bold(style.fg("accent", "OpenUsage"))}${style.fg("dim", " · /usage")} `;
    const right = loading ? ` ${style.fg("dim", "refreshing…")} ` : "";
    const fill = Math.max(0, width - 2 - 1 - visibleWidth(title) - visibleWidth(right) - 1);
    const lines = [border(box.topLeft + box.horizontal) + title + border(box.horizontal.repeat(fill)) + right +
      border(box.horizontal + box.topRight)];
    for (let row = 0; row < viewportRows; row++) {
      const text = body[scroll + row] ?? "";
      lines.push(`${border(box.vertical)} ${truncateToWidth(text, inner, undefined, true)} ${border(box.vertical)}`);
    }
    lines.push(border(box.teeRight + box.horizontal.repeat(Math.max(0, width - 2)) + box.teeLeft));
    const scrollHint = maxScroll > 0 ? `↑/↓ scroll (${scroll + 1}-${Math.min(body.length, scroll + viewportRows)}/${body.length}) · ` : "";
    const hint = `${scrollHint}r refresh · q/esc close · /usage show opens OMP's own view`;
    lines.push(`${border(box.vertical)} ${truncateToWidth(style.fg("dim", hint), inner, undefined, true)} ${border(box.vertical)}`);
    lines.push(border(box.bottomLeft + box.horizontal.repeat(Math.max(0, width - 2)) + box.bottomRight));
    return lines;
  }
}

// ---------------------------------------------------------------------------
// Extension entry point
// ---------------------------------------------------------------------------

// A check that lacks a provider (rate-limited, signed out) keeps that
// provider's previous reading while it is younger than QUOTA_KEEP_MS; the
// renderer labels it with its age.
function mergeQuota(latest: QuotaSummary | undefined, previous: QuotaSummary | undefined, now: number): QuotaSummary | undefined {
  const kept = (previous?.providers ?? []).filter((provider) =>
    provider.fetchedAt !== undefined && now - provider.fetchedAt <= QUOTA_KEEP_MS &&
    !latest?.providers.some((candidate) => candidate.key === provider.key),
  );
  if (!latest && kept.length === 0) {
    return undefined;
  }
  // ChatGPT then Claude (the statusline order), then anything else by name.
  const rank = (key: string): number => (key === "chatgpt" ? 0 : key === "claude" ? 1 : 2);
  const providers = [...latest?.providers ?? [], ...kept].sort((a, b) =>
    rank(a.key) - rank(b.key) || a.name.localeCompare(b.name),
  );
  return { schema: OPENUSAGE_SCHEMA, providers };
}

export default function registerOpenUsageStatusline(pi: OmpExtensionAPI): void {
  let activeContext: OmpExtensionContext | undefined;
  let quota: QuotaSummary | undefined;
  let quotaError: string | undefined;
  let costs: LocalCosts | undefined;
  let history: LocalCosts | undefined;
  let limitHistory: LimitHistory | undefined;
  let limitHistoryAt = 0;
  let toolCosts: ToolCosts = {};
  let costDate = "";
  let lastToolCostRefreshAt = 0;
  let lastFreshQuotaAt = 0;
  let lastStatusRefreshAt = 0;
  let statuses: Record<string, ProviderIncident> = {};
  let alertState: unknown;
  let quotaRefreshInProgress = false;
  let freshQuotaQueued = false;
  let alertsInProgress = false;
  let costsRefreshInProgress = false;
  let historyRefreshInProgress = false;
  let limitHistoryInProgress = false;
  let toolCostRefreshInProgress = false;
  let statusRefreshInProgress = false;
  let timerStarted = false;
  let renderInFlight = false;
  let renderQueued = false;
  let publishTimer: NodeJS.Timeout | undefined;
  let schemaMismatch = false;
  let lastColor = true;
  let quotaDisplay: QuotaDisplay = "used";
  // Unknown until the first render reports the config; the status pages are
  // not polled before then.
  let providerStatusEnabled: boolean | undefined;
  let dashboardOpen = false;
  let dataVersion = 0;
  let latestSession: SessionCosts | undefined;
  let latestNames: ReadonlyMap<string, string> = new Map();
  let subagents: SubagentUsageTracker | undefined;
  let subagentScanTimer: NodeJS.Timeout | undefined;
  let subagentScanInProgress = false;
  let subagentRescanRequested = false;
  const dashboardListeners = new Set<() => void>();

  const notifyDashboards = (): void => {
    dataVersion++;
    for (const listener of dashboardListeners) {
      listener();
    }
  };

  // OMP's own recorded costs plus whichever other CLIs have a usable report.
  // Without OMP's figure the sum would silently drop the main tool.
  const todayCostUSD = (): number | undefined => {
    const omp = costs?.todayUSD;
    return omp === undefined ? undefined : omp + (toolCosts.codex ?? 0) + (toolCosts.claude ?? 0);
  };

  const applyRender = (context: OmpExtensionContext, result: RenderResult): void => {
    schemaMismatch = result.kind === "mismatch";
    if (result.kind !== "ok") {
      context.ui.setStatus(STATUS_KEY, schemaMismatch ? SCHEMA_MISMATCH_STATUS : "OpenUsage unavailable");
      context.ui.setWidget(STATUS_KEY, undefined, { placement: widgetPlacement(context.ui) });
      return;
    }
    const rendered = result.rendered;
    lastColor = rendered.color;
    quotaDisplay = rendered.quotaDisplay;
    const firstSettings = providerStatusEnabled === undefined;
    providerStatusEnabled = rendered.providerStatus;
    if (firstSettings) {
      refreshStatuses(false);
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
  };

  // One render in flight; requests that arrive meanwhile collapse into a
  // single re-run with the newest values.
  const publish = (): void => {
    const context = activeContext;
    if (!context) {
      notifyDashboards();
      return;
    }
    latestNames = modelNames(context);
    latestSession = sessionCosts(context, latestNames, subagents?.turns() ?? []);
    notifyDashboards();
    if (renderInFlight) {
      renderQueued = true;
      return;
    }
    renderInFlight = true;
    const values = {
      sessionCostUSD: latestSession.costUSD,
      modelCosts: latestSession.models.map((model) => ({
        provider: model.provider,
        model: model.name,
        costUSD: model.priced ? model.costUSD : undefined,
      })),
      todayCostUSD: todayCostUSD(),
      block: costs?.block,
      quotas: quota,
      statuses: providerStatusEnabled ? statuses : undefined,
    };
    void renderStatusLine(values).then((result) => {
      renderInFlight = false;
      if (activeContext === context) {
        applyRender(context, result);
      }
      if (renderQueued) {
        renderQueued = false;
        publish();
      }
    });
  };

  const schedulePublish = (): void => {
    if (publishTimer !== undefined) {
      return;
    }
    publishTimer = setTimeout(() => {
      publishTimer = undefined;
      publish();
    }, PUBLISH_DEBOUNCE_MS);
  };

  const runAlerts = (summary: QuotaSummary): void => {
    if (alertsInProgress) {
      return;
    }
    alertsInProgress = true;
    void evaluateAlerts(summary, alertState).then((result) => {
      alertsInProgress = false;
      if (!result) {
        return;
      }
      alertState = result.state;
      const context = activeContext;
      for (const alert of context ? result.alerts : []) {
        context?.ui.notify(alert.message, alert.level);
      }
    });
  };

  // `fresh` forces OMP to re-fetch from the vendors (at most once a minute);
  // otherwise OMP answers from its own 5-minute cache.
  const refreshQuota = (fresh = false): void => {
    if (quotaRefreshInProgress) {
      // A routine check just started is as good as another one; only a
      // forced refresh waits for its turn.
      if (fresh) {
        freshQuotaQueued = true;
      }
      return;
    }
    const now = Date.now();
    const useFresh = fresh && now - lastFreshQuotaAt >= FRESH_REFRESH_MIN_INTERVAL_MS;
    if (useFresh) {
      lastFreshQuotaAt = now;
    }
    quotaRefreshInProgress = true;
    notifyDashboards();
    void readQuotaSummary(useFresh).then((summary) => {
      quotaRefreshInProgress = false;
      const received = Date.now();
      schemaMismatch = summary !== undefined && summary.schema !== OPENUSAGE_SCHEMA;
      if (summary && !schemaMismatch && !summary.error) {
        quota = mergeQuota(summary, quota, received);
        quotaError = undefined;
        runAlerts(summary);
      } else {
        quotaError = schemaMismatch
          ? SCHEMA_MISMATCH_STATUS
          : summary?.error ?? "openusage omp-statusline --json failed";
        // A transient failure keeps each provider's last reading while it is
        // recent; its countdowns stay exact and its age is shown.
        quota = mergeQuota(undefined, quota, received);
      }
      schedulePublish();
      if (freshQuotaQueued) {
        freshQuotaQueued = false;
        refreshQuota(true);
      }
    });
  };

  // Spend since the start of every current quota window, for /usage.
  const quotaWindows = (): Array<{ key: string; since: number }> => {
    const windows: Array<{ key: string; since: number }> = [];
    for (const provider of quota?.providers ?? []) {
      for (const window of provider.windows) {
        if (window.durationMs && window.resetsAt && window.resetsAt > Date.now()) {
          windows.push({ key: `${provider.key}/${window.label}`, since: window.resetsAt - window.durationMs });
        }
      }
    }
    return windows.slice(0, 16);
  };

  // historyDays > 0 also loads the dashboard's daily, session and window spend.
  const refreshLocalCosts = (historyDays: number): void => {
    const withHistory = historyDays > 0;
    if (withHistory ? historyRefreshInProgress : costsRefreshInProgress) {
      return;
    }
    if (withHistory) {
      historyRefreshInProgress = true;
    } else {
      costsRefreshInProgress = true;
    }
    notifyDashboards();
    void readLocalCosts(historyDays, withHistory ? quotaWindows() : []).then((result) => {
      if (withHistory) {
        historyRefreshInProgress = false;
        history = result ?? history;
      } else {
        costsRefreshInProgress = false;
      }
      if (result || !withHistory) {
        costs = result && { block: result.block, todayUSD: result.todayUSD, lastHourUSD: result.lastHourUSD };
      }
      schedulePublish();
    });
  };

  const refreshLimitHistory = (force: boolean): void => {
    if (limitHistoryInProgress || (!force && Date.now() - limitHistoryAt < LIMIT_HISTORY_REFRESH_MS)) {
      return;
    }
    limitHistoryInProgress = true;
    notifyDashboards();
    void readLimitHistory(LIMIT_HISTORY_DAYS).then((result) => {
      limitHistoryInProgress = false;
      if (result) {
        limitHistory = result;
        limitHistoryAt = Date.now();
      }
      notifyDashboards();
    });
  };

  const refreshToolCosts = (date: string): void => {
    if (toolCostRefreshInProgress) {
      return;
    }
    toolCostRefreshInProgress = true;
    lastToolCostRefreshAt = Date.now();
    notifyDashboards();
    void Promise.all([readDailyCost("codex", date), readDailyCost("claude_code", date)]).then(([codex, claude]) => {
      toolCostRefreshInProgress = false;
      if (costDate === date) {
        toolCosts = { codex, claude };
      }
      schedulePublish();
    });
  };

  const refreshStatuses = (force: boolean): void => {
    if (providerStatusEnabled !== true || statusRefreshInProgress ||
        (!force && Date.now() - lastStatusRefreshAt < STATUS_REFRESH_INTERVAL_MS - REFRESH_SLACK_MS)) {
      return;
    }
    statusRefreshInProgress = true;
    lastStatusRefreshAt = Date.now();
    const keys = Object.keys(STATUS_PAGES);
    void Promise.all(keys.map((key) => readProviderStatus(STATUS_PAGES[key]))).then((results) => {
      statusRefreshInProgress = false;
      const next = { ...statuses };
      results.forEach((result, index) => {
        // An unreachable page keeps its previous verdict until it answers.
        if (result === null) {
          delete next[keys[index]];
        } else if (result !== undefined) {
          next[keys[index]] = result;
        }
      });
      if (JSON.stringify(next) !== JSON.stringify(statuses)) {
        statuses = next;
        schedulePublish();
      }
    });
  };

  const scanSubagents = (): void => {
    const tracker = subagents;
    if (!tracker) {
      return;
    }
    if (subagentScanInProgress) {
      subagentRescanRequested = true;
      return;
    }
    subagentScanInProgress = true;
    void tracker.scan().catch(() => false).then((changed) => {
      subagentScanInProgress = false;
      if (changed && subagents === tracker) {
        schedulePublish();
      }
      if (subagentRescanRequested) {
        subagentRescanRequested = false;
        scheduleSubagentScan();
      }
    });
  };

  // Coalesces bursts of subagent activity into one incremental scan.
  const scheduleSubagentScan = (): void => {
    if (!subagents || subagentScanTimer !== undefined) {
      return;
    }
    subagentScanTimer = setTimeout(() => {
      subagentScanTimer = undefined;
      scanSubagents();
    }, SUBAGENT_SCAN_DEBOUNCE_MS);
  };

  const refresh = (): void => {
    if (!activeContext) {
      return;
    }
    // Fallback for subagent turns no event announced (e.g. out-of-process runs).
    scheduleSubagentScan();
    const now = new Date();
    const today = localDate(now);
    if (costDate !== today) {
      costDate = today;
      toolCosts = {};
      lastToolCostRefreshAt = 0;
      schedulePublish();
    }
    refreshQuota();
    // An open dashboard also keeps its daily, session and window spend current.
    refreshLocalCosts(dashboardOpen ? DASHBOARD_HISTORY_DAYS : 0);
    if (dashboardOpen) {
      refreshLimitHistory(false);
    }
    if (now.getTime() - lastToolCostRefreshAt >= TOOL_COST_REFRESH_INTERVAL_MS - REFRESH_SLACK_MS) {
      refreshToolCosts(today);
    }
    refreshStatuses(false);
  };

  const dashboardSource: DashboardSource = {
    get color() {
      return lastColor;
    },
    version: () => dataVersion,
    snapshot() {
      return {
        now: Date.now(),
        quota,
        quotaError,
        quotaLoading: quotaRefreshInProgress,
        costs,
        history,
        historyLoading: historyRefreshInProgress,
        limitHistory,
        toolCosts,
        toolCostsLoading: toolCostRefreshInProgress,
        todayUSD: todayCostUSD(),
        session: latestSession,
        names: latestNames,
        statuses: providerStatusEnabled ? statuses : {},
        quotaDisplay,
      };
    },
    refresh() {
      refreshQuota(true);
      refreshLocalCosts(DASHBOARD_HISTORY_DAYS);
      refreshToolCosts(costDate || localDate(new Date()));
      refreshLimitHistory(true);
      refreshStatuses(true);
    },
    subscribe(listener) {
      dashboardListeners.add(listener);
      return () => dashboardListeners.delete(listener);
    },
  };

  const openDashboard = (context: OmpExtensionContext): void => {
    if (dashboardOpen) {
      return;
    }
    dashboardOpen = true;
    refreshQuota();
    refreshLocalCosts(DASHBOARD_HISTORY_DAYS);
    refreshLimitHistory(false);
    if (Date.now() - lastToolCostRefreshAt >= TOOL_COST_REFRESH_INTERVAL_MS - REFRESH_SLACK_MS) {
      refreshToolCosts(costDate || localDate(new Date()));
    }
    void context.ui.custom<void>(
      (tui, theme, _keybindings, done) => createUsageDashboard(tui, theme, dashboardSource, () => done()),
      {
        overlay: true,
        overlayOptions: { anchor: "bottom-center", width: "100%", maxHeight: "100%", margin: 0, fullscreen: true },
      },
    ).catch(() => undefined).finally(() => {
      dashboardOpen = false;
    });
  };

  const attach = (_event: unknown, context: OmpExtensionContext): void => {
    // Subagent sessions load this extension too, with a no-op UI; their usage
    // reaches the UI session through its artifacts scan instead.
    if (!context.hasUI) {
      return;
    }
    activeContext = context;
    const artifactsDir = context.sessionManager.getArtifactsDir();
    subagents = artifactsDir ? createSubagentUsageTracker(artifactsDir) : undefined;
    subagentUsageListeners.add(scheduleSubagentScan);
    context.ui.setStatus(STATUS_KEY, "OpenUsage loading...");
    context.ui.setWidget(STATUS_KEY, undefined, { placement: widgetPlacement(context.ui) });
    publish();
    scanSubagents();
    refresh();
    if (!timerStarted) {
      timerStarted = true;
      context.setInterval(refresh, REFRESH_INTERVAL_MS);
    }
  };

  pi.on("session_start", attach);
  pi.on("session_switch", attach);
  // /branch and /btw write a new session file with its own artifacts directory.
  pi.on("session_branch", attach);
  pi.on("message_end", (event, context) => {
    if (!context.hasUI) {
      // A subagent (at any depth) finished a turn.
      signalSubagentUsage();
      return;
    }
    // Only assistant turns cost money; user and tool-result messages would
    // just spawn renders.
    if (event.message?.role !== "assistant") {
      return;
    }
    if (context.sessionManager.getSessionId() === activeContext?.sessionManager.getSessionId()) {
      activeContext = context;
      schedulePublish();
    }
  });
  // Bare `/usage` opens the OpenUsage dashboard; `/usage show` and
  // `/usage reset` still reach OMP's built-in command.
  pi.on("input", (event, context) => {
    if (!context.hasUI || event.source !== "interactive" || !/^\/usage\s*$/.test(event.text.trim())) {
      return undefined;
    }
    openDashboard(context);
    return { handled: true };
  });
  // Direct children report progress and start/finish/abort on the session bus.
  for (const channel of SUBAGENT_CHANNELS) {
    pi.events.on(channel, signalSubagentUsage);
  }
  pi.on("session_shutdown", () => {
    activeContext?.ui.setWidget(STATUS_KEY, undefined, { placement: "belowFooter" });
    activeContext?.ui.setStatus(STATUS_KEY, undefined);
    activeContext = undefined;
    subagentUsageListeners.delete(scheduleSubagentScan);
    clearTimeout(subagentScanTimer);
    subagentScanTimer = undefined;
    clearTimeout(publishTimer);
    publishTimer = undefined;
    subagents = undefined;
    dashboardListeners.clear();
  });
}
