#!/usr/bin/env bash
# Authentication integration tests for DMWork API
source "$(dirname "$0")/lib.sh"
reset_counters
log_section "Auth Tests"

# 1) /user/login (bcrypt compatible)
login_payload=$(cat <<JSON
{
  "username": "${TEST_USER}",
  "password": "${TEST_PASS}",
  "flag": 0,
  "device": {"device_id":"cli-$$-login","device_name":"${DEVICE_NAME}","device_model":"${DEVICE_MODEL}"}
}
JSON
)
perform_request POST "/v1/user/login" "$login_payload"
if expect_http 200 "username login"; then
  LOGIN_TOKEN=$(json_get '.token')
  LOGIN_UID=$(json_get '.uid')
  if [[ -n "$LOGIN_TOKEN" && "$LOGIN_TOKEN" != "null" ]]; then
    pass "username login returned token"
  else
    fail "username login missing token"
  fi
else
  LOGIN_TOKEN=""
fi

# 2) /user/usernamelogin
usernamelogin_payload=$(cat <<JSON
{
  "username": "${TEST_USER}",
  "password": "${TEST_PASS}",
  "flag": 0,
  "device": {"device_id":"cli-$$-ulogin","device_name":"${DEVICE_NAME}","device_model":"${DEVICE_MODEL}"}
}
JSON
)
perform_request POST "/v1/user/usernamelogin" "$usernamelogin_payload"
if expect_http 200 "usernamelogin"; then
  USERLOGIN_TOKEN=$(json_get '.data.token // .token')
  USERLOGIN_UID=$(json_get '.data.uid // .uid')
  if [[ -n "$USERLOGIN_TOKEN" && "$USERLOGIN_TOKEN" != "null" ]]; then
    pass "usernamelogin returned token"
  else
    fail "usernamelogin missing token"
  fi
else
  USERLOGIN_TOKEN=""
fi

# 3) wrong password should be rejected
bad_payload=$(cat <<JSON
{
  "username": "${TEST_USER}",
  "password": "wrong-${TEST_PASS}",
  "flag": 0,
  "device": {"device_id":"cli-$$-bad","device_name":"${DEVICE_NAME}","device_model":"${DEVICE_MODEL}"}
}
JSON
)
perform_request POST "/v1/user/usernamelogin" "$bad_payload"
if [[ "$RESP_CODE" != "200" ]]; then
  pass "wrong password rejected (HTTP $RESP_CODE)"
else
  fail "wrong password unexpectedly accepted"
fi

# 4) register new user via /user/usernameregister
new_username="e2e$(date +%s | tail -c 9)"
register_payload=$(cat <<JSON
{
  "name": "E2E ${new_username}",
  "username": "${new_username}",
  "password": "${TEST_PASS}",
  "flag": 0,
  "device": {"device_id":"cli-$$-reg","device_name":"${DEVICE_NAME}","device_model":"${DEVICE_MODEL}"}
}
JSON
)
perform_request POST "/v1/user/usernameregister" "$register_payload"
if expect_http 200 "usernameregister"; then
  REG_UID=$(json_get '.data.uid // .uid')
  if [[ -n "$REG_UID" && "$REG_UID" != "null" ]]; then
    pass "new user registered: $REG_UID"
  else
    fail "registration response missing uid"
  fi
fi

# 5) token verification via /user/online
TOKEN_FOR_CHECK=${USERLOGIN_TOKEN:-$LOGIN_TOKEN}
if [[ -z "$TOKEN_FOR_CHECK" ]]; then
  fail "token verification skipped (no token from login)"
else
  perform_request GET "/v1/user/online" "" -H "token: ${TOKEN_FOR_CHECK}"
  if expect_http 200 "token validation via /user/online"; then
    if echo "$RESP_BODY" | jq -e 'type == "object"' >/dev/null 2>&1; then
      pass "online endpoint returned data"
    else
      fail "online endpoint bad response format"
    fi
  fi
fi

print_summary || exit 1
