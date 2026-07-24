#!/usr/bin/env pwsh
# scripts/dev.ps1 鈥? local one-shot rebuild + restart for yolorouter (Windows / PowerShell).
# Usage: .\scripts\dev.ps1 [-Backend] [-Frontend] [-Migrate] [-Restart] [-Help]
#
# Modes:
#   (none)       Full rebuild: frontend + backend + restart (default)
#   -Backend     Rebuild the Go binary and restart (skip frontend)
#   -Frontend    Rebuild the frontend and restart (skip backend-only steps)
#   -Migrate     Run db:migrate and restart
#   -Restart     Restart only, no build
#   -Help, -h    Show this help
#
# Environment:
#   YOLO_LANG=zh|en   Force output language (default: auto-detect from locale)
#   NO_COLOR          Disable coloured output when set

param(
  [switch]$Backend,
  [switch]$Frontend,
  [switch]$Migrate,
  [switch]$Restart,
  [switch]$Help,
  [Alias('h')] [switch]$HelpAlias
)

$ErrorActionPreference = 'Stop'

# ---------------------------------------------------------------------------
# Language detection
# ---------------------------------------------------------------------------
$script:LangSel = if ($env:YOLO_LANG) { $env:YOLO_LANG } else {
  if ([System.Globalization.CultureInfo]::CurrentUICulture.Name -like 'zh*') { 'zh' } else { 'en' }
}

function t($en, $zh) {
  if ($script:LangSel -eq 'zh') { $zh } else { $en }
}

# ---------------------------------------------------------------------------
# Colours
# ---------------------------------------------------------------------------
$script:HasColor = -not $env:NO_COLOR -and $Host.UI.RawUI.ForegroundColor -ne $null
function step($msg) { if ($script:HasColor) { Write-Host "==> $msg" -ForegroundColor Cyan } else { Write-Host "==> $msg" } }
function ok($msg)   { if ($script:HasColor) { Write-Host ">> $msg" -ForegroundColor Green } else { Write-Host ">> $msg" } }
function warn($msg) { if ($script:HasColor) { Write-Host " ! $msg" -ForegroundColor Yellow } else { Write-Host " ! $msg" } }
function err($msg)  { if ($script:HasColor) { Write-Host " !! $msg" -ForegroundColor Red } else { Write-Host " !! $msg" } }

# ---------------------------------------------------------------------------
# Help
# ---------------------------------------------------------------------------
if ($Help -or $HelpAlias) {
  if ($script:LangSel -eq 'zh') {
    Write-Host @"
用法: $($MyInvocation.ScriptName) [参数]

参数:
  (无)         全量：构建前端 + 后端 + 重启（默认）
  -Backend     仅重新构建 Go 二进制并重启（跳过前端）
  -Frontend    仅重新构建前端并重启（跳过后端专有步骤）
  -Migrate     仅执行 db:migrate 并重启
  -Restart     仅重启，不构建
  -Help, -h    显示本帮助

环境变量:
  YOLO_LANG=zh|en   强制输出语言（默认按系统 locale 自动判定）
  NO_COLOR          设置后禁用彩色输出
"@
  } else {
    Write-Host @"
Usage: $($MyInvocation.ScriptName) [-Backend] [-Frontend] [-Migrate] [-Restart] [-Help]

Modes:
  (none)       Full rebuild: frontend + backend + restart (default)
  -Backend     Rebuild the Go binary and restart (skip frontend)
  -Frontend    Rebuild the frontend and restart (skip backend-only steps)
  -Migrate     Run db:migrate and restart
  -Restart     Restart only, no build
  -Help, -h    Show this help

Environment:
  YOLO_LANG=zh|en   Force output language (default: auto-detect from locale)
  NO_COLOR          Disable coloured output when set
"@
  }
  exit 0
}

# ---------------------------------------------------------------------------
# Mode logic
# ---------------------------------------------------------------------------
$buildFrontend = $true
$buildBackend = $true
$explicitMigrate = $false

if ($Backend)  { $buildFrontend = $false }
if ($Frontend) { $buildBackend = $false }
if ($Migrate)  { $buildFrontend = $false; $buildBackend = $false; $explicitMigrate = $true }
if ($Restart)  { $buildFrontend = $false; $buildBackend = $false }

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------
$scriptRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$rootDir = Split-Path -Parent $scriptRoot
$logDir = Join-Path $rootDir 'logs'
$pidFile = Join-Path $logDir 'dev.pid'
$binPath = Join-Path (Join-Path $rootDir 'bin') 'yolorouter.exe'
$null = mkdir -Path $logDir -Force

# ---------------------------------------------------------------------------
# Dependency check
# ---------------------------------------------------------------------------
function Require-Command($cmd, $hint) {
  if (-not (Get-Command $cmd -ErrorAction SilentlyContinue)) {
    err "$(t "Missing required command: " "缺少必需命令：")$cmd"
    err "  $hint"
    exit 1
  }
}

