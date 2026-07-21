#!/usr/bin/env bash
#
# yolorouter one-command installer.
#
#   curl -fsSL https://raw.githubusercontent.com/yolorouter/yolorouter/main/scripts/install.sh | bash
#
# Installs yolorouter as a boot-persistent background service on Linux
# (systemd) and macOS (launchd). Downloads a prebuilt release binary, verifies
# its sha256, sets up a single self-contained app-home directory, starts the
# service and health-checks it. Re-run to upgrade; pass --uninstall to remove.
#
# Written for bash 3.2 (the macOS system bash) — no associative arrays, no
# bash-4 string ops. Robust under `curl | bash`: interactive prompts read from
# /dev/tty, and a missing tty falls back to environment variables + defaults.
#
# Environment overrides (all optional):
#   YOLO_LANG=zh|en          force UI language, skip the language prompt
#   YOLO_SCOPE=system|user   force install level, skip the scope prompt
#   YOLO_VERSION=vX.Y.Z      pin a specific release (default: latest)
#   YOLO_REPO=owner/repo     override the download repo (default: yolorouter/yolorouter)
#   YOLO_UNINSTALL=1         uninstall instead of install (same as --uninstall)

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
REPO="${YOLO_REPO:-yolorouter/yolorouter}"
BINARY_NAME="yolorouter"
DEFAULT_PORT=8080
HEALTH_TIMEOUT=15          # seconds to wait for /healthz after start
GITHUB_API="https://api.github.com/repos/${REPO}"
GITHUB_DL="https://github.com/${REPO}/releases"
LAUNCHD_LABEL="com.yolorouter"

# Populated as we go.
LANG_CHOICE=""             # zh | en
SCOPE=""                   # system | user
OS=""                      # linux | darwin
ARCH=""                    # amd64 | arm64
SUDO=""                    # "" or "sudo"
APP_HOME=""
BIN_DIR=""                 # <app-home>/bin
BIN_LINK=""                # symlink target on PATH
SVC_USER=""                # dedicated service user (linux system scope only)
RUN_USER=""                # account the service process runs as
TAG=""                     # resolved release tag, e.g. v0.1.0
IS_UPGRADE=false
TMP_DIR=""
HAVE_TTY=false
SERVICE_START_OK=true      # set by setup_service; gates upgrade success
BACKUP_TAKEN=false         # set true only when a pre-upgrade backup succeeded

# ---------------------------------------------------------------------------
# Colors (tput with safe fallback; no-op when stderr is not a terminal)
# ---------------------------------------------------------------------------
if [ -t 2 ] && command -v tput >/dev/null 2>&1; then
  C_BOLD="$(tput bold 2>/dev/null || printf '')"
  C_RED="$(tput setaf 1 2>/dev/null || printf '')"
  C_GREEN="$(tput setaf 2 2>/dev/null || printf '')"
  C_YELLOW="$(tput setaf 3 2>/dev/null || printf '')"
  C_BLUE="$(tput setaf 4 2>/dev/null || printf '')"
  C_RESET="$(tput sgr0 2>/dev/null || printf '')"
else
  C_BOLD=""; C_RED=""; C_GREEN=""; C_YELLOW=""; C_BLUE=""; C_RESET=""
fi

# ---------------------------------------------------------------------------
# i18n — bash-3.2-safe. Each key carries both languages on one line so the two
# translations stay visibly in sync. `m KEY` prints a printf format string for
# the chosen language; callers that need interpolation do `printf "$(m KEY)\n" a b`.
# Before the language is chosen, LANG_CHOICE is empty and we fall back to en.
# ---------------------------------------------------------------------------
m() {
  local zh="$1" en="$2"
  if [ "${LANG_CHOICE}" = "zh" ]; then printf '%s' "$zh"; else printf '%s' "$en"; fi
}

say()  { printf '%s\n' "$*"; }
info() { printf '%s\n' "${C_BLUE}${C_BOLD}==>${C_RESET} $*"; }
ok()   { printf '%s\n' "${C_GREEN}${C_BOLD} ✓ ${C_RESET} $*"; }
warn() { printf '%s\n' "${C_YELLOW}${C_BOLD} ! ${C_RESET} $*" >&2; }
die()  { printf '%s\n' "${C_RED}${C_BOLD} ✗ ${C_RESET} $*" >&2; exit 1; }

available() { command -v "$1" >/dev/null 2>&1; }

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------
cleanup() { [ -n "${TMP_DIR}" ] && rm -rf "${TMP_DIR}" 2>/dev/null || true; }
trap cleanup EXIT

# ---------------------------------------------------------------------------
# TTY / interactivity
# ---------------------------------------------------------------------------
detect_tty() {
  # Usable if this process has a controlling terminal we can read from, even
  # when stdin itself is the curl|bash pipe. /dev/tty is the reliable handle.
  if [ -e /dev/tty ] && (exec 3</dev/tty) 2>/dev/null; then
    HAVE_TTY=true
  else
    HAVE_TTY=false
  fi
}

# prompt_line VAR_DEFAULT  -> echoes a line read from the tty, or the default.
read_tty() {
  local default="$1" reply=""
  if [ "${HAVE_TTY}" = "true" ]; then
    # Never let a read failure (EOF/^D) abort the whole script under set -e.
    if ! IFS= read -r reply </dev/tty; then reply=""; fi
  fi
  [ -n "${reply}" ] && printf '%s' "${reply}" || printf '%s' "${default}"
}

