// 压测 /load 端点：每个请求烧 200ms CPU，逼 HPA 扩容
import http from 'k6/http';
import { check } from 'k6';

export const options = {
  scenarios: {
    // 50 个虚拟用户持续打 3 分钟（给 HPA 充分时间扩容）
    sustained: {
      executor: 'constant-vus',
      vus: 50,
      duration: '3m',
    },
  },
};

export default function () {
  const res = http.get('http://localhost:30080/load?ms=200');
  check(res, { 'is 200': (r) => r.status === 200 });
}