if ($buildFrontend) { Require-Command 'npm' "$(t 'Install from https://nodejs.org/' '请从 https://nodejs.org/ 安装 Node.js')" }
if ($buildBackend)  { Require-Command 'go'  "$(t 'Install from https://go.dev/' '请从 https://go.dev/ 安装 Go')" }

# ---------------------------------------------------------------------------
# Lock (mutual exclusion via a directory)
# ---------------------------------------------------------------------------
$lockDir = Join-Path $logDir 'dev.ps1.lock'
try {
  $null = mkdir $lockDir -ErrorAction Stop
} catch {
  err "$(t "Another dev.ps1 instance appears to be in progress (lock: ${lockDir})" "另一个 dev.ps1 实例似乎正在运行（锁: ${lockDir}）")"
  err "$(t "If sure no other instance is running, delete that directory and retry:" "如果确定没有其他实例在运行，删除该目录后重试：")"
  err "  Remove-Item -Recurse -Force '${lockDir}'"
  exit 1
}

# ---------------------------------------------------------------------------
# Cleanup handler
# ---------------------------------------------------------------------------
$script:CleanupLock = $true
Register-EngineEvent -SourceIdentifier PowerShell.Exiting -Action {
  if ($script:CleanupLock -and (Test-Path $lockDir)) {
    Remove-Item -Recurse -Force $lockDir -ErrorAction SilentlyContinue
  }
} | Out-Null

