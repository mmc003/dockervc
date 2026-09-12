# dockervc installer for Windows
#
# Usage (from the unzipped release folder containing dockervc.exe):
#   powershell -ExecutionPolicy Bypass -File .\install.ps1
#
# Options:
#   -InstallDir <path>   where to place dockervc.exe
#                        (default: %LOCALAPPDATA%\Programs\dockervc)
#   -NoPath              do not add the install dir to the user PATH
param(
    [string]$InstallDir = "$env:LOCALAPPDATA\Programs\dockervc",
    [switch]$NoPath
)

$ErrorActionPreference = "Stop"

$exe = Join-Path $PSScriptRoot "dockervc.exe"
if (-Not (Test-Path $exe)) {
    Write-Host "ERROR: dockervc.exe was not found next to install.ps1 ($PSScriptRoot)." -ForegroundColor Red
    Write-Host "Download the release .zip, extract it fully, and run this script from inside the extracted folder."
    exit 1
}

# --- Place the binary ---------------------------------------------------------
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Copy-Item $exe -Destination (Join-Path $InstallDir "dockervc.exe") -Force
Write-Host "Installed dockervc.exe to $InstallDir" -ForegroundColor Green

# --- Add to user PATH ---------------------------------------------------------
if (-Not $NoPath) {
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if ($userPath -split ";" -notcontains $InstallDir) {
        [Environment]::SetEnvironmentVariable("Path", "$userPath;$InstallDir", "User")
        Write-Host "Added $InstallDir to your user PATH." -ForegroundColor Green
        Write-Host "NOTE: open a NEW terminal for the PATH change to take effect."
    } else {
        Write-Host "$InstallDir is already on your PATH."
    }
}

# --- Verify -------------------------------------------------------------------
& (Join-Path $InstallDir "dockervc.exe") version

# --- Check Docker -------------------------------------------------------------
try {
    $null = docker version 2>&1
    Write-Host "Docker detected." -ForegroundColor Green
} catch {
    Write-Host "WARNING: 'docker' was not found on PATH. Install/start Docker Desktop first:" -ForegroundColor Yellow
    Write-Host "         https://www.docker.com/products/docker-desktop"
}

Write-Host ""
Write-Host "Next steps:" -ForegroundColor Cyan
Write-Host "  dockervc init                # one-time, creates the snapshot store"
Write-Host '  dockervc snapshot -m "first" # take your first snapshot'