# ---------------------------------------------------------------------------
# Step 1 — language
# ---------------------------------------------------------------------------
choose_language() {
  if [ -n "${YOLO_LANG:-}" ]; then
    case "${YOLO_LANG}" in
      zh|en) LANG_CHOICE="${YOLO_LANG}" ;;
      *) LANG_CHOICE="en" ;;
    esac
    return
  fi

  # Default from locale.
  local locale_default="en"
  case "${LC_ALL:-${LANG:-}}" in zh*|ZH*) locale_default="zh" ;; esac

  if [ "${HAVE_TTY}" != "true" ]; then
    LANG_CHOICE="${locale_default}"
    return
  fi

  printf '%s\n' "${C_BOLD}Select language / 选择语言${C_RESET}"
  printf '  1) 中文\n'
  printf '  2) English\n'
  local def_idx="2"; [ "${locale_default}" = "zh" ] && def_idx="1"
  printf 'Enter 1 or 2 [%s]: ' "${def_idx}"
  local choice; choice="$(read_tty "${def_idx}")"
  case "${choice}" in
    1) LANG_CHOICE="zh" ;;
    2) LANG_CHOICE="en" ;;
    *) LANG_CHOICE="${locale_default}" ;;
  esac
}

# ---------------------------------------------------------------------------
# Step 2 — dependency check
# ---------------------------------------------------------------------------
require_cmd() {
  local cmd="$1"
  available "${cmd}" && return 0
  local hint
  hint="$(m \
    "缺少必需命令 '${cmd}'。请先安装它（如 'apt install ${cmd}' 或 'brew install ${cmd}'），然后重试。" \
    "Required command '${cmd}' is missing. Install it first (e.g. 'apt install ${cmd}' or 'brew install ${cmd}'), then retry.")"
  die "${hint}"
}

check_deps() {
  require_cmd curl
  require_cmd tar
  require_cmd uname
  require_cmd grep
  require_cmd sed
  require_cmd awk
  # sha256 tool is platform-specific; presence is enforced in sha256_of.
}

# ---------------------------------------------------------------------------
# Step 3 — platform detection
# ---------------------------------------------------------------------------
detect_platform() {
  local os_raw arch_raw
  os_raw="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch_raw="$(uname -m | tr '[:upper:]' '[:lower:]')"

  case "${os_raw}" in
    linux)  OS="linux" ;;
    darwin) OS="darwin" ;;
    *) die "$(m "不支持的操作系统: ${os_raw}（仅支持 Linux 和 macOS）" \
                "Unsupported OS: ${os_raw} (only Linux and macOS are supported)")" ;;
  esac

  case "${arch_raw}" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) die "$(m "不支持的架构: ${arch_raw}（仅支持 amd64/arm64）" \
                "Unsupported architecture: ${arch_raw} (only amd64/arm64 are supported)")" ;;
  esac

  info "$(printf "$(m '平台: %s/%s' 'Platform: %s/%s')" "${OS}" "${ARCH}")"
}

# ---------------------------------------------------------------------------
# Step 4 — scope (system vs user) + derived paths
# ---------------------------------------------------------------------------
have_root() { [ "$(id -u)" -eq 0 ]; }

choose_scope() {
  local want=""
  if [ -n "${YOLO_SCOPE:-}" ]; then
    case "${YOLO_SCOPE}" in
      system|user) want="${YOLO_SCOPE}" ;;
      *) die "$(m "YOLO_SCOPE 只能是 system 或 user" "YOLO_SCOPE must be 'system' or 'user'")" ;;
    esac
  elif [ "${HAVE_TTY}" = "true" ]; then
    printf '%s\n' "${C_BOLD}$(m '选择安装级别' 'Select install level')${C_RESET}"
    printf '  1) %s\n' "$(m '系统级服务（开机自启，需要 root/sudo）[推荐]' 'System service (starts on boot, needs root/sudo) [recommended]')"
    printf '  2) %s\n' "$(m '用户级服务（免 sudo，随当前用户运行）' 'User service (no sudo, runs under the current user)')"
    printf 'Enter 1 or 2 [1]: '
    case "$(read_tty 1)" in
      2) want="user" ;;
      *) want="system" ;;
    esac
  else
    # Non-interactive: system if we can get root without a password prompt,
    # else fall back to user (a piped shell cannot type a sudo password).
    if have_root; then
      want="system"
    elif available sudo && sudo -n true 2>/dev/null; then
      want="system"
    else
      want="user"
      warn "$(m '无 tty 且无免密 root，自动退回用户级安装（设 YOLO_SCOPE=system 可强制）' \
                'No tty and no passwordless root; falling back to user install (set YOLO_SCOPE=system to force)')"
    fi
  fi

  SCOPE="${want}"
  configure_scope_paths
}

configure_scope_paths() {
  if [ "${SCOPE}" = "system" ]; then
    if have_root; then
      SUDO=""
    elif available sudo; then
      SUDO="sudo"
    else
      die "$(m '系统级安装需要 root 或 sudo，但都不可用。改用 YOLO_SCOPE=user 或以 root 运行。' \
                'System install needs root or sudo, neither is available. Use YOLO_SCOPE=user or run as root.')"
    fi
    BIN_LINK="/usr/local/bin/${BINARY_NAME}"
    if [ "${OS}" = "linux" ]; then
      APP_HOME="/opt/${BINARY_NAME}"
      SVC_USER="${BINARY_NAME}"
      RUN_USER="${BINARY_NAME}"
    else
      APP_HOME="/usr/local/var/${BINARY_NAME}"
      SVC_USER=""                      # macOS: run as the invoking admin, no dedicated user
      RUN_USER="$(logname 2>/dev/null || echo "${SUDO_USER:-$(id -un)}")"
    fi
  else
    SUDO=""
    APP_HOME="${HOME}/.${BINARY_NAME}"
    BIN_LINK="${HOME}/.local/bin/${BINARY_NAME}"
    SVC_USER=""
    RUN_USER="$(id -un)"
  fi
  BIN_DIR="${APP_HOME}/bin"

  # An existing config marks a prior install: this run becomes an upgrade.
  if [ -f "${APP_HOME}/configs/config.yaml" ]; then
    IS_UPGRADE=true
  fi
}

