// Load script for the observability stack. Run with `make load` (see README).
//   RATE      target requests/s for the steady scenario (default 50)
//   DURATION  how long to hold RATE (default 5m)
//   SCENARIO  steady (default) or burst (one IP at 5 req/s for 2m, to trigger 429s)
//   BASE_URL  set by docker-compose.observability.yml
import http from "k6/http";
import { check } from "k6";

const BASE_URL = __ENV.BASE_URL || "http://server:8080";
const RATE = parseInt(__ENV.RATE || "50", 10);
const DURATION = __ENV.DURATION || "5m";
const SCENARIO = __ENV.SCENARIO || "steady";
const LINKS = 50;

const scenarios = {
  steady: {
    executor: "ramping-arrival-rate",
    startRate: 1,
    timeUnit: "1s",
    preAllocatedVUs: 50,
    maxVUs: 500,
    stages: [
      { target: RATE, duration: "10s" },
      { target: RATE, duration: DURATION },
    ],
  },
  burst: {
    executor: "constant-arrival-rate",
    rate: 5,
    timeUnit: "1s",
    duration: "2m",
    preAllocatedVUs: 10,
    maxVUs: 50,
  },
};

export const options = {
  scenarios: { [SCENARIO]: scenarios[SCENARIO] },
  thresholds: {
    "http_req_failed{expected_response:true}": ["rate<0.01"],
    "http_req_duration{name:redirect}": ["p(95)<300"],
  },
};

// 200-404, 410 (expired/disabled link) and 429 (rate limit or daily quota) are
// expected: unknown codes and the limits are part of the traffic mix.
http.setResponseCallback(http.expectedStatuses({ min: 200, max: 404 }, 410, 429));

function randomIP() {
  const b = () => Math.floor(Math.random() * 256);
  return `10.${b()}.${b()}.${1 + Math.floor(Math.random() * 254)}`;
}

// Burst uses one fixed address so the per-IP limits are hit.
const BURST_IP = "10.250.250.250";
const clientIP = () => (SCENARIO === "burst" ? BURST_IP : randomIP());

function headers(token) {
  const h = { "Content-Type": "application/json", "X-Real-IP": clientIP() };
  if (token) h.Authorization = `Bearer ${token}`;
  return h;
}

export function setup() {
  const creds = JSON.stringify({
    email: `k6-${Date.now()}@example.test`,
    password: "k6-load-test-password",
  });
  // Setup always uses distinct IPs so it stays under the per-IP create limit.
  const h = () => ({ "Content-Type": "application/json", "X-Real-IP": randomIP() });
  const reg = http.post(`${BASE_URL}/api/auth/register`, creds, { headers: h() });
  const login = http.post(`${BASE_URL}/api/auth/login`, creds, { headers: h() });
  if (reg.status !== 201 || login.status !== 200) {
    throw new Error(`setup: register ${reg.status}, login ${login.status}`);
  }
  const token = login.json("access_token");

  const codes = [];
  for (let i = 0; i < LINKS; i++) {
    const res = http.post(
      `${BASE_URL}/api/links`,
      JSON.stringify({ url: `https://example.com/k6/${Date.now()}-${i}` }),
      { headers: { ...h(), Authorization: `Bearer ${token}` } },
    );
    if (res.status !== 201) throw new Error(`setup: create link ${res.status}`);
    codes.push(res.json("code"));
  }
  return { token, codes };
}

export default function (data) {
  const r = Math.random();
  if (r < 0.9) {
    // 5% of redirects use an unknown code and return 404.
    const code =
      Math.random() < 0.05
        ? `nf${Math.random().toString(36).slice(2, 10)}`
        : data.codes[Math.floor(Math.random() * data.codes.length)];
    const res = http.get(`${BASE_URL}/${code}`, {
      redirects: 0,
      headers: { "X-Real-IP": clientIP() },
      tags: { name: "redirect" },
    });
    check(res, { "redirect 302/404/429": (x) => [302, 404, 429].includes(x.status) });
  } else if (r < 0.98) {
    http.get(`${BASE_URL}/api/links`, {
      headers: headers(data.token),
      tags: { name: "list" },
    });
  } else {
    http.post(
      `${BASE_URL}/api/links`,
      JSON.stringify({ url: `https://example.com/k6/${Date.now()}-${Math.random()}` }),
      { headers: headers(data.token), tags: { name: "create" } },
    );
  }
}
