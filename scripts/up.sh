#!/usr/bin/env bash
# up.sh - 一键启动容器(host 网络唯一模式)
#
# 设计见 docs/design.md §10.2
#
# 为什么需要这个脚本:
#   - 自动 ./logs bind mount 目录准备(避免 docker compose up 因目录不存在失败)
#   - docker compose up -d 的薄封装,加 stderr 染色 + 错误处理
#
# v2.86-PR14 收敛:删除网络模式探测(v2-79~v2-85 时期的 auto-network.sh 已删除)。
# 唯一支持 host 网络,无需探测。
#
# 用法:
#   ./scripts/up.sh                  # 默认启动
#   ./scripts/up.sh --no-detect      # 兼容老用户参数(已 no-op,保留)
#   ./scripts/up.sh --help           # 看帮助
#
# 等价于:
#   mkdir -p logs && chmod 1777 logs && docker compose up -d

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
for arg in "$@"; do
  case "$arg" in
    --no-detect) warn "--no-detect 已是 no-op(v2.86-PR14 删了网络模式探测)" ;;
    -h|--help)
      cat <<EOF
用法: ./scripts/up.sh [选项]

选项:
  --no-detect    兼容老参数(已 no-op)
  -h, --help     显示这个帮助

v2.86-PR14 后:网络模式固定 host,docker-compose.yml 已写死 network_mode: host,
                不再需要探测 / override 文件。
EOF
      exit 0
      ;;
    *) warn "unknown arg: $arg (ignored)" ;;
  esac
done

# ---------- 准备 ./logs bind mount 目标(v2.86-PR13.4)----------
# docker-compose.yml 把容器内 /var/log bind 到宿主 ./logs。
# 如果宿主目录不存在或权限不对,容器启动会 mount 失败 / 容器内进程写不进去。
# 必须在 docker compose up 之前建好。
if [ ! -d ./logs ]; then
  log "creating ./logs bind mount target (chmod 1777)..."
  mkdir -p ./logs
  chmod 1777 ./logs
  ok "./logs created (chmod 1777, sticky bit so different containers can't delete each other's logs)"
else
  # 已存在:只校验权限,避免破坏用户已有日志
  if [ "$(stat -c '%a' ./logs 2>/dev/null)" != "1777" ]; then
    warn "./logs exists with mode $(stat -c '%a' ./logs), recommend chmod 1777 ./logs"
  fi
fi

# ---------- docker compose up ----------
log "running: docker compose up -d"
exec docker compose up -d "$@"