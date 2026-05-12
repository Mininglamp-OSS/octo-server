/**
 * bot-stress.js — HTTP API 高并发消息压测
 *
 * 场景1: 并发 Bot 消息发送   (POST /v1/bot/sendMessage)
 * 场景2: 并发 Bot 事件轮询   (POST /v1/bot/events)
 * 场景3: 并发用户登录        (POST /v1/user/usernamelogin)
 * 场景4: 并发消息历史查询    (POST /v1/message/channel/sync)
 *
 * 运行示例:
 *   k6 run tests/load/bot-stress.js --env BOT_TOKEN=bf_...
 *   k6 run tests/load/bot-stress.js --env BOT_TOKEN=bf_... --env API_URL=http://localhost:8090
 *   k6 run tests/load/bot-stress.js --env BOT_TOKEN=bf_... --env VUS_SEND=20 --env VUS_EVENTS=15
 *
 * 注意: k6 通过 snap 安装时无法访问 /tmp，脚本需放在非 /tmp 目录下运行
 */

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';

// ── 配置 ─────────────────────────────────────────────────────────────────────

const BASE_URL   = __ENV.API_URL   || 'http://localhost:8090';
const BOT_TOKEN  = __ENV.BOT_TOKEN;
if (!BOT_TOKEN) { throw new Error('BOT_TOKEN env var is required'); }
const GROUP_ID   = __ENV.GROUP_ID  || 'f1f2f95f8d324b6ea1ee4b626dfd16b8';

// Fix 4: 用 Number() || default 替代 parseInt，避免 NaN 静默传入
const VUS_SEND   = Number(__ENV.VUS_SEND)   || 10;
const VUS_EVENTS = Number(__ENV.VUS_EVENTS) || 10;
const VUS_LOGIN  = Number(__ENV.VUS_LOGIN)  || 20;
const VUS_SYNC   = Number(__ENV.VUS_SYNC)   || 10;

// Fix 2: 模块级变量 = per-VU（每个 VU 独立维护 bot events 游标）
let lastBotEventId = 0;

// ── 自定义指标 ────────────────────────────────────────────────────────────────

const botSendErrors   = new Counter('bot_send_errors');
const botEventsErrors = new Counter('bot_events_errors');
const loginErrors     = new Counter('login_errors');
const syncErrors      = new Counter('sync_errors');

const botSendDuration   = new Trend('bot_send_duration',   true);
const botEventsDuration = new Trend('bot_events_duration', true);
const loginDuration     = new Trend('login_duration',      true);
const syncDuration      = new Trend('sync_duration',       true);

const errorRate = new Rate('error_rate');

// ── 测试选项 ─────────────────────────────────────────────────────────────────

export const options = {
  scenarios: {
    // 场景1: Bot 发送消息（匀速，持续施压）
    bot_send: {
      executor: 'ramping-vus',
      startVUs: 1,
      stages: [
        { duration: '15s', target: VUS_SEND },
        { duration: '30s', target: VUS_SEND },
        { duration: '10s', target: 0 },
      ],
      exec: 'scenarioBotSend',
      tags: { scenario: 'bot_send' },
    },

    // 场景2: Bot 事件轮询（常驻 VU，模拟多 Bot 实例长轮询）
    bot_events: {
      executor: 'constant-vus',
      vus: VUS_EVENTS,
      duration: '55s',
      exec: 'scenarioBotEvents',
      startTime: '0s',
      tags: { scenario: 'bot_events' },
    },

    // 场景3: 用户登录（先爬坡，后稳定，模拟早晚高峰）
    user_login: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: '10s', target: VUS_LOGIN },
        { duration: '25s', target: VUS_LOGIN },
        { duration: '10s', target: 0 },
      ],
      exec: 'scenarioUserLogin',
      startTime: '5s',
      tags: { scenario: 'user_login' },
    },

    // 场景4: 消息历史查询（token 在 setup 中预取，VU 直接复用）
    message_sync: {
      executor: 'ramping-vus',
      startVUs: 1,
      stages: [
        { duration: '10s', target: VUS_SYNC },
        { duration: '25s', target: VUS_SYNC },
        { duration: '5s',  target: 0 },
      ],
      exec: 'scenarioMessageSync',
      startTime: '15s',
      tags: { scenario: 'message_sync' },
    },
  },

  thresholds: {
    // 全局错误率低于 5%
    'error_rate':               ['rate<0.05'],
    // p95 延迟要求
    'bot_send_duration':        ['p(95)<1000'],
    'bot_events_duration':      ['p(95)<800'],
    'login_duration':           ['p(95)<1500'],
    'sync_duration':            ['p(95)<1000'],
    // HTTP 请求全局 p95
    'http_req_duration':        ['p(95)<2000'],
    // 失败率
    'http_req_failed':          ['rate<0.05'],
  },
};

