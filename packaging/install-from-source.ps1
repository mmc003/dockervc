# Build and install dockervc from a Windows source checkout.
#
# Usage (from the repository root):
#   .\install.cmd
#   .\install.cmd -InstallDir C:\Tools\dockervc
#   .\install.cmd -NoPath

[CmdletBinding()]
param(
    [string]$InstallDir,
    [switch]$NoPath
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$repoRoot = Split-Path -Parent $PSScriptRoot
$makefile = Join-Path $repoRoot "Makefile"
$stage = Join-Path $repoRoot "dist\windows-amd64"
$stageExe = Join-Path $stage "dockervc.exe"

if ($null -eq (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "Go was not found on PATH. Install Go, open a new terminal, and run .\install.cmd again."
}

$versionMatch = [regex]::Match(
    (Get-Content -LiteralPath $makefile -Raw),
    '(?m)^VERSION\s*\?=\s*([^\s#]+)'
)
if (-not $versionMatch.Success) {
    throw "Could not read VERSION from $makefile."
}
$version = $versionMatch.Groups[1].Value
$ldflags = "-s -w -X dockervc/internal/cli.Version=$version"

New-Item -ItemType Directory -Force -Path $stage | Out-Null
Write-Host "Building dockervc $version for windows/amd64..." -ForegroundColor Cyan

$savedCgo = $env:CGO_ENABLED
$savedGoos = $env:GOOS
$savedGoarch = $env:GOARCH
try {
    $env:CGO_ENABLED = "0"
    $env:GOOS = "windows"
    $env:GOARCH = "amd64"
    Push-Location $repoRoot
    try {
        & go build -ldflags $ldflags -o $stageExe .
        if ($LASTEXITCODE -ne 0) {
            throw "go build failed with exit code $LASTEXITCODE."
        }
    } finally {
        Pop-Location
    }
} finally {
    if ($null -eq $savedCgo) { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue } else { $env:CGO_ENABLED = $savedCgo }
    if ($null -eq $savedGoos) { Remove-Item Env:GOOS -ErrorAction SilentlyContinue } else { $env:GOOS = $savedGoos }
    if ($null -eq $savedGoarch) { Remove-Item Env:GOARCH -ErrorAction SilentlyContinue } else { $env:GOARCH = $savedGoarch }
}

$stageInstaller = Join-Path $stage "install.ps1"
Copy-Item -LiteralPath (Join-Path $PSScriptRoot "install.ps1") -Destination $stageInstaller -Force
Copy-Item -LiteralPath (Join-Path $repoRoot "README.md") -Destination $stage -Force

$installerParams = @{}
if ($PSBoundParameters.ContainsKey("InstallDir")) {
    $installerParams.InstallDir = $InstallDir
}
if ($NoPath) {
    $installerParams.NoPath = $true
}

& $stageInstaller @installerParams
