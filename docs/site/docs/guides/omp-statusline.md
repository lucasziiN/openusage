---
title: OMP ChatGPT and Claude statusline
description: Show the Claude Code-style statusline with ChatGPT and Claude quotas in Oh My Pi on Windows.
sidebar_label: OMP ChatGPT and Claude statusline
keywords: [OMP statusline, Oh My Pi quota, ChatGPT Claude usage, OpenUsage OMP extension, Windows quota status]
---

# OMP ChatGPT and Claude statusline

The OMP extension reuses **OpenUsage's Claude Code cost formatter**. OMP's native status row appears first, followed by OpenUsage; the native row's contents and settings are unchanged. No duplicate active-model or context row is added:

```text
 pi · model             · project                · git                         · context
 💰 · $12.40 sess       · $18.70 today est       · $8.30 block (2h41m left)     · 🔥 $1.20/hr
 🐙 · ChatGPT           · 🕔 5h 15% used
    · GPT-6-Sol $7.20   · GPT-5.3 Codex $1.10
 🐙 · Claude            · 🕔 5h 28% used
    · Opus 4.8 $3.60    · Sonnet $0.50
```

This schematic uses sample values; actual columns follow OMP's native status fields. Icons occupy the brand column, and the remaining blocks align with the native ` · ` separators when space permits. Both rows share one-column outer padding and no extra blank rows. OpenUsage requests enough native column width to keep blocks together when spare terminal space permits. Native fields are never removed to satisfy that request. At half-monitor width, if native spacing would split a cost label or block countdown, only the cost row tightens its separators while keeping the whole row on one line; provider and model rows retain native alignment. Smaller terminals wrap remaining fields. No `statusLine.showHookStatus` setting change is required.

### Theme matching

OpenUsage follows OMP's active theme without a separate theme setting. These five themes were visually checked in the patched OMP 18.3.0 TUI with sample session/model costs:

| OMP theme | Provider headings | Model names | Amounts and percentages |
| --- | --- | --- | --- |
| `obsidian` | Gold | Violet | Cyan |
| `dark-catppuccin` | Pastel yellow | Pink | Teal |
| `dark-tokyo-night` | Amber | Purple | Cyan |
| `dark-nord` | Soft gold | Mauve | Frost teal |
| `dark-gruvbox` | Yellow | Pink-purple | Aqua |

`ChatGPT` and `Claude` headings are bold and use the theme's warm `warning` colour; their model names use `statusLineModel`, keeping the two levels distinct. Amounts and percentages use `statusLinePath`, labels and countdowns use `statusLineContext`, separators use `statusLineSep`, and `n/a` stays muted. Each text run is coloured separately so an amount's ANSI reset cannot erase the following label's colour.

Live checks covered provider/model contrast, text following highlighted amounts, native-column alignment, and switching between all five themes. Other themes inherit these same roles but have not been visually checked. The configurator's colour-off option still renders plain text.

### Required local OMP layout patch

Stock OMP only supports widgets above or below the editor, both before its native footer. This extension's full layout requires the local patch in `integrations/omp/omp-below-footer.patch`, including `belowFooter`, native-footer inspection and shared column sizing; merely updating OpenUsage is not sufficient. The patch currently targets OMP `v18.3.1` (first written for `v18.3.0`).

Build and install it with the script beside the patch, from Git Bash:

```bash
integrations/omp/rebuild-omp-patched.sh          # latest OMP release
integrations/omp/rebuild-omp-patched.sh 18.3.1   # a specific release
```

The script creates a worktree of a clean OMP source checkout (`OMP_SRC`, default `~/dev/omp-layout`) at the release tag, applies the patch, downloads the matching `@oh-my-pi/pi-natives-win32-x64` addon from npm, runs `bun install --frozen-lockfile`, the patch's tests and `bun run build`, verifies the result, then installs it next to `omp` on `PATH` (`OMP_INSTALL_DIR` overrides). It uses `bun` from `PATH` if present, otherwise the installed `omp.exe` in Bun mode (`BUN_BE_BUN=1`). The previous binary is kept as `omp-<version>-<timestamp>.exe.bak`. If the patch no longer applies to a new release, the script stops and names the worktree to resolve and the command that refreshes the patch.

The patch adds a third placement without reordering other extensions. It preserves the native status component, including composer-shape behavior. Restart OMP after installing; an already-running process keeps its old layout.

#### After `omp update`

`omp update` installs a stock release over the patched build. The extension feature-detects the patch, so on stock OMP it keeps working in a degraded layout instead of failing: the widget moves below the editor, columns follow OpenUsage's own widths rather than the native footer, and the status line shows `OpenUsage: stock OMP, run rebuild-omp-patched.sh`. Run the script to restore the full layout.

