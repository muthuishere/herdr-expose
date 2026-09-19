#!/usr/bin/env bash
#
# herdr-expose — the real end-to-end acceptance test.
#
# It builds the binary, stands up its OWN throwaway Herdr session with its own
# agent and its own shell pane, exposes THAT session (and only that session) on
# the LAN, drives a real browser through pairing / reading / typing, and then
# takes everything back down.
#
# It never touches a pane, a session, a share or a daemon it did not create.
# The session name is forced to start with `hexe2e-`, teardown runs from a
# trap, and every herdr call is pinned to this session's socket by environment,
# so a bug in this script cannot address anything of yours.
#
#   ./e2e/acceptance.sh                run it
#   HEX_HEADED=1 ./e2e/acceptance.sh   watch the browser
#   HEX_KEEP=1   ./e2e/acceptance.sh   leave the session + share up afterwards
#   HEX_IDLE_SECS=90 ./e2e/acceptance.sh
#
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

SESSION="${HEX_SESSION:-hexe2e-$(date +%s | tail -c 5)}"
case "$SESSION" in
  hexe2e-*) ;;
  *) echo "refusing to run: HEX_SESSION must start with 'hexe2e-' so teardown can never hit a real session" >&2; exit 2 ;;
esac

WORK="$(mktemp -d "${TMPDIR:-/tmp}/hexe2e.XXXXXX")"
SESSION_DIR="$HOME/.config/herdr/sessions/$SESSION"
BIN="${HEX_BIN:-$WORK/herdr-expose}"
SHARE_ID=""
SERVER_PID=""

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# Every herdr call in this script is scoped to OUR session by these two vars.
hx() { env -u HERDR_PANE_ID -u HERDR_TAB_ID -u HERDR_WORKSPACE_ID \
        HERDR_SOCKET_PATH="$SESSION_DIR/herdr.sock" HERDR_SESSION="$SESSION" herdr "$@"; }

jget() { python3 -c 'import json,sys;d=json.load(sys.stdin);exec("v=d"+sys.argv[1]);print(v)' "$1"; }

# ------------------------------------------------------------------ teardown
teardown() {
  local code=$?
  if [ "${HEX_KEEP:-0}" = "1" ]; then
    log "HEX_KEEP=1 — leaving session $SESSION and share $SHARE_ID up"
    return
  fi
  log "teardown"
  [ -n "$SHARE_ID" ] && "$BIN" share revoke "$SHARE_ID" >/dev/null 2>&1 || true
  hx server stop >/dev/null 2>&1 || true
  sleep 1
  herdr session delete "$SESSION" >/dev/null 2>&1 || true
  rm -rf "$SESSION_DIR" 2>/dev/null || true
  # Orphan sweep, reported not killed: anything still holding OUR session name.
  local orphans
  orphans="$(pgrep -fl "$SESSION" 2>/dev/null | grep -v acceptance.sh || true)"
  if [ -n "$orphans" ]; then
    printf '\033[1;31mORPHANS LEFT BEHIND:\033[0m\n%s\n' "$orphans" >&2
  else
    log "no orphan processes for $SESSION"
  fi
  return $code
}
trap teardown EXIT

# --------------------------------------------------------------------- build
if [ -z "${HEX_BIN:-}" ]; then
  log "building the web app"
  ( cd web && npm run build >/dev/null ) || die "web build failed"
  log "building herdr-expose -> $BIN"
  CGO_ENABLED=0 go build -trimpath -o "$BIN" ./cmd/herdr-expose || die "go build failed"
fi
[ -x "$BIN" ] || die "no binary at $BIN"

command -v herdr >/dev/null || die "herdr is not on PATH"
[ -d e2e/node_modules ] || ( cd e2e && npm install >/dev/null ) || die "npm install failed"