# run a command with the right privilege for the chosen scope
priv() { if [ -n "${SUDO}" ]; then ${SUDO} "$@"; else "$@"; fi; }

# ---------------------------------------------------------------------------
# Step 5 — resolve version
# ---------------------------------------------------------------------------
resolve_version() {
  if [ -n "${YOLO_VERSION:-}" ]; then
    TAG="${YOLO_VERSION}"
    case "${TAG}" in v*) : ;; *) TAG="v${TAG}" ;; esac
    return
  fi
  info "$(m '查询最新版本...' 'Resolving latest release...')"
  local json
  json="$(curl -fsSL "${GITHUB_API}/releases/latest" 2>/dev/null || true)"
  # Trailing `|| true`: under `set -o pipefail` a no-match grep makes the whole
  # substitution non-zero, and a bare assignment would then abort via `set -e`
  # BEFORE the friendly `[ -n "${TAG}" ] || die` guard below ever runs.
  TAG="$(printf '%s' "${json}" | grep '"tag_name"' | head -1 | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/' || true)"
  [ -n "${TAG}" ] || die "$(m "无法获取最新版本（检查网络或 YOLO_REPO=${REPO} 是否有发布）。也可用 YOLO_VERSION 指定。" \
                              "Could not resolve the latest release (check network, or whether YOLO_REPO=${REPO} has any release). You can also set YOLO_VERSION.")"
}

# ---------------------------------------------------------------------------
# Step 6 — download + verify + extract
# ---------------------------------------------------------------------------
sha256_of() {
  if available sha256sum; then
    sha256sum "$1" | awk '{print $1}'
  elif available shasum; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    die "$(m '找不到 sha256sum 或 shasum，无法校验下载完整性。' \
              'Neither sha256sum nor shasum found; cannot verify the download.')"
  fi
}

download_and_extract() {
  TMP_DIR="$(mktemp -d 2>/dev/null || mktemp -d -t yoloinstall)"
  local asset="${BINARY_NAME}_${TAG}_${OS}_${ARCH}.tar.gz"
  local asset_url="${GITHUB_DL}/download/${TAG}/${asset}"
  local sums_url="${GITHUB_DL}/download/${TAG}/checksums.txt"

  info "$(printf "$(m '下载 %s' 'Downloading %s')" "${asset}")"
  curl -fsSL "${asset_url}" -o "${TMP_DIR}/${asset}" \
    || die "$(m "下载失败: ${asset_url}（该平台可能没有对应发布资产）" \
                "Download failed: ${asset_url} (there may be no release asset for this platform)")"

  info "$(m '校验 sha256...' 'Verifying sha256...')"
  curl -fsSL "${sums_url}" -o "${TMP_DIR}/checksums.txt" \
    || die "$(m "下载 checksums.txt 失败: ${sums_url}" "Failed to download checksums.txt: ${sums_url}")"

  local expected actual
  # `|| true` so a missing asset line doesn't abort via set -e/pipefail before
  # the explicit "No checksum entry" die below.
  expected="$(grep " ${asset}\$" "${TMP_DIR}/checksums.txt" | head -1 | awk '{print $1}' || true)"
  [ -n "${expected}" ] || die "$(m "checksums.txt 中找不到 ${asset} 的校验值" \
                                    "No checksum entry for ${asset} in checksums.txt")"
  actual="$(sha256_of "${TMP_DIR}/${asset}")"
  if [ "${expected}" != "${actual}" ]; then
    die "$(m "sha256 校验失败，已中止安装。期望 ${expected}，实得 ${actual}" \
            "sha256 verification failed, aborting. Expected ${expected}, got ${actual}")"
  fi
  ok "$(m 'sha256 校验通过' 'sha256 verified')"

  tar -xzf "${TMP_DIR}/${asset}" -C "${TMP_DIR}" \
    || die "$(m '解压失败' 'Extraction failed')"
  [ -f "${TMP_DIR}/${BINARY_NAME}" ] \
    || die "$(m "压缩包里找不到 ${BINARY_NAME} 二进制" "Binary ${BINARY_NAME} not found in the archive")"
  chmod +x "${TMP_DIR}/${BINARY_NAME}"
}

# ---------------------------------------------------------------------------
# Step 7 — service account (linux system scope only)
# ---------------------------------------------------------------------------
# Set true only when THIS run creates the service account, so uninstall can
# avoid deleting a pre-existing account it merely adopted.
USER_CREATED_BY_INSTALLER=false

ensure_service_user() {
  [ "${OS}" = "linux" ] && [ "${SCOPE}" = "system" ] || return 0
  [ -n "${SVC_USER}" ] || return 0
  if id "${SVC_USER}" >/dev/null 2>&1; then
    return 0
  fi
  info "$(printf "$(m '创建服务用户 %s' 'Creating service user %s')" "${SVC_USER}")"
  if available useradd; then
    priv useradd --system --no-create-home --shell /usr/sbin/nologin "${SVC_USER}" 2>/dev/null \
      || priv useradd -r -s /bin/false "${SVC_USER}" || true
  elif available adduser; then
    priv adduser --system --no-create-home --shell /usr/sbin/nologin "${SVC_USER}" || true
  else
    die "$(m '找不到 useradd/adduser，无法创建服务用户' 'Neither useradd nor adduser found')"
  fi
  # Verify the account actually exists rather than trusting the create command's
  # exit status: adduser variants (e.g. BusyBox) reject the GNU long options and
  # a swallowed failure would otherwise let install proceed to a chown that dies
  # with a confusing "invalid user" instead of this clear message.
  if ! id "${SVC_USER}" >/dev/null 2>&1; then
    die "$(printf "$(m '创建服务用户 %s 失败' 'Failed to create service user %s')" "${SVC_USER}")"
  fi
  USER_CREATED_BY_INSTALLER=true
}

