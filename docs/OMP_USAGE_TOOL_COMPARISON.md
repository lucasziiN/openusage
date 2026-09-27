# OpenUsage for OMP vs. comparable usage tools

Notes from a September 2026 survey of tools that show AI coding usage, limits
and cost, compared with the OpenUsage OMP integration (the statusline rows
below OMP's footer and the `/usage` dashboard). Each gap records what the
other tool does, whether this round adopted it, and why not when declined.

## Tools reviewed

| Tool | Kind | Closest overlap with ours |
| --- | --- | --- |
| [ccusage](https://ccusage.com) | CLI reports + Claude Code statusline | Our cost statusline is modelled on it; 5h blocks, burn rate, reports |
| [Claude-Code-Usage-Monitor](https://github.com/Maciek-roboblog/Claude-Code-Usage-Monitor) | Live TUI | Pace/forecast labels, staleness, warnings with cooldown |
| [ccstatusline](https://github.com/sirmalloc/ccstatusline) | Widget statusline | Used/remaining toggle, service status, cache widgets |
| [claude-powerline](https://github.com/Owloops/claude-powerline) | Powerline statusline | Budgets, pace marker, month total |
| [CCometixLine](https://github.com/Haleclipse/CCometixLine), [cc-statusline](https://github.com/chongdashu/cc-statusline) | Statuslines | Usage %, reset clocks |
| [pi-quotas](https://github.com/latentminds-ai/pi-quotas) | pi extension | Quota footer, `/quotas`, pace-projected warnings via `ui.notify` |
| [herdr-agent-usage](https://github.com/levi-qiao/herdr-agent-usage) | Agent sidebar (reads `omp usage`) | OMP cache/staleness handling, fail-closed parsing |
| [abtop](https://github.com/graykode/abtop), [claude-statusline-burnrate](https://github.com/Gui-Gou/claude-statusline-burnrate) | TUI / statusline | Stale-data rules, weekly budgeting |
| [CodexBar](https://github.com/steipete/CodexBar) | Menu bar app (Windows ports exist) | Most complete limit monitor: notifications, pace, status pages, spend |
| [ClaudeBar](https://github.com/tddworks/ClaudeBar) | Menu bar app with an OMP provider | Pace colours, used/remaining, OMP payload fields |
| [Usage4Claude](https://github.com/f-is-h/Usage4Claude), [usage-monitor-for-claude](https://github.com/jens-duttke/usage-monitor-for-claude), Claude Usage Tracker, ClaudeMeter | Tray / menu bar apps | Threshold and reset notifications, time-aware alerts |
| Claude Code `/usage`, Codex CLI `/status` | Built-in | Warning ladders, last-known fallback, saved-reset hints |
| [tokscale](https://github.com/junhoyeo/tokscale), [splitrail](https://github.com/Piebald-AI/splitrail), [sniffly](https://github.com/chiphuyen/sniffly), [CodeBurn](https://github.com/getagentseal/codeburn), [Token Monitor](https://github.com/Javis603/token-monitor), vibe-log | Cost analytics | Session lists, tokens and cache, month views, subscription value |
| OMP itself (`usage` status segment, `/usage show`, `omp usage`, `omp stats`) | Built-in | Countdowns frozen at fetch time; no near-limit alerts |

## What they do that we did not

### Adopted this round

| Gap | Seen in | What we added |
| --- | --- | --- |
| Near-limit and reset notifications | CodexBar, pi-quotas, usage-monitor-for-claude, Usage4Claude, Claude Code, Codex CLI | OMP notices (`ui.notify`) at 75/90% used (below 90% only when ahead of an even pace), when a limit is reached (with the saved-reset hint), when the measured pace runs out before reset, when a limit resets after being exhausted, when a saved reset expires within 24 h, and when a sign-in is disabled. One notice per event; re-armed only after usage drops or the window resets. Configurable (`alerts`, `alertThresholds`). |
| Honest data age | herdr, abtop, Claude-Code-Usage-Monitor, Claude Code | Quota cells older than 15 min say `as of 2h ago` and drop pace warnings; last-good values survive a failed check per provider; `r` in `/usage` forces `omp usage invalidate` before refetching (at most once a minute). |
| Used vs. remaining display | ccstatusline, ClaudeBar, CodeZeno, Codex CLI | `quotaDisplay: "left"` shows `5h 58% left`. |
| Provider incidents | CodexBar, ccstatusline, aqua5230/usage, Claude Usage Tracker | Statuspage feeds for Claude (Claude Code, Claude API) and OpenAI (Codex API, CLI) add a `⚠ <incident>` cell to the affected row and a status line in `/usage`. |
| Spend per quota window | CodexBar "Recent windows" | `/usage` shows OMP spend since each window's start (`resetsAt − duration`). |
| Per-session list | ccusage, CodexBar, tokscale, pi-quotas | Top sessions of the last 7 days by cost with title, project, subagent share and last activity; subagent and `/tan` transcripts roll up into their session. |
| Token and cache statistics | ccusage, ccstatusline, ClaudeBar, sniffly | Today and 7-day tokens (input, output, cache read/write) and cache-hit rate. |
| Month-to-date and projection | claude-powerline, CodexBar, ccusage monthly | Month total with a month-end projection; last-60-minute spend. |
| Limit history | CodexBar utilization charts, Claude-Code-Usage-Monitor limit-hit counts | From OMP's own hourly snapshots (`omp usage --history`): limit hits and daily peaks per window for the last 7 days. |
| Sustainable daily budget | claude-statusline-burnrate | 7d windows show the remaining share per day until reset. |
| Earlier pace | CodexBar (3%), herdr (5%) | Projection starts after 5% of a window (15 min minimum) instead of 10%/30 min. |
| Extra usage, sign-in state | ClaudeBar (OMP provider), `omp usage` | Claude extra-usage spend shown separately instead of as a fake window; disabled sign-ins and accounts without usage data are named instead of turning into `n/a`. |

### Already covered before this round

Live-ticking countdowns (OMP freezes them at fetch), both 5h and 7d windows
per subscription, pace projection and elapsed-time markers, fork-aware cost
dedupe, local-day buckets, daily/hourly charts, top models and projects,
binding-only model buckets, saved-reset counts.

### Declined

| Gap | Seen in | Why not |
| --- | --- | --- |
| Auto-continue after a limit resets | Claude Code | OMP has it natively (`retry.waitForUsageReset`). |
| Cheaper-model nudge near a limit | Codex CLI | OMP has fallback chains and usage-aware fallback (`retry.fallbackChains`, `retry.usageAwareFallback`). |
| Dollar budgets on session/day/month | claude-powerline, CodeBurn | Spend is API-equivalent on flat subscriptions; the quota windows are the real constraint and now alert. |
| Subscription value multiple (`7× your plan`) | CodeBurn, Token Monitor | Needs plan prices OMP does not report (Claude's plan is not in `omp usage`); would be a guess. |
| Awake-hours / work-days weekly pace | claude-statusline-burnrate, CodexBar | Assumes a schedule; OMP agents often run unattended. Linear pace stays honest. |
| Historical pace model, session-equivalent forecast | CodexBar | Needs weeks of paired 5h/7d samples; revisit on top of `omp usage --history`. |
| Event hooks, exit codes, terminal title, MCP / agent tool | CodexBar, Claude-Code-Usage-Monitor, splitrail | Outside the in-OMP display; OMP notices cover the alerting need. |
| Cache-expiry timer, token speed, compaction counter | ccstatusline, claude-powerline | OMP's footer already carries context state; low value for quota. |
| Waste findings, error analysis, work-output stats, standup digests | CodeBurn, sniffly, splitrail, vibe-log | Analytics beyond usage display. |
| Codex global-reset announcements | Usage4Claude | Third-party, unofficial source. |
| In-process quota fetch through OMP internals | OMP scout | Keeps the privacy filter in Go and avoids coupling to OMP's auth storage; a shared 45 s summary cache removes the duplicate `omp usage` spawns instead. |

## Defects found in our own passthrough

Fixed this round:

- `/tan` clones (inside the parent's artifacts directory, walked first, cost zeroed) made fork de-duplication drop the parent's real spend. Inherited turns are now recognised by the session header (`parentSession`, header time) instead of walk order.
- The every-minute cost scan held OMP's live transcripts open without delete-sharing on Windows, which can make OMP's atomic save fail ("Session persistence failed"). Transcripts are now opened with `FILE_SHARE_DELETE`, and a per-file parse cache means unchanged files are not reopened.
- Subagent transcripts that OMP rewrites in place (same or larger size) were read from a stale offset; replacement is now detected by file identity.
- The statusline republished before OMP persisted the finished turn and spawned a render per message; it now publishes after persistence, for assistant turns only, with one render in flight.
- `/branch` (`session_branch`) left the subagent scan on the previous session.
- A block's burn rate from two turns seconds apart showed ~$100/hr; the rate now spans at least 10 minutes.
- `omp usage` was killed at 10 s, OMP's own per-credential budget.
- Quota cells never showed their age when OMP served an old cached report.
- Codex model meters with slightly different window lengths were always shown.
- A fractional millisecond anywhere in `omp usage --json` voided the whole response.
- OMP `model_usage` side calls (auto-thinking, judges) were not counted.
- Every dashboard frame recomputed all session sums.
