#!/usr/bin/env bash
# End-to-end AIGate demo against a local mock OpenAI-compatible upstream.
#
# Requirements: Docker, curl, python3.
# Usage:
#   ./demo/quickstart.sh
#   AIGATE_IMAGE=aigate:0.1.0 ./demo/quickstart.sh   # use a locally built image
set -euo pipefail

IMAGE="${AIGATE_IMAGE:-ghcr.io/tencentcloud/aigate:0.1.0}"
CONTAINER="aigate-demo"
ADMIN_TOKEN="demo-admin-token"
BASE="http://127.0.0.1:18080"
UPSTREAM_PORT="18081"
TMP_DIR="$(mktemp -d)"

cleanup() {
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  if [[ -n "${MOCK_PID:-}" ]]; then
    kill "$MOCK_PID" >/dev/null 2>&1 || true
    wait "$MOCK_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

json_field() {
  python3 - "$1" "$2" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    print(json.load(f)[sys.argv[2]])
PY
}

echo "==> starting mock upstream on :$UPSTREAM_PORT"
python3 "$(dirname "$0")/mock_upstream.py" >"$TMP_DIR/mock.log" 2>&1 &
MOCK_PID=$!
sleep 1

echo "==> starting AIGate ($IMAGE) on :18080"
docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
docker run -d --name "$CONTAINER" \
  -p "18080:8080" \
  -v aigate-demo-data:/data \
  --add-host=host.docker.internal:host-gateway \
  "$IMAGE" \
  -db-type sqlite -db-dsn /data/aigate.db \
  -init-admin-token "$ADMIN_TOKEN" \
  -allow-private-upstreams >/dev/null

for _ in $(seq 1 30); do
  if curl -fsS "$BASE/healthz" >/dev/null 2>&1; then break; fi
  sleep 1
done
curl -fsS "$BASE/healthz" >/dev/null

echo "==> creating model, user and key"
curl -fsS -X POST "$BASE/admin/models" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{
    \"provider\": \"demo\",
    \"model_id\": \"demo-model\",
    \"model_name\": \"Demo Model\",
    \"url\": \"http://host.docker.internal:$UPSTREAM_PORT/v1\",
    \"api_key\": \"demo-upstream-key\",
    \"model_type\": \"openai-completions\",
    \"enabled\": true
  }" >"$TMP_DIR/model.json"
MODEL_ID="$(json_field "$TMP_DIR/model.json" id)"

curl -fsS -X POST "$BASE/admin/users" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"username":"demo"}' >"$TMP_DIR/user.json"
USER_ID="$(json_field "$TMP_DIR/user.json" id)"

curl -fsS -X POST "$BASE/admin/keys" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{
    \"name\": \"demo key\",
    \"user_id\": $USER_ID,
    \"bindings\": [{\"ai_model_id\": $MODEL_ID, \"role\": \"primary\"}]
  }" >"$TMP_DIR/key.json"
API_KEY="$(json_field "$TMP_DIR/key.json" token)"

echo "==> calling /v1/chat/completions with model=demo/demo-model"
curl -fsS -X POST "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "demo/demo-model",
    "messages": [{"role": "user", "content": "Say hello"}],
    "max_tokens": 64
  }' | python3 -m json.tool

echo
echo "==> demo complete. Admin API: $BASE/admin (Bearer $ADMIN_TOKEN)"