# ---------------------------------------------------------------------------
# Step 8 — lay down app-home + binary
# ---------------------------------------------------------------------------
install_files() {
  info "$(printf "$(m '安装到 %s' 'Installing into %s')" "${APP_HOME}")"
  priv mkdir -p "${BIN_DIR}" "${APP_HOME}/configs" "${APP_HOME}/data" "${APP_HOME}/logs"

  # Stage the new binary next to the target, then atomically rename it into
  # place. A rename replaces the directory entry with a fresh inode, so a
  # still-running old process keeps its own inode — this avoids the ETXTBSY
  # ("Text file busy") that an in-place `cp` over a running executable hits on
  # Linux, and it is never observed half-written. The previous binary is kept
  # as .old so a failed upgrade can roll back to it.
  local staged="${BIN_DIR}/${BINARY_NAME}.new"
  priv cp "${TMP_DIR}/${BINARY_NAME}" "${staged}"
  priv chmod 0755 "${staged}"
  if [ -f "${BIN_DIR}/${BINARY_NAME}" ]; then
    priv cp -p "${BIN_DIR}/${BINARY_NAME}" "${BIN_DIR}/${BINARY_NAME}.old"
  fi
  priv mv -f "${staged}" "${BIN_DIR}/${BINARY_NAME}"

  # Symlink onto PATH.
  local link_dir; link_dir="$(dirname "${BIN_LINK}")"
  priv mkdir -p "${link_dir}"
  priv ln -sf "${BIN_DIR}/${BINARY_NAME}" "${BIN_LINK}"

  # Record that this run created the service account (consumed by uninstall).
  if [ "${USER_CREATED_BY_INSTALLER}" = "true" ]; then
    priv touch "${APP_HOME}/.user_created_by_installer"
  fi

  # Ownership. A FRESH install recurses once to make the whole tree writable by
  # the run user. On UPGRADE we avoid re-walking the (growing) data/ + backups/
  # tree, but still re-own the freshly-copied binary — `priv cp` made it
  # root-owned, and leaving it so breaks a later non-root `yolorouter update`
  # whose self-replace matches the executable's owner.
  if [ "${SCOPE}" = "system" ] && [ "${OS}" = "linux" ] && [ -n "${SVC_USER}" ]; then
    if [ "${IS_UPGRADE}" != "true" ]; then
      priv chown -R "${SVC_USER}:${SVC_USER}" "${APP_HOME}"
    else
      priv chown "${SVC_USER}:${SVC_USER}" "${BIN_DIR}/${BINARY_NAME}"
    fi
  elif [ "${SCOPE}" = "system" ] && [ "${OS}" = "darwin" ]; then
    if [ "${IS_UPGRADE}" != "true" ]; then
      priv chown -R "${RUN_USER}" "${APP_HOME}" 2>/dev/null || true
    else
      priv chown "${RUN_USER}" "${BIN_DIR}/${BINARY_NAME}" 2>/dev/null || true
      # If a DIFFERENT admin account is running this upgrade than installed it,
      # the launchd plist is rewritten for the new account but the 0600
      # config/db stay owned by the old one and the new service can't read
      # them. Re-own the tree only in that (rare) account-change case.
      local cur_owner; cur_owner="$(stat -f '%Su' "${APP_HOME}/configs/config.yaml" 2>/dev/null || echo '')"
      if [ -n "${cur_owner}" ] && [ "${cur_owner}" != "${RUN_USER}" ]; then
        priv chown -R "${RUN_USER}" "${APP_HOME}" 2>/dev/null || true
      fi
    fi
  fi
}

# Roll the binary back to the pre-upgrade copy kept by install_files.
rollback_binary() {
  [ -f "${BIN_DIR}/${BINARY_NAME}.old" ] || return 1
  priv mv -f "${BIN_DIR}/${BINARY_NAME}.old" "${BIN_DIR}/${BINARY_NAME}"
  priv ln -sf "${BIN_DIR}/${BINARY_NAME}" "${BIN_LINK}"
}

# Discard the retained pre-upgrade binary after a confirmed-healthy upgrade.
discard_old_binary() { priv rm -f "${BIN_DIR}/${BINARY_NAME}.old" 2>/dev/null || true; }

# Stop the running service without removing its unit (used before an upgrade
# swaps the binary). Safe/no-op when nothing is installed yet.
stop_service() {
  if [ "${OS}" = "linux" ]; then
    if [ "${SCOPE}" = "system" ] && available systemctl; then
      priv systemctl stop "${BINARY_NAME}" 2>/dev/null || true
    elif available systemctl; then
      systemctl --user stop "${BINARY_NAME}" 2>/dev/null || true
    fi
  else
    local plist; plist="$([ "${SCOPE}" = "system" ] && launchd_daemon_plist || launchd_agent_plist)"
    [ -f "${plist}" ] && { priv launchctl unload "${plist}" 2>/dev/null || true; }
  fi
}

