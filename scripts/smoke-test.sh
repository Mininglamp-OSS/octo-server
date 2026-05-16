#!/bin/bash
# Octo API 冒烟测试
# 用法: ./smoke-test.sh [--verbose]
# 测试关键路径：注册、登录、Bot register、WS 握手、消息发送

set -o pipefail

BASE_URL="https://api-test.example.com"
API="$BASE_URL/api/v1"
WUKONGIM_API="http://wukongim:5001"
PASSED=0
FAILED=0
VERBOSE=false
[[ "$1" == "--verbose" ]] && VERBOSE=true

ts() { date '+%H:%M:%S'; }
pass() { PASSED=$((PASSED+1)); echo "[$(ts)] ✅ $1"; }
fail() { FAILED=$((FAILED+1)); echo "[$(ts)] ❌ $1: $2"; }
info() { $VERBOSE && echo "[$(ts)] 🔍 $1"; }

# ============ T1: 后端健康 ============
test_health() {
  STATUS=$(curl -s -o /dev/null -w "%{http_code}" --max-time 5 "$API/health" 2>/dev/null)
  if [ "$STATUS" = "200" ]; then
    pass "T1: 后端 API 健康 (200)"
  else
    fail "T1: 后端 API" "HTTP $STATUS"
  fi
}

# ============ T2: WuKongIM 健康 ============
test_wukongim() {
  CONNS=$(curl -s --max-time 5 "$WUKONGIM_API/varz" 2>/dev/null | python3 -c "import sys,json;print(json.load(sys.stdin).get('connections',0))" 2>/dev/null)
  if [ -n "$CONNS" ] && [ "$CONNS" -gt 0 ]; then
    pass "T2: WuKongIM 健康 ($CONNS 连接)"
  else
    fail "T2: WuKongIM" "连接数: $CONNS"
  fi
}

# ============ T3: 邮箱注册 ============
test_register() {
  local EMAIL="smoke_test_$(date +%s)@test.com"
  RESP=$(curl -s --max-time 10 "$API/user/emailregister" -X POST \
    -H "Content-Type: application/json" \
    -d "{\"email\":\"$EMAIL\",\"password\":\"Test123456\",\"name\":\"SmokeTest\"}" 2>/dev/null)
  
  UID_VAL=$(echo "$RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('uid',''))" 2>/dev/null)
  TOKEN_VAL=$(echo "$RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('token',''))" 2>/dev/null)
  
  if [ -n "$UID_VAL" ] && [ -n "$TOKEN_VAL" ]; then
    pass "T3: 邮箱注册成功 (uid=$UID_VAL)"
    # 保存用于后续测试
    export SMOKE_UID="$UID_VAL"
    export SMOKE_TOKEN="$TOKEN_VAL"
  else
    fail "T3: 邮箱注册" "$RESP"
  fi
}

# ============ T4: 邮箱登录 ============
test_login() {
  RESP=$(curl -s --max-time 10 "$API/user/emaillogin" -X POST \
    -H "Content-Type: application/json" \
    -d '{"email":"coda_smoke_test@test.com","password":"SmokeTest123","flag":1}' 2>/dev/null)
  
  UID_VAL=$(echo "$RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('uid',''))" 2>/dev/null)
  if [ -n "$UID_VAL" ]; then
    pass "T4: 邮箱登录成功"
  else
    fail "T4: 邮箱登录" "$RESP"
  fi
}

