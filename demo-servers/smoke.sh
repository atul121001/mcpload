#!/usr/bin/env bash
# Smoke-test the demo targets with curl and assert the expected behaviour.
# Usage: ./smoke.sh [target...]
# targets: ts-healthy ts-leaky py-healthy lb-stateful stateless-2026 ts-oauth (default: all)
# Exits non-zero if any check fails. HOST overrides the host (default localhost).
set -euo pipefail

HOST=${HOST:-localhost}
ACCEPT='Accept: application/json, text/event-stream'
CT='Content-Type: application/json'
PV=2025-11-25
TOOLS='fast slow flaky big search'
LB_CALLS=${LB_CALLS:-10}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
FAILS=0
CHECKS=0

ok()   { CHECKS=$((CHECKS + 1)); echo "   ok   $*"; }
fail() { CHECKS=$((CHECKS + 1)); FAILS=$((FAILS + 1)); echo "   FAIL $*"; }

# req METHOD URL [BODY] [curl args...] -> sets CODE, BODY (JSON-RPC payload, SSE "data:" unwrapped), HDRS file
req() {
  local method=$1 url=$2 data=${3:-}
  shift 3 || shift $#
  local args=(-sS -X "$method" -o "$TMP/body" -D "$TMP/hdrs" -w '%{http_code}' "$@")
  [ -n "$data" ] && args+=(-H "$CT" -H "$ACCEPT" --data-binary "$data")
  : >"$TMP/body"; : >"$TMP/hdrs"
  CODE=$(curl "${args[@]}" "$url" 2>"$TMP/err") || CODE=000
  BODY=$(sed -n -e 's/^data: //p' -e t -e '/^[{[]/p' "$TMP/body" | tr -d '\r')
  HDRS="$TMP/hdrs"
}

expect_code() { # $1=expected $2=label
  if [ "$CODE" = "$1" ]; then ok "$2 -> HTTP $CODE"; else fail "$2 -> HTTP $CODE (want $1) $(cat "$TMP/err" "$TMP/body" 2>/dev/null | head -c 200 | tr '\n' ' ')"; fi
}

expect_result() { # $1=label: body must be a JSON-RPC result without isError:true
  if printf '%s' "$BODY" | grep -q '"result"' && ! printf '%s' "$BODY" | grep -q '"isError":true'; then
    ok "$1 has a result"
  else
    fail "$1 has no successful result: $(printf '%s' "$BODY" | head -c 200)"
  fi
}

expect_tools() { # $1=label: tools/list must list all 5 tools
  local missing="" t
  for t in $TOOLS; do printf '%s' "$BODY" | grep -q "\"name\":\"$t\"" || missing="$missing $t"; done
  if [ -z "$missing" ]; then ok "$1 lists: $TOOLS"; else fail "$1 missing tools:$missing"; fi
}

header() { grep -i "^$1:" "$HDRS" | head -1 | cut -d: -f2- | tr -d ' \r' || true; }

INIT='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"'$PV'","capabilities":{},"clientInfo":{"name":"smoke","version":"1.0"}}}'
LIST='{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
CALL='{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search","arguments":{"query":"mcp","limit":2}}}'

initialize() { # $1=url, rest=extra curl args; sets SID
  local url=$1; shift
  SID=""
  req POST "$url" "$INIT" "$@"
  expect_code 200 "initialize"
  SID=$(header mcp-session-id)
  if [ -n "$SID" ]; then ok "Mcp-Session-Id issued"; else fail "no Mcp-Session-Id header"; fi
}

stateful() { # $1=url, rest=extra curl args (e.g. -H "Authorization: Bearer ...")
  local url=$1; shift
  initialize "$url" "$@"
  [ -n "$SID" ] || return 0
  local s=(-H "Mcp-Session-Id: $SID" -H "MCP-Protocol-Version: $PV" "$@")
  req POST "$url" '{"jsonrpc":"2.0","method":"notifications/initialized"}' "${s[@]}"
  expect_code 202 "notifications/initialized"
  req POST "$url" "$LIST" "${s[@]}"
  expect_code 200 "tools/list"; expect_tools "tools/list"
  req POST "$url" "$CALL" "${s[@]}"
  expect_code 200 "tools/call search"; expect_result "tools/call search"
  req DELETE "$url" "" "${s[@]}"
  expect_code 200 "DELETE session"
  # (ts-leaky also closes the transport on DELETE; it only keeps the session's memory.)
  req POST "$url" "$CALL" "${s[@]}"
  expect_code 404 "tools/call after DELETE"
}

