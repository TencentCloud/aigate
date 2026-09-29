#!/usr/bin/env bash
# Tencent Cloud AIGate M1 end-to-end smoke test entry point: reset the test database -> build the gateway
# binary -> run test/e2e.
#
# Usage (works from any directory in the repository):
#   bash test/e2e/run.sh
#
# Environment variables can override the defaults:
#   MYSQL_HOST (default 127.0.0.1), MYSQL_PORT (default 3306), MYSQL_USER (default root),
#   MYSQL_ARGS (extra arguments passed to the mysql client, e.g. "-p'password'", default empty)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
cd "${REPO_ROOT}"

DB_NAME="aigate_e2e"
MYSQL_HOST="${MYSQL_HOST:-127.0.0.1}"
MYSQL_PORT="${MYSQL_PORT:-3306}"
MYSQL_USER="${MYSQL_USER:-root}"

INIT_SQL="${REPO_ROOT}/sql/init.sql"
AIGATE_BIN="${REPO_ROOT}/bin/aigate"

log() { printf '[e2e] %s\n' "$*"; }

# Assemble the mysql client arguments; MYSQL_ARGS may carry extra arguments (word-split by the shell).
mysql_cli=(mysql -h "${MYSQL_HOST}" -P "${MYSQL_PORT}" -u "${MYSQL_USER}")
if [[ -n "${MYSQL_ARGS:-}" ]]; then
  # shellcheck disable=SC2206
  mysql_cli+=(${MYSQL_ARGS})
fi

if [[ ! -f "${INIT_SQL}" ]]; then
  log "Schema file ${INIT_SQL} not found, cannot initialize the test database; make sure the repository contains sql/init.sql."
  exit 1
fi

if ! command -v mysql >/dev/null 2>&1; then
  log "mysql client not found, please install it and make sure a local MySQL is reachable."
  exit 1
fi

log "Resetting test database ${DB_NAME} ..."
"${mysql_cli[@]}" -e "DROP DATABASE IF EXISTS \`${DB_NAME}\`; CREATE DATABASE \`${DB_NAME}\` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;"

log "Importing ${INIT_SQL} ..."
"${mysql_cli[@]}" "${DB_NAME}" < "${INIT_SQL}"

log "Building gateway binary ${AIGATE_BIN} ..."
mkdir -p "${REPO_ROOT}/bin"
go build -o "${AIGATE_BIN}" ./cmd/aigate

export AIGATE_E2E_BIN="${AIGATE_BIN}"
export AIGATE_E2E_DSN="${MYSQL_USER}@tcp(${MYSQL_HOST}:${MYSQL_PORT})/${DB_NAME}?charset=utf8mb4&parseTime=True&loc=Local"
log "AIGATE_E2E_DSN=${AIGATE_E2E_DSN}"

log "Running e2e (go test -tags e2e) ..."
go test -tags e2e -count=1 -v ./test/e2e/
