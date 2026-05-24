#!/usr/bin/env bash
# scripts/status.sh — 一目了然看 gotrader 现在啥状态
#
# 输出：
#   1. 进程活着没（PID + 跑了多久）
#   2. 今天的日志路径 + 大小
#   3. 最近 3 次 TSMOM 评估
#   4. 最近 3 次下单
#   5. 最近 3 次错误（如果有）
set -uo pipefail
# 故意不用 set -e：grep / wc 在没匹配时返回 1，正常但会触发 -e 退出。
# 这个脚本是"看状态"的，任何子命令失败都应该继续往下显示，而不是骤停。

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PID_FILE="$ROOT/logs/gotrader.pid"
TODAY_LOG="$ROOT/logs/gotrader-$(date +%Y%m%d).log"

echo "===== gotrader 状态 ($(date '+%Y-%m-%d %H:%M:%S')) ====="
echo ""

# 1) 进程状态
if [[ -f "$PID_FILE" ]]; then
    PID=$(cat "$PID_FILE")
    if kill -0 "$PID" 2>/dev/null; then
        # ps 拿 etime（已运行时间）
        ETIME=$(ps -o etime= -p "$PID" 2>/dev/null | tr -d ' ')
        RSS=$(ps -o rss= -p "$PID" 2>/dev/null | tr -d ' ')
        RSS_MB=$((RSS / 1024))
        echo "🟢 进程运行中  PID=$PID  已运行=$ETIME  内存=${RSS_MB}MB"
    else
        echo "🔴 PID=$PID 进程已死，但 PID 文件还在（异常）"
    fi
else
    echo "⚪ 进程未在运行（无 PID 文件）"
fi
echo ""

# 2) 日志文件
if [[ -f "$TODAY_LOG" ]]; then
    SIZE=$(du -h "$TODAY_LOG" | awk '{print $1}')
    LINES=$(wc -l <"$TODAY_LOG" | tr -d ' ')
    echo "📁 今日日志: $TODAY_LOG ($SIZE, $LINES 行)"
else
    echo "📁 今日日志尚未生成: $TODAY_LOG"
    # 找最近的日志兜底
    LAST_LOG=$(ls -t "$ROOT"/logs/gotrader-*.log 2>/dev/null | head -1 || true)
    if [[ -n "$LAST_LOG" ]]; then
        echo "   最近一次: $LAST_LOG"
        TODAY_LOG="$LAST_LOG"
    fi
fi
echo ""

# 没有日志就停在这
[[ -f "$TODAY_LOG" ]] || exit 0

# 3) 最近 3 次 TSMOM 评估
echo "--- 最近 3 次策略评估 ---"
grep -E "(TSMOM 评估|VMM 评估|缠论买卖点)" "$TODAY_LOG" 2>/dev/null | tail -3 \
    | sed 's/^/  /' || echo "  (暂无)"
echo ""

# 4) 最近 3 次下单
echo "--- 最近 3 次下单 ---"
grep "订单已下" "$TODAY_LOG" 2>/dev/null | tail -3 | sed 's/^/  /' || echo "  (暂无)"
echo ""

# 5) 最近错误
# 注意：grep -c 在 no-match 时输出 "0" 并返回 exit 1，叠加 || echo 0 会输出 "0\n0"。
# 用 || true 让 exit 码归零，再用默认值兜底。
ERR_COUNT=$(grep -cE "level=(ERROR|WARN)" "$TODAY_LOG" 2>/dev/null || true)
ERR_COUNT=${ERR_COUNT:-0}
if [[ "$ERR_COUNT" -gt 0 ]]; then
    echo "--- 最近 3 条 ERROR/WARN（共 $ERR_COUNT 条）---"
    grep -E "level=(ERROR|WARN)" "$TODAY_LOG" | tail -3 | sed 's/^/  /'
else
    echo "✅ 今日无 ERROR/WARN"
fi
