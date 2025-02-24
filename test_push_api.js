import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter } from 'k6/metrics';

// 사용자 정의 메트릭
const customMetrics = {
  successfulPushes: new Counter('successful_pushes'),
  failedPushes: new Counter('failed_pushes'),
};

// 환경 변수에서 최대 client_id 값 가져오기 (기본값: 100)
const MAX_CLIENT_ID = __ENV.MAX_CLIENT_ID ? parseInt(__ENV.MAX_CLIENT_ID) : 100;

// 테스트 설정
export const options = {
  scenarios: {
    push_requests: {
      executor: 'per-vu-iterations',
      vus: 10000,               // 동시 실행 VU 수
      iterations: 100,       // 총 반복 횟수
      maxDuration: '60s',   // 최대 실행 시간
    },
  },
  thresholds: {
    http_req_duration: ['p(95)<500'],  // 95%의 요청이 500ms 이내
    http_req_failed: ['rate<0.01'],    // 1% 미만의 실패율
  },
};

const BASE_URL = 'http://k8s-meditbac-wsserver-47900150f5-a1083ea59ce1dfb7.elb.us-east-1.amazonaws.com:8080';

export default function () {
  // 랜덤한 client_id 생성
  const randomId = Math.floor(Math.random() * (MAX_CLIENT_ID - 1));
  const clientId = `client_${randomId}`;
  
  const payload = JSON.stringify({
    message: `test message from ${clientId}`
  });

  const params = {
    headers: {
      'Content-Type': 'application/json',
    },
    tags: {
      clientId: clientId,
    },
  };

  // POST 요청 실행
  const response = http.post(
    `${BASE_URL}/push?client_id=${clientId}`,
    payload,
    params
  );

  // 응답 검증 및 메트릭 기록
  const checkResult = check(response, {
    'status is 200': (r) => r.status === 200,
    // 'response body has message': (r) => r.body.includes('message'),
  });

  if (checkResult) {
    customMetrics.successfulPushes.add(1);
    console.log(`✅ Success: ${clientId}`);
  } else {
    customMetrics.failedPushes.add(1);
    console.log(`❌ Failed: ${clientId} (Status: ${response.status})`);
  }

  // 요청 간 간격 (1초)
  sleep(0.01);
}

// 결과 리포트 생성
// export function handleSummary(data) {
//   const summary = {
//     metrics: {
//       total_requests: data.metrics.http_reqs.values.count,
//       successful_requests: data.metrics.successful_pushes.values.count,
//       failed_requests: data.metrics.failed_pushes.values.count,
//       avg_duration: `${data.metrics.http_req_duration.values.avg.toFixed(2)}ms`,
//       p95_duration: `${data.metrics.http_req_duration.values['p(95)'].toFixed(2)}ms`,
//       max_client_id: MAX_CLIENT_ID,
//     },
//     checks: data.metrics.checks,
//   };

//   return {
//     'stdout': JSON.stringify(summary, null, 2),
//     'push_test_summary.json': JSON.stringify(data, null, 2),
//   };
// }
