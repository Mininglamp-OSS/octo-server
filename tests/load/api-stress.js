import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate, Trend } from 'k6/metrics';

const BASE_URL   = __ENV.API_URL   || 'http://localhost:8090';
const BOT_TOKEN  = __ENV.BOT_TOKEN;
if (!BOT_TOKEN) { throw new Error('BOT_TOKEN env var is required'); }
const errorRate = new Rate('errors');
const loginTrend = new Trend('login_duration');

export const options = {
  scenarios: {
    login_stress: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: '10s', target: 20 },
        { duration: '20s', target: 50 },
        { duration: '10s', target: 0 },
      ],
      exec: 'loginTest',
    },
    bot_events: {
      executor: 'constant-vus',
      vus: 10,
      duration: '30s',
      exec: 'botEventsTest',
      startTime: '5s',
    },
    bot_send: {
      executor: 'constant-vus',
      vus: 5,
      duration: '20s',
      exec: 'botSendTest',
      startTime: '10s',
    },
  },
  thresholds: {
    http_req_duration: ['p(95)<500'],
    errors: ['rate<0.01'],
  },
};

export function loginTest() {
  const uniqueUser = `lt_${__VU}_${__ITER}_${Date.now()}`;
  http.post(`${BASE_URL}/v1/user/usernameregister`, JSON.stringify({
    username: uniqueUser, password: 'testpass123', name: `LoadUser ${__VU}`,
  }), { headers: { 'Content-Type': 'application/json' } });

  const start = Date.now();
  const loginRes = http.post(`${BASE_URL}/v1/user/usernamelogin`, JSON.stringify({
    username: uniqueUser, password: 'testpass123',
  }), { headers: { 'Content-Type': 'application/json' } });
  loginTrend.add(Date.now() - start);

  const success = check(loginRes, {
    'login 200': (r) => r.status === 200,
    'has uid': (r) => { try { return JSON.parse(r.body).data.uid !== undefined; } catch { return false; } },
  });
  errorRate.add(!success);
  sleep(0.5);
}

export function botEventsTest() {
  const res = http.post(`${BASE_URL}/v1/bot/events`, JSON.stringify({ event_id: 0, limit: 10 }), {
    headers: { 'Content-Type': 'application/json', 'Authorization': `Bearer ${BOT_TOKEN}` },
  });
  check(res, { 'events 200': (r) => r.status === 200 });
  sleep(1);
}

export function botSendTest() {
  const res = http.post(`${BASE_URL}/v1/bot/sendMessage`, JSON.stringify({
    channel_id: 'f1f2f95f8d324b6ea1ee4b626dfd16b8', channel_type: 2,
    payload: { type: 1, content: `k6 stress ${__VU}-${__ITER}` },
  }), {
    headers: { 'Content-Type': 'application/json', 'Authorization': `Bearer ${BOT_TOKEN}` },
  });
  check(res, { 'send 200': (r) => r.status === 200 });
  sleep(1);
}
