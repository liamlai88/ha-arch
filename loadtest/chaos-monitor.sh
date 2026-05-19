#!/bin/bash
# 持续打 K8s 服务，统计成功率和 P99 延迟
# 用法：./chaos-monitor.sh  （Ctrl+C 退出后看汇总）

total=0
success=0
declare -a times

trap '
  echo ""
  echo "===== 实验汇总 ====="
  echo "Total requests:  $total"
  echo "Success (2xx):   $success"
  if [ $total -gt 0 ]; then
    awk "BEGIN { printf \"Success rate:    %.2f%%\\n\", $success / $total * 100 }"
  fi
  if [ ${#times[@]} -gt 0 ]; then
    printf "%s\n" "${times[@]}" | sort -n > /tmp/chaos-times.txt
    n=$(wc -l < /tmp/chaos-times.txt)
    p50_line=$(( n / 2 ))
    p95_line=$(( n * 95 / 100 ))
    p99_line=$(( n * 99 / 100 ))
    echo "P50 latency:     $(sed -n "${p50_line}p" /tmp/chaos-times.txt)s"
    echo "P95 latency:     $(sed -n "${p95_line}p" /tmp/chaos-times.txt)s"
    echo "P99 latency:     $(sed -n "${p99_line}p" /tmp/chaos-times.txt)s"
  fi
  exit 0
' INT

echo "Starting chaos monitor... (Ctrl+C to stop and see summary)"
while true; do
  result=$(curl -s -o /dev/null -w "%{http_code} %{time_total}" --max-time 5 http://localhost:30080/ 2>/dev/null)
  code=$(echo "$result" | awk '{print $1}')
  time=$(echo "$result" | awk '{print $2}')
  total=$((total + 1))
  if [[ "$code" =~ ^2 ]]; then
    success=$((success + 1))
  fi
  times+=("$time")
  printf "\rrequests=%d success=%d code=%s time=%ss" "$total" "$success" "$code" "$time"
  sleep 0.1
done
