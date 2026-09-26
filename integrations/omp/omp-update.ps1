# Defines `omp-update`: runs `omp update`, then reinstalls the OpenUsage
# below-footer patch when the update replaced the patched build.
#
# Load it from your PowerShell profile ($PROFILE):
#   . "$HOME\dev\openusage-omp\integrations\omp\omp-update.ps1"
#
# Arguments pass through to `omp update`, e.g. `omp-update --check`.

function omp-update {
    $gitBash = Join-Path $env:ProgramFiles 'Git\bin\bash.exe'
    if (-not (Test-Path $gitBash)) {
        Write-Error "Git Bash not found at $gitBash; it is needed to run rebuild-omp-patched.sh."
        return
    }
    # Git Bash accepts forward-slash Windows paths without conversion.
    $rebuildScript = (Join-Path $PSScriptRoot 'rebuild-omp-patched.sh') -replace '\\', '/'

    omp update @args
    if ($LASTEXITCODE -ne 0) {
        Write-Error 'omp update failed; the installed OMP was left as it was.'
        return
    }

    $ompExe = ((Get-Command omp -CommandType Application).Source) -replace '\\', '/'
    # The patch adds getNativeFooter. grep scans the 200+ MB binary far faster
    # than Select-String.
    & $gitBash -c 'grep -q -a getNativeFooter "$1"' omp-update $ompExe
    if ($LASTEXITCODE -eq 0) {
        Write-Host 'OMP still has the OpenUsage patch; nothing to rebuild.'
        return
    }

    $version = (omp --version) -replace '^omp/', ''
    Write-Host "Rebuilding OMP $version with the OpenUsage patch..."
    & $gitBash $rebuildScript $version
    if ($LASTEXITCODE -ne 0) {
        Write-Warning ("Rebuild failed. Stock OMP $version is still installed and works; " +
            'OpenUsage shows its degraded layout until the patch is refreshed.')
        return
    }
    Write-Host 'Restart open OMP sessions to load the patched build.'
}
