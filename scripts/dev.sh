#!/usr/bin/env bash
# scripts/dev.sh — local one-shot rebuild + restart for yolorouter.
# Usage: ./scripts/dev.sh [--backend|--frontend|--migrate|--restart|--help]
set -euo pipefail

# ---------------------------------------------------------------------------
# Language: YOLO_LANG (zh|en) overrides; otherwise auto-detect from locale.
# Zero interaction — never prompt. Regular user-facing output is bilingual via
# t(); rare deep-diagnostic branches (PID reuse, lock TOCTOU) stay English.
# ---------------------------------------------------------------------------
_detect_lang() {
  case "${YOLO_LANG:-}" in
    zh|en) echo "${YOLO_LANG}"; return ;;
  esac
  case "${LC_ALL:-${LANG:-}}" in
    zh*) echo "zh" ;;
    *)   echo "en" ;;
  esac
}
LANG_SEL="$(_detect_lang)"

# t <en> <zh> — print the string matching the selected language.
t() { if [ "${LANG_SEL}" = "zh" ]; then printf '%s' "$2"; else printf '%s' "$1"; fi; }

# ---------------------------------------------------------------------------
# Colours: only when stdout is a tty and NO_COLOR is unset.
# ---------------------------------------------------------------------------
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_CYAN=$'\033[36m'; C_GREEN=$'\033[32m'; C_RED=$'\033[31m'; C_YELLOW=$'\033[33m'; C_RESET=$'\033[0m'
else
  C_CYAN=""; C_GREEN=""; C_RED=""; C_YELLOW=""; C_RESET=""
fi
step()  { printf '%s==> %s%s\n' "${C_CYAN}" "$*" "${C_RESET}"; }
ok()    { printf '%s✓ %s%s\n'  "${C_GREEN}" "$*" "${C_RESET}"; }
warn()  { printf '%s%s%s\n'    "${C_YELLOW}" "$*" "${C_RESET}"; }
err()   { printf '%s%s%s\n'    "${C_RED}" "$*" "${C_RESET}" >&2; }

usage() {
  if [ "${LANG_SEL}" = "zh" ]; then
    cat <<EOF
用法: $0 [模式]

模式:
  (无)         全量：构建前端 + 后端 + 重启（默认）
  --backend    仅重新构建 Go 二进制并重启（跳过前端）
  --frontend   仅重新构建前端并重启（跳过后端专有步骤）
  --migrate    仅执行 db:migrate 并重启
  --restart    仅重启，不构建
  --help, -h   显示本帮助

环境变量:
  YOLO_LANG=zh|en   强制输出语言（默认按系统 locale 自动判定）
  NO_COLOR          设置后禁用彩色输出
EOF
  else
    cat <<EOF
Usage: $0 [mode]

Modes:
  (none)       Full rebuild: frontend + backend + restart (default)
  --backend    Rebuild the Go binary and restart (skip frontend)
  --frontend   Rebuild the frontend and restart (skip backend-only steps)
  --migrate    Run db:migrate and restart
  --restart    Restart only, no build
  --help, -h   Show this help

Environment:
  YOLO_LANG=zh|en   Force output language (default: auto-detect from locale)
  NO_COLOR          Disable coloured output when set
EOF
  fi
}

MODE="${1:-}"
BUILD_FRONTEND=true
BUILD_BACKEND=true
EXPLICIT_MIGRATE=false

case "${MODE}" in
  --backend)  BUILD_FRONTEND=false ;;
  --frontend) BUILD_BACKEND=false ;;
  --migrate)  BUILD_FRONTEND=false; BUILD_BACKEND=false; EXPLICIT_MIGRATE=true ;;
  --restart)  BUILD_FRONTEND=false; BUILD_BACKEND=false ;;
  --help|-h)  usage; exit 0 ;;
  "") ;;
  *)
    err "$(t "Unknown option: " "未知参数：")${MODE}"
    usage >&2
    exit 1
    ;;
esac

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
LOG_DIR="${ROOT_DIR}/logs"
PID_FILE="${LOG_DIR}/dev.pid"
BIN_PATH="${ROOT_DIR}/bin/yolorouter"
mkdir -p "${LOG_DIR}"

