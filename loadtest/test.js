import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter } from 'k6/metrics';

const cacheHits = new Counter('cache_hits');
const cacheMisses = new Counter('cache_misses');
const app1Hits = new Counter('app1_hits');
const app2Hits = new Counter('app2_hits');

export const options = {
  stages: [
    { duration: '15s', target: 50 },
    { duration: '30s', target: 200 },
    { duration: '15s', target: 0 },
  ],
  thresholds: {
    http_req_duration: ['p(95)<200', 'p(99)<500'],
    http_req_failed:   ['rate<0.01'],
  },
};

export default function () {
  const id = Math.floor(Math.random() * 5) + 1;
  const res = http.get(`http://localhost:8080/product?id=${id}`);

  check(res, {
    'status 200': (r) => r.status === 200,
  });

  const cache = res.headers['X-Cache'];
  if (cache === 'HIT')  cacheHits.add(1);
  if (cache === 'MISS') cacheMisses.add(1);

  const inst = res.headers['X-Instance'];
  if (inst === 'app1') app1Hits.add(1);
  if (inst === 'app2') app2Hits.add(1);

  sleep(0.05);
}
