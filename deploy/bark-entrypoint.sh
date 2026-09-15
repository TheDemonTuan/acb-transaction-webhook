#!/usr/bin/env sh
set -eu

load_secret() {
    file="$1"
    variable="$2"
    [ -r "$file" ] || { printf 'Required Bark secret is missing or unreadable: %s\n' "$file" >&2; exit 1; }
    if ! value="$(tr -d '\r\n' < "$file")"; then
        printf 'Required Bark secret cannot be opened: %s\n' "$file" >&2
        exit 1
    fi
    [ -n "$value" ] || { printf 'Required Bark secret is empty: %s\n' "$file" >&2; exit 1; }
    export "$variable=$value"
}

load_secret /run/secrets/bark_basic_auth_user BARK_SERVER_BASIC_AUTH_USER
load_secret /run/secrets/bark_basic_auth_password BARK_SERVER_BASIC_AUTH_PASSWORD

if [ "${1:-}" = "bark-server" ]; then
    shift
fi

cmd="bark-server"
if [ -x /usr/local/bin/bark-server ]; then
    cmd="/usr/local/bin/bark-server"
fi

exec "$cmd" "$@"
