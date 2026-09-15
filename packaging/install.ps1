# dockervc installer for Windows
#
# Usage (from the extracted release folder containing dockervc.exe):
#   powershell -ExecutionPolicy Bypass -File .\install.ps1
#   powershell -ExecutionPolicy Bypass -File .\install.ps1 -InstallDir C:\Tools\dockervc
#   powershell -ExecutionPolicy Bypass -File .\install.ps1 -NoPath

[CmdletBinding()]
param(
    [string]$InstallDir = "$env:LOCALAPPDATA\Programs\dockervc",
    [switch]$NoPath
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$sourceExe = Join-Path $PSScriptRoot "dockervc.exe"
if (-not (Test-Path -LiteralPath $sourceExe -PathType Leaf)) {
    throw "dockervc.exe was not found next to install.ps1 ($PSScriptRoot). Extract the complete release zip and run the script from that folder."
}

$InstallDir = [IO.Path]::GetFullPath($InstallDir)
$installedExe = Join-Path $InstallDir "dockervc.exe"
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
if (-not ([IO.Path]::GetFullPath($sourceExe) -ieq [IO.Path]::GetFullPath($installedExe))) {
    Copy-Item -LiteralPath $sourceExe -Destination $installedExe -Force
}
Write-Host "Installed dockervc.exe to $installedExe" -ForegroundColor Green

if (-not $NoPath) {
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $installPathForComparison = $InstallDir.TrimEnd('\', '/')
    $alreadyPresent = @($userPath -split ';' | Where-Object {
        $_.Trim().Trim('"').TrimEnd('\', '/') -ieq $installPathForComparison
    }).Count -gt 0

    if ($alreadyPresent) {
        Write-Host "$InstallDir is already on your user PATH."
    } else {
        $newUserPath = if ([string]::IsNullOrWhiteSpace($userPath)) {
            $InstallDir
        } else {
            "$userPath;$InstallDir"
        }
        [Environment]::SetEnvironmentVariable("Path", $newUserPath, "User")
        Write-Host "Added $InstallDir to your user PATH." -ForegroundColor Green
        Write-Host "Open a new terminal for the PATH change to take effect." -ForegroundColor Yellow
    }
}

& $installedExe version
if ($LASTEXITCODE -ne 0) {
    throw "Installed dockervc.exe failed its version check (exit $LASTEXITCODE)."
}

$dockerCommand = Get-Command docker -ErrorAction SilentlyContinue
if ($null -eq $dockerCommand) {
    Write-Warning "Docker was not found on PATH. Install and start Docker Desktop before taking snapshots."
} else {
    try {
        # Windows PowerShell promotes native stderr to an ErrorRecord when the
        # script-wide preference is Stop. Docker can emit harmless config
        # warnings on stderr, so judge reachability by its exit code instead.
        $savedErrorPreference = $ErrorActionPreference
        $dockerExitCode = -1
        try {
            $ErrorActionPreference = "Continue"
            & $dockerCommand.Source version *> $null
            $dockerExitCode = $LASTEXITCODE
        } finally {
            $ErrorActionPreference = $savedErrorPreference
        }
        if ($dockerExitCode -ne 0) {
            throw "docker version exited with code $dockerExitCode"
        }
        Write-Host "Docker Desktop is running." -ForegroundColor Green
    } catch {
        Write-Warning "Docker is installed but Docker Desktop is not reachable. Start Docker Desktop in Linux containers mode before taking snapshots."
    }
}

Write-Host ""
Write-Host "Next steps:" -ForegroundColor Cyan
Write-Host "  dockervc init                  # one-time, creates the snapshot store"
Write-Host '  dockervc snapshot -m "first"  # take your first snapshot'
