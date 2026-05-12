#!/usr/bin/env bash
# Shared helpers for DMWork API shell tests
set -o pipefail

API_BASE=${API_BASE:-http://localhost:8090}
TEST_USER=${TEST_USER:-test_user_a}
TEST_PASS=${TEST_PASS:-testpass123}
TEST_UID=${TEST_UID:-aee7e220529141bfa5e0ac4a3a3dd40d}
BOT_TOKEN=${BOT_TOKEN:-}
BOT_ID=${BOT_ID:-e2e_test_bot}
GROUP_ID=${GROUP_ID:-f1f2f95f8d324b6ea1ee4b626dfd16b8}
DEVICE_ID=${DEVICE_ID:-cli-e2e}
DEVICE_NAME=${DEVICE_NAME:-CodexCLI}
DEVICE_MODEL=${DEVICE_MODEL:-CLI}
CHANNEL_TYPE_PERSON=${CHANNEL_TYPE_PERSON:-1}
CHANNEL_TYPE_GROUP=${CHANNEL_TYPE_GROUP:-2}

GREEN="\033[32m"
RED="\033[31m"
YELLOW="\033[33m"
BLUE="\033[34m"
BOLD="\033[1m"
RESET="\033[0m"

[[ -n "$(command -v jq)" ]] || { echo "jq is required"; exit 1; }

reset_counters() { PASS=0; FAIL=0; FAIL_DETAILS=(); }

print_summary() {
  echo
  if (( FAIL == 0 )); then
    echo -e "${GREEN}${BOLD}PASS${RESET} $PASS tests";
  else
    echo -e "${RED}${BOLD}FAIL${RESET} $FAIL of $((PASS+FAIL)) tests";
    for item in "${FAIL_DETAILS[@]}"; do
      echo -e "  - $item"
    done
  fi
  (( FAIL == 0 ))
}

log_section() { echo -e "${BLUE}${BOLD}==> $1${RESET}"; }
pass() { ((PASS++)); echo -e "${GREEN}✔${RESET} $1"; }
fail() { ((FAIL++)); FAIL_DETAILS+=("$1"); echo -e "${RED}✘${RESET} $1"; }
warn() { echo -e "${YELLOW}!${RESET} $1"; }

# Globals to hold the last HTTP response
RESP_CODE=""
RESP_BODY=""

perform_request() {
  local method=$1 path=$2 body=${3-}
  local extra_args=()
  [[ $# -gt 3 ]] && extra_args=("${@:4}")
  local url="${API_BASE}${path}"
  local tmp
  tmp=$(mktemp)
  if [[ -n $body ]]; then
    RESP_CODE=$(curl -sS -w "%{http_code}" -o "$tmp" -H "Content-Type: application/json" "${extra_args[@]}" -X "$method" "$url" -d "$body")
  else
    RESP_CODE=$(curl -sS -w "%{http_code}" -o "$tmp" "${extra_args[@]}" -X "$method" "$url")
  fi
  RESP_BODY=$(cat "$tmp")
  rm -f "$tmp"
}

json_get() { echo "$RESP_BODY" | jq -r "$1" 2>/dev/null; }

expect_http() {
  local expected=$1 desc=$2
  if [[ "$RESP_CODE" == "$expected" ]]; then
    pass "$desc (HTTP $RESP_CODE)"
    return 0
  fi
  fail "$desc (HTTP $RESP_CODE)"; echo "$RESP_BODY" | sed 's/^/    /'
  return 1
}

login_user() {
  local user=${1:-$TEST_USER}
  local pass=${2:-$TEST_PASS}
  local device_id=${3:-$DEVICE_ID}
  local payload
  payload=$(cat <<JSON
{
  "username": "${user}",
  "password": "${pass}",
  "flag": 1,
  "device": {"device_id":"${device_id}","device_name":"${DEVICE_NAME}","device_model":"${DEVICE_MODEL}"}
}
JSON
)
  perform_request POST "/v1/user/usernamelogin" "$payload"
  if [[ "$RESP_CODE" != "200" ]]; then
    return 1
  fi
  USER_TOKEN=$(json_get '.data.token // .token')
  USER_UID=$(json_get '.data.uid // .uid')
  USER_IM_TOKEN=$(json_get '.data.im_token // .im_token')
  if [[ -z "$USER_TOKEN" || "$USER_TOKEN" == "null" ]]; then
    return 1
  fi
  return 0
}
