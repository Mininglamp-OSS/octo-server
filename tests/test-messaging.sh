#!/usr/bin/env bash
source "$(dirname "$0")/lib.sh"
: "${BOT_TOKEN:?BOT_TOKEN must be set (e.g. export BOT_TOKEN=bf_...)}"
reset_counters
log_section "Messaging Tests"

login_user
if [[ $? -ne 0 ]]; then
  fail "login failed for messaging tests"
  print_summary
  exit 0
fi
pass "login succeeded (uid=$USER_UID)"

# Send via Bot API
bot_payload="{\"channel_id\":\"${GROUP_ID}\",\"channel_type\":2,\"payload\":{\"type\":1,\"content\":\"test-msg\"}}"
perform_request POST "/v1/bot/sendMessage" "$bot_payload" -H "Authorization: Bearer ${BOT_TOKEN}"
expect_http 200 "bot send message" && pass "bot send message"

# Channel sync
sync_payload="{\"channel_id\":\"${GROUP_ID}\",\"channel_type\":2,\"start_message_seq\":0,\"end_message_seq\":0,\"pull_mode\":1,\"limit\":10,\"login_uid\":\"${USER_UID}\",\"device_uuid\":\"cli\"}"
perform_request POST "/v1/message/channel/sync" "$sync_payload" -H "token: ${USER_TOKEN}"
expect_http 200 "channel history" && pass "channel history"

# Ping
perform_request GET "/v1/ping"
expect_http 200 "server ping" && pass "server ping"

print_summary
