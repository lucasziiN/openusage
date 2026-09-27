---
title: OMP ChatGPT and Claude statusline
description: Show the Claude Code-style statusline with ChatGPT and Claude quotas in Oh My Pi on Windows.
sidebar_label: OMP ChatGPT and Claude statusline
keywords: [OMP statusline, Oh My Pi quota, ChatGPT Claude usage, OpenUsage OMP extension, Windows quota status]
---

# OMP ChatGPT and Claude statusline

The OMP extension reuses **OpenUsage's Claude Code cost formatter**. OMP's native status row appears first, followed by OpenUsage; the native row's contents and settings are unchanged. No duplicate active-model or context row is added:

```text
 pi · model             · project                          · git                               · context
 💰 · $12.40 sess       · $18.70 today est                 · $8.30 block (2h41m left)           · 🔥 $1.20/hr
 🐙 · ChatGPT           · 🕔 5h 15% used (reset in 3h10m)   · 📅 7d 42% used (reset in 4d6h)
    · GPT-6-Sol $7.20   · GPT-5.3 Codex $1.10
 🐙 · Claude            · 🕔 5h 28% used (reset in 1h45m)   · 📅 7d 61% used (reset in 2d9h)
    · Opus 4.8 $3.60    · Sonnet $0.50
```

This schematic uses sample values; actual columns follow OMP's native status fields. Icons occupy the brand column, and the remaining blocks align with the native ` · ` separators when space permits. Both rows share one-column outer padding and no extra blank rows. OpenUsage requests enough native column width to keep blocks together when spare terminal space permits. Native fields are never removed to satisfy that request. When the native columns are too narrow for a block, OpenUsage lays its rows out on its own shared column widths (starting under the native model column) so every row stays on one line and aligned with the others; only terminals too narrow even for that wrap the remaining fields. No `statusLine.showHookStatus` setting change is required.

### Theme matching

OpenUsage follows OMP's active theme without a separate theme setting. These five themes were visually checked in the patched OMP 18.3.0 TUI with sample session/model costs:

| OMP theme | Provider headings | Model names | Amounts and percentages |
| --- | --- | --- | --- |
| `obsidian` | Gold | Violet | Cyan |
| `dark-catppuccin` | Pastel yellow | Pink | Teal |
| `dark-tokyo-night` | Amber | Purple | Cyan |
| `dark-nord` | Soft gold | Mauve | Frost teal |
| `dark-gruvbox` | Yellow | Pink-purple | Aqua |

`ChatGPT` and `Claude` headings are bold and use the theme's warm `warning` colour; their model names use `statusLineModel`, keeping the two levels distinct. Amounts and percentages use `statusLinePath`, except a quota percentage OMP rates as a warning (90% or more), or one on pace to run out before its reset, uses `warning`, and an exhausted one uses `error`. Labels and countdowns use `statusLineContext`, separators use `statusLineSep`, and `n/a` and quota older than 15 minutes stay muted. Word cells are coloured whole: `sign-in disabled (/login)` and a minor or maintenance incident use `warning`, a major or critical incident uses `error`. Each text run is coloured separately so an amount's ANSI reset cannot erase the following label's colour.

Live checks covered provider/model contrast, text following highlighted amounts, native-column alignment, and switching between all five themes. Other themes inherit these same roles but have not been visually checked. The configurator's colour-off option still renders plain text.

### Required local OMP layout patch

Stock OMP only supports widgets above or below the editor, both before its native footer. This extension's full layout requires the local patch in `integrations/omp/omp-below-footer.patch`, including `belowFooter`, native-footer inspection and shared column sizing; merely updating OpenUsage is not sufficient. The patch currently applies cleanly to OMP `v18.3.2` (first written for `v18.3.0`).

Build and install it with the script beside the patch, from Git Bash:

```bash
integrations/omp/rebuild-omp-patched.sh          # latest OMP release
integrations/omp/rebuild-omp-patched.sh 18.3.2   # a specific release
```

The script creates a worktree of a clean OMP source checkout (`OMP_SRC`, default `~/dev/omp-layout`) at the release tag, applies the patch, downloads the matching `@oh-my-pi/pi-natives-win32-x64` addon from npm, runs `bun install --frozen-lockfile`, the patch's tests and `bun run build`, verifies the result, then installs it next to `omp` on `PATH` (`OMP_INSTALL_DIR` overrides). It uses `bun` from `PATH` if present, otherwise the installed `omp.exe` in Bun mode (`BUN_BE_BUN=1`). The previous binary is kept as `omp-<version>-<timestamp>.exe.bak`. If the patch no longer applies to a new release, the script stops and names the worktree to resolve and the command that refreshes the patch.

The patch adds a third placement without reordering other extensions. It preserves the native status component, including composer-shape behavior. Restart OMP after installing; an already-running process keeps its old layout.

#### After `omp update`

`omp update` installs a stock release over the patched build. The extension feature-detects the patch, so on stock OMP it keeps working in a degraded layout instead of failing: the widget moves below the editor, columns follow OpenUsage's own widths rather than the native footer, and the status line shows `OpenUsage: stock OMP, run rebuild-omp-patched.sh`. Run the script to restore the full layout.