// ── 共用 headers ──────────────────────────────────────────────────────────────

const BOT_HEADERS = {
  'Content-Type':  'application/json',
  'Authorization': `Bearer ${BOT_TOKEN}`,
};

const JSON_HEADERS = {
  'Content-Type': 'application/json',
};

// ── Fix 3: setup — 一次性预注册并登录所有 message_sync 用户 ──────────────────

export function setup() {
  const password = 'Stress@123456';
  const syncTokens = [];

  for (let i = 1; i <= VUS_SYNC; i++) {
    const username = `sync_vu_${i}`;

    // 注册（幂等，已存在时忽略错误）
    http.post(
      `${BASE_URL}/v1/user/usernameregister`,
      JSON.stringify({ username, password, name: `SyncUser${i}` }),
      { headers: JSON_HEADERS, tags: { name: 'setup_register' } },
    );

    const loginRes = http.post(
      `${BASE_URL}/v1/user/usernamelogin`,
      JSON.stringify({ username, password }),
      { headers: JSON_HEADERS, tags: { name: 'setup_login' } },
    );

    let token = '';
    try {
      const body = JSON.parse(loginRes.body);
      token = (body.data && body.data.token) || body.token || '';
    } catch (_) {}
    syncTokens.push(token);
  }

  return { syncTokens };
}

// ── 场景实现 ──────────────────────────────────────────────────────────────────

/**
 * 场景1: Bot 发送消息
 * POST /v1/bot/sendMessage
 * Auth: Bearer bot_token
 */
export function scenarioBotSend() {
  const payload = JSON.stringify({
    channel_id:   GROUP_ID,
    channel_type: 2,          // 2 = 群组
    payload: {
      type:    1,
      content: `stress-${__VU}-${__ITER}-${Date.now()}`,
    },
  });

  const res = http.post(`${BASE_URL}/v1/bot/sendMessage`, payload, {
    headers: BOT_HEADERS,
    tags:    { name: 'bot_sendMessage' },
  });
  botSendDuration.add(res.timings.duration);  // Fix 5

  const ok = check(res, {
    'bot_send: status 200': (r) => r.status === 200,
    'bot_send: no error':   (r) => {
      try { return !JSON.parse(r.body).msg; } catch { return true; }
    },
  });
  if (!ok) {
    botSendErrors.add(1);
    errorRate.add(1);
  } else {
    errorRate.add(0);
  }

  sleep(0.5 + Math.random() * 0.5); // 0.5~1s 间隔，避免完全同步
}

/**
 * 场景2: Bot 事件轮询
 * POST /v1/bot/events
 * Auth: Bearer bot_token
 * Fix 2: 每次用上次返回的最大 event_id 推进游标（per-VU 状态）
 */
export function scenarioBotEvents() {
  const payload = JSON.stringify({
    event_id: lastBotEventId,
    limit:    20,
  });

  const res = http.post(`${BASE_URL}/v1/bot/events`, payload, {
    headers: BOT_HEADERS,
    tags:    { name: 'bot_events' },
  });
  botEventsDuration.add(res.timings.duration);  // Fix 5

  // 推进游标：取本批次 events 中最大的 event_id
  if (res.status === 200) {
    try {
      const body = JSON.parse(res.body);
      if (Array.isArray(body.events) && body.events.length > 0) {
        const maxId = Math.max(...body.events.map((e) => e.event_id || 0));
        if (maxId > lastBotEventId) lastBotEventId = maxId;
      }
    } catch (_) {}
  }

  const ok = check(res, {
    'bot_events: status 200': (r) => r.status === 200,
    'bot_events: has events': (r) => {
      try {
        const body = JSON.parse(r.body);
        return Array.isArray(body.events) || body.events === null || body.events === undefined;
      } catch { return false; }
    },
  });
  if (!ok) {
    botEventsErrors.add(1);
    errorRate.add(1);
  } else {
    errorRate.add(0);
  }

  sleep(1);
}