# ------------------------------------------------------- the throwaway session
log "starting a throwaway Herdr session: $SESSION"
mkdir -p "$SESSION_DIR"
env -u HERDR_PANE_ID -u HERDR_TAB_ID -u HERDR_WORKSPACE_ID \
  HERDR_SOCKET_PATH="$SESSION_DIR/herdr.sock" HERDR_SESSION="$SESSION" \
  nohup herdr server >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 40); do [ -S "$SESSION_DIR/herdr.sock" ] && break; sleep 0.25; done
[ -S "$SESSION_DIR/herdr.sock" ] || die "session socket never appeared (see $WORK/server.log)"

log "creating a workspace and an agent pane"
hx workspace create --cwd /tmp --label "hexe2e" --no-focus >/dev/null
AGENT_PANE=w1:p1
hx agent start hexe2eagent --kind "${HEX_AGENT_KIND:-claude}" --pane "$AGENT_PANE" --timeout 90000 >/dev/null \
  || die "could not start an agent in $AGENT_PANE"

# The token the agent will be asked for. The PROMPT is sent from the browser
# half, with the pane already open, so the reply has to travel the live path.
TOKEN="HEXE2E-OK-$(openssl rand -hex 3)"
log "the agent will be asked to reply with: $TOKEN"

log "adding two plain shell panes (silent ones: the idle-stream and blocked tests)"
hx pane split --pane "$AGENT_PANE" --direction right --cwd /tmp --no-focus >/dev/null
SHELL_PANE=w1:p2
hx pane split --pane "$SHELL_PANE" --direction down --cwd /tmp --no-focus >/dev/null
BLOCKED_PANE=w1:p3
# Name the two shells. The browser half asserts (non-fatally) whether these
# names survive the trip: herdr-expose builds its titles from the TERMINAL
# TITLE and carries no `label` field, so today they do not.
hx pane rename "$SHELL_PANE"   hexe2e-idle    >/dev/null
hx pane rename "$BLOCKED_PANE" hexe2e-blocked >/dev/null

# --------------------------------------------------------------------- share
log "sharing $SESSION on the LAN for 1 hour"
"$BIN" share --lan --session "$SESSION" --hours 1 --json >"$WORK/share.json" 2>"$WORK/share.err" \
  || { cat "$WORK/share.err" >&2; die "share failed"; }
# The CLI prints progress before the JSON; keep only the object.
python3 -c 'import sys,json; raw=open(sys.argv[1]).read(); json.dump(json.loads(raw[raw.index("{"):]), sys.stdout)' \
  "$WORK/share.json" >"$WORK/share.clean.json"
URL="$(jget "['share']['url']"   <"$WORK/share.clean.json")"
CODE="$(jget "['pairing_code']"  <"$WORK/share.clean.json")"
SHARE_ID="$(jget "['share']['id']" <"$WORK/share.clean.json")"
log "share $SHARE_ID at $URL (code $CODE)"

# ------------------------------------------------------------------- browser
log "driving the browser"
set +e
env -u HERDR_PANE_ID -u HERDR_TAB_ID -u HERDR_WORKSPACE_ID \
    HEX_URL="$URL" HEX_PAIR="$CODE" HEX_TOKEN="$TOKEN" HEX_SESSION="$SESSION" \
    HEX_AGENT_PANE="$AGENT_PANE" HEX_SHELL_PANE="$SHELL_PANE" \
    HEX_BLOCKED_PANE="$BLOCKED_PANE" \
    HERDR_SOCKET_PATH="$SESSION_DIR/herdr.sock" \
    HEX_ARTIFACTS="${HEX_ARTIFACTS:-$REPO/e2e/artifacts}" \
    node e2e/acceptance.mjs
RC=$?
set -e

# ----------------------------------------------------------------- the owner
# The permanent daemon must be exactly as healthy as we found it.
if curl -fsS --max-time 3 http://127.0.0.1:21118/healthz >/dev/null 2>&1; then
  log "the daemon on 21118 is still answering"
else
  printf '\033[1;33mnote:\033[0m nothing healthy on 127.0.0.1:21118 (it may simply not be running)\n'
fi

exit $RC
