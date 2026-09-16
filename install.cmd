@echo off
setlocal

rem One-command Windows source install. PowerShell's execution policy is
rem bypassed only for this process; the user's policy is not changed.
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0packaging\install-from-source.ps1" %*
exit /b %ERRORLEVEL%
