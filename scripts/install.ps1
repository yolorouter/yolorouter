#!/usr/bin/env pwsh
<#
.SYNOPSIS
    yolorouter one-command installer for Windows.
.DESCRIPTION
    Installs yolorouter as a boot-persistent background service on Windows.
    Downloads a prebuilt release binary, verifies its sha256, sets up a single
    self-contained app-home directory, starts the service and health-checks it.
    Re-run to upgrade; pass --uninstall to remove.
.LINK
    https://raw.githubusercontent.com/yolorouter/yolorouter/main/scripts/install.ps1
.EXAMPLE
    irm https://raw.githubusercontent.com/yolorouter/yolorouter/main/scripts/install.ps1 | iex
.EXAMPLE
    .\install.ps1 --uninstall
.NOTES
    Environment overrides (all optional):
      YOLO_LANG=zh|en          force UI language, skip the language prompt
      YOLO_SCOPE=system|user   force install level, skip the scope prompt
      YOLO_VERSION=vX.Y.Z      pin a specific release (default: latest)
      YOLO_REPO=owner/repo     override the download repo (default: yolorouter/yolorouter)
      YOLO_UNINSTALL=1         uninstall instead of install (same as --uninstall)
#>

#Requires -Version 5.1

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
$REPO = if ($env:YOLO_REPO) { $env:YOLO_REPO } else { 'yolorouter/yolorouter' }
$BINARY_NAME = 'yolorouter'
$DEFAULT_PORT = 8080
$HEALTH_TIMEOUT = 15
$GITHUB_API = "https://api.github.com/repos/$REPO"
$GITHUB_DL = "https://github.com/$REPO/releases"
$SERVICE_NAME = 'Yolorouter'

# Populated as we go.
$script:LANG_CHOICE = ''       # zh | en
$script:SCOPE = ''             # system | user
$script:ARCH = ''              # amd64 | arm64
$script:IS_ADMIN = $false
$script:APP_HOME = ''
$script:BIN_DIR = ''           # <app-home>/bin
$script:BIN_LINK = ''          # symlink target on PATH
$script:RUN_USER = ''          # account the service process runs as
$script:TAG = ''               # resolved release tag, e.g. v0.1.0
$script:IS_UPGRADE = $false
$script:TMP_DIR = ''
$script:SERVICE_START_OK = $true
$script:BACKUP_TAKEN = $false
$script:USER_CREATED_BY_INSTALLER = $false

# ---------------------------------------------------------------------------
# Helper functions
# ---------------------------------------------------------------------------
function info  { Write-Host "==> $args" -ForegroundColor Cyan }
function ok    { Write-Host " ? $args" -ForegroundColor Green }
function warn  { Write-Host " ! $args" -ForegroundColor Yellow -ErrorAction Continue }
function die   { Write-Host " ? $args" -ForegroundColor Red -ErrorAction Continue; exit 1 }

# ---------------------------------------------------------------------------
# i18n — returns a string for the chosen language.
# ---------------------------------------------------------------------------
function m($zh, $en) {
    if ($script:LANG_CHOICE -eq 'zh') { return $zh } else { return $en }
}

