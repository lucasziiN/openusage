---
title: OMP quota statusline
description: Add OpenUsage provider quota remaining and reset information to Oh My Pi's footer on Windows without replacing its built-in token and cost status.
sidebar_label: OMP quota statusline
keywords: [OMP statusline, Oh My Pi quota, OpenUsage OMP extension, Windows quota status]
---

# OMP quota statusline

This OMP extension adds the most constrained provider quota to the footer as a separate status item, for example:

```text
openusage openai-codex 5h 35% left; reset in 2h
```

It reads quota data from `omp usage --json --redact`. It does not estimate API cost, modify OMP's status-line configuration, or require the OpenUsage daemon. OMP's existing `statusLine` token and cost segments stay as configured; the extension uses its own `openusage-quota` status key through `ctx.ui.setStatus`.

OMP displays extension hook statuses in the footer by default. If you have disabled `statusLine.showHookStatus`, enable it in your OMP settings; keep your existing token/cost status-line configuration.

## Install OpenUsage on Windows

1. Use an OpenUsage Windows x64 [release](https://github.com/janekbaraniewski/openusage/releases) that includes `omp-statusline`, or build this checkout with Go 1.26+ and a CGO-capable C compiler (`go build -o openusage.exe ./cmd/openusage`). Place `openusage.exe` in a stable directory, for example `%LOCALAPPDATA%\Programs\OpenUsage`.
2. Add that directory to your **user** `Path` in Windows Environment Variables. Then close and reopen OMP so its process inherits the updated path.
3. In PowerShell, confirm both executables are discoverable and the quota command works:

   ```powershell
   where.exe openusage
   where.exe omp
   openusage version
   openusage omp-statusline
   ```

`openusage omp-statusline` is the safe preview: it prints one short status string and does not print OMP's report metadata. If it says `omp not found`, add OMP's install directory to the same user `Path`, then restart OMP and the terminal.

## Install and enable the OMP extension

Clone the OpenUsage repository containing this integration, or set `$repo` to an existing checkout with `integrations/omp/openusage-statusline.ts`:

```powershell
$repo = Join-Path $HOME 'dev\openusage'
git clone https://github.com/janekbaraniewski/openusage.git $repo
$extensionDirectory = Join-Path $HOME '.omp\agent\extensions'
New-Item -ItemType Directory -Force $extensionDirectory | Out-Null
Copy-Item (Join-Path $repo 'integrations\omp\openusage-statusline.ts') $extensionDirectory
```

OMP loads TypeScript extensions from `%USERPROFILE%\.omp\agent\extensions`; restart OMP or start a new OMP process to load the extension.

The extension invokes `openusage omp-statusline` directly (not through a shell), once on session start and then every 60 seconds. It skips a refresh while the previous invocation is still running. It only writes the `openusage-quota` status key, so it does not replace OMP's existing token/cost status or other extension statuses. If the OpenUsage executable cannot be started or returns no text, the footer shows `OpenUsage unavailable`.

## What the quota means

OpenUsage selects the valid quota window with the **lowest remaining fraction** across the reports returned by OMP. The status shows that report's provider, window label, rounded percent **remaining**, and a relative reset countdown from OMP's `resetsAt` timestamp. A reset already due is shown as `now`; missing reset times are shown as `unknown`.

This is quota availability, not usage cost: `35% left` means 35 percent of that provider window remains. The OMP JSON response can contain account identifiers even with `--redact`; the command parses only provider, window, and amount fields and never displays account metadata. If OMP has no valid quota fraction, the footer reports that quota data is unavailable.
