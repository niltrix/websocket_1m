#!/bin/bash

# 사용법 체크
if [ $# -ne 1 ]; then
  echo "Usage: $0 <number_of_requests>"
  echo "Example: $0 10"
  exit 1
fi

# 입력값이 숫자인지 확인
if ! [[ $1 =~ ^[0-9]+$ ]]; then
  echo "Error: Please provide a valid number"
  exit 1
fi

# 기본 설정
BASE_URL="http://localhost:8080"
ENDPOINT="/push"
CONTENT_TYPE="Content-Type: application/json"
MESSAGE='{"message":"test message"}'

# 색상 설정
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m' # No Color

# 요청 결과 저장을 위한 배열
declare -A results

# 테스트 함수
test_push() {
  local client_id=$1
  local url="${BASE_URL}${ENDPOINT}?client_id=client_${client_id}"

  echo -e "\n${GREEN}Testing client_${client_id}${NC}"
  echo "URL: $url"
  echo "Payload: $MESSAGE"

  # curl 요청 실행 및 결과 저장
  local response=$(curl -s -w "\n%{http_code}" -X POST \
    -H "$CONTENT_TYPE" \
    -d "$MESSAGE" \
    "$url")

  # HTTP 상태 코드 추출
  local status_code=$(echo "$response" | tail -n1)
  local body=$(echo "$response" | sed '$d')

  # 결과 출력
  echo "Status Code: $status_code"
  echo "Response: $body"

  # 결과 저장
  results[$client_id]="$status_code"
}

# 입력받은 숫자만큼 테스트 실행
NUM_REQUESTS=$1
echo "Running $NUM_REQUESTS tests..."

for i in $(seq 0 $((NUM_REQUESTS - 1))); do
  test_push $i
  sleep 1 # 요청 간 간격
done

# 결과 요약 출력
echo -e "\n${GREEN}Test Summary:${NC}"
success_count=0
fail_count=0

for client_id in "${!results[@]}"; do
  status="${results[$client_id]}"
  if [ "$status" == "200" ]; then
    echo -e "client_${client_id}: ${GREEN}Success${NC} (${status})"
    ((success_count++))
  else
    echo -e "client_${client_id}: ${RED}Failed${NC} (${status})"
    ((fail_count++))
  fi
done

# 최종 통계 출력
echo -e "\n${GREEN}Final Statistics:${NC}"
echo "Total Requests: $NUM_REQUESTS"
echo -e "Successful: ${GREEN}$success_count${NC}"
echo -e "Failed: ${RED}$fail_count${NC}"