# ---------------------------------------------------------------------------
# Dependency check (mode-aware): only require what the selected mode uses.
#   frontend build -> npm ; backend build -> go ; healthz probe -> curl
# Missing -> precise install hint + non-zero exit. Never auto-install.
# ---------------------------------------------------------------------------
_require() {
  # _require <cmd> <macos-hint> <linux-hint>
  command -v "$1" >/dev/null 2>&1 && return 0
  err "$(t "Missing required command: " "缺少必需命令：")$1"
  if [ "$(uname -s)" = "Darwin" ]; then
    err "  $(t "Install it with: " "安装方式：")$2"
  else
    err "  $(t "Install it with: " "安装方式：")$3"
  fi
  exit 1
}
[ "${BUILD_FRONTEND}" = "true" ] && _require npm  "brew install node" "apt install nodejs npm"
[ "${BUILD_BACKEND}"  = "true" ] && _require go   "brew install go"   "apt install golang-go"
_require curl "brew install curl" "apt install curl"

# Serialize the whole stop/build/start/publish sequence below across
# concurrent dev.sh invocations — mkdir is atomic on any POSIX filesystem, so
# exactly one concurrent invocation wins it. Without this, two invocations
# could both pass the "is the old PID still running our binary" check before
# either signals it, then race to write PID_FILE — the loser's `rm -f
# "${PID_FILE}"` cleanup could delete the winner's freshly-published file.
#
# Deliberately fail-closed with no automatic stale-lock reclaim: an earlier
# version tried detecting a dead owner (via a recorded PID) and
# rm -rf + re-mkdir'ing the lock automatically, but that reintroduced a real
# TOCTOU race between the read-owner/rm/mkdir steps across two concurrent
# invocations — a `flock`-style kernel advisory lock (held via an open fd,
# auto-released on process exit, immune to this class of race) would fix it
# properly, but `flock` isn't reliably available on macOS dev machines
# without an extra Homebrew dependency. For a local single-developer
# convenience script, requiring a manual `rmdir` after confirming no other
# instance is running is a better trade than a lock that can silently let
# two instances both believe they're holding it.
LOCK_DIR="${LOG_DIR}/dev.sh.lock"
if ! mkdir "${LOCK_DIR}" 2>/dev/null; then
  echo "ERROR: another dev.sh invocation appears to be in progress (lock dir: ${LOCK_DIR})" >&2
  echo "If you're sure no other dev.sh is running, remove that directory and retry:" >&2
  echo "  rmdir '${LOCK_DIR}'" >&2
  exit 1
fi
trap 'rmdir "${LOCK_DIR}" 2>/dev/null || true' EXIT