To update and restore the patch in one step, use `omp-update` from Windows PowerShell instead of `omp update`. Load it once from your profile (`notepad $PROFILE`):

```powershell
. "$HOME\dev\openusage-omp\integrations\omp\omp-update.ps1"
```

`omp-update` runs `omp update` (arguments pass through, so `omp-update --check` only checks). If the installed binary no longer contains the patch, it rebuilds the installed version with `rebuild-omp-patched.sh` through Git Bash (`%ProgramFiles%\Git\bin\bash.exe`); otherwise it reports that nothing needs rebuilding. If `omp update` fails, nothing else runs. If the rebuild fails—typically because a new release conflicts with the patch—stock OMP stays installed and working in the degraded layout until the patch is refreshed.

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

The extension invokes `openusage omp-statusline --json` for display-safe quota data and `openusage omp-statusline costs` for the recorded Pi/OMP active block and today's OMP spend every 60 seconds, and `openusage omp-statusline --render --json` to format the selected rows. Quota summaries are shared through `%LOCALAPPDATA%\openusage\omp-quota-summary.json` for 45 seconds, so several OMP windows start `omp usage` about once per 45 seconds between them rather than once each; OMP itself answers from its own five-minute cache. Parsed transcripts are cached in `%LOCALAPPDATA%\openusage\pi-sessions-v1.gob`, so a poll re-reads only the transcripts that changed. It reads today's local reports for Codex CLI (`codex`) and Claude Code (`claude_code`) every five minutes. Session totals and per-model costs are rebuilt from the current session plus every subagent transcript under its artifacts directory, on attach, session switch, `/branch`, each finished assistant message and subagent progress; a transcript OMP rewrote in place is recounted from the start. Restart OMP after reinstalling the extension; `/reload` does not re-import it. Local scans may take seconds; provider/model rows can appear before daily cost. Disabling every segment removes the widget. If OpenUsage cannot start, the widget is cleared and the hook status says `OpenUsage unavailable`. Rebuild `openusage.exe` whenever you update the extension (and the other way round): the two exchange versioned JSON, and a mismatched pair says `OpenUsage: openusage.exe and this extension are different builds` in the hook status instead of quietly showing `n/a`.

### Settings in `openusage-statusline.json`

Besides the segments and colour, `%USERPROFILE%\.omp\agent\openusage-statusline.json` holds four optional keys. The configurator shows the first, third and fourth as rows after **Color**; missing keys take these defaults, so an older file keeps alerts and provider status on:

| Key | Default | Meaning |
| --- | --- | --- |
| `alerts` | `true` | Quota alerts in the OMP transcript (see below). |
| `alertThresholds` | `[75, 90]` | Used percentages that raise an alert; whole numbers 1–99, strictly ascending. `[]` keeps only limit, pace, sign-in and reset-expiry alerts. |
| `quotaDisplay` | `"used"` | `"left"` shows `5h 58% left` instead of `5h 42% used`, in the statusline and `/usage`. Alerts always say "used". |
| `providerStatus` | `true` | Polls the Claude and OpenAI status pages every five minutes and shows open incidents. |

### Quota alerts

After each quota check, the extension prints an OMP notice (a yellow `Warning:` line in the transcript, never an error that would interrupt a turn) when a ChatGPT or Claude window:

