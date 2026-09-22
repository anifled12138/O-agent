@echo off
setlocal

set "SCRIPT_DIR=%~dp0"
set "NODE_CMD=node"

where node >nul 2>nul
if errorlevel 1 (
    if exist "D:\node.js\node.exe" (
        set "NODE_CMD=D:\node.js\node.exe"
    ) else (
        echo [ERROR] Node.js not found in PATH or D:\node.js\node.exe
        exit /b 1
    )
)

"%NODE_CMD%" "%SCRIPT_DIR%package.mjs" %*
exit /b %errorlevel%