# ---------------------------------------------------------------------------
# Step 9 — upgrade safety: stop old service + back up the DB first
# ---------------------------------------------------------------------------
backup_before_upgrade() {
  [ "${IS_UPGRADE}" = "true" ] || return 0
  if [ "${YOLO_SKIP_BACKUP:-}" = "1" ]; then
    warn "$(m '已按 YOLO_SKIP_BACKUP=1 跳过升级前备份' 'Skipping pre-upgrade backup (YOLO_SKIP_BACKUP=1)')"
    return 0
  fi
  info "$(m '升级前备份数据库...' 'Backing up the database before upgrade...')"
  # Runs from app-home so `configs/config.yaml` resolves; writes into
  # app-home/backups. Fail-closed: this backup is the upgrade's recovery point
  # (serve applies migrations on the next start), so a failed backup MUST abort
  # the upgrade before the binary is touched. Called while the old service is
  # still running, so aborting here leaves it untouched and up. Set
  # YOLO_SKIP_BACKUP=1 to opt out deliberately.
  local rc=0
  if [ "${SCOPE}" = "system" ] && [ "${OS}" = "linux" ] && [ -n "${SVC_USER}" ]; then
    priv su -s /bin/sh "${SVC_USER}" -c \
      "cd '${APP_HOME}' && '${BIN_DIR}/${BINARY_NAME}' db:backup --output-dir '${APP_HOME}/backups'" || rc=$?
  else
    ( cd "${APP_HOME}" && "${BIN_DIR}/${BINARY_NAME}" db:backup --output-dir "${APP_HOME}/backups" ) || rc=$?
  fi
  if [ "${rc}" -ne 0 ]; then
    die "$(m '升级前数据库备份失败，已中止升级（现有服务未受影响）。修复后重试，或设 YOLO_SKIP_BACKUP=1 显式跳过。' \
            'Pre-upgrade database backup failed; upgrade aborted (the running service is untouched). Fix the cause and retry, or set YOLO_SKIP_BACKUP=1 to skip deliberately.')"
  fi
  BACKUP_TAKEN=true
  ok "$(m '数据库已备份' 'Database backed up')"
}

# ---------------------------------------------------------------------------
# Step 10 — service unit (systemd / launchd)
# ---------------------------------------------------------------------------
systemd_system_unit_path() { printf '/etc/systemd/system/%s.service' "${BINARY_NAME}"; }
systemd_user_unit_path()   { printf '%s/.config/systemd/user/%s.service' "${HOME}" "${BINARY_NAME}"; }
launchd_daemon_plist()     { printf '/Library/LaunchDaemons/%s.plist' "${LAUNCHD_LABEL}"; }
launchd_agent_plist()      { printf '%s/Library/LaunchAgents/%s.plist' "${HOME}" "${LAUNCHD_LABEL}"; }

write_systemd_system() {
  local unit; unit="$(systemd_system_unit_path)"
  info "$(printf "$(m '写入 systemd 单元 %s' 'Writing systemd unit %s')" "${unit}")"
  ${SUDO} tee "${unit}" >/dev/null <<EOF
[Unit]
Description=Yolorouter Service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${BIN_DIR}/${BINARY_NAME} serve
WorkingDirectory=${APP_HOME}
User=${SVC_USER}
Group=${SVC_USER}
Restart=always
RestartSec=3
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
  # daemon-reload / restart must not hard-abort under `set -e`: a failure here
  # (non-systemd-PID1 host, a bad unit, a service that won't start) has to fall
  # through to health_check so the upgrade rollback safety net can run instead
  # of leaving a swapped-in binary with the script killed mid-flight.
  priv systemctl daemon-reload || true
  priv systemctl enable "${BINARY_NAME}" >/dev/null 2>&1 || true
  # Capture whether restart actually succeeded (via `if`, which is set-e-safe)
  # instead of swallowing it with `|| true`: a swallowed failure would let a
  # still-running OLD process answer /healthz and be mistaken for a healthy
  # upgrade. The success decision in main() requires this to be true.
  if priv systemctl restart "${BINARY_NAME}"; then SERVICE_START_OK=true; else SERVICE_START_OK=false; fi
}

write_systemd_user() {
  local unit; unit="$(systemd_user_unit_path)"
  info "$(printf "$(m '写入 systemd 用户单元 %s' 'Writing systemd user unit %s')" "${unit}")"
  mkdir -p "$(dirname "${unit}")"
  cat > "${unit}" <<EOF
[Unit]
Description=Yolorouter Service
After=network-online.target

[Service]
Type=simple
ExecStart=${BIN_DIR}/${BINARY_NAME} serve
WorkingDirectory=${APP_HOME}
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
EOF
  # Same as the system path: never let a user-bus failure (headless SSH with no
  # XDG_RUNTIME_DIR / user manager) abort the installer — fall through to
  # health_check, which reports a clear failure instead of a raw bus error.
  systemctl --user daemon-reload || true
  systemctl --user enable "${BINARY_NAME}" >/dev/null 2>&1 || true
  if systemctl --user restart "${BINARY_NAME}"; then SERVICE_START_OK=true; else SERVICE_START_OK=false; fi
  # Boot-start without an active login session requires lingering.
  if available loginctl; then
    loginctl enable-linger "$(id -un)" >/dev/null 2>&1 || true
  fi
}

