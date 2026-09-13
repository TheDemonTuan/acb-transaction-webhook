#!/bin/sh
set -eu

user="$(tr -d '\r\n' < /run/secrets/bark_basic_auth_user)"
pass="$(tr -d '\r\n' < /run/secrets/bark_basic_auth_password)"
auth="$(printf '%s:%s' "$user" "$pass" | base64 | tr -d '\r\n')"
exec wget -q --header="Authorization: Basic $auth" -O /dev/null http://127.0.0.1:8080/