lb_stateful() { # no sticky sessions: some in-session calls must land on the other replica (404)
  local url=$1 n200=0 n404=0 other=0 i
  initialize "$url"
  [ -n "$SID" ] || return 0
  for i in $(seq 1 "$LB_CALLS"); do
    req POST "$url" "$CALL" -H "Mcp-Session-Id: $SID" -H "MCP-Protocol-Version: $PV"
    case $CODE in 200) n200=$((n200 + 1)) ;; 404) n404=$((n404 + 1)) ;; *) other=$((other + 1)) ;; esac
  done
  echo "   $LB_CALLS x tools/call: $n200 x 200, $n404 x 404, $other other"
  if [ "$n404" -ge 1 ]; then ok "session_not_found reproduced ($n404 x 404)"; else fail "expected at least one 404 in $LB_CALLS calls"; fi
  if [ "$other" -eq 0 ]; then ok "only 200/404 responses"; else fail "$other responses were neither 200 nor 404"; fi
}

stateless_legacy() { # plain calls, no initialize, no session
  local h=(-H "MCP-Protocol-Version: $PV")
  req POST "$1" "$LIST" "${h[@]}"
  expect_code 200 "tools/list (no initialize)"; expect_tools "tools/list (no initialize)"
  req POST "$1" "$CALL" "${h[@]}"
  expect_code 200 "tools/call search (no initialize)"; expect_result "tools/call search (no initialize)"
}

modern() { # 2026-07-28: per-request _meta + Mcp-Method / Mcp-Name headers
  local meta='"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"smoke","version":"1.0"}}'
  local h=(-H "MCP-Protocol-Version: 2026-07-28")
  req POST "$1" '{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{'"$meta"'}}' "${h[@]}" -H "Mcp-Method: server/discover"
  expect_code 200 "server/discover (2026-07-28)"; expect_result "server/discover (2026-07-28)"
  req POST "$1" '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"query":"mcp","limit":2},'"$meta"'}}' \
    "${h[@]}" -H "Mcp-Method: tools/call" -H "Mcp-Name: search"
  expect_code 200 "tools/call search (2026-07-28)"; expect_result "tools/call search (2026-07-28)"
}

oauth() {
  local url=http://$HOST:3007/mcp tok
  req POST "$url" "$INIT"
  expect_code 401 "initialize without token"
  req POST "http://$HOST:3006/token" "" -u mcpload:secret -d grant_type=client_credentials
  expect_code 200 "POST /token (client_credentials)"
  tok=$(sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p' "$TMP/body")
  if [ -n "$tok" ]; then ok "access_token issued"; else fail "no access_token in /token response"; return 0; fi
  stateful "$url" -H "Authorization: Bearer $tok"
}

targets=${*:-ts-healthy ts-leaky py-healthy lb-stateful stateless-2026 ts-oauth}
for t in $targets; do
  echo; echo "######## $t"
  case $t in
    ts-healthy) stateful "http://$HOST:3001/mcp" ;;
    ts-leaky) stateful "http://$HOST:3002/mcp" ;;
    py-healthy) stateless_legacy "http://$HOST:3003/mcp"; modern "http://$HOST:3003/mcp" ;;
    lb-stateful) lb_stateful "http://$HOST:3004/mcp" ;;
    stateless-2026) stateless_legacy "http://$HOST:3005/mcp"; modern "http://$HOST:3005/mcp" ;;
    ts-oauth) oauth ;;
    *) fail "unknown target $t" ;;
  esac
done

echo
if [ "$FAILS" -gt 0 ]; then
  echo "smoke: $FAILS of $CHECKS checks FAILED"
  exit 1
fi
echo "smoke: all $CHECKS checks passed"