write_launchd() {
  local plist run_user_line=""
  if [ "${SCOPE}" = "system" ]; then
    plist="$(launchd_daemon_plist)"
    run_user_line="  <key>UserName</key><string>${RUN_USER}</string>"
  else
    plist="$(launchd_agent_plist)"
  fi
  info "$(printf "$(m '写入 launchd 配置 %s' 'Writing launchd plist %s')" "${plist}")"
  priv mkdir -p "$(dirname "${plist}")"
  priv tee "${plist}" >/dev/null <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>${LAUNCHD_LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${BIN_DIR}/${BINARY_NAME}</string>
    <string>serve</string>
  </array>
  <key>WorkingDirectory</key><string>${APP_HOME}</string>
${run_user_line}
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>${APP_HOME}/logs/server.log</string>
  <key>StandardErrorPath</key><string>${APP_HOME}/logs/server.log</string>
</dict>
</plist>
EOF
  # Reload: unload (ignore errors) then load -w. `load -w` works across all
  # supported macOS versions, unlike the newer bootstrap/bootout verbs.
  # `|| true` on load for the same reason as the systemd paths: a load failure
  # must fall through to health_check / rollback, not abort under `set -e`.
  if [ "${SCOPE}" = "system" ]; then
    priv launchctl unload "${plist}" >/dev/null 2>&1 || true
    if priv launchctl load -w "${plist}"; then SERVICE_START_OK=true; else SERVICE_START_OK=false; fi
  else
    launchctl unload "${plist}" >/dev/null 2>&1 || true
    if launchctl load -w "${plist}"; then SERVICE_START_OK=true; else SERVICE_START_OK=false; fi
  fi
}

setup_service() {
  if [ "${OS}" = "linux" ]; then
    if ! available systemctl; then
      die "$(m '未检测到 systemd（systemctl 不可用），当前脚本仅支持 systemd 服务化。' \
              'systemd not detected (no systemctl); this installer only supports systemd on Linux.')"
    fi
    if [ "${SCOPE}" = "system" ]; then write_systemd_system; else write_systemd_user; fi
  else
    available launchctl || die "$(m 'launchctl 不可用' 'launchctl is not available')"
    write_launchd
  fi
}

# ---------------------------------------------------------------------------
# Step 11 — health check + summary
# ---------------------------------------------------------------------------
config_port() {
  local cfg="${APP_HOME}/configs/config.yaml" p=""
  # Read via priv: in system scope the config is 0600 owned by the service
  # user, so a plain read as the invoking user fails and would silently fall
  # back to DEFAULT_PORT — making health_check and the summary probe the wrong
  # port whenever the deployment uses a non-default one.
  # Parse ONLY inside the top-level `server:` block (indented lines up to the
  # next unindented key), so an unrelated `database.port:` a few lines below
  # can never be mistaken for the server port. A `server: {}` with no nested
  # port yields nothing here and correctly falls back to DEFAULT_PORT. Only
  # ever called after the service has started (config exists, sudo cached).
  p="$(priv cat "${cfg}" 2>/dev/null | awk '
    /^server:/ { f=1; next }
    /^[^[:space:]#]/ { f=0 }
    f && match($0, /port:[ \t]*[0-9]+/) {
      s = substr($0, RSTART, RLENGTH); gsub(/[^0-9]/, "", s); print s; exit
    }' || true)"
  [ -n "${p}" ] && printf '%s' "${p}" || printf '%s' "${DEFAULT_PORT}"
}

port_in_use() {
  local port="$1"
  if available lsof; then
    lsof -iTCP:"${port}" -sTCP:LISTEN >/dev/null 2>&1 && return 0
  elif available ss; then
    ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${port}\$" && return 0
  fi
  return 1
}

primary_ip() {
  local ip=""
  if [ "${OS}" = "linux" ]; then
    if available hostname; then ip="$(hostname -I 2>/dev/null | awk '{print $1}')"; fi
    if [ -z "${ip}" ] && available ip; then
      ip="$(ip -4 route get 1.1.1.1 2>/dev/null | grep -oE 'src [0-9.]+' | awk '{print $2}' | head -1)"
    fi
  else
    if available ipconfig; then
      ip="$(ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null || true)"
    fi
  fi
  printf '%s' "${ip}"
}

health_check() {
  local port="$1"
  local i=0
  local url="http://localhost:${port}/healthz"
  info "$(printf "$(m '等待服务就绪（最多 %ss）...' 'Waiting for the service (up to %ss)...')" "${HEALTH_TIMEOUT}")"
  while [ "${i}" -lt "${HEALTH_TIMEOUT}" ]; do
    if curl -fsS "${url}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
    i=$((i + 1))
  done
  return 1
}

logs_hint() {
  if [ "${OS}" = "linux" ] && [ "${SCOPE}" = "system" ]; then
    printf 'journalctl -u %s -f' "${BINARY_NAME}"
  elif [ "${OS}" = "linux" ]; then
    printf 'journalctl --user -u %s -f' "${BINARY_NAME}"
  else
    printf 'tail -f %s/logs/server.log' "${APP_HOME}"
  fi
}

# Populates SVC_STATUS / SVC_STOP / SVC_RESTART. Set via globals (not a
# delimited string) because the commands themselves contain pipes and '&&'.
SVC_STATUS=""; SVC_STOP=""; SVC_RESTART=""
svc_cmds() {
  if [ "${OS}" = "linux" ] && [ "${SCOPE}" = "system" ]; then
    SVC_STATUS="${SUDO:+sudo }systemctl status ${BINARY_NAME}"
    SVC_STOP="${SUDO:+sudo }systemctl stop ${BINARY_NAME}"
    SVC_RESTART="${SUDO:+sudo }systemctl restart ${BINARY_NAME}"
  elif [ "${OS}" = "linux" ]; then
    SVC_STATUS="systemctl --user status ${BINARY_NAME}"
    SVC_STOP="systemctl --user stop ${BINARY_NAME}"
    SVC_RESTART="systemctl --user restart ${BINARY_NAME}"
  else
    local p; p="$([ "${SCOPE}" = "system" ] && launchd_daemon_plist || launchd_agent_plist)"
    SVC_STATUS="launchctl list | grep ${LAUNCHD_LABEL}"
    SVC_STOP="${SUDO:+sudo }launchctl unload ${p}"
    SVC_RESTART="${SUDO:+sudo }launchctl unload ${p} && ${SUDO:+sudo }launchctl load -w ${p}"
  fi
}

print_summary() {
  local port="$1" ip; ip="$(primary_ip)"
  svc_cmds
  local install_url="https://raw.githubusercontent.com/${REPO}/main/scripts/install.sh"

  printf '\n'
  ok "$(m 'yolorouter 已安装并启动' 'yolorouter is installed and running')"
  printf '\n'
  printf '%s\n' "${C_BOLD}$(m '访问控制台：' 'Open the console:')${C_RESET}"
  printf '  http://localhost:%s/\n' "${port}"
  [ -n "${ip}" ] && printf '  http://%s:%s/\n' "${ip}" "${port}"
  printf '%s\n' "$(m '  然后在浏览器里创建第一个管理员账号。' '  Then create the first admin account in your browser.')"
  printf '\n'
  printf '%s\n' "${C_BOLD}$(m '常用命令：' 'Handy commands:')${C_RESET}"
  printf '%s  %s\n' "$(m '  查看状态:' '  status: ')" "${SVC_STATUS}"
  printf '%s  %s\n' "$(m '  查看日志:' '  logs:   ')" "$(logs_hint)"
  printf '%s  %s\n' "$(m '  停止服务:' '  stop:   ')" "${SVC_STOP}"
  printf '%s  %s\n' "$(m '  重启服务:' '  restart:')" "${SVC_RESTART}"
  printf '%s  %s\n' "$(m '  升级:    ' '  upgrade:')" "$(m '重跑安装命令，或 ' 're-run the installer, or ')${BINARY_NAME} update"
  printf '%s  curl -fsSL %s | bash -s -- --uninstall\n' "$(m '  卸载:    ' '  remove: ')" "${install_url}"
  # If the symlink dir isn't on PATH (common for ~/.local/bin in user scope),
  # the bare `yolorouter …` commands above won't resolve — point at the real
  # binary and how to fix PATH.
  case ":${PATH}:" in
    *":$(dirname "${BIN_LINK}"):"*) : ;;
    *)
      printf '%s\n' "$(printf "$(m '  注意：%s 不在 PATH 中——用 %s 调用，或把该目录加进 PATH。' \
                                   '  Note: %s is not on PATH — invoke %s directly, or add that dir to PATH.')" \
                              "$(dirname "${BIN_LINK}")" "${BIN_DIR}/${BINARY_NAME}")" ;;
  esac
  printf '\n'
  printf '%s\n' "$(m "  改端口 = 编辑 ${APP_HOME}/configs/config.yaml 的 server.port 后重启服务。" \
                     "  Change port = edit server.port in ${APP_HOME}/configs/config.yaml, then restart.")"
  printf '%s\n' "$(m "  需要 PostgreSQL？编辑同一文件的 database 段后重启。" \
                     "  Need PostgreSQL? Edit the database section in that file, then restart.")"
}