# ---------------------------------------------------------------------------
# Admin check
# ---------------------------------------------------------------------------
function Test-Admin {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Require-Admin {
    if (-not $script:IS_ADMIN) {
        $msg = m '此操作需要管理员权限。请以管理员身份重新运行 PowerShell。' 'This operation requires admin privileges. Re-run PowerShell as Administrator.'
        die $msg
    }
}

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------
function Cleanup {
    if ($script:TMP_DIR -and (Test-Path $script:TMP_DIR)) {
        Remove-Item -Path $script:TMP_DIR -Recurse -Force -ErrorAction SilentlyContinue
    }
}

# ---------------------------------------------------------------------------
# Step 1 — language
# ---------------------------------------------------------------------------
function Choose-Language {
    if ($env:YOLO_LANG) {
        switch ($env:YOLO_LANG) {
            'zh' { $script:LANG_CHOICE = 'zh'; break }
            'en' { $script:LANG_CHOICE = 'en'; break }
            default { $script:LANG_CHOICE = 'en' }
        }
        return
    }

    # Default from culture.
    $localeDefault = 'en'
    if ((Get-Culture).Name -like 'zh*') { $localeDefault = 'zh' }

    try {
        $host.UI.RawUI | Out-Null
        $interactive = $true
    } catch {
        $interactive = $false
    }

    if (-not $interactive) {
        $script:LANG_CHOICE = $localeDefault
        return
    }

    Write-Host "Select language / 选择语言" -ForegroundColor White
    Write-Host "  1) 中文"
    Write-Host "  2) English"
    $defIdx = if ($localeDefault -eq 'zh') { '1' } else { '2' }
    $choice = Read-Host "Enter 1 or 2 [$defIdx]"
    if (-not $choice) { $choice = $defIdx }
    switch ($choice) {
        '1' { $script:LANG_CHOICE = 'zh' }
        '2' { $script:LANG_CHOICE = 'en' }
        default { $script:LANG_CHOICE = $localeDefault }
    }
}

# ---------------------------------------------------------------------------
# Step 2 — dependency check
# ---------------------------------------------------------------------------
function Require-Cmd {
    param($Cmd, $Hint)
    if (Get-Command $Cmd -ErrorAction SilentlyContinue) { return }
    die $Hint
}

function Check-Deps {
    Require-Cmd 'curl.exe' "$(m '缺少必需命令 curl.exe（Windows 10+ 自带）。请安装或修复后重试。' 'Required command curl.exe is missing (available on Windows 10+). Install or fix it, then retry.')"
    Require-Cmd 'tar.exe'  "$(m '缺少必需命令 tar.exe（Windows 10 build 17063+ 自带）。请安装或修复后重试。' 'Required command tar.exe is missing (available on Windows 10 build 17063+). Install or fix it, then retry.')"
    # Get-FileHash is built into PowerShell 4+, no check needed.
}

# ---------------------------------------------------------------------------
# Step 3 — platform detection
# ---------------------------------------------------------------------------
function Detect-Platform {
    $archRaw = $env:PROCESSOR_ARCHITECTURE
    switch -Regex ($archRaw) {
        'AMD64' { $script:ARCH = 'amd64' }
        'ARM64' { $script:ARCH = 'arm64' }
        default { 
          $msg = m "不支持的架构: $archRaw（仅支持 amd64/arm64）" "Unsupported architecture: $archRaw (only amd64/arm64 are supported)"
          die $msg
      }
    }

    info ("$(m '架构: {0}' 'Architecture: {0}')" -f $script:ARCH)
}

# ---------------------------------------------------------------------------
# Step 4 — scope (system vs user) + derived paths
# ---------------------------------------------------------------------------
function Choose-Scope {
    $script:IS_ADMIN = Test-Admin

    $want = ''
    if ($env:YOLO_SCOPE) {
        switch ($env:YOLO_SCOPE) {
            'system' { $want = 'system' }
            'user'   { $want = 'user' }
            default  { 
              $msg = m "YOLO_SCOPE 只能是 system 或 user" "YOLO_SCOPE must be 'system' or 'user'"
              die $msg
            }
        }
    } else {
        try {
            $host.UI.RawUI | Out-Null
            $interactive = $true
        } catch {
            $interactive = $false
        }

        if ($interactive) {
            Write-Host "$(m '选择安装级别' 'Select install level')" -ForegroundColor White
            Write-Host "  1) $(m '系统级服务（开机自启，需要管理员权限）[推荐]' 'System service (starts on boot, needs admin) [recommended]')"
            Write-Host "  2) $(m '用户级服务（免管理员，随当前用户运行）' 'User service (no admin, runs under the current user)')"
            $choice = Read-Host "Enter 1 or 2 [1]"
            if (-not $choice) { $choice = '1' }
            $want = if ($choice -eq '2') { 'user' } else { 'system' }
        } else {
            # Non-interactive: system if admin, else user.
            if ($script:IS_ADMIN) {
                $want = 'system'
            } else {
                $want = 'user'
                warn "$(m '非交互模式且非管理员，自动退回用户级安装（设 YOLO_SCOPE=system 可强制）' 'Non-interactive and not admin; falling back to user install (set YOLO_SCOPE=system to force)')"
            }
        }
    }

    $script:SCOPE = $want
    Configure-ScopePaths
}

function Configure-ScopePaths {
    if ($script:SCOPE -eq 'system') {
        $script:APP_HOME = Join-Path $env:ProgramFiles $BINARY_NAME
        $script:BIN_LINK = Join-Path (Join-Path $env:ProgramFiles $BINARY_NAME) "$BINARY_NAME.exe"
    } else {
        $script:APP_HOME = Join-Path $env:LOCALAPPDATA ".$BINARY_NAME"
        $script:BIN_LINK = Join-Path $env:LOCALAPPDATA ".$BINARY_NAME\bin\$BINARY_NAME.exe"
    }
    $script:BIN_DIR = Join-Path $script:APP_HOME 'bin'
    $script:RUN_USER = "$env:USERDOMAIN\$env:USERNAME"

    # An existing config marks a prior install: this run becomes an upgrade.
    if (Test-Path (Join-Path $script:APP_HOME 'configs\config.yaml')) {
        $script:IS_UPGRADE = $true
    }
}
function Set-DirectoryPermissions {
    param($Path)
    
    if ($script:SCOPE -ne 'system') {
        return
    }
    
    info "$(m '设置目录权限...' 'Setting directory permissions...')"
    
    try {
        # 1. 把所有权交给 SYSTEM
        takeown /F "$Path" /R /D Y 2>$null | Out-Null
        
        # 2. 把所有权从你转移给 Administrators（让管理员组拥有）
        icacls "$Path" /setowner "NT AUTHORITY\SYSTEM" /T /Q 2>$null
        
        # 3. SYSTEM 完全控制
        icacls "$Path" /grant SYSTEM:F /T /Q 2>$null
        
        # 4. Administrators 完全控制
        icacls "$Path" /grant Administrators:F /T /Q 2>$null
        
        ok "$(m '目录权限设置完成' 'Directory permissions set')"
    } catch {
        warn "$(m '设置目录权限失败' 'Failed to set directory permissions'): $_"
    }
}

# ---------------------------------------------------------------------------
# Step 5 — resolve version
# ---------------------------------------------------------------------------
function Resolve-Version {
    if ($env:YOLO_VERSION) {
        $script:TAG = $env:YOLO_VERSION
        if ($script:TAG -notlike 'v*') { $script:TAG = "v$($script:TAG)" }
        return
    }

    info "$(m '查询最新版本...' 'Resolving latest release...')"
    try {
        $json = Invoke-WebRequest -Uri "$GITHUB_API/releases/latest" -UseBasicParsing -ErrorAction Stop
        $data = $json.Content | ConvertFrom-Json
        $script:TAG = $data.tag_name
    } catch {
        die (m "无法获取最新版本（检查网络或 YOLO_REPO=$REPO 是否有发布）。也可用 YOLO_VERSION 指定。" "Could not resolve the latest release (check network, or whether YOLO_REPO=$REPO has any release). You can also set YOLO_VERSION.")
    }
}

# ---------------------------------------------------------------------------
# Step 6 — download + verify + extract
# ---------------------------------------------------------------------------
function Get-Sha256 {
    param($Path)
    $hash = Get-FileHash -Path $Path -Algorithm SHA256
    return $hash.Hash.ToLower()
}

function Download-AndExtract {
    $script:TMP_DIR = Join-Path $env:TEMP "yolorouter_$(Get-Random)"
    New-Item -Path $script:TMP_DIR -ItemType Directory -Force | Out-Null

    $asset = "${BINARY_NAME}_${script:TAG}_windows_${script:ARCH}.zip"
    $assetPath = Join-Path $script:TMP_DIR $asset
    $sumsPath = Join-Path $script:TMP_DIR 'checksums.txt'
    $skipVerify = $false

    # ---- local dev path: copy from repo bin/ if available ----
    $localBin = Join-Path $PSScriptRoot '..\bin'
    $localAsset = Join-Path $localBin $asset
    $localSums = Join-Path $localBin 'checksums.txt'
    $localBinary = Join-Path $localBin "$BINARY_NAME.exe"

    if ((Test-Path $localAsset) -or (Test-Path $localBinary)) {
        info "$(m '使用本地 bin/ 目录的文件...' 'Using local files from bin/...')"

        if (Test-Path $localAsset) {
            Copy-Item $localAsset $assetPath -Force
        } else {
            # No zip — copy the binary directly, skip checksum and extraction.
            Copy-Item $localBinary (Join-Path $script:TMP_DIR "$BINARY_NAME.exe") -Force
            $skipVerify = $true
            info "$(m '跳过校验和解压（本地开发模式，直接使用 .exe）' 'Skipping checksum and extraction (local dev mode, using .exe directly)')"
        }

        if (-not $skipVerify) {
            if (Test-Path $localSums) {
                Copy-Item $localSums $sumsPath -Force
            } else {
                warn "$(m '本地 bin/ 下没有 checksums.txt，跳过校验' 'No checksums.txt in local bin/, skipping verification')"
                $skipVerify = $true
            }
        }
    } else {
        # ---- remote path: download from GitHub ----
        $assetUrl = "$GITHUB_DL/download/$($script:TAG)/$asset"
        $sumsUrl = "$GITHUB_DL/download/$($script:TAG)/checksums.txt"

        info ("$(m '下载 {0}' 'Downloading {0}')" -f $asset)
        $prevProgress = $ProgressPreference
        $ProgressPreference = 'SilentlyContinue'
        try {
            Invoke-WebRequest -Uri $assetUrl -OutFile $assetPath -UseBasicParsing -ErrorAction Stop
        } catch {
            die (m "下载失败: $assetUrl（该平台可能没有对应发布资产）" "Download failed: $assetUrl (there may be no release asset for this platform)")
        } finally {
            $ProgressPreference = $prevProgress
        }

        info "$(m '校验 sha256...' 'Verifying sha256...')"
        $prevProgress = $ProgressPreference
        $ProgressPreference = 'SilentlyContinue'
        try {
            Invoke-WebRequest -Uri $sumsUrl -OutFile $sumsPath -UseBasicParsing -ErrorAction Stop
        } catch {
            die (m "下载 checksums.txt 失败: $sumsUrl" "Failed to download checksums.txt: $sumsUrl")
        } finally {
            $ProgressPreference = $prevProgress
        }
    }

    # ---- verify sha256 ----
    if (-not $skipVerify) {
        $expected = $null
        foreach ($line in (Get-Content $sumsPath)) {
            if ($line.EndsWith(" $asset")) {
                $expected = $line.Split()[0]
                break
            }
        }
        if (-not $expected) {
            die (m "checksums.txt 中找不到 $asset 的校验值" "No checksum entry for $asset in checksums.txt")
        }

        $actual = Get-Sha256 $assetPath
        if ($expected -ne $actual) {
            die (m "sha256 校验失败，已中止安装。期望 $expected，实得 $actual" "sha256 verification failed, aborting. Expected $expected, got $actual")
        }
        ok (m 'sha256 校验通过' 'sha256 verified')
    }

    # ---- extract ----
    if (-not $skipVerify) {
        # Only extract if we have a zip archive (downloaded or copied as zip).
        info "$(m '解压中...' 'Extracting...')"
        tar.exe -xf $assetPath -C $script:TMP_DIR
    }
    $binaryPath = Join-Path $script:TMP_DIR "$BINARY_NAME.exe"
    if (-not (Test-Path $binaryPath)) {
        die (m "压缩包里找不到 $BINARY_NAME.exe 二进制" "Binary $BINARY_NAME.exe not found in the archive")
    }
}

# ---------------------------------------------------------------------------
# Step 7 — install files
# ---------------------------------------------------------------------------
function Install-Files {
    info ("$(m '安装到 {0}' 'Installing into {0}')" -f $script:APP_HOME)

    # Create directory structure.
    $dirs = @(
        $script:BIN_DIR,
        (Join-Path $script:APP_HOME 'configs'),
        (Join-Path $script:APP_HOME 'data'),
        (Join-Path $script:APP_HOME 'logs')
    )
    foreach ($d in $dirs) {
        if (-not (Test-Path $d)) { New-Item -Path $d -ItemType Directory -Force | Out-Null }
    }

    # Stage the new binary, then rename atomically into place.
    $staged = Join-Path $script:BIN_DIR "$BINARY_NAME.new.exe"
    $target = Join-Path $script:BIN_DIR "$BINARY_NAME.exe"
    Copy-Item (Join-Path $script:TMP_DIR "$BINARY_NAME.exe") $staged -Force

    if (Test-Path $target) {
        Copy-Item $target "$target.old" -Force
    }
    # Move-Item $staged $target -Force
    # 使用 Copy-Item + Remove-Item 避免跨卷 Move-Item 报错
    Copy-Item $staged $target -Force
    Remove-Item $staged -Force -ErrorAction SilentlyContinue

    # Symlink / copy onto PATH.
    if ($script:BIN_LINK -ne $target) {
        $linkDir = Split-Path $script:BIN_LINK -Parent
        if (-not (Test-Path $linkDir)) { 
            New-Item -Path $linkDir -ItemType Directory -Force | Out-Null 
        }
        if (Test-Path $script:BIN_LINK) { 
            Remove-Item $script:BIN_LINK -Force 
        }
        Copy-Item $target $script:BIN_LINK -Force
    } else {
        Write-Host "BIN_LINK 和 target 相同，跳过复制"
    }

    # Record that this run created the service (consumed by uninstall).
    if ($script:USER_CREATED_BY_INSTALLER) {
        New-Item -Path (Join-Path $script:APP_HOME '.user_created_by_installer') -ItemType File -Force | Out-Null
    }
        # 如果是系统级安装，设置权限
    if ($script:SCOPE -eq 'system') {
        info "$(m '设置目录权限...' 'Setting directory permissions...')"
        Set-DirectoryPermissions -Path $script:APP_HOME
    }
    Add-BinToPath
     # ========== 新增：创建包装脚本 ==========
    Create-WrapperScript
}
function Create-WrapperScript {
    $wrapperPath = Join-Path $script:BIN_DIR "yolorouter.cmd"
    $appHome = $script:APP_HOME
    $exePath = Join-Path $script:BIN_DIR "$BINARY_NAME.exe"
    
    info "$(m '创建包装脚本...' 'Creating wrapper script...')"
    
    $wrapperContent = @"
@echo off
setlocal
cd /d "$appHome"
"$exePath" %*
"@
    
    $wrapperContent | Out-File -FilePath $wrapperPath -Encoding ASCII -Force
    ok "$(m '包装脚本创建成功' 'Wrapper script created')"
}
function Add-BinToPath {
    $binDir = $script:BIN_DIR
    
    # 检查是否已经在 PATH 中
    $currentPath = [Environment]::GetEnvironmentVariable("Path", "Machine")
    if ($currentPath -like "*$binDir*") {
        info "$(m 'bin 目录已在 PATH 中' 'bin directory already in PATH')"
        return
    }

    info "$(m '添加 bin 目录到 PATH...' 'Adding bin directory to PATH...')"
    
    try {
        # 系统级安装 → 加到系统 PATH
        if ($script:SCOPE -eq 'system') {
            if (-not $script:IS_ADMIN) {
                warn "$(m '非管理员，无法修改系统 PATH' 'Not admin, cannot modify system PATH')"
                return
            }
            $newPath = $currentPath + ";$binDir"
            [Environment]::SetEnvironmentVariable("Path", $newPath, "Machine")
            ok "$(m '已添加到系统 PATH' 'Added to system PATH')"
        } else {
            # 用户级安装 → 加到用户 PATH
            $currentPath = [Environment]::GetEnvironmentVariable("Path", "User")
            if ($currentPath -like "*$binDir*") { return }
            $newPath = $currentPath + ";$binDir"
            [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
            ok "$(m '已添加到用户 PATH' 'Added to user PATH')"
        }
        
        # 更新当前进程的 PATH（立即生效）
        $env:Path += ";$binDir"
        
        ok "$(m "PATH 已更新，现在可以使用 'yolorouter' 命令" 'PATH updated, you can now use ''yolorouter'' command')"
    } catch {
        warn "$(m '添加 PATH 失败' 'Failed to add to PATH'): $_"
    }
}

function Rollback-Binary {
    $target = Join-Path $script:BIN_DIR "$BINARY_NAME.exe"
    $old = "$target.old"
    if (-not (Test-Path $old)) { return $false }
    Move-Item $old $target -Force
    Copy-Item $target $script:BIN_LINK -Force
    return $true
}

function Discard-OldBinary {
    $old = Join-Path $script:BIN_DIR "$BINARY_NAME.exe.old"
    if (Test-Path $old) { Remove-Item $old -Force -ErrorAction SilentlyContinue }
}

function Stop-ServiceIfRunning {
    if ($script:SCOPE -eq 'system') {
        $svc = Get-Service -Name $SERVICE_NAME -ErrorAction SilentlyContinue
        if ($svc -and $svc.Status -ne 'Stopped') {
            Stop-Service -Name $SERVICE_NAME -Force -ErrorAction SilentlyContinue
        }
    } else {
        $task = schtasks.exe /Query /TN $SERVICE_NAME /FO CSV /NH 2>$null
        if ($LASTEXITCODE -eq 0) {
            schtasks.exe /End /TN $SERVICE_NAME 2>$null
        }
    }
}

# ---------------------------------------------------------------------------
# Step 8 — upgrade safety: back up before upgrade
# ---------------------------------------------------------------------------
function Backup-BeforeUpgrade {
    if (-not $script:IS_UPGRADE) { return }
    if ($env:YOLO_SKIP_BACKUP -eq '1') {
        warn "$(m '已按 YOLO_SKIP_BACKUP=1 跳过升级前备份' 'Skipping pre-upgrade backup (YOLO_SKIP_BACKUP=1)')"
        return
    }

    info "$(m '升级前备份数据库...' 'Backing up the database before upgrade...')"
    $backupDir = Join-Path $script:APP_HOME 'backups'
    $binary = Join-Path $script:BIN_DIR "$BINARY_NAME.exe"
    Push-Location $script:APP_HOME
    try {
        & $binary db:backup --output-dir $backupDir
        if (-not $?) { throw "backup failed" }
    } catch {
        Pop-Location
        die (m '升级前数据库备份失败，已中止升级（现有服务未受影响）。修复后重试，或设 YOLO_SKIP_BACKUP=1 显式跳过。' 'Pre-upgrade database backup failed; upgrade aborted (the running service is untouched). Fix the cause and retry, or set YOLO_SKIP_BACKUP=1 to skip deliberately.')
    }
    Pop-Location
    $script:BACKUP_TAKEN = $true
    ok "$(m '数据库已备份' 'Database backed up')"
}

# ---------------------------------------------------------------------------
# Step 9 — service setup (Windows Service or Scheduled Task)
# ---------------------------------------------------------------------------
function Setup-Service {
    # 不管是 system 还是 user，都用计划任务
    Setup-ScheduledTask
}

function Setup-WindowsService {
    Require-Admin

    $binary = Join-Path $script:BIN_DIR "$BINARY_NAME.exe"
    $appHome = $script:APP_HOME
    $binPath = "`"$binary`" serve"

    # Stop existing service if any.
    $existing = Get-Service -Name $SERVICE_NAME -ErrorAction SilentlyContinue
    if ($existing) {
        Stop-Service -Name $SERVICE_NAME -Force -ErrorAction SilentlyContinue
        sc.exe delete $SERVICE_NAME 2>$null
        Start-Sleep -Seconds 2
    }

    info "$(m '创建 Windows 服务...' 'Creating Windows service...')"
    
    # 先用最简单的命令创建服务
    # sc.exe create "Yolorouter" binPath= "\"C:\Program Files\yolorouter\bin\yolorouter.exe\" serve" start= auto DisplayName= "Yolorouter Service"
    
    $scCommand = 'sc.exe create "' + $SERVICE_NAME + '" binPath= "' + $binPath + '" start= auto DisplayName= "Yolorouter Service"'
    info "调试: $scCommand"
    
    $output = cmd /c $scCommand 2>&1
    $exitCode = $LASTEXITCODE
    
    if ($exitCode -ne 0) {
        warn "sc.exe 退出码: $exitCode"
        warn "sc.exe 输出: $output"
        $script:SERVICE_START_OK = $false
        return
    }
    
    # 通过注册表设置工作目录
    try {
        $serviceKey = "HKLM:\SYSTEM\CurrentControlSet\Services\$SERVICE_NAME"
        # 创建 ImagePath（带工作目录）
        $newBinPath = "cmd /c cd /d `"$appHome`" && `"$binary`" serve"
        New-ItemProperty -Path $serviceKey -Name "ImagePath" -Value $newBinPath -Force | Out-Null
        info "$(m '工作目录已设置' 'Working directory set')"
    } catch {
        warn "$(m '设置工作目录失败' 'Failed to set working directory'): $_"
    }
    
    info "$(m '服务创建成功' 'Service created successfully')"

    # 设置服务启动超时 60 秒
    try {
        $null = New-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Control" -Name "ServicesPipeTimeout" -Value 60000 -PropertyType DWORD -Force
    } catch {
        # 忽略
    }

    # Set description
    sc.exe description $SERVICE_NAME "Yolorouter — a lightweight API router" 2>$null

    # Set failure recovery
    sc.exe failure $SERVICE_NAME reset= 86400 actions= restart/3000/restart/30000/restart/60000 2>$null

    # 启动服务
    try {
        Start-Service -Name $SERVICE_NAME -ErrorAction Stop
        $script:SERVICE_START_OK = $true
        info "$(m '服务已启动' 'Service started')"
    } catch {
        warn "$(m '服务启动失败' 'Service start failed'): $_"
        $script:SERVICE_START_OK = $false
    }
}

function Setup-ScheduledTask {
    $binary = Join-Path $script:BIN_DIR "$BINARY_NAME.exe"
    $appHome = $script:APP_HOME

    # 先删除已存在的同名任务
    schtasks.exe /Delete /TN $SERVICE_NAME /F 2>$null | Out-Null

    # -WorkingDirectory 把进程工作目录固定到 app-home，让 serve 能找到
    # configs\config.yaml（否则 ONSTART/ONLOGON 任务默认在 System32 启动）。
    $action = New-ScheduledTaskAction -Execute $binary -Argument 'serve' -WorkingDirectory $appHome
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
        -StartWhenAvailable -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)

    if ($script:SCOPE -eq 'system') {
        Require-Admin
        info "$(m '创建计划任务（开机自启，SYSTEM 账户）...' 'Creating scheduled task (starts on boot as SYSTEM)...')"
        # SYSTEM + AtStartup：在任何用户登录前就运行。这正是旧版
        # /TR 缺少 /RU 时开机自启失效的原因。
        $trigger = New-ScheduledTaskTrigger -AtStartup
        $principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
    } else {
        info "$(m '创建计划任务（登录时自启）...' 'Creating scheduled task (starts at logon)...')"
        # 用户级：无管理员、不存密码。以当前交互账户在登录时启动，
        # 是这个场景下最合理的开机等价方案。
        $trigger = New-ScheduledTaskTrigger -AtLogOn -User $script:RUN_USER
        $principal = New-ScheduledTaskPrincipal -UserId $script:RUN_USER -LogonType Interactive -RunLevel Highest
    }

    try {
        Register-ScheduledTask -TaskName $SERVICE_NAME -Action $action -Trigger $trigger `
            -Principal $principal -Settings $settings -Force -ErrorAction Stop | Out-Null
    } catch {
        warn "$(m '创建计划任务失败' 'Failed to create scheduled task'): $_"
        $script:SERVICE_START_OK = $false
        return
    }

    # 触发器只在开机/登录时触发；本次会话也立即拉起一次。
    Start-Process -FilePath $binary -ArgumentList "serve" -WorkingDirectory $appHome -WindowStyle Hidden
    $script:SERVICE_START_OK = $true
    info "$(m '服务已启动' 'Service started')"
}

# ---------------------------------------------------------------------------
# Step 10 — health check + summary
# ---------------------------------------------------------------------------
function Get-ConfigPort {
    $cfg = Join-Path $script:APP_HOME 'configs\config.yaml'
    if (-not (Test-Path $cfg)) { return $DEFAULT_PORT }

    $inServer = $false
    foreach ($line in (Get-Content $cfg)) {
        if ($line -match '^server:') { $inServer = $true; continue }
        if ($line -match '^[^ `t#]') { $inServer = $false }
        if ($inServer -and $line -match 'port:\s*(\d+)') {
            return [int]$Matches[1]
        }
    }
    return $DEFAULT_PORT
}

