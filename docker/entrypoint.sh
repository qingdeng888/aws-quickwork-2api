# Render the deployment config from environment variables, then start the proxy.
#
#   CONFIG_PATH       where config.json lives (default /app/data/config.json)
#   CONFIG_FROM_ENV   false to skip env merging and keep the file untouched
#   DATA_DIR          directory that holds config.json and the SQLite database
#
# Any extra arguments are forwarded to the binary, so `docker run ... login
# -manual` works and always receives the right -config path.
set -eu

BIN="${QUICKPROXY_BIN:-/app/quickproxy}"
CONFIG="${CONFIG_PATH:-/app/data/config.json}"
DATA_DIR="${DATA_DIR:-/app/data}"
CONFIG_DIR="$(dirname "$CONFIG")"

mkdir -p "$DATA_DIR" "$CONFIG_DIR"

if [ ! -w "$CONFIG_DIR" ]; then
  echo "[entrypoint] warning: $CONFIG_DIR is not writable by uid $(id -u); /admin config saves will fail" >&2
fi

case "$(printf '%s' "${CONFIG_FROM_ENV:-true}" | tr '[:upper:]' '[:lower:]')" in
  1|true|yes|on)
    "$BIN" config -config "$CONFIG" -from-env
    ;;
  *)
    echo "[entrypoint] CONFIG_FROM_ENV=${CONFIG_FROM_ENV:-} -> keeping $CONFIG unchanged"
    ;;
esac

exec "$BIN" "$@" -config "$CONFIG"