step "$(t "Stopping existing yolorouter process (if any)" "停止已有的 yolorouter 进程（如果有）")"
# Track our own PID (plus its process start time) via a PID file rather than
# `pkill -f`/`pgrep -f` pattern matching — a path-based pattern like
# "./bin/yolorouter serve" can match an unrelated process (a different
# checkout, a different user's shell) that happens to share the same
# relative invocation. We only ever signal a PID this exact script
# previously recorded, and only after confirming that PID's exact full
# command line still points at our own BIN_PATH *and* its process start
# time still matches what we recorded — a bare comm-name substring match
# isn't enough, since PIDs get reused: a fast enough exit-and-reuse could
# hand the same PID to an unrelated process that happens to also be named
# "yolorouter" (e.g. another checkout on the same machine).
STOPPED_OLD=false
if [ -f "${PID_FILE}" ]; then
  OLD_PID="$(sed -n '1p' "${PID_FILE}" 2>/dev/null || true)"
  OLD_START="$(sed -n '2p' "${PID_FILE}" 2>/dev/null || true)"
  # A malformed/half-written PID file (or one from an older script version
  # without a start-time line) must never be trusted enough to signal —
  # reject anything that isn't a plain positive integer PID. This also
  # rejects "0"/"00": `kill -0 0` targets the entire process group, not a
  # single process, which a leading-zero value would otherwise slip through
  # as if it were just another digit string.
  case "${OLD_PID}" in
    ''|*[!0-9]*|0*) OLD_PID="" ;;
  esac
  if [ -n "${OLD_PID}" ] && kill -0 "${OLD_PID}" 2>/dev/null; then
    OLD_ARGS="$(ps -p "${OLD_PID}" -o args= 2>/dev/null || true)"
    CUR_START="$(ps -p "${OLD_PID}" -o lstart= 2>/dev/null || true)"
    # Exact match (or BIN_PATH followed by a space, i.e. "BIN_PATH serve"),
    # not a bare prefix — a prefix match like "${BIN_PATH}"* would also
    # accept an unrelated "yolorouter-helper" binary whose path happens
    # to start with the same string.
    case "${OLD_ARGS}" in
      "${BIN_PATH}"|"${BIN_PATH} "*) : ;;
      *)
        echo "  PID ${OLD_PID} is no longer running our binary; not signaling it"
        rm -f "${PID_FILE}"
        OLD_PID=""
        ;;
    esac
    # An empty recorded start time (malformed file, or `ps` failed at
    # write time) must be treated as "can't verify" rather than "skip the
    # check" — silently falling through here is what let a reused PID slip
    # past this guard in an earlier version.
    if [ -n "${OLD_PID}" ] && { [ -z "${OLD_START}" ] || [ "${OLD_START}" != "${CUR_START}" ]; }; then
      echo "  PID ${OLD_PID} was reused by a different process (start time changed or unverifiable); not signaling it"
      rm -f "${PID_FILE}"
      OLD_PID=""
    fi
  fi
  if [ -n "${OLD_PID}" ] && kill -0 "${OLD_PID}" 2>/dev/null; then
    kill -TERM "${OLD_PID}" 2>/dev/null || true
    STOPPED_OLD=true
    # serve holds the cross-process instance lock for its entire graceful
    # shutdown sequence (up to a ~15s budget), so the next `serve`
    # invocation below would otherwise race it and fail with "another
    # instance appears to be running". Wait for the old process to actually
    # exit instead of just firing the signal and moving on.
    echo "  waiting for previous instance (PID ${OLD_PID}) to shut down..."
    i=0
    while [ "${i}" -lt 60 ]; do
      kill -0 "${OLD_PID}" 2>/dev/null || break
      sleep 0.5
      i=$((i + 1))
    done
    if kill -0 "${OLD_PID}" 2>/dev/null; then
      echo "  ERROR: previous instance did not exit within 30s" >&2
      exit 1
    fi
  fi
  rm -f "${PID_FILE}"
fi
if [ "${STOPPED_OLD}" = "false" ]; then
  echo "  $(t "no running process" "没有正在运行的进程")"
fi

# ---------------------------------------------------------------------------
# Port pre-check: our own instance is now stopped, so anything still listening
# on the configured port is a foreign process. Probe with bash's built-in
# /dev/tcp (zero extra dependency) and fail with a clear message rather than
# letting serve die on a buried "bind: address already in use".
# ---------------------------------------------------------------------------
PORT="$(grep -A2 '^server:' "${ROOT_DIR}/configs/config.yaml" 2>/dev/null | grep 'port:' | head -1 | sed 's/[^0-9]*//g' || true)"
PORT="${PORT:-8080}"
if (exec 3<>"/dev/tcp/127.0.0.1/${PORT}") 2>/dev/null; then
  exec 3>&- 3<&-
  err "$(t "Port ${PORT} is already in use by another process (not managed by dev.sh)." "端口 ${PORT} 已被非本脚本管理的进程占用。")"
  err "$(t "Free it, or change server.port in configs/config.yaml, then retry." "请释放该端口，或修改 configs/config.yaml 的 server.port 后重试。")"
  exit 1
fi

if [ "${BUILD_FRONTEND}" = "true" ]; then
  step "$(t "Building frontend" "构建前端")"
  # Remove frontend/dist first so a misconfigured build (e.g. an outDir
  # typo in vite.config that leaves npm run build writing somewhere else
  # entirely) can't leave a stale, previously-successful build sitting
  # there for the copy step below to pick up as if it were fresh.
  rm -rf "${ROOT_DIR}/frontend/dist"
  # npm ci (not npm install) for a reproducible install from package-lock.json.
  (cd "${ROOT_DIR}/frontend" && npm ci && npm run build)
  step "$(t "Copying frontend dist into Go embed target" "复制前端 dist 到 Go embed 目标目录")"
  # web/dist/ is fully gitignored — nothing tracked lives in there, so a plain
  # rm -rf + recopy is safe and never touches git-tracked state.
  rm -rf "${ROOT_DIR}/web/dist"
  mkdir -p "${ROOT_DIR}/web/dist"
  cp -r "${ROOT_DIR}/frontend/dist/." "${ROOT_DIR}/web/dist/"
fi

