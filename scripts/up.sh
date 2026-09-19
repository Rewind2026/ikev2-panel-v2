#!/usr/bin/env bash
# up.sh - 一键启动容器(自动探测网络模式)
#
# 设计见 docs/design.md §18.2 / scripts/auto-network.sh
#
# 为什么需要这个脚本:
#   docker compose 启动前就要决定 network_mode (host/bridge/ipvlan),
#   entrypoint.sh 在容器内运行无法热切换。所以探测 + 生成 override 必须
#   在宿主上跑一次 → 这个脚本就是封装 "探测 + 启动" 两步。
#
# 用法:
#   ./scripts/up.sh                  # 自动探测 + 启动
#   ./scripts/up.sh --no-detect      # 跳过探测(直接用 docker-compose.yml 默认 host)
#   IKEV2_NETWORK_MODE=host ./scripts/up.sh   # 显式指定模式(给 auto-network.sh)
#
# 等价于:
#   ./scripts/auto-network.sh && docker compose up -d

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "$PROJECT_DIR"

if [ -t 1 ]; then
  BLUE='\033[0;34m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
else
  BLUE=''; GREEN=''; YELLOW=''; NC=''
fi
log()  { echo -e "${BLUE}[up.sh]${NC} $*" >&2; }
ok()   { echo -e "${GREEN}[up.sh] ✓${NC} $*" >&2; }
warn() { echo -e "${YELLOW}[up.sh] !${NC} $*" >&2; }

# ---------- 解析参数 ----------
NO_DETECT=0
for arg in "$@"; do
  case "$arg" in
    --no-detect) NO_DETECT=1 ;;
    -h|--help)
      cat <<EOF
用法: ./scripts/up.sh [选项]

选项:
  --no-detect    跳过网络模式探测(直接用 docker-compose.yml 默认 host 网络)
  -h, --help     显示这个帮助

环境变量:
  IKEV2_NETWORK_MODE    auto (默认) | host | bridge | ipvlan
                        auto 模式由 scripts/auto-network.sh 探测;显式指定时直接尊重

示例:
  ./scripts/up.sh                          # 日常启动(自动探测)
  IKEV2_NETWORK_MODE=host ./scripts/up.sh  # 强制 host 网络
  ./scripts/up.sh --no-detect              # 跳过探测(老用户习惯)
EOF
      exit 0
      ;;
    *) warn "unknown arg: $arg (ignored)" ;;
  esac
done

# ---------- 1. 探测网络模式(生成 override) ----------
if [ "$NO_DETECT" = "1" ]; then
  warn "--no-detect passed, skipping auto-network.sh (using docker-compose.yml default = host)"
else
  if [ "${IKEV2_NETWORK_MODE:-auto}" = "auto" ]; then
    log "IKEV2_NETWORK_MODE=auto, running auto-network.sh..."
  else
    log "IKEV2_NETWORK_MODE=${IKEV2_NETWORK_MODE}, running auto-network.sh (will respect user choice)..."
  fi
  if bash "${SCRIPT_DIR}/auto-network.sh"; then
    ok "auto-network.sh done"
  else
    warn "auto-network.sh failed (exit=$?), continuing with docker-compose.yml default"
  fi
fi

# ---------- 2. docker compose up ----------
log "running: docker compose up -d"
exec docker compose up -d "$@"
