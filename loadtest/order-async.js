// 异步下单：写 Kafka，毫秒级 202
import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';

const accepted = new Counter('accepted');
const failed = new Counter('failed');

export const options = {
  scenarios: {
    burst: {
      executor: 'constant-arrival-rate',
      rate: 1000,
      timeUnit: '1s',
      duration: '10s',         // 10 秒 × 1000/s = 1 万订单
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
  const res = http.post('http://localhost:9080/order', payload, {
    headers: { 'Content-Type': 'application/json' },
  });
  if (res.status === 202) accepted.add(1);
  else failed.add(1);
  check(res, { 'is 202': (r) => r.status === 202 });
}
