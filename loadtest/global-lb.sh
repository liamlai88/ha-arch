#!/bin/bash
# 模拟全局 LB：先试 Cluster A，失败 fallback 到 Cluster B
# 用法：./global-lb.sh        持续打 60 秒
#       ./global-lb.sh once   只打 1 次

PRIMARY="http://localhost:30080/"      # Cluster A
SECONDARY="http://localhost:30081/"    # Cluster B
TIMEOUT=2                              # 健康检查超时（秒）

call_once() {
  # 先试 Primary
  resp=$(curl -s -m $TIMEOUT --connect-timeout 1 "$PRIMARY" 2>/dev/null)
  if [ -n "$resp" ]; then
    cluster="A"
  else
    # Primary 挂了，切到 Secondary
    resp=$(curl -s -m $TIMEOUT --connect-timeout 1 "$SECONDARY" 2>/dev/null)
    if [ -n "$resp" ]; then
      cluster="B (failover)"
    else
      cluster="BOTH DOWN"
    fi
  fi
  echo "$(date +%H:%M:%S) cluster=$cluster resp=${resp:-EMPTY}"
}

if [ "$1" = "once" ]; then
  call_once
  exit 0
fi

echo "持续 60 秒模拟全局 LB...(Ctrl+C 终止)"
end=$(( $(date +%s) + 60 ))
total=0; a=0; b=0; down=0
while [ $(date +%s) -lt $end ]; do
  result=$(call_once)
  echo "$result"
  total=$((total + 1))
  if echo "$result" | grep -q "cluster=A "; then
    a=$((a + 1))
  elif echo "$result" | grep -q "cluster=B"; then
    b=$((b + 1))
  else
    down=$((down + 1))
  fi
  sleep 0.5
done

echo ""
echo "===== 汇总 ====="
echo "Total:                $total"
echo "Cluster A served:     $a"
echo "Cluster B (failover): $b"
echo "Both down:            $down"
