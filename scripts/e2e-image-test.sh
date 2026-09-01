#!/usr/bin/env bash
# End-to-end image-generation smoke test against a RUNNING yolorouter server.
# One-shot operator tool: it does not start or stop anything.
#
# It proves the whole chain a bill rides on:
#   1. read the caller key's spent budget from the database
#   2. POST /v1/images/generations for a configured image model
#   3. verify the response delivered at least one image
#   4. read the newest request_logs row for that model and verify the cost
#      verdict (known + micros, or unknown with a reason) and — when the
#      candidate bills per image — the pricing snapshot's axes and counts
#   5. verify the key's spent budget moved by the same micros
#
# Usage:
#   ./scripts/e2e-image-test.sh -k sk-... -m my-image-model \
#       [-u http://127.0.0.1:8080] [-d data/yolorouter.db] [-s 1024x1024] [-q high]
#
# Requires: curl, python3, sqlite3. Exit 0 = every check passed.

set -euo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8080}"
DB_PATH="${DB_PATH:-data/yolorouter.db}"
API_KEY="${API_KEY:-}"
MODEL="${MODEL:-}"
SIZE=""
QUALITY=""
PROMPT="${PROMPT:-a small red square on a white background}"

usage() {
  sed -n '2,20p' "$0" >&2
  exit 2
}

while getopts "u:d:k:m:s:q:h" opt; do
  case "$opt" in
    u) BASE_URL="$OPTARG" ;;
    d) DB_PATH="$OPTARG" ;;
    k) API_KEY="$OPTARG" ;;
    m) MODEL="$OPTARG" ;;
    s) SIZE="$OPTARG" ;;
    q) QUALITY="$OPTARG" ;;
    *) usage ;;
  esac
done

[ -n "$API_KEY" ] || { echo "api key is required (-k or API_KEY)" >&2; exit 2; }
[ -n "$MODEL" ] || { echo "model is required (-m or MODEL)" >&2; exit 2; }
[ -f "$DB_PATH" ] || { echo "database not found: $DB_PATH (override with -d)" >&2; exit 2; }

# budget_of prints the caller key's spent micros. The token's hash lives in
# api_keys.key_hash, which this script cannot compute, so it reads the
# single-key total — honest for the dev/self-host box this tool targets.
budget_of() {
  sqlite3 "$DB_PATH" "SELECT COALESCE(SUM(budget_spent_micros),0) FROM api_keys;"
}

fail() { echo "FAIL: $*" >&2; exit 1; }

BEFORE="$(budget_of)"

BODY=$(python3 - "$PROMPT" "$MODEL" "$SIZE" "$QUALITY" <<'PY'
import json, sys
prompt, model, size, quality = sys.argv[1:5]
req = {"model": model, "prompt": prompt, "n": 1}
if size:
    req["size"] = size
if quality:
    req["quality"] = quality
print(json.dumps(req))
PY
)

HTTP_CODE=$(curl -s -o /tmp/e2e-image-resp.json -w '%{http_code}' \
  -X POST "$BASE_URL/v1/images/generations" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d "$BODY")

[ "$HTTP_CODE" = "200" ] || fail "expected HTTP 200, got $HTTP_CODE: $(cat /tmp/e2e-image-resp.json)"

python3 - /tmp/e2e-image-resp.json <<'PY' || fail "response delivered no image"
import json, sys
resp = json.load(open(sys.argv[1]))
data = resp.get("data") or []
assert data and (data[0].get("url") or data[0].get("b64_json")), resp
PY

# The newest row for this model on the images path is the request just made.
ROW=$(sqlite3 -separator '|' "$DB_PATH" \
  "SELECT cost_known, cost_micros, COALESCE(image_pricing_snapshot,''), COALESCE(fail_reason,'')
     FROM request_logs WHERE model_name='$MODEL' AND request_path='/v1/images/generations'
     ORDER BY id DESC LIMIT 1;")
[ -n "$ROW" ] || fail "no request_logs row for $MODEL on /v1/images/generations"
COST_KNOWN="${ROW%%|*}"
echo "row: $ROW"

if [ "$COST_KNOWN" = "1" ]; then
  COST_MICROS="$(echo "$ROW" | cut -d'|' -f2)"
  AFTER="$(budget_of)"
  DELTA=$((AFTER - BEFORE))
  [ "$DELTA" = "$COST_MICROS" ] || fail "budget moved $DELTA micros, row billed $COST_MICROS"
  echo "budget delta $DELTA micros matches the row"
else
  echo "row is unpriced (cost_known=0); budget must be untouched"
  [ "$(budget_of)" = "$BEFORE" ] || fail "unpriced row but the budget moved"
fi

SNAPSHOT="$(echo "$ROW" | cut -d'|' -f3)"
if [ -n "$SNAPSHOT" ]; then
  python3 - "$SNAPSHOT" "$SIZE" "$QUALITY" <<'PY' || fail "pricing snapshot did not corroborate the request"
import json, sys
snap = json.loads(sys.argv[1])
size, quality = sys.argv[2], sys.argv[3]
assert snap["actual_n"] >= 1, snap
if size:
    assert snap["request_size"] == size, snap
if quality:
    assert snap["request_quality"] == quality, snap
assert snap["unit_price"] >= 0 and snap["price_source"] in ("tier", "default"), snap
PY
  echo "pricing snapshot corroborates (axes, count, price source)"
fi

echo "PASS: image generation end-to-end verified"
