#!/usr/bin/env bash
# scripts/run.sh — 后台挂起 gotrader 模拟盘
#
# 设计原则（Linus "实用主义"）：
#   - 一个进程就够，不搞 docker/systemd 之类的过度工程
#   - 日志按日期归档（每天一个文件，方便复盘）
#   - 启动前检查 config.yaml 不是实盘（live: true）
#   - 用 PID 文件防止重复启动
#
# 用法：
#   scripts/run.sh           # 启动
#   scripts/status.sh        # 看状态
#   scripts/stop.sh          # 优雅停止
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

BIN="$ROOT/bin/gotrader"
CFG="$ROOT/config.yaml"
PID_FILE="$ROOT/logs/gotrader.pid"
LOG_FILE="$ROOT/logs/gotrader-$(date +%Y%m%d).log"

# 1) 前置检查：编译过没有
if [[ ! -x "$BIN" ]]; then
    echo "❌ $BIN 不存在，请先 go build -o bin/gotrader ./cmd/gotrader" >&2
    exit 1
fi

# 2) 前置检查：配置文件存在
if [[ ! -f "$CFG" ]]; then
    echo "❌ $CFG 不存在" >&2
    exit 1
fi

# 3) 前置检查：是不是真的模拟盘？（live: true 拒绝启动）
if grep -E "^\s*live:\s*true\b" "$CFG" >/dev/null 2>&1; then
    echo "❌ 配置里 live: true（实盘）。脚本拒绝在没有确认的情况下挂起实盘进程。" >&2
    echo "   如确认要跑实盘，手动用：./bin/gotrader -config config.yaml" >&2
    exit 1
fi

# 4) 防止重复启动：PID 文件存在 + 进程还活着 → 拒绝
if [[ -f "$PID_FILE" ]]; then
    OLD_PID=$(cat "$PID_FILE")
    if kill -0 "$OLD_PID" 2>/dev/null; then
        echo "❌ gotrader 已在运行 (PID=$OLD_PID)。先 scripts/stop.sh 再重启。" >&2
        exit 1
    else
        echo "ℹ️  陈旧 PID 文件已清理 (PID=$OLD_PID 已死)"
        rm -f "$PID_FILE"
    fi
fi

# 5) 启动
echo "▶ 启动 gotrader 模拟盘"
echo "  配置:    $CFG"
echo "  日志:    $LOG_FILE"
echo ""

nohup "$BIN" -config "$CFG" >>"$LOG_FILE" 2>&1 &
PID=$!
echo $PID > "$PID_FILE"
disown $PID 2>/dev/null || true

# 6) 等 2 秒确认进程没立刻挂掉
sleep 2
if ! kill -0 "$PID" 2>/dev/null; then
    echo "❌ 进程启动 2 秒内就挂了，看日志：tail -50 $LOG_FILE" >&2
    rm -f "$PID_FILE"
    exit 1
fi

echo "✅ 已启动 (PID=$PID)"
echo ""
echo "查看状态: scripts/status.sh"
echo "看日志:   tail -f $LOG_FILE"
echo "停止:     scripts/stop.sh"