# ---------------------------------------------------------------------------
# Uninstall
# ---------------------------------------------------------------------------
# For uninstall, resolve the scope from what is actually on disk rather than
# the install-oriented guess choose_scope made. Without this, a no-tty uninstall
# of a system install (choose_scope falls back to user scope when it can't get
# root non-interactively) would target the empty user location and report
# success while the real system service keeps running. An explicit YOLO_SCOPE
# still wins.
resolve_uninstall_scope() {
  # An explicit YOLO_SCOPE, or an interactive choice the user just made in
  # choose_scope, both win — only infer from disk in the non-interactive
  # fallback, where choose_scope merely guessed a scope from privilege.
  [ -z "${YOLO_SCOPE:-}" ] || return 0
  [ "${HAVE_TTY}" = "true" ] && return 0
  local sys_home sys_unit user_home user_unit
  if [ "${OS}" = "linux" ]; then
    sys_home="/opt/${BINARY_NAME}"
    sys_unit="/etc/systemd/system/${BINARY_NAME}.service"
    user_home="${HOME}/.${BINARY_NAME}"
    user_unit="${HOME}/.config/systemd/user/${BINARY_NAME}.service"
  else
    sys_home="/usr/local/var/${BINARY_NAME}"
    sys_unit="/Library/LaunchDaemons/${LAUNCHD_LABEL}.plist"
    user_home="${HOME}/.${BINARY_NAME}"
    user_unit="${HOME}/Library/LaunchAgents/${LAUNCHD_LABEL}.plist"
  fi
  if [ -d "${sys_home}" ] || [ -e "${sys_unit}" ] || [ -e "/usr/local/bin/${BINARY_NAME}" ]; then
    SCOPE="system"
  elif [ -d "${user_home}" ] || [ -e "${user_unit}" ]; then
    SCOPE="user"
  else
    return 0
  fi
  # Re-derive SUDO/APP_HOME/BIN_LINK/SVC_USER for the resolved scope. For a
  # system scope with no root/sudo this deliberately dies with a clear message
  # rather than pretending to uninstall.
  configure_scope_paths
}

