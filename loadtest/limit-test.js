import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';

// 验证 APISIX 限流：限 10 req/s + burst 5，我们打 100 RPS，约 85% 应被拒
const got200 = new Counter('got_200');
const got429 = new Counter('got_429');
const gotOther = new Counter('got_other');

export const options = {
  scenarios: {
    burst: {
      executor: 'constant-arrival-rate',
      rate: 100,           // 每秒发起 100 个新请求
      timeUnit: '1s',
      duration: '15s',
      preAllocatedVUs: 50,
      maxVUs: 200,
    },
  },
};

export default function () {
  const res = http.get('http://localhost:9080/product?id=1');
  if (res.status === 200) got200.add(1);
  else if (res.status === 429) got429.add(1);
  else gotOther.add(1);
  check(res, { 'status is 200 or 429': (r) => r.status === 200 || r.status === 429 });
}