- reaches its limit (with the reset time, the provider's other windows and any saved resets: `/usage reset`);
- crosses a threshold of 90% or more;
- is at least half used and on pace to run out before it resets (`Claude 5h at 57% used · at this pace it runs out in ~1h8m (11:58), before the reset at 14:20`);
- crosses a lower threshold while ahead of an even pace (75% used with a day of the week left is on course and stays quiet).

It also warns when a sign-in is disabled and when a saved rate-limit reset expires within 24 hours, and a dim notice says when an exhausted window has reset. Each episode is announced once per OMP process: a threshold re-arms only after usage falls 10 points below it, and a new window starts over. Numbers older than 15 minutes never raise an alert.

### Provider status

With `providerStatus` on, the extension reads `status.claude.com` and `status.openai.com` every five minutes (a six-second timeout; an unreachable page keeps its last verdict). An open incident or degraded component that OMP's traffic depends on — **Claude Code** and **Claude API** for Claude, **Codex API** and **CLI** for ChatGPT — adds a cell to that provider's row, e.g. `⚠ Elevated errors on Claude Opus…`, and a line under the provider in `/usage`.

## The `/usage` dashboard

With the extension loaded, a bare `/usage` opens OpenUsage's fullscreen dashboard instead of OMP's built-in view; `/usage show` still opens OMP's own dashboard and `/usage reset` still spends a saved rate-limit reset. The dashboard shows:

- **Subscription limits** — every window OMP reports for each subscription (5h, 7d and tier- or model-specific buckets), as a bar of the share used with a `│` marker at the share of the window already elapsed. Fill beyond the marker means usage is ahead of an even pace. Each window lists its reset countdown and local reset time, the OMP spend recorded since the window started (`$110.99 this window`), and, once max(5% of the window, 15 minutes) had passed when OMP measured it, a linear projection: `on pace for ~64% by reset`, or `at this pace, runs out in ~28m` when the limit would be hit before the reset. A week that lasts also shows what it can spare per day (`budget ≈13%/day until reset`). Under each subscription: an open status-page incident, a disabled sign-in, accounts OMP has no usage for, Claude extra usage spent against its cap, and the last 7 days from OMP's hourly usage history — a sparkline of each day's peak per window and how often the limit was hit. The heading says how long ago OMP last fetched the numbers; plan, pooled accounts, saved resets (with how many are usable now and when the next expires) appear beside the name, and a reading older than 15 minutes says how old it is and drops its projection.
- **Spend** — this session (including subagents and side calls, with its costliest models, excluding history a fork copied from its parent), today's estimate split into OMP, Codex CLI and Claude Code, the last hour, the active 5h block with when its last turn ran, its burn rate and projected total at the block's end, this month with a linear projection to month end, and today's and the week's tokens with the share of input served from the prompt cache.
- **Daily spend · OMP** — up to 14 local days of recorded OMP/Pi spend and turns, starting at the first day with activity, plus today's spend per hour.
- **Where the OMP spend went** — the top models and projects by spend today and over the last 7 days, with each one's share of the week.
- **Top sessions** — the eight costliest sessions of the last 7 days, subagents rolled into their parent, with project, subagent share, today's part and when each was last active.

Keys: `↑`/`↓`, `PgUp`/`PgDn`, `Home`/`End` or the mouse wheel scroll, `r` refreshes everything and asks OMP to re-fetch quotas from the providers (at most once a minute; OMP otherwise answers from its five-minute cache), `q` or `Esc` closes. While the dashboard is open, countdowns keep ticking and its history refreshes every minute (the limit history every 30 minutes).

## What the numbers mean

ChatGPT and Claude each show **every shared quota window** OMP reports—normally `🕔 5h` and `📅 7d`—with the rounded percent **used** (or **left**, with `quotaDisplay`) and the reset countdown. When the pace OMP last measured would use up a window before it resets, the countdown adds when: `🕔 5h 88% used (reset in 1h40m, out in ~28m)`. That projection is linear from the share of the window elapsed at the measurement, needs max(5% of the window, 15 minutes) behind it, and is made per account, so pooled windows get none. Windows are named by their length, so both subscriptions read alike (`5h`, `7d`) whatever label the provider uses. A tier- or model-specific bucket (for example `7d Opus`) joins the row only while it is more used than the shared window of the same length, i.e. while it is the binding limit; the `/usage` dashboard always lists it. With several accounts per subscription, a window shows their mean usage and the busiest account's reset, as OMP pools them. Percentages never round up to `100%` before the limit is actually reached. When a reset time passes before the next check, the window shows `reset` instead of its stale usage. A subscription missing from a check (rate-limited, signed out) keeps its last reading for up to an hour; numbers OMP measured more than 15 minutes ago say so (`5h 42% used (reset in 1h7m, as of 2h ago)`), drop their pace note and are dimmed. Countdowns stay exact because they are computed from the reset time each time the row is drawn. A subscription with no windows says `sign-in disabled (/login)`, `no usage reported` or `n/a`; `n/a` means no valid quota was supplied, not 0% usage. Model rows can still show recorded costs without a quota report.

Indented amounts are **current-session, per-model API-equivalent estimates**, grouped by the provider recorded on each assistant message—not the currently selected model. Returning to a previously used model adds to its existing total. Display names come from OMP's model catalog when available, otherwise the recorded model ID is shown. A model with any missing/invalid recorded price shows `n/a`; zero-priced turns remain `$0.00`. Providers other than Codex/ChatGPT and Claude are not falsely attributed to either group.

`$… sess` sums recorded costs across **all models and providers** in the current OMP session, including side calls OMP records as `model_usage` entries (auto-thinking, judges), starting at `$0.00` in a new session, and is omitted if any turn with usage lacks a valid price. A forked or continued session starts with a copy of its parent's history (and subagent transcripts), which keep their original timestamps; turns older than the fork were spent in the parent, so `sess` and the model rows leave them out. Switching the active model does not reset it; switching sessions rebuilds the breakdown. `$… block` and `🔥 $…/hr` use the current five-hour **local Pi/OMP activity block**, omitted when it contains unpriced turns. The burn rate spans the block's first turn to its last (a single turn has no rate yet, and a span under 10 minutes counts as 10), not the hour-floored block start, so it is not diluted early in a block. This is a local activity estimate, not the provider's quota window.

`$… today est` is recorded OMP/Pi spend since **local** midnight plus today's Codex CLI and Claude Code estimates when their reports have usable pricing; an unpriced CLI is left out rather than zeroing the others. It is omitted while any OMP turn today lacks a recorded price. Forked and continued OMP sessions start with a copy of their parent's history; those copied turns are counted once, in the session where they happened. This is not a subscription charge or provider invoice. The OMP JSON response can contain account identifiers even with `--redact`; the quota command reads only known provider, window, amount, status and plan fields and never displays account metadata.