/**
 * 场景3: 用户登录
 * POST /v1/user/usernamelogin
 * 使用预置测试账号（格式 stress_VU_ITER），若不存在则先注册
 */
export function scenarioUserLogin() {
  const username = `stress_${__VU}_${__ITER % 10}`; // 复用账号（取模减少注册量）
  const password = 'Stress@123456';

  const loginRes = http.post(
    `${BASE_URL}/v1/user/usernamelogin`,
    JSON.stringify({ username, password }),
    { headers: JSON_HEADERS, tags: { name: 'user_login' } },
  );
  loginDuration.add(loginRes.timings.duration);  // Fix 5

  if (loginRes.status === 200) {
    const ok = check(loginRes, {
      'login: status 200': (r) => r.status === 200,
      'login: has token':  (r) => {
        try { const b = JSON.parse(r.body); return !!(b.data && b.data.token) || !!b.token; } catch { return false; }
      },
    });
    if (!ok) { loginErrors.add(1); errorRate.add(1); } else { errorRate.add(0); }
  } else {
    // 账号不存在，先注册再登录
    http.post(
      `${BASE_URL}/v1/user/usernameregister`,
      JSON.stringify({ username, password, name: `StressUser${__VU}` }),
      { headers: JSON_HEADERS, tags: { name: 'user_register' } },
    );

    const retryRes = http.post(
      `${BASE_URL}/v1/user/usernamelogin`,
      JSON.stringify({ username, password }),
      { headers: JSON_HEADERS, tags: { name: 'user_login_retry' } },
    );

    const ok = check(retryRes, {
      'login(retry): status 200': (r) => r.status === 200,
      'login(retry): has token':  (r) => {
        try { const b = JSON.parse(r.body); return !!(b.data && b.data.token) || !!b.token; } catch { return false; }
      },
    });
    if (!ok) { loginErrors.add(1); errorRate.add(1); } else { errorRate.add(0); }
  }

  sleep(0.3 + Math.random() * 0.7);
}

/**
 * 场景4: 消息历史查询
 * POST /v1/message/channel/sync
 * Fix 3: token 由 setup() 预取，VU 按索引复用，避免每次迭代重新注册登录
 */
export function scenarioMessageSync(data) {
  // 按 VU 编号轮换取 token（VU 从 1 起，取模防止越界）
  const userToken = data.syncTokens[(__VU - 1) % data.syncTokens.length];
  if (!userToken) {
    syncErrors.add(1);
    errorRate.add(1);
    sleep(1);
    return;
  }

  const syncPayload = JSON.stringify({
    channel_id:        GROUP_ID,
    channel_type:      2,         // 2 = 群组
    start_message_seq: 0,
    end_message_seq:   0,
    pull_mode:         1,         // 1 = 向下拉取（最新消息）
    limit:             20,
  });

  const syncRes = http.post(
    `${BASE_URL}/v1/message/channel/sync`,
    syncPayload,
    {
      headers: { 'Content-Type': 'application/json', 'token': userToken },
      tags:    { name: 'message_channel_sync' },
    },
  );
  syncDuration.add(syncRes.timings.duration);  // Fix 5

  const ok = check(syncRes, {
    'sync: status 200':   (r) => r.status === 200,
    'sync: has messages': (r) => {
      try {
        const body = JSON.parse(r.body);
        return Array.isArray(body.messages);
      } catch { return false; }
    },
  });
  if (!ok) {
    syncErrors.add(1);
    errorRate.add(1);
  } else {
    errorRate.add(0);
  }

  sleep(1 + Math.random());
}
