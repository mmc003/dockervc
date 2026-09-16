# dockervc uninstaller for Windows
#
# Usage (from the extracted release folder, or anywhere else):
#   powershell -ExecutionPolicy Bypass -File .\uninstall.ps1
#   powershell -ExecutionPolicy Bypass -File .\uninstall.ps1 -InstallDir C:\Tools\dockervc
#   powershell -ExecutionPolicy Bypass -File .\uninstall.ps1 -StoreDir D:\dockervc-data

[CmdletBinding()]
param(
    [string]$InstallDir = "$env:LOCALAPPDATA\Programs\dockervc",
    [string]$StoreDir
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

function Get-NormalizedPath {
    param([Parameter(Mandatory = $true)][string]$Path)

    return [IO.Path]::GetFullPath($Path).TrimEnd([char[]]@('\', '/'))
}

$InstallDir = Get-NormalizedPath $InstallDir
$installedExe = Join-Path $InstallDir "dockervc.exe"

if (Test-Path -LiteralPath $installedExe -PathType Leaf) {
    Remove-Item -LiteralPath $installedExe -Force
    Write-Host "Removed $installedExe" -ForegroundColor Green
} else {
    Write-Host "No dockervc.exe found at $installedExe."
}

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (-not [string]::IsNullOrWhiteSpace($userPath)) {
    $installPathForComparison = $InstallDir.TrimEnd('\', '/')
    $pathEntries = @($userPath -split ';')
    $keptEntries = @($pathEntries | Where-Object {
        $_.Trim().Trim('"').TrimEnd('\', '/') -ine $installPathForComparison
    })

    if ($keptEntries.Count -ne $pathEntries.Count) {
        [Environment]::SetEnvironmentVariable("Path", ($keptEntries -join ';'), "User")
        Write-Host "Removed $InstallDir from your user PATH." -ForegroundColor Green
        Write-Host "Open a new terminal for the PATH change to take effect." -ForegroundColor Yellow
    }
}

if ((Test-Path -LiteralPath $InstallDir -PathType Container) -and
    @((Get-ChildItem -LiteralPath $InstallDir -Force)).Count -eq 0) {
    Remove-Item -LiteralPath $InstallDir -Force
    Write-Host "Removed empty install folder $InstallDir"
}

if ($PSBoundParameters.ContainsKey("StoreDir")) {
    $storePath = Get-NormalizedPath $StoreDir
} elseif (-not [string]::IsNullOrWhiteSpace($env:DOCKERVC_HOME)) {
    $storePath = Get-NormalizedPath $env:DOCKERVC_HOME
} else {
    $programData = if ([string]::IsNullOrWhiteSpace($env:ProgramData)) {
        "C:\ProgramData"
    } else {
        $env:ProgramData
    }
    $systemStore = Get-NormalizedPath (Join-Path $programData "dockervc")
    $userStore = Get-NormalizedPath (Join-Path $env:USERPROFILE ".dockervc")
    $storePath = if (Test-Path -LiteralPath $systemStore -PathType Container) {
        $systemStore
    } elseif (Test-Path -LiteralPath $userStore -PathType Container) {
        $userStore
    } else {
        $systemStore
    }
}

Write-Host ""
if (-not (Test-Path -LiteralPath $storePath -PathType Container)) {
    Write-Host "No snapshot store found at $storePath."
    return
}

$protectedPaths = @(
    [IO.Path]::GetPathRoot($storePath),
    $env:USERPROFILE,
    $env:ProgramData,
    $env:SystemRoot
) | Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | ForEach-Object {
    Get-NormalizedPath $_
}
if ($protectedPaths -icontains $storePath) {
    throw "Refusing to treat protected directory '$storePath' as a snapshot store. Pass the exact store directory with -StoreDir."
}

Write-Host "Snapshot store found at $storePath" -ForegroundColor Yellow
$answer = Read-Host "Permanently delete this store and every snapshot in it? [y/N]"
if ($answer -match '^(?i:y|yes)$') {
    Remove-Item -LiteralPath $storePath -Recurse -Force
    Write-Host "Deleted snapshot store $storePath" -ForegroundColor Green
} else {
    Write-Host "Kept snapshot store $storePath"
}

Write-Host ""
Write-Host "dockervc uninstalled."