function Test-PortInUse {
    param($Port)
    try {
        $connections = Get-NetTCPConnection -LocalPort $Port -ErrorAction SilentlyContinue
        return ($null -ne $connections)
    } catch {
        # Fallback: use netstat.
        $result = netstat -ano | Select-String ":$Port\s"
        return ($null -ne $result)
    }
}

function Get-PrimaryIP {
    try {
        $ip = (Get-NetIPAddress -AddressFamily IPv4 -PrefixOrigin Dhcp -ErrorAction SilentlyContinue | Select-Object -First 1).IPAddress
        if (-not $ip) {
            $ip = (Get-NetIPAddress -AddressFamily IPv4 -InterfaceIndex (Get-NetRoute -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue).InterfaceIndex -ErrorAction SilentlyContinue).IPAddress
        }
        return $ip
    } catch {
        return $null
    }
}

function Test-Health {
    param($Port)
    $url = "http://localhost:$Port/healthz"
    info ("$(m '等待服务就绪（最多 {0}s）...' 'Waiting for the service (up to {0}s)...')" -f $HEALTH_TIMEOUT)
    for ($i = 0; $i -lt $HEALTH_TIMEOUT; $i++) {
        try {
            $req = Invoke-WebRequest -Uri $url -UseBasicParsing -ErrorAction Stop
            if ($req.StatusCode -eq 200) { 
              ok "$(m '服务健康检查通过' 'Health check passed')"
              return $true 
            }
        } catch {
            if ($i -eq $HEALTH_TIMEOUT - 1) {
                warn "$(m '健康检查失败：' 'Health check failed:') $_"
            }
        }
        Start-Sleep -Seconds 1
    }
    return $false
}

