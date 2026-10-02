#!/bin/sh
set -eu

export DISPLAY=:99
mkdir -p /tmp/.X11-unix /tmp/acb-browser

xvfb_pid=""
openbox_pid=""
controller_pid=""
pids=""

proc_name() {
  case "$1" in
    "$xvfb_pid") echo "Xvfb" ;;
    "$openbox_pid") echo "openbox-session" ;;
    "$controller_pid") echo "auth-browser" ;;
    *) echo "process-$1" ;;
  esac
}

shutdown() {
  trap - INT TERM EXIT
  echo "[entrypoint] Termination signal caught; shutting down background processes..." >&2
  if [ -n "$pids" ]; then
    for pid in $pids; do
      if kill -0 "$pid" 2>/dev/null; then
        echo "[entrypoint] Stopping $(proc_name "$pid") (PID $pid)..." >&2
      fi
    done
    kill $pids 2>/dev/null || true
    wait $pids 2>/dev/null || true
  fi
  echo "[entrypoint] Shutdown complete." >&2
}
trap shutdown INT TERM EXIT

echo "[entrypoint] Starting Xvfb on :99..."
Xvfb :99 -screen 0 1280x900x24 -nolisten tcp &
xvfb_pid=$!
pids="$xvfb_pid"
echo "[entrypoint] Started Xvfb (PID $xvfb_pid)"

attempt=0
while [ ! -S /tmp/.X11-unix/X99 ]; do
  if ! kill -0 "$xvfb_pid" 2>/dev/null; then
    echo "[entrypoint] Xvfb (PID $xvfb_pid) exited before the display socket was ready" >&2
    exit 1
  fi
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 100 ]; then
    echo "[entrypoint] Timed out waiting for Xvfb display socket /tmp/.X11-unix/X99" >&2
    exit 1
  fi
  sleep 0.1
done
echo "[entrypoint] Xvfb display socket /tmp/.X11-unix/X99 is ready"

echo "[entrypoint] Starting openbox-session..."
openbox-session &
openbox_pid=$!
pids="$pids $openbox_pid"
echo "[entrypoint] Started openbox-session (PID $openbox_pid)"

echo "[entrypoint] Starting auth-browser controller..."
/auth-browser &
controller_pid=$!
pids="$pids $controller_pid"
echo "[entrypoint] Started auth-browser (PID $controller_pid)"

echo "[entrypoint] All required processes running (Xvfb:$xvfb_pid, openbox-session:$openbox_pid, auth-browser:$controller_pid). Supervised loop active."

while :; do
  for pid in $pids; do
    if ! kill -0 "$pid" 2>/dev/null; then
      status=0
      if ! wait "$pid" 2>/dev/null; then
        status=$?
      fi
      name="$(proc_name "$pid")"
      echo "[entrypoint] Required process '$name' (PID $pid) exited unexpectedly (status ${status})" >&2
      exit 1
    fi
  done
  sleep 1
done
