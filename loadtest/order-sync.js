// 同步下单对照组：直接写 MySQL，DB 顶不住时会失败
import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';

const ok = new Counter('ok_200');
const failed = new Counter('failed');

export const options = {
  scenarios: {
    burst: {
      executor: 'constant-arrival-rate',
      rate: 1000,
      timeUnit: '1s',
      duration: '10s',
      preAllocatedVUs: 200,
      maxVUs: 500,
    },
  },
};

export default function () {
  const payload = JSON.stringify({
    user_id: Math.floor(Math.random() * 1000),
    product_id: Math.floor(Math.random() * 5) + 1,
    quantity: 1,
  });
  const res = http.post('http://localhost:9080/order/sync', payload, {
    headers: { 'Content-Type': 'application/json' },
    timeout: '10s',
  });
  if (res.status === 200) ok.add(1);
  else failed.add(1);
  check(res, { 'is 200': (r) => r.status === 200 });
}
