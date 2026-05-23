#!/bin/bash
# 并发压测脚本

set -e

API_KEY="${API_KEY:-}"
ENDPOINT="${ENDPOINT:-http://localhost:8080/v1/messages}"
CONCURRENT="${1:-50}"
TOTAL="${2:-500}"

if [ -z "$API_KEY" ]; then
    echo "错误: 需设置 API_KEY 环境变量"
    echo "用法: export API_KEY=your-key && $0 [并发数] [总请求数]"
    exit 1
fi

echo "【压测配置】"
echo "并发数: $CONCURRENT"
echo "总请求: $TOTAL"
echo "端点: $ENDPOINT"
echo ""

# 检查 hey 是否安装
if ! command -v hey &> /dev/null; then
    echo "安装 hey 压测工具..."
    go install github.com/rakyll/hey@latest
fi

# 执行压测
echo "【开始压测】"
hey -n $TOTAL -c $CONCURRENT -m POST \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet-4.6","messages":[{"role":"user","content":"hi"}],"max_tokens":10}' \
  $ENDPOINT

echo ""
echo "【关键指标】"
echo "- 成功率目标: >99%"
echo "- P99 延迟目标: <5s"
echo "- 429 错误率目标: <1%"
echo ""
echo "【监控命令】"
echo "# 观察连接数"
echo "netstat -an | grep ESTABLISHED | grep amazonaws.com | wc -l"
echo ""
echo "# 观察账号分布"
echo "curl -H 'X-Admin-Password: \$PWD' http://localhost:8080/admin/api/status"