do_uninstall() {
  info "$(m '卸载 yolorouter...' 'Uninstalling yolorouter...')"

  # Stop + remove the service unit.
  if [ "${OS}" = "linux" ]; then
    if [ "${SCOPE}" = "system" ] && available systemctl; then
      priv systemctl stop "${BINARY_NAME}" 2>/dev/null || true
      priv systemctl disable "${BINARY_NAME}" 2>/dev/null || true
      priv rm -f "$(systemd_system_unit_path)"
      priv systemctl daemon-reload 2>/dev/null || true
    elif available systemctl; then
      systemctl --user stop "${BINARY_NAME}" 2>/dev/null || true
      systemctl --user disable "${BINARY_NAME}" 2>/dev/null || true
      rm -f "$(systemd_user_unit_path)"
      systemctl --user daemon-reload 2>/dev/null || true
    fi
  else
    local plist; plist="$([ "${SCOPE}" = "system" ] && launchd_daemon_plist || launchd_agent_plist)"
    priv launchctl unload "${plist}" 2>/dev/null || true
    priv rm -f "${plist}"
  fi

  # Remove binary + symlink (including any retained pre-upgrade copy).
  priv rm -f "${BIN_LINK}"
  priv rm -f "${BIN_DIR}/${BINARY_NAME}" "${BIN_DIR}/${BINARY_NAME}.old"

  # Remove the dedicated service user only if THIS installer created it — a
  # pre-existing account we merely adopted may belong to another service or be
  # externally provisioned, so deleting it could break unrelated access.
  if [ "${OS}" = "linux" ] && [ "${SCOPE}" = "system" ] && [ -n "${SVC_USER}" ] && id "${SVC_USER}" >/dev/null 2>&1; then
    if [ -f "${APP_HOME}/.user_created_by_installer" ]; then
      if available userdel; then priv userdel "${SVC_USER}" 2>/dev/null || true; fi
    else
      warn "$(printf "$(m '服务用户 %s 非本安装器创建，保留不删。' \
                          'Service user %s was not created by this installer; leaving it in place.')" "${SVC_USER}")"
    fi
  fi

  # Data is preserved unless the user explicitly confirms deletion.
  if [ -d "${APP_HOME}" ]; then
    local del="n"
    if [ "${HAVE_TTY}" = "true" ]; then
      printf '%s ' "$(m "删除全部数据？这会清除 ${APP_HOME}（配置 + 数据库，不可恢复） [y/N]:" \
                         "Delete all data? This wipes ${APP_HOME} (config + database, irreversible) [y/N]:")"
      del="$(read_tty n)"
    fi
    case "${del}" in
      y|Y|yes|YES)
        priv rm -rf "${APP_HOME}"
        ok "$(m '数据已删除' 'Data removed')" ;;
      *)
        ok "$(printf "$(m '已保留数据目录：%s' 'Data directory preserved: %s')" "${APP_HOME}")" ;;
    esac
  fi

  ok "$(m '卸载完成' 'Uninstall complete')"
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
main() {
  local do_remove=false
  for arg in "$@"; do
    case "${arg}" in
      --uninstall) do_remove=true ;;
      -h|--help)
        printf 'yolorouter installer\n  --uninstall   remove yolorouter\n'
        printf 'Env: YOLO_LANG YOLO_SCOPE YOLO_VERSION YOLO_REPO YOLO_UNINSTALL\n'
        exit 0 ;;
    esac
  done
  [ "${YOLO_UNINSTALL:-}" = "1" ] && do_remove=true

  detect_tty
  choose_language
  check_deps
  detect_platform
  choose_scope

  if [ "${do_remove}" = "true" ]; then
    resolve_uninstall_scope
    do_uninstall
    exit 0
  fi

  # Warn (don't block) on a FRESH install if the default port already has a
  # listener. Skipped on upgrade: the existing config's port is respected, and
  # reading it here would be a needless privileged (sudo) read whose result the
  # warning below never consumes anyway.
  local port="${DEFAULT_PORT}"
  if [ "${IS_UPGRADE}" != "true" ] && port_in_use "${DEFAULT_PORT}"; then
    warn "$(printf "$(m '端口 %s 已被占用，服务可能无法监听；装完请改端口或释放占用。' \
                        'Port %s is already in use; the service may fail to bind. Change the port or free it after install.')" "${DEFAULT_PORT}")"
  fi

  resolve_version
  download_and_extract
  ensure_service_user

  # Upgrade ordering: back up first (fail-closed, while the old service is
  # still up so an abort leaves it untouched), then stop it before the binary
  # is swapped so the new process starts clean and migrations apply against a
  # backed-up DB.
  backup_before_upgrade
  [ "${IS_UPGRADE}" = "true" ] && stop_service
  install_files
  setup_service

  port="$(config_port)"
  # Require BOTH: the service manager reported a successful (re)start AND the
  # port is healthy. Without the start check, a failed restart that left an old
  # process listening (e.g. a headless user-scope upgrade with no user bus)
  # would answer /healthz and be misread as a successful upgrade.
  if [ "${SERVICE_START_OK}" = "true" ] && health_check "${port}"; then
    [ "${IS_UPGRADE}" = "true" ] && discard_old_binary
    print_summary "${port}"
  else
    warn "$(m '服务未在预期时间内通过健康检查。' 'The service did not pass the health check in time.')"
    # On a failed upgrade, roll the binary back to the retained copy and
    # restart the old version so the box isn't left with a down service.
    if [ "${IS_UPGRADE}" = "true" ] && rollback_binary; then
      warn "$(m '已回滚到升级前的二进制并重启。' 'Rolled back to the pre-upgrade binary and restarted.')"
      stop_service
      setup_service
      if health_check "${port}"; then
        warn "$(m '旧版本已恢复运行；本次升级失败，请查日志。' 'The previous version is running again; the upgrade failed — check the logs.')"
      elif [ "${BACKUP_TAKEN}" = "true" ]; then
        # The new binary may have already migrated the DB schema on start, which
        # the rolled-back older binary can reject. Point at the pre-upgrade
        # backup — only if one was actually taken, and without assuming the
        # driver (SQLite ships a .db.gz, PostgreSQL a .sql.gz restored via psql).
        warn "$(printf "$(m '旧版本仍未起来——新版本可能已迁移数据库。升级前备份在 %s，必要时停服后按你的数据库类型恢复最近一次备份。' \
                            'The old version is still down — the newer one may have migrated the database. A pre-upgrade backup is in %s; if needed, stop the service and restore the latest one per your database driver.')" "${APP_HOME}/backups")"
      else
        warn "$(m '旧版本仍未起来，且本次未做升级前备份（YOLO_SKIP_BACKUP=1）；请查日志手动处理。' \
                 'The old version is still down and no pre-upgrade backup was taken (YOLO_SKIP_BACKUP=1); check the logs and recover manually.')"
      fi
    fi
    printf '%s\n' "$(m "  查看日志: $(logs_hint)" "  Check logs: $(logs_hint)")"
    exit 1
  fi
}

main "$@"
