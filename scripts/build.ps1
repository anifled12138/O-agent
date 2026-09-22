# Agent Harness 便捷打包脚本 (PowerShell)
param (
    [switch]$Fast,
    [switch]$UI,
    [switch]$Backend,
    [switch]$Clean,
    [switch]$SkipInstaller,
    [switch]$Help
)

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$PackageScript = Join-Path $ScriptDir "package.mjs"

$ArgsList = @()
if ($Help) { $ArgsList += "--help" }
if ($Fast) { $ArgsList += "--fast" }
if ($UI) { $ArgsList += "--ui" }
if ($Backend) { $ArgsList += "--backend" }
if ($Clean) { $ArgsList += "--clean" }
if ($SkipInstaller) { $ArgsList += "--skip-installer" }

$NodeCmd = "node"
if (-not (Get-Command node -ErrorAction SilentlyContinue)) {
    if (Test-Path "D:\node.js\node.exe") {
        $NodeCmd = "D:\node.js\node.exe"
    } else {
        Write-Error "未找到 Node.js，请先安装 Node.js"
        exit 1
    }
}

& $NodeCmd $PackageScript @ArgsList