# ============ T5: Bot Register ============
test_bot_register() {
  # 用一个已知存在的 bot token
  BOT_TOKEN=$(docker exec octo-mysql-1 mysql -u root -ptsdd123456 --default-character-set=utf8mb4 im -N -e \
    "SELECT bot_token FROM robot WHERE status=1 AND bot_token!='' LIMIT 1;" 2>/dev/null | grep -v Warning)
  
  if [ -z "$BOT_TOKEN" ]; then
    fail "T5: Bot Register" "无可用 bot token"
    return
  fi
  
  RESP=$(curl -s --max-time 10 "$API/bot/register" -X POST \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $BOT_TOKEN" 2>/dev/null)
  
  IM_TOKEN=$(echo "$RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('im_token',''))" 2>/dev/null)
  WS_URL=$(echo "$RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('ws_url',''))" 2>/dev/null)
  
  if [ -n "$IM_TOKEN" ] && [ -n "$WS_URL" ]; then
    pass "T5: Bot Register 成功 (token=${IM_TOKEN:0:10}...)"
  else
    fail "T5: Bot Register" "$RESP"
  fi
}

# ============ T6: Nginx WS 握手 ============
test_ws_handshake() {
  WS_STATUS=$(curl -s --max-time 5 -o /dev/null -w "%{http_code}" --http1.1 \
    -H "Connection: Upgrade" -H "Upgrade: websocket" \
    -H "Sec-WebSocket-Version: 13" -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==" \
    "$BASE_URL/ws" 2>/dev/null)
  if [ "$WS_STATUS" = "101" ]; then
    pass "T6: WS 握手成功 (101)"
  else
    fail "T6: WS 握手" "HTTP $WS_STATUS"
  fi
}

# ============ T7: 前端页面 ============
test_frontend() {
  STATUS=$(curl -s --max-time 5 -o /dev/null -w "%{http_code}" "$BASE_URL/web/" 2>/dev/null)
  if [ "$STATUS" = "200" ] || [ "$STATUS" = "304" ]; then
    pass "T7: 前端页面正常 (HTTP $STATUS)"
  else
    fail "T7: 前端页面" "HTTP $STATUS"
  fi
}

# ============ T8: Space API ============
test_space_api() {
  # 用 jiawei 的 token 测试
  RESP=$(curl -s --max-time 10 "$API/user/emaillogin" -X POST \
    -H "Content-Type: application/json" \
    -d '{"email":"coda_smoke_test@test.com","password":"SmokeTest123","flag":1}' 2>/dev/null)
  
  TOKEN=$(echo "$RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('token',''))" 2>/dev/null)
  
  if [ -z "$TOKEN" ]; then
    fail "T8: Space API" "无法获取 token"
    return
  fi
  
  SPACES=$(curl -s --max-time 5 "$API/space/my" -H "token: $TOKEN" 2>/dev/null)
  COUNT=$(echo "$SPACES" | python3 -c "import sys,json;d=json.load(sys.stdin);print(len(d) if isinstance(d,list) else 0)" 2>/dev/null)
  
  if [ -n "$COUNT" ] && [ "$COUNT" -ge 0 ]; then
    pass "T8: Space API 正常 ($COUNT 个 Space)"
  else
    fail "T8: Space API" "返回: $SPACES"
  fi
}

# ============ T9: Redis ============
test_redis() {
  PING=$(docker exec tsdd-redis-1 redis-cli ping 2>/dev/null)
  if [ "$PING" = "PONG" ]; then
    pass "T9: Redis 正常"
  else
    fail "T9: Redis" "$PING"
  fi
}

# ============ T10: MySQL ============
test_mysql() {
  RESULT=$(docker exec octo-mysql-1 mysql -u root -ptsdd123456 --default-character-set=utf8mb4 im -N -e "SELECT COUNT(*) FROM user;" 2>/dev/null | grep -v Warning)
  if [ -n "$RESULT" ] && [ "$RESULT" -gt 0 ]; then
    pass "T10: MySQL 正常 ($RESULT 用户)"
  else
    fail "T10: MySQL" "查询失败"
  fi
}

# ============ 运行所有测试 ============
echo ""
echo "========================================="
echo "  Octo 冒烟测试 — $(date '+%Y-%m-%d %H:%M:%S')"
echo "  环境: $BASE_URL"
echo "========================================="
echo ""

START_TIME=$(date +%s)

test_health
test_wukongim
test_register
test_login
test_bot_register
test_ws_handshake
test_frontend
test_space_api
test_redis
test_mysql

END_TIME=$(date +%s)
DURATION=$((END_TIME - START_TIME))

echo ""
echo "========================================="
echo "  结果: $PASSED 通过 / $FAILED 失败 (${DURATION}s)"
echo "========================================="

# 清理测试用户
# Validate UID format to prevent SQL injection
if [[ ! "$SMOKE_UID" =~ ^[a-zA-Z0-9_-]+$ ]]; then
  echo "❌ Invalid SMOKE_UID format" >&2
  exit 1
fi
if [ -n "$SMOKE_UID" ]; then
  docker exec octo-mysql-1 mysql -u root -ptsdd123456 --default-character-set=utf8mb4 im \
    -e "DELETE FROM user WHERE uid='$SMOKE_UID';" 2>/dev/null
  info "已清理测试用户 $SMOKE_UID"
fi

exit $FAILED
