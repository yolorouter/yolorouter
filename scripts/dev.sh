#!/usr/bin/env bash
# scripts/dev.sh — local one-shot rebuild + restart for yolorouter-ce.
# Usage: ./scripts/dev.sh [--backend|--frontend|--migrate|--restart]
set -euo pipefail

MODE="${1:-}"
BUILD_FRONTEND=true
BUILD_BACKEND=true
EXPLICIT_MIGRATE=false

case "${MODE}" in
  --backend)  BUILD_FRONTEND=false ;;
  --frontend) BUILD_BACKEND=false ;;
  --migrate)  BUILD_FRONTEND=false; BUILD_BACKEND=false; EXPLICIT_MIGRATE=true ;;
  --restart)  BUILD_FRONTEND=false; BUILD_BACKEND=false ;;
  "") ;;
  *) echo "Unknown option: ${MODE}"; echo "Usage: $0 [--backend|--frontend|--migrate|--restart]"; exit 1 ;;
esac

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
LOG_DIR="${ROOT_DIR}/logs"
PID_FILE="${LOG_DIR}/dev.pid"
BIN_PATH="${ROOT_DIR}/bin/yolorouter-ce"
mkdir -p "${LOG_DIR}"

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

echo "==> Stopping existing yolorouter-ce process (if any)"
# Track our own PID (plus its process start time) via a PID file rather than
# `pkill -f`/`pgrep -f` pattern matching — a path-based pattern like
# "./bin/yolorouter-ce serve" can match an unrelated process (a different
# checkout, a different user's shell) that happens to share the same
# relative invocation. We only ever signal a PID this exact script
# previously recorded, and only after confirming that PID's exact full
# command line still points at our own BIN_PATH *and* its process start
# time still matches what we recorded — a bare comm-name substring match
# isn't enough, since PIDs get reused: a fast enough exit-and-reuse could
# hand the same PID to an unrelated process that happens to also be named
# "yolorouter-ce" (e.g. another checkout on the same machine).
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
    # accept an unrelated "yolorouter-ce-helper" binary whose path happens
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
    # shutdown sequence (up to the ~15s budget in §9), so the next `serve`
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
  echo "  no running process"
fi

if [ "${BUILD_FRONTEND}" = "true" ]; then
  echo "==> Building frontend"
  # npm ci (not npm install) for a reproducible install from package-lock.json.
  (cd "${ROOT_DIR}/frontend" && npm ci && npm run build)
  echo "==> Copying frontend dist into Go embed target"
  rm -rf "${ROOT_DIR}/internal/web/dist"
  mkdir -p "${ROOT_DIR}/internal/web/dist"
  cp -r "${ROOT_DIR}/frontend/dist/." "${ROOT_DIR}/internal/web/dist/"
fi

if [ "${BUILD_BACKEND}" = "true" ] || [ "${BUILD_FRONTEND}" = "true" ]; then
  echo "==> Building Go binary"
  (cd "${ROOT_DIR}" && go build -o "${BIN_PATH}" ./cmd/yolorouter-ce)
fi

if [ "${EXPLICIT_MIGRATE}" = "true" ]; then
  echo "==> Running explicit db:migrate"
  (cd "${ROOT_DIR}" && "${BIN_PATH}" db:migrate)
fi

echo "==> Starting yolorouter-ce serve"
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
sleep 1

if ! kill -0 "${SERVER_PID}" 2>/dev/null; then
  echo "ERROR: yolorouter-ce exited immediately — check ${LOG_DIR}/server.log"
  rm -f "${PID_FILE}"
  exit 1
fi

# Read the actual configured port rather than assuming the default — config.yaml
# is guaranteed to exist by now (either pre-existing or just auto-generated by
# the serve invocation above), and its port may not be 8080.
PORT="$(grep -A2 '^server:' "${ROOT_DIR}/configs/config.yaml" 2>/dev/null | grep 'port:' | head -1 | sed 's/[^0-9]*//g')"
PORT="${PORT:-8080}"

echo ""
echo "yolorouter-ce running (PID ${SERVER_PID})"
echo "  http://127.0.0.1:${PORT}/healthz"
echo "  log: ${LOG_DIR}/server.log"
echo "  stop: kill ${SERVER_PID}"