function Get-LogsHint {
    return "$(m '查看日志: Get-Content -Path ''{0}\logs\server.log'' -Wait' 'Check logs: Get-Content -Path ''{0}\logs\server.log'' -Wait')" -f $script:APP_HOME
}

function Get-SvcCmds {
    return @{
        Status  = "schtasks.exe /Query /TN `"$SERVICE_NAME`" /FO LIST /V"
        Stop    = "schtasks.exe /End /TN `"$SERVICE_NAME`""
        Restart = "schtasks.exe /End /TN `"$SERVICE_NAME`" 2>`$null; schtasks.exe /Run /TN `"$SERVICE_NAME`""
      }
}


function Check-Path {
    $binDir = Split-Path $script:BIN_LINK -Parent
    if ($script:SCOPE -eq 'user') {
        $envPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        if ($envPath -notlike "*$binDir*") {
            warn "$(m "~/.local/bin 不在 PATH 中，yolorouter 命令可能无法直接使用" "~/.local/bin is not in PATH, yolorouter command may not work directly")"
            warn "$(m "  解决: 将以下内容添加到 PATH: $binDir" "  Fix: Add this to PATH: $binDir")"
        }
    }
}

function Write-Summary {
    param($Port)
    $ip = Get-PrimaryIP
    $svcCmds = Get-SvcCmds
    $installUrl = "https://raw.githubusercontent.com/$REPO/main/scripts/install.ps1"

    Write-Host
    ok "$(m 'yolorouter 已安装并启动' 'yolorouter is installed and running')"
    Write-Host
    Write-Host "$(m '访问控制台：' 'Open the console:')" -ForegroundColor White
    Write-Host "  http://localhost:$Port/"
    if ($ip) { Write-Host "  http://$($ip):$Port/" }
    Write-Host "$(m '  然后在浏览器里创建第一个管理员账号。' '  Then create the first admin account in your browser.')"
    Write-Host
    Write-Host "$(m '常用命令：' 'Handy commands:')" -ForegroundColor White
    Write-Host "  $(m '状态:   ' 'status:  ') $($svcCmds.Status)"
    Write-Host "  $(m '日志:   ' 'logs:    ') $(Get-LogsHint)"
    Write-Host "  $(m '停止:   ' 'stop:    ') $($svcCmds.Stop)"
    Write-Host "  $(m '重启:   ' 'restart: ') $($svcCmds.Restart)"
    Write-Host "  $(m '升级:   ' 'upgrade:') $(m '重跑安装命令，或 ' 're-run the installer, or ')$BINARY_NAME update"
    Write-Host "  $(m '卸载:   ' 'remove: ') irm $installUrl | iex --uninstall"
    Write-Host
    Write-Host "$(m "  改端口 = 编辑 $($script:APP_HOME)\configs\config.yaml 的 server.port 后重启服务。" "  Change port = edit server.port in $($script:APP_HOME)\configs\config.yaml, then restart.")"
    Write-Host "$(m "  需要 PostgreSQL？编辑同一文件的 database 段后重启。" "  Need PostgreSQL? Edit the database section in that file, then restart.")"
}

