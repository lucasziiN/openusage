import { execFile } from "node:child_process";

type OmpExtensionContext = {
  ui: {
    setStatus(key: string, text: string): void;
  };
  setInterval(callback: () => void, delayMs: number): unknown;
};

type OmpExtensionAPI = {
  on(
    event: "session_start" | "session_switch" | "session_shutdown",
    listener: (_event: unknown, context: OmpExtensionContext) => void,
  ): void;
};

const STATUS_KEY = "openusage-quota";
const REFRESH_INTERVAL_MS = 60_000;
const COMMAND_TIMEOUT_MS = 15_000;

function readOpenUsageStatus(done: (status: string) => void): void {
  execFile(
    "openusage",
    ["omp-statusline"],
    {
      encoding: "utf8",
      maxBuffer: 4 * 1024,
      timeout: COMMAND_TIMEOUT_MS,
      windowsHide: true,
    },
    (error, stdout) => {
      if (error) {
        done("OpenUsage unavailable");
        return;
      }

      const status = stdout.trim();
      done(status || "OpenUsage unavailable");
    },
  );
}

export default function registerOpenUsageStatusline(pi: OmpExtensionAPI): void {
  let activeContext: OmpExtensionContext | undefined;
  let refreshInProgress = false;
  let timerStarted = false;

  const refresh = (): void => {
    if (!activeContext || refreshInProgress) {
      return;
    }

    refreshInProgress = true;
    readOpenUsageStatus((status) => {
      try {
        activeContext?.ui.setStatus(STATUS_KEY, status);
      } finally {
        refreshInProgress = false;
      }
    });
  };

  const attach = (_event: unknown, context: OmpExtensionContext): void => {
    activeContext = context;
    refresh();

    if (!timerStarted) {
      timerStarted = true;
      context.setInterval(refresh, REFRESH_INTERVAL_MS);
    }
  };

  pi.on("session_start", attach);
  pi.on("session_switch", attach);
  pi.on("session_shutdown", () => {
    activeContext = undefined;
  });
}
