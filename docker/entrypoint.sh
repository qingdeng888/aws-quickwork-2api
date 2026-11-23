#!/bin/sh
# Render the deployment config from environment variables, then start the proxy.
#
#   CONFIG_PATH       where config.json lives (default /app/data/config.json)
#   CONFIG_FROM_ENV   false to skip env merging and keep the file untouched
#   DATA_DIR          directory that holds config.json and the SQLite database
#   APP_UID / APP_GID uid/gid the server runs as (default 10001)
#
# The container starts as root only to take ownership of the data directory: a
# bind mount created by the Docker daemon is owned by the host root, so the
# unprivileged uid could not create config.json or the SQLite database. After
# that the script drops to APP_UID/APP_GID before running anything. When the
# container is started unprivileged (compose `user:`), the chown is impossible
# and the script only verifies the directory is writable.
set -eu

BIN="${QUICKPROXY_BIN:-/app/quickproxy}"
CONFIG="${CONFIG_PATH:-/app/data/config.json}"
DATA_DIR="${DATA_DIR:-/app/data}"
APP_UID="${APP_UID:-10001}"
APP_GID="${APP_GID:-10001}"
CONFIG_DIR="$(dirname "$CONFIG")"

mkdir -p "$DATA_DIR" "$CONFIG_DIR"

if [ "$(id -u)" = "0" ]; then
  needs_chown=0
  for path in "$DATA_DIR" "$CONFIG_DIR" "$CONFIG"; do
    [ -e "$path" ] || continue
    if [ "$(stat -c %u "$path")" != "$APP_UID" ] || [ "$(stat -c %g "$path")" != "$APP_GID" ]; then
      needs_chown=1
      break
    fi
  done
  if [ "$needs_chown" = "1" ]; then
    echo "[entrypoint] taking ownership of $DATA_DIR (uid=$APP_UID gid=$APP_GID)"
    chown -R "$APP_UID:$APP_GID" "$DATA_DIR" "$CONFIG_DIR"
  fi

  render_config() {
    su-exec "$APP_UID:$APP_GID" "$BIN" config -config "$CONFIG" -from-env
  }
  exec_app() {
    exec su-exec "$APP_UID:$APP_GID" "$BIN" "$@"
  }
else
  if ! touch "$CONFIG_DIR/.write-test" 2>/dev/null; then
    echo "[entrypoint] ERROR: $CONFIG_DIR is not writable by uid $(id -u)." >&2
    echo "[entrypoint] Remove 'user:' from compose so the image can fix ownership at startup," >&2
    echo "[entrypoint] or pre-own the host directory: sudo chown -R $APP_UID:$APP_GID <host data dir>" >&2
    exit 1
  fi
  rm -f "$CONFIG_DIR/.write-test"

  render_config() {
    "$BIN" config -config "$CONFIG" -from-env
  }
  exec_app() {
    exec "$BIN" "$@"
  }
fi

case "$(printf '%s' "${CONFIG_FROM_ENV:-true}" | tr '[:upper:]' '[:lower:]')" in
  1|true|yes|on)
    render_config
    ;;
  *)
    echo "[entrypoint] CONFIG_FROM_ENV=${CONFIG_FROM_ENV:-} -> keeping $CONFIG unchanged"
    ;;
esac

echo "[entrypoint] starting '$*' as uid $(id -u)"
exec_app "$@" -config "$CONFIG"
