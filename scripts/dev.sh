#!/usr/bin/env bash
# Start an isolated tgfile server backed by local files and SQLite.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEV_DATA_DIR="${TGFILE_DEV_DATA_DIR:-$ROOT/.dev-data}"
if [[ "$DEV_DATA_DIR" != /* ]]; then
  DEV_DATA_DIR="$ROOT/$DEV_DATA_DIR"
fi

DEFAULT_CONFIG_PATH="$DEV_DATA_DIR/config.json"
CONFIG_PATH="${TGFILE_DEV_CONFIG:-${1:-$DEFAULT_CONFIG_PATH}}"
PORT="${TGFILE_DEV_PORT:-9901}"
HOST="${TGFILE_DEV_HOST:-127.0.0.1}"
BUCKET="${TGFILE_DEV_BUCKET:-hackmd}"
USERNAME="${TGFILE_DEV_USERNAME:-dev}"
PASSWORD="${TGFILE_DEV_PASSWORD:-dev-secret}"
GO="${TGFILE_DEV_GO:-go}"

if [[ "$CONFIG_PATH" != /* ]]; then
  CONFIG_PATH="$ROOT/$CONFIG_PATH"
fi

PID_DIR="${TGFILE_DEV_PID_DIR:-${TMPDIR:-/tmp}}"
mkdir -p "$PID_DIR"
ROOT_HASH="$(printf '%s' "$ROOT" | cksum | awk '{print $1}')"
PID_FILE="${TGFILE_DEV_PID_FILE:-$PID_DIR/tgfile-dev-$ROOT_HASH.pid}"
server_pid=""
server_started=""

require_command() {
  local command_name="$1"
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "[tgfile] required command not found: $command_name" >&2
    exit 1
  fi
}

proc_start_time() {
  local pid="$1"
  awk '{print $22}' "/proc/$pid/stat" 2>/dev/null || true
}

kill_tree() {
  local pid="$1"
  local child
  while read -r child; do
    [[ -n "$child" ]] && kill_tree "$child"
  done < <(pgrep -P "$pid" 2>/dev/null || true)
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null || true
  fi
}

kill_recorded_pid() {
  local pid="$1"
  local expected_start="$2"
  local current_start

  [[ -n "$pid" ]] || return 0
  kill -0 "$pid" 2>/dev/null || return 0

  current_start="$(proc_start_time "$pid")"
  if [[ -n "$expected_start" && -n "$current_start" && "$current_start" != "$expected_start" ]]; then
    echo "[tgfile] skip stale pid=$pid; process id was reused"
    return 0
  fi

  echo "[tgfile] stopping previous development server pid=$pid"
  kill_tree "$pid"
}

cleanup_previous() {
  local pid
  local started

  [[ -f "$PID_FILE" ]] || return 0
  read -r pid started <"$PID_FILE" || true
  kill_recorded_pid "${pid:-}" "${started:-}"
  rm -f "$PID_FILE"
}

cleanup() {
  trap - INT TERM EXIT
  if [[ -n "$server_pid" ]]; then
    kill_recorded_pid "$server_pid" "$server_started"
  fi
  wait 2>/dev/null || true
  rm -f "$PID_FILE"
}

wait_for_server() {
  local attempt

  echo "[tgfile] waiting for server readiness on $HOST:$PORT"
  for ((attempt = 1; attempt <= 120; attempt++)); do
    if ! kill -0 "$server_pid" 2>/dev/null; then
      echo "[tgfile] server exited before becoming ready" >&2
      wait "$server_pid" 2>/dev/null || true
      exit 1
    fi
    if (exec 3<>"/dev/tcp/$HOST/$PORT") 2>/dev/null; then
      echo "[tgfile] server is ready on http://$HOST:$PORT"
      return 0
    fi
    sleep 1
  done

  echo "[tgfile] server did not become ready on $HOST:$PORT" >&2
  exit 1
}

validate_port() {
  if [[ ! "$PORT" =~ ^[0-9]+$ ]] || ((PORT < 1 || PORT > 65535)); then
    echo "[tgfile] invalid development port: $PORT" >&2
    exit 1
  fi
}

generate_config() {
  mkdir -p "$DEV_DATA_DIR/blocks" "$DEV_DATA_DIR/cache"
  cat >"$CONFIG_PATH" <<JSON
{
  "bind": "$HOST:$PORT",
  "log_info": {
    "console": true,
    "level": "debug"
  },
  "db_file": "$DEV_DATA_DIR/tgfile.db",
  "bot_kind": "localfile",
  "bot_config": {
    "dir": "$DEV_DATA_DIR/blocks",
    "block_size": 20971520
  },
  "user_info": {
    "$USERNAME": "$PASSWORD"
  },
  "s3": {
    "enable": true,
    "bucket": [
      "$BUCKET"
    ]
  },
  "rotate_stream": 0,
  "webdav": {
    "enable": true,
    "root": "/"
  },
  "io_cache": {
    "enable_l1_cache": true,
    "l1_cache_size": 16777216,
    "l1_key_size_limit": 4096,
    "enable_l2_cache": true,
    "l2_cache_size": 536870912,
    "l2_key_size_limit": 524288,
    "l2_cache_dir": "$DEV_DATA_DIR/cache"
  }
}
JSON
}

validate_port
require_command "$GO"
require_command awk
require_command cksum
require_command pgrep

mkdir -p "$DEV_DATA_DIR"
if [[ "$CONFIG_PATH" == "$DEFAULT_CONFIG_PATH" ]]; then
  generate_config
elif [[ ! -f "$CONFIG_PATH" ]]; then
  echo "[tgfile] development config not found: $CONFIG_PATH" >&2
  exit 1
fi

cleanup_previous
if (exec 3<>"/dev/tcp/$HOST/$PORT") 2>/dev/null; then
  echo "[tgfile] port already in use: $HOST:$PORT" >&2
  exit 1
fi

: >"$PID_FILE"
trap cleanup INT TERM EXIT

echo "[tgfile] starting server with config=$CONFIG_PATH"
(
  cd "$ROOT"
  exec "$GO" run ./cmd -config="$CONFIG_PATH"
) &
server_pid="$!"
server_started="$(proc_start_time "$server_pid")"
printf '%s %s\n' "$server_pid" "$server_started" >"$PID_FILE"

wait_for_server
echo "[tgfile] S3 endpoint: http://$HOST:$PORT/$BUCKET"
echo "[tgfile] WebDAV endpoint: http://$HOST:$PORT/webdav"
if [[ "$CONFIG_PATH" == "$DEFAULT_CONFIG_PATH" ]]; then
  echo "[tgfile] development credentials: $USERNAME / $PASSWORD"
fi
echo "[tgfile] press Ctrl+C to stop"

wait "$server_pid"