try {
  # ---------------------------------------------------------------------------
  # Stop existing process
  # ---------------------------------------------------------------------------
  step "$(t "Stopping existing yolorouter process (if any)" "停止已有的 yolorouter 进程（如果有）")"
  $stoppedOld = $false

  if (Test-Path $pidFile) {
    $oldPid = $null
    try {
      $oldPid = Get-Content $pidFile -Raw -ErrorAction Stop | ForEach-Object { $_.Trim() }
    } catch { $oldPid = $null }

    if ($oldPid -match '^\d+$') {
      $oldPid = [int]$oldPid
      try {
        $proc = Get-Process -Id $oldPid -ErrorAction Stop
        # Verify the process is actually our binary
        if ($proc.Path -and $proc.Path -like "$binPath*") {
          step "$(t "  Stopping PID ${oldPid}..." "  正在停止进程 ${oldPid}...")"
          $proc.CloseMainWindow() | Out-Null
          Start-Sleep -Milliseconds 500
          if (-not $proc.HasExited) {
            $proc.Kill() | Out-Null
          }
          $proc.WaitForExit(15000) | Out-Null
          $stoppedOld = $true
          step "$(t "  Previous instance stopped" "  旧实例已停止")"
        } else {
          warn "$(t "  PID ${oldPid} is no longer running our binary" "  PID ${oldPid} 不再运行我们的程序")"
        }
      } catch {
        warn "$(t "  No process with PID ${oldPid}" "  没有 PID 为 ${oldPid} 的进程")"
      }
    }
    Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
  }

  if (-not $stoppedOld) {
    step "$(t "  no running process" "  没有正在运行的进程")"
  }

  # ---------------------------------------------------------------------------
  # Port pre-check
  # ---------------------------------------------------------------------------
  $configPath = Join-Path (Join-Path $rootDir 'configs') 'config.yaml'
  $port = 8080
  if (Test-Path $configPath) {
    try {
      $content = Get-Content $configPath -Raw
      if ($content -match 'port:\s*(\d+)') {
        $port = [int]$Matches[1]
      }
    } catch {}
  }

  $connection = $null
  try {
    $tcpConnection = New-Object System.Net.Sockets.TcpClient
    $tcpConnection.ConnectAsync('127.0.0.1', $port).Wait(1000) | Out-Null
    if ($tcpConnection.Connected) {
      $tcpConnection.Close()
      $connection = $true
    } else {
      $connection = $false
    }
  } catch {
    $connection = $false
  }

  if ($connection) {
    err "$(t "Port ${port} is already in use by another process (not managed by dev.ps1)." "端口 ${port} 已被非本脚本管理的进程占用。")"
    err "$(t "Free it, or change server.port in configs/config.yaml, then retry." "请释放该端口，或修改 configs/config.yaml 的 server.port 后重试。")"
    exit 1
  }

  # ---------------------------------------------------------------------------
  # Build frontend
  # ---------------------------------------------------------------------------
  if ($buildFrontend) {
    step "$(t "Building frontend" "构建前端")"
    $frontendDist = Join-Path (Join-Path $rootDir 'frontend') 'dist'
    if (Test-Path $frontendDist) {
      Remove-Item -Recurse -Force $frontendDist
    }
    Push-Location (Join-Path $rootDir 'frontend')
    try {
      npm ci
      npm run build
    } finally {
      Pop-Location
    }

    step "$(t "Copying frontend dist into Go embed target" "复制前端 dist 到 Go embed 目标目录")"
    $webDist = Join-Path (Join-Path $rootDir 'web') 'dist'

    if (Test-Path $webDist) {
      Remove-Item -Recurse -Force $webDist
    }
    $null = mkdir -Path $webDist -Force
    Copy-Item -Recurse -Path "$frontendDist\*" -Destination $webDist
  }

  # ---------------------------------------------------------------------------
  # Build Go binary
  # ---------------------------------------------------------------------------
if ($buildBackend -or $buildFrontend) {
    step "$(t "Building Go binary" "构建 Go 二进制")"
    
    $webDist = Join-Path (Join-Path $rootDir 'web') 'dist'
    $useEmbed = $false
    if (Test-Path $webDist) {
        $files = Get-ChildItem $webDist -File -ErrorAction SilentlyContinue
        $hasFiles = $files.Count -gt 0
        if ($hasFiles) {
            $useEmbed = $true
        } 
    }
    Push-Location $rootDir
    try {
        if ($useEmbed) {
            step "[DEBUG] 执行: go build -tags embed -o $binPath ./cmd/yolorouter"
            go build -tags embed -o $binPath ./cmd/yolorouter
        } else {
            go build -o $binPath ./cmd/yolorouter
        }
    } finally {
        Pop-Location
    }
}

  # ---------------------------------------------------------------------------
  # Run explicit db:migrate
  # ---------------------------------------------------------------------------
  if ($explicitMigrate) {
    step "$(t "Running explicit db:migrate" "执行 db:migrate")"
    Push-Location $rootDir
    try {
      & $binPath db:migrate
    } finally {
      Pop-Location
    }
  }

  # ---------------------------------------------------------------------------
  # Start server
  # ---------------------------------------------------------------------------
  step "$(t "Starting yolorouter serve" "启动 yolorouter serve")"
  $logFile = Join-Path $logDir 'server.log'

  # Start process in background
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = $binPath
  $psi.Arguments = 'serve'
  $psi.WorkingDirectory = $rootDir
  $psi.RedirectStandardOutput = $true
  $psi.RedirectStandardError = $true
  $psi.UseShellExecute = $false
  $psi.CreateNoWindow = $true

  $serverProc = New-Object System.Diagnostics.Process
  $serverProc.StartInfo = $psi
  $serverProc.Start() | Out-Null

  # Write output to log file
  $serverProc.StandardOutput.ReadToEnd() | Out-File -FilePath $logFile -Encoding utf8
  $serverProc.StandardError.ReadToEnd() | Out-File -FilePath $logFile -Encoding utf8 -Append

  $serverPid = $serverProc.Id

  # Write PID file
  Set-Content -Path $pidFile -Value $serverPid -NoNewline -Encoding utf8

  # Re-read port from config (serve may have generated it)
  if (Test-Path $configPath) {
    try {
      $content = Get-Content $configPath -Raw -ErrorAction Stop
      if ($content -match 'port:\s*(\d+)') {
        $port = [int]$Matches[1]
      }
    } catch {}
  }
  step "$(t "111" "启动 111")"

  # ---------------------------------------------------------------------------
  # Health check
  # ---------------------------------------------------------------------------
  step "$(t "Waiting for /healthz" "等待 /healthz 就绪")"
  $ready = $false
  for ($i = 0; $i -lt 30; $i++) {
    try {
      $proc = Get-Process -Id $serverPid -ErrorAction Stop
      if ($proc.HasExited) {
        err "$(t "yolorouter exited before becoming ready." "yolorouter 在就绪前已退出。")"
        break
      }
    } catch {
      err "$(t "yolorouter exited before becoming ready." "yolorouter 在就绪前已退出。")"
      break
    }

    try {
      $req = [System.Net.WebRequest]::Create("http://127.0.0.1:${port}/healthz")
      $req.Timeout = 2000
      $resp = $req.GetResponse()
      if ($resp.StatusCode -eq 200) {
        $ready = $true
        $resp.Close()
        break
      }
      $resp.Close()
    } catch {
      # Not ready yet
    }
    Start-Sleep -Milliseconds 500
  }

  if (-not $ready) {
    err "$(t "Health check failed — last lines of ${logFile}:" "健康检查失败 —— ${logFile} 尾部：")"
    try {
      $lines = Get-Content $logFile -Tail 20 -ErrorAction SilentlyContinue
      foreach ($line in $lines) { err "  $line" }
    } catch {}
    Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
    exit 1
  }

  Write-Host
  ok "$(t "yolorouter ready" "yolorouter 已就绪") (PID ${serverPid})"
  Write-Host "  $(t 'App:'     '访问:')  http://127.0.0.1:${port}/"
  Write-Host "  $(t 'Health:'  '健康:')  http://127.0.0.1:${port}/healthz"
  Write-Host "  $(t 'Logs:'    '日志:')  Get-Content '${logFile}' -Wait"
  Write-Host "  $(t 'Restart:' '重启:')  $($MyInvocation.ScriptName) -Restart"
  Write-Host "  $(t 'Stop:'    '停止:')  Stop-Process -Id ${serverPid}"

} finally {
  # Clean up the lock directory
  $script:CleanupLock = $false
  if (Test-Path $lockDir) {
    Remove-Item -Recurse -Force $lockDir -ErrorAction SilentlyContinue
  }
}