# ---------------------------------------------------------------------------
# Uninstall
# ---------------------------------------------------------------------------
function Resolve-UninstallScope {
    # An explicit YOLO_SCOPE wins — only infer from disk in non-interactive fallback.
    if ($env:YOLO_SCOPE) { return }

    try {
        $host.UI.RawUI | Out-Null
        $interactive = $true
    } catch {
        $interactive = $false
    }
    if ($interactive) { return }

    $sysHome = Join-Path $env:ProgramFiles $BINARY_NAME
    $userHome = Join-Path $env:LOCALAPPDATA ".$BINARY_NAME"

    if ((Test-Path $sysHome) -or (Get-Service -Name $SERVICE_NAME -ErrorAction SilentlyContinue)) {
        $script:SCOPE = 'system'
    } elseif (Test-Path $userHome) {
        $script:SCOPE = 'user'
    } else {
        return
    }
    Configure-ScopePaths
}

function Do-Uninstall {
    info "$(m '卸载 yolorouter...' 'Uninstalling yolorouter...')"

    # ========== 停止并删除计划任务（不管 system 还是 user） ==========
    schtasks.exe /End /TN $SERVICE_NAME 2>$null
    schtasks.exe /Delete /TN $SERVICE_NAME /F 2>$null

    # ========== 强制结束残留进程 ==========
    Get-Process yolorouter -ErrorAction SilentlyContinue | Stop-Process -Force

    # ========== 删除文件 ==========
    if (Test-Path $script:BIN_LINK) { Remove-Item $script:BIN_LINK -Force -ErrorAction SilentlyContinue }
    $target = Join-Path $script:BIN_DIR "$BINARY_NAME.exe"
    if (Test-Path $target) { Remove-Item $target -Force -ErrorAction SilentlyContinue }
    $old = "$target.old"
    if (Test-Path $old) { Remove-Item $old -Force -ErrorAction SilentlyContinue }

    # 从 PATH 中移除 bin 目录
    Remove-BinFromPath

    # 询问是否删除数据
    if (Test-Path $script:APP_HOME) {
        $del = 'n'
        try {
            $host.UI.RawUI | Out-Null
            $interactive = $true
        } catch {
            $interactive = $false
        }

        if ($interactive) {
            $del = Read-Host "$(m "删除全部数据？这会清除 $($script:APP_HOME)（配置 + 数据库，不可恢复） [y/N]:" "Delete all data? This wipes $($script:APP_HOME) (config + database, irreversible) [y/N]:")"
            if (-not $del) { $del = 'n' }
        }

        if ($del -match '^(y|yes)$') {
            Remove-Item -Path $script:APP_HOME -Recurse -Force -ErrorAction SilentlyContinue
            ok "$(m '数据已删除' 'Data removed')"
        } else {
            ok ("$(m '已保留数据目录：{0}' 'Data directory preserved: {0}')" -f $script:APP_HOME)
        }
    }

    ok "$(m '卸载完成' 'Uninstall complete')"
}

