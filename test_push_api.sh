#!/bin/bash

# 사용법 체크
if [ $# -ne 2 ]; then
  echo "Usage: $0 <number_of_requests> <client_id_range>"
  echo "Example: $0 10 100"
  exit 1
fi

# 입력값이 숫자인지 확인
if ! [[ $1 =~ ^[0-9]+$ ]] || ! [[ $2 =~ ^[0-9]+$ ]]; then
  echo "Error: Please provide valid numbers"
  exit 1
fi

# 기본 설정
BASE_URL="http://k8s-meditbac-wsserver-47900150f5-a1083ea59ce1dfb7.elb.us-east-1.amazonaws.com:8080"
ENDPOINT="/push"
CONTENT_TYPE="Content-Type: application/json"
MESSAGE='{"message":"test message"}'

# 색상 설정
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m' # No Color

# 성공 및 실패 카운트 변수
success_count=0
fail_count=0

# 테스트 함수
test_push() {
  local client_id=$1
  local url="${BASE_URL}${ENDPOINT}?client_id=client_${client_id}"

  echo -e "\n${GREEN}Testing client_${client_id}${NC}"
  echo "URL: $url"
  echo "Payload: $MESSAGE"

  # curl 요청 실행 및 결과 저장
  local response
  response=$(curl -s -w "\n%{http_code}" -X POST \
    -H "$CONTENT_TYPE" \
    -d "$MESSAGE" \
    "$url")

  # HTTP 상태 코드 추출
  local status_code
  status_code=$(echo "$response" | tail -n1)
  local body
  body=$(echo "$response" | sed '$d')

  # 결과 출력
  echo "Status Code: $status_code"
  echo "Response: $body"

  # 결과에 따라 카운트 증가
  if [ "$status_code" == "200" ]; then
    ((success_count++))
  else
    ((fail_count++))
  fi
}

# 입력받은 숫자만큼 테스트 실행
NUM_REQUESTS=$1
CLIENT_ID_RANGE=$2
echo "Running $NUM_REQUESTS tests..."

for ((i = 0; i < NUM_REQUESTS; i++)); do
  random_id=$((RANDOM % CLIENT_ID_RANGE))
  test_push $random_id
  sleep 0.01 # 요청 간 간격
done

# 최종 통계 출력
echo -e "\n${GREEN}Final Statistics:${NC}"
echo "Total Requests: $NUM_REQUESTS"
echo -e "Successful: ${GREEN}$success_count${NC}"
echo -e "Failed: ${RED}$fail_count${NC}"