if [ "${BUILD_BACKEND}" = "true" ] || [ "${BUILD_FRONTEND}" = "true" ]; then
  step "$(t "Building Go binary" "构建 Go 二进制")"
  # -tags embed only when web/dist/ actually has a real frontend build to
  # embed — building with the tag against an empty dist/ fails to compile
  # (see web/embed_real.go), which would break --backend/--restart modes on
  # a checkout that never ran the frontend build at all.
  BUILD_TAGS=""
  if find "${ROOT_DIR}/web/dist" -mindepth 1 -type f 2>/dev/null | grep -q .; then
    BUILD_TAGS="-tags embed"
  fi
  # shellcheck disable=SC2086 # BUILD_TAGS is intentionally either empty or a single flag token
  (cd "${ROOT_DIR}" && go build ${BUILD_TAGS} -o "${BIN_PATH}" ./cmd/yolorouter)
fi

if [ "${EXPLICIT_MIGRATE}" = "true" ]; then
  step "$(t "Running explicit db:migrate" "执行 db:migrate")"
  (cd "${ROOT_DIR}" && "${BIN_PATH}" db:migrate)
fi

step "$(t "Starting yolorouter serve" "启动 yolorouter serve")"
(cd "${ROOT_DIR}" && exec "${BIN_PATH}" serve >> "${LOG_DIR}/server.log" 2>&1) &
SERVER_PID=$!
sleep 0.2 # give the OS a moment to make the process visible to `ps` below
SERVER_START="$(ps -p "${SERVER_PID}" -o lstart= 2>/dev/null || true)"
# Write via a temp file + mv (same-filesystem rename is atomic) rather than
# a direct redirect, so a concurrent reader of PID_FILE (e.g. another
# dev.sh invocation) never observes a partially-written file.
PID_FILE_TMP="${PID_FILE}.tmp.$$"
{ echo "${SERVER_PID}"; echo "${SERVER_START}"; } > "${PID_FILE_TMP}"
mv "${PID_FILE_TMP}" "${PID_FILE}"

# Re-read the actual configured port rather than assuming the default — config.yaml
# is guaranteed to exist by now (either pre-existing or just auto-generated by
# the serve invocation above), and its port may differ from the pre-check default.
PORT="$(grep -A2 '^server:' "${ROOT_DIR}/configs/config.yaml" 2>/dev/null | grep 'port:' | head -1 | sed 's/[^0-9]*//g')"
PORT="${PORT:-8080}"

# ---------------------------------------------------------------------------
# Readiness: poll /healthz until it answers 2xx (up to ~15s) instead of a bare
# "process didn't crash in 1s" check. A live process is not a ready server —
# DB migration / port binding may still be in flight. On timeout or an early
# exit, print the tail of the log and fail non-zero.
# ---------------------------------------------------------------------------
step "$(t "Waiting for /healthz" "等待 /healthz 就绪")"
READY=false
i=0
while [ "${i}" -lt 30 ]; do
  if ! kill -0 "${SERVER_PID}" 2>/dev/null; then
    err "$(t "yolorouter exited before becoming ready." "yolorouter 在就绪前已退出。")"
    break
  fi
  if curl -fsS -o /dev/null "http://127.0.0.1:${PORT}/healthz" 2>/dev/null; then
    READY=true
    break
  fi
  sleep 0.5
  i=$((i + 1))
done

if [ "${READY}" != "true" ]; then
  err "$(t "Health check failed — last lines of ${LOG_DIR}/server.log:" "健康检查失败 —— ${LOG_DIR}/server.log 尾部：")"
  tail -n 20 "${LOG_DIR}/server.log" >&2 2>/dev/null || true
  rm -f "${PID_FILE}"
  exit 1
fi

printf '\n'
ok "$(t "yolorouter ready" "yolorouter 已就绪") (PID ${SERVER_PID})"
printf '  %-8s http://127.0.0.1:%s/\n'         "$(t 'App:'     '访问:')"  "${PORT}"
printf '  %-8s http://127.0.0.1:%s/healthz\n'  "$(t 'Health:'  '健康:')"  "${PORT}"
printf '  %-8s tail -f %s/server.log\n'        "$(t 'Logs:'    '日志:')"  "${LOG_DIR}"
printf '  %-8s ./scripts/dev.sh --restart\n'   "$(t 'Restart:' '重启:')"
printf '  %-8s kill %s\n'                      "$(t 'Stop:'    '停止:')"  "${SERVER_PID}"
