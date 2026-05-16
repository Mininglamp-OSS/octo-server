#!/usr/bin/env bash
# Bot API integration tests
source "$(dirname "$0")/lib.sh"
: "${BOT_TOKEN:?BOT_TOKEN must be set (e.g. export BOT_TOKEN=bf_...)}"
reset_counters
log_section "Bot Tests"

# Bot register
perform_request POST "/v1/bot/register" "{}" -H "Authorization: Bearer ${BOT_TOKEN}"
if expect_http 200 "bot register"; then
  bot_id_resp=$(json_get '.robot_id')
  if [[ -n "$bot_id_resp" && "$bot_id_resp" != "null" ]]; then
    pass "bot registered as ${bot_id_resp}"
  else
    fail "bot register response missing robot_id"
  fi
fi

# Bot send message
BOT_MSG="Bot ping $(date +%s)"
bot_send_payload=$(cat <<JSON
{
  "channel_id": "${GROUP_ID}",
  "channel_type": ${CHANNEL_TYPE_GROUP},
  "payload": {"type":1,"content":"${BOT_MSG}"}
}
JSON
)
perform_request POST "/v1/bot/sendMessage" "$bot_send_payload" -H "Authorization: Bearer ${BOT_TOKEN}"
expect_http 200 "bot sendMessage" || true

# Bot events polling
perform_request POST "/v1/bot/events" '{"event_id":0,"limit":10}' -H "Authorization: Bearer ${BOT_TOKEN}"
if expect_http 200 "bot events"; then
  status_val=$(json_get '.status')
  if [[ "$status_val" == "1" || "$status_val" == "true" ]]; then
    pass "bot events status ok"
  else
    warn "bot events status unexpected: ${status_val}"
  fi
fi

# skill.md fetch
perform_request GET "/v1/bot/skill.md" ""
if expect_http 200 "skill.md"; then
  if echo "$RESP_BODY" | grep -q "Octo Bot Skill"; then
    pass "skill.md content looks correct"
  else
    warn "skill.md content missing expected heading"
  fi
fi

print_summary || exit 1
