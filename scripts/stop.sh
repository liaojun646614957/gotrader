#!/usr/bin/env bash
# scripts/stop.sh — 优雅停止 gotrader
#
# 用 SIGINT (=Ctrl+C) 而不是 SIGTERM，因为 main.go 用 signal.NotifyContext
# 捕获的是 SIGINT/SIGTERM，但代码里所有清理逻辑都按 Ctrl+C 路径测试过。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PID_FILE="$ROOT/logs/gotrader.pid"

if [[ ! -f "$PID_FILE" ]]; then
    echo "ℹ️  没有 PID 文件，进程可能没在跑" >&2
    exit 0
fi

PID=$(cat "$PID_FILE")
if ! kill -0 "$PID" 2>/dev/null; then
    echo "ℹ️  PID=$PID 进程已不存在，清理陈旧 PID 文件"
    rm -f "$PID_FILE"
    exit 0
fi

echo "▶ 发送 SIGINT 给 PID=$PID"
kill -INT "$PID"

# 等最多 10 秒优雅退出
for i in {1..10}; do
    if ! kill -0 "$PID" 2>/dev/null; then
        echo "✅ 进程已退出"
        rm -f "$PID_FILE"
        exit 0
    fi
    sleep 1
done

echo "⚠️  10 秒后仍未退出，发送 SIGKILL"
kill -KILL "$PID" 2>/dev/null || true
rm -f "$PID_FILE"
