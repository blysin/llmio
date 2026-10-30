@echo off
rem ============================================================================
rem  build-image.bat - Windows wrapper for build-image.sh (runs inside WSL)
rem
rem  Usage:
rem    build-image.bat                  reuse latest tag version (no increment)
rem    build-image.bat -v v0.8.17       specify version
rem    build-image.bat --no-push        build and tag locally, skip push
rem    build-image.bat --no-latest      push <version> but not latest (latest is pushed by default)
rem    build-image.bat --dry-run        print the plan only
rem
rem  Override the WSL distro with the LLMIO_WSL_DISTRO environment variable
rem  (default: Ubuntu-24.04).
rem ============================================================================
setlocal

rem Switch console to UTF-8 so the script's Chinese output renders correctly.
chcp 65001 >nul 2>&1

set "DISTRO=%LLMIO_WSL_DISTRO%"
if "%DISTRO%"=="" set "DISTRO=Ubuntu-24.04"

rem Run the bash script from the project root (this file's directory) inside WSL.
rem latest 现已是默认推送行为，无需再显式传 --push-latest；如需跳过用 --no-latest。
wsl.exe -d %DISTRO% --cd "%~dp0." -- bash ./build-image.sh %*
set "RC=%ERRORLEVEL%"

rem Keep the window open when launched by double-click.
if "%~1"=="" (
  echo.
  echo [exit code %RC%]
  pause
)

exit /b %RC%