## Install OpenUsage on Windows

1. After cloning the fork below, build `omp-statusline` with Go 1.26+ and a CGO-capable C compiler (`go build -o openusage.exe ./cmd/openusage`). Upstream releases do not contain this unmerged integration yet. Put `openusage.exe` in a stable directory, for example `%LOCALAPPDATA%\Programs\OpenUsage`.
2. Add that directory to your **user** `Path` in Windows Environment Variables. Then close and reopen OMP so its process inherits the updated path.
3. In PowerShell, confirm both executables are discoverable:

   ```powershell
   where.exe openusage
   where.exe omp
   openusage version
   openusage omp-statusline
   openusage omp-statusline install
   ```

`openusage omp-statusline install` opens the live-preview toggle menu: arrows/j/k move, space toggles a segment, left/right changes colors, Enter on **Apply** saves, and q/Esc cancels without writing. It saves to `%USERPROFILE%\.omp\agent\openusage-statusline.json`, without changing OMP settings. **Per-model session costs** toggles the indented breakdown; disabling a provider hides its breakdown too. To script a selection, use `openusage omp-statusline install --segments session,today,block,burn,chatgpt,claude,models --color=false`. Previous saved model/context/five-hour toggles migrate to this nonduplicated layout. `openusage omp-statusline` by itself remains a quota-only diagnostic. If it says `omp not found`, add OMP's directory to your user `Path`, then restart OMP and the terminal.

## Install and enable the OMP extension

Clone the fork's `codex/omp-statusline` branch, or set `$repo` to an existing checkout containing `integrations/omp/openusage-statusline.ts`:

```powershell
$repo = Join-Path $HOME 'dev\openusage-omp'
git clone --branch codex/omp-statusline https://github.com/lucasziiN/openusage.git $repo
$extensionDirectory = Join-Path $HOME '.omp\agent\extensions'
New-Item -ItemType Directory -Force $extensionDirectory | Out-Null
Copy-Item (Join-Path $repo 'integrations\omp\openusage-statusline.ts') $extensionDirectory
```

OMP loads TypeScript extensions from `%USERPROFILE%\.omp\agent\extensions`; restart OMP or start a new OMP process to load the extension. Running the configurator does not copy the extension.

The extension invokes `openusage omp-statusline --json` for display-safe quota data every 60 seconds, reads the priced Pi/OMP active block on the same interval, and invokes `openusage omp-statusline --render --json` to format the selected rows. It reads local daily reports for Codex CLI (`codex`), Claude Code (`claude_code`), and OMP/Pi (`pi`) every five minutes. Session totals and per-model costs are rebuilt from the current session plus every subagent transcript under its artifacts directory, on attach, session switch, message completion and subagent progress. Restart OMP after reinstalling the extension; `/reload` does not re-import it. Local scans may take seconds; provider/model rows can appear before daily cost. Disabling every segment removes the widget. If OpenUsage cannot start, the widget is cleared and the hook status says `OpenUsage unavailable`.

## What the numbers mean

ChatGPT and Claude each show their **most constrained valid quota window** across authenticated accounts: window label, rounded percent **used**, and reset countdown when supplied. The clock label may be `5h`, `7d`, or another reported window; it is never relabelled as five hours. `n/a` means no valid quota was supplied, not 0% usage. Model rows can still show recorded costs without a quota report.

Indented amounts are **current-session, per-model API-equivalent estimates**, grouped by the provider recorded on each assistant message—not the currently selected model. Returning to a previously used model adds to its existing total. Display names come from OMP's model catalog when available, otherwise the recorded model ID is shown. A model with any missing/invalid recorded price shows `n/a`; zero-priced turns remain `$0.00`. Providers other than Codex/ChatGPT and Claude are not falsely attributed to either group.

`$… sess` sums recorded costs across **all models and providers** in the current OMP session and is omitted if any assistant turn with usage lacks a valid price. Switching the active model does not reset it; switching sessions rebuilds the breakdown. `$… block` and `🔥 $…/hr` use the current five-hour **local Pi/OMP activity block**, omitted when it contains unpriced turns. This is a local activity estimate, not the provider's quota window.

`$… today est` sums API-equivalent estimates for today's local OMP/Pi, Codex CLI, and Claude Code logs when all three reports have usable pricing. If another CLI is unpriced, it falls back to recorded OMP/Pi costs only; the compact label remains `today est`. This is not a subscription charge or provider invoice. The OMP JSON response can contain account identifiers even with `--redact`; the quota command reads only known provider, window, and amount fields and never displays account metadata.