function Remove-BinFromPath {
    $binDir = $script:BIN_DIR
    
    try {
        if ($script:SCOPE -eq 'system') {
            if (-not $script:IS_ADMIN) { return }
            $currentPath = [Environment]::GetEnvironmentVariable("Path", "Machine")
            $newPath = ($currentPath -split ';' | Where-Object { $_ -ne $binDir }) -join ';'
            [Environment]::SetEnvironmentVariable("Path", $newPath, "Machine")
            ok "$(m '已从系统 PATH 中移除' 'Removed from system PATH')"
        } else {
            $currentPath = [Environment]::GetEnvironmentVariable("Path", "User")
            $newPath = ($currentPath -split ';' | Where-Object { $_ -ne $binDir }) -join ';'
            [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
            ok "$(m '已从用户 PATH 中移除' 'Removed from user PATH')"
        }
    } catch {
        warn "$(m '从 PATH 移除失败' 'Failed to remove from PATH'): $_"
    }
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
function Main {
    $doRemove = $false
    foreach ($arg in $args) {
        switch ($arg) {
            '--uninstall' { $doRemove = $true }
            '-h' { exit 0 }
            '--help' {
                Write-Host "yolorouter installer for Windows`n  --uninstall   remove yolorouter"
                Write-Host "Env: YOLO_LANG YOLO_SCOPE YOLO_VERSION YOLO_REPO YOLO_UNINSTALL"
                exit 0
            }
        }
    }
    if ($env:YOLO_UNINSTALL -eq '1') { $doRemove = $true }

    Choose-Language
    Check-Deps
    Detect-Platform
    Choose-Scope

    if ($doRemove) {
        Resolve-UninstallScope
        Do-Uninstall
        exit 0
    }

    # Warn on a FRESH install if the default port already has a listener.
    $port = $DEFAULT_PORT
    if (-not $script:IS_UPGRADE -and (Test-PortInUse $DEFAULT_PORT)) {
        warn ("$(m '端口 {0} 已被占用，服务可能无法监听；装完请改端口或释放占用。' 'Port {0} is already in use; the service may fail to bind. Change the port or free it after install.')" -f $DEFAULT_PORT)
    }

    Resolve-Version
    Download-AndExtract

    # Upgrade ordering: back up first, then stop before binary swap.
    Backup-BeforeUpgrade
    if ($script:IS_UPGRADE) { Stop-ServiceIfRunning }
    Install-Files
    Setup-Service

    $port = Get-ConfigPort
    if ($script:SERVICE_START_OK -and (Test-Health $port)) {
        if ($script:IS_UPGRADE) { Discard-OldBinary }
        Write-Summary $port
    } else {
        warn "$(m '服务未在预期时间内通过健康检查。' 'The service did not pass the health check in time.')"
        if ($script:IS_UPGRADE -and (Rollback-Binary)) {
            warn "$(m '已回滚到升级前的二进制并重启。' 'Rolled back to the pre-upgrade binary and restarted.')"
            Stop-ServiceIfRunning
            Setup-Service
            if (Test-Health $port) {
                warn "$(m '旧版本已恢复运行；本次升级失败，请查日志。' 'The previous version is running again; the upgrade failed — check the logs.')"
            } elseif ($script:BACKUP_TAKEN) {
                warn ("$(m '旧版本仍未起来——新版本可能已迁移数据库。升级前备份在 {0}，必要时停服后按你的数据库类型恢复最近一次备份。' 'The old version is still down — the newer one may have migrated the database. A pre-upgrade backup is in {0}; if needed, stop the service and restore the latest one per your database driver.')" -f (Join-Path $script:APP_HOME 'backups'))
            } else {
                warn "$(m '旧版本仍未起来，且本次未做升级前备份（YOLO_SKIP_BACKUP=1）；请查日志手动处理。' 'The old version is still down and no pre-upgrade backup was taken (YOLO_SKIP_BACKUP=1); check the logs and recover manually.')"
            }
        }
        Write-Host "  $(Get-LogsHint)"
        exit 1
    }
}

# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------
try {
    Main @args
} finally {
    Cleanup
}