#!/usr/bin/env bash
# auto-network.sh - 自动探测宿主网络环境,生成 docker-compose.override.yml
#
# 设计见 docs/design.md §18.2 / architecture §10
#
# 为什么需要这个脚本:
#   docker compose 启动前就要决定 network_mode (host/bridge/ipvlan),
#   而 entrypoint.sh 在容器内运行,无法热切换 network_mode。
#   所以探测逻辑必须在 docker compose 启动前(宿主上)跑一次。
#
# 业界标准做法:
#   - 生成 docker-compose.override.yml,docker compose 会自动合并
#   - 用户原本怎么跑(docker compose up -d)不用改,只是推荐用 ./scripts/up.sh
#
# 探测规则 (§18.2):
#   1. 用户显式传 IKEV2_NETWORK_MODE=host|bridge|ipvlan → 完全尊重用户选择
#   2. 否则按优先级探测:
#      a. OUT_IF 上有 2000::/3 公网 IPv6 → host      (直绑宿主网卡,推荐)
#      b. OUT_IF 上有公网 IPv4          → bridge+libipsec (端口映射)
#      c. 都没有                         → bridge+ipvlan   (用 ipvlan 网络,自动建)
#
# ipvlan 处理:
#   - 仅在选 bridge+ipvlan 时尝试 docker network create -d ipvlan (--parent OUT_IF, --subnet 自动算)
#   - 已存在 → 跳过 (幂等)
#   - 创建失败 → 退回 bridge+libipsec (端口映射),打 WARN 提示用户手工处理

set -euo pipefail

# =====================================================================
# §0. 颜色 / 日志
# =====================================================================
if [ -t 1 ]; then
  RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
else
  RED=''; GREEN=''; YELLOW=''; BLUE=''; NC=''
fi
log()  { echo -e "${BLUE}[auto-network]${NC} $*" >&2; }
ok()   { echo -e "${GREEN}[auto-network] ✓${NC} $*" >&2; }
warn() { echo -e "${YELLOW}[auto-network] !${NC} $*" >&2; }
die()  { echo -e "${RED}[auto-network] FATAL:${NC} $*" >&2; exit 1; }

# =====================================================================
# §1. 路径 / 默认值
# =====================================================================
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

if [ -f "${PROJECT_DIR}/.env" ]; then
  set -a
  # shellcheck disable=SC1091
  . "${PROJECT_DIR}/.env"
  set +a
  log "loaded .env"
fi

# 默认值 (跟 docker-compose.yml 保持一致)
IKEV2_OUT_IF="${IKEV2_OUT_IF:-}"
IKEV2_NETWORK_MODE="${IKEV2_NETWORK_MODE:-auto}"
IKEV2_IPV6_ONLY="${IKEV2_IPV6_ONLY:-true}"

OVERRIDE_FILE="${PROJECT_DIR}/docker-compose.override.yml"

# 全局探测结果 (所有函数共享)
OUT_IF_DETECTED=""
PUBLIC_V6=""
PUBLIC_V4=""
HAS_PUBLIC_V6=false
HAS_PUBLIC_V4=false
SELECTED_MODE=""
SELECTED_REASON=""
IPV6_SUBNET=""
IPV4_SUBNET=""

# =====================================================================
# §2. 函数定义 (前置,主流程在 §3+ 调用)
# =====================================================================

# 探测对外网卡
detect_out_if() {
  # 1. 用户传了 → 验证存在
  if [ -n "$IKEV2_OUT_IF" ]; then
    if ip link show "$IKEV2_OUT_IF" >/dev/null 2>&1; then
      OUT_IF_DETECTED="$IKEV2_OUT_IF"
      return 0
    fi
    warn "IKEV2_OUT_IF=$IKEV2_OUT_IF not found, falling back to auto-detect"
  fi

  # 2. IPv4 默认路由 → 接口名
  local v4_if
  v4_if=$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')
  if [ -n "$v4_if" ]; then
    OUT_IF_DETECTED="$v4_if"
    return 0
  fi

  # 3. IPv6 默认路由 → 接口名
  local v6_if
  v6_if=$(ip -6 route show default 2>/dev/null | awk '/default/ {print $5; exit}')
  if [ -n "$v6_if" ]; then
    OUT_IF_DETECTED="$v6_if"
    return 0
  fi

  # 4. 第一个非 lo 的 UP 接口
  OUT_IF_DETECTED=$(ip -o link show up 2>/dev/null | awk -F': ' '!/lo/ {print $2; exit}' | tr -d ' ')
  if [ -z "$OUT_IF_DETECTED" ]; then
    die "cannot detect outbound interface (set IKEV2_OUT_IF explicitly)"
  fi
}

# 检测公网 IPv6 (2000::/3)
detect_public_v6() {
  # 用 awk 实现 (避免 case+return 在子 shell 里让 set -e 误判)
  # || true 防止 awk 没匹配时 exit 1 触发 set -e 退出
  PUBLIC_V6=$(ip -6 -o addr show dev "$OUT_IF_DETECTED" 2>/dev/null \
    | awk '/inet6/ {print $4}' | cut -d/ -f1 \
    | awk '/^2[0-9a-fA-F][0-9a-fA-F][0-9a-fA-F]:/ { print; exit }' \
    ) || true
  [ -n "$PUBLIC_V6" ] && HAS_PUBLIC_V6=true || true
}

# 检测公网 IPv4 (非 RFC1918)
detect_public_v4() {
  # 用 awk 实现 (避免 grep 在没匹配时 return 1 触发 set -e)
  PUBLIC_V4=$(ip -4 addr show dev "$OUT_IF_DETECTED" 2>/dev/null \
    | awk '/inet / {print $2}' | cut -d/ -f1 \
    | awk '
        /^(127\.|0\.|169\.254\.|10\.|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.)/ { next }
        { print; exit }
      ') || true
  [ -n "$PUBLIC_V4" ] && HAS_PUBLIC_V4=true || true
}

# 取 OUT_IF 上 2000::/3 段的 /64 子网前缀作为 ipvlan 子网
get_out_if_ipv6_subnet() {
  ip -6 -o addr show dev "$OUT_IF_DETECTED" 2>/dev/null \
    | awk '/inet6/ {print $4}' \
    | awk '/^2[0-9a-fA-F][0-9a-fA-F][0-9a-fA-F]:/ { print; exit }' \
    || true
}

# 取 OUT_IF 上第一个 IPv4 的 /24 子网 (家用场景足够)
get_out_if_ipv4_subnet() {
  ip -4 -o addr show dev "$OUT_IF_DETECTED" 2>/dev/null \
    | awk '/inet / {print $4}' | head -n1
}

# 自动创建 ipvlan 网络 (幂等)
ensure_ipvlan() {
  local parent_if="$1"
  local net_name="ikev2-ipvlan"

  if docker network inspect "$net_name" >/dev/null 2>&1; then
    ok "ipvlan network '${net_name}' already exists (skip create)"
    IPV6_SUBNET=$(get_out_if_ipv6_subnet || true)
    IPV4_SUBNET=$(get_out_if_ipv4_subnet || true)
    return 0
  fi

  log "creating ipvlan network '${net_name}' (parent=${parent_if})..."
  IPV6_SUBNET=$(get_out_if_ipv6_subnet || true)
  IPV4_SUBNET=$(get_out_if_ipv4_subnet || true)

  local args=(-d ipvlan -o parent="$parent_if")
  if [ -n "$IPV6_SUBNET" ]; then
    local v6_gw="${IPV6_SUBNET%/*}1"
    args+=(--subnet="$IPV6_SUBNET" --gateway="$v6_gw")
  fi
  if [ -n "$IPV4_SUBNET" ]; then
    local v4_gw="${IPV4_SUBNET%.*}.1"
    args+=(--subnet="$IPV4_SUBNET" --gateway="$v4_gw")
  fi

  if docker network create "${args[@]}" "$net_name" >/dev/null 2>&1; then
    ok "ipvlan network '${net_name}' created"
  else
    warn "docker network create failed; will fall back to bridge+libipsec"
    SELECTED_MODE="bridge"
    SELECTED_REASON="ipvlan create failed, fallback to bridge"
  fi
}

# 生成 docker-compose.override.yml
write_override() {
  local mode="$1"

  case "$mode" in
    host)
      cat > "$OVERRIDE_FILE" <<'EOF'
# docker-compose.override.yml (auto-generated by scripts/auto-network.sh)
# network_mode: host (auto-detected, direct bind to host NIC)
#
# Hand-edit? Re-run scripts/auto-network.sh to regenerate.
services:
  ikev2-panel:
    network_mode: host
EOF
      ;;
    bridge)
      cat > "$OVERRIDE_FILE" <<'EOF'
# docker-compose.override.yml (auto-generated by scripts/auto-network.sh)
# network_mode: bridge + libipsec (port-mapped UDP 500/4500)
#
# Hand-edit? Re-run scripts/auto-network.sh to regenerate.
services:
  ikev2-panel:
    network_mode: bridge
    ports:
      - "500:500/udp"     # IKE
      - "4500:4500/udp"   # IKE + NAT-T (libipsec ESP-in-UDP 封装)
      - "8443:8443/tcp"   # 面板
EOF
      ;;
    ipvlan)
      # ipvlan 模式下容器直接拿到公网 IP,不需要端口映射
      {
        echo "# docker-compose.override.yml (auto-generated by scripts/auto-network.sh)"
        echo "# network_mode: ipvlan (L2 直通宿主网卡,容器拿到公网 IP)"
        echo "#"
        echo "# Hand-edit? Re-run scripts/auto-network.sh to regenerate."
        echo "services:"
        echo "  ikev2-panel:"
        echo "    networks:"
        echo "      ikev2-ipvlan:"
        if [ -n "$PUBLIC_V6" ]; then
          echo "        ipv6_address: ${PUBLIC_V6}"
        fi
        if [ -n "$PUBLIC_V4" ]; then
          echo "        ipv4_address: ${PUBLIC_V4}"
        fi
        echo ""
        echo "networks:"
        echo "  ikev2-ipvlan:"
        echo "    external: true"
      } > "$OVERRIDE_FILE"
      ;;
  esac
}

# =====================================================================
# §3. 主流程
# =====================================================================

# 3.1 探测基础信息 (OUT_IF / 公网 IP)
detect_out_if
log "outbound interface: ${OUT_IF_DETECTED}"
detect_public_v6
detect_public_v4
[ "$HAS_PUBLIC_V6" = "true" ] && log "public IPv6 detected: ${PUBLIC_V6}"
[ "$HAS_PUBLIC_V4" = "true" ] && log "public IPv4 detected: ${PUBLIC_V4}"

# 3.2 用户显式指定 → 直接尊重 (host/bridge 直接写 override,ipvlan 走探测建网络)
case "$IKEV2_NETWORK_MODE" in
  host|bridge)
    SELECTED_MODE="$IKEV2_NETWORK_MODE"
    SELECTED_REASON="user-specified"
    log "user specified network mode: ${IKEV2_NETWORK_MODE} (skip auto-detect)"
    write_override "$SELECTED_MODE"
    ok "wrote override for mode=${SELECTED_MODE}: ${OVERRIDE_FILE}"
    exit 0
    ;;
  ipvlan)
    SELECTED_MODE="ipvlan"
    SELECTED_REASON="user-specified"
    log "user specified network mode: ipvlan (will create ipvlan network)"
    ensure_ipvlan "$OUT_IF_DETECTED"
    write_override "$SELECTED_MODE"
    ok "wrote override for mode=${SELECTED_MODE}: ${OVERRIDE_FILE}"
    exit 0
    ;;
esac

# 3.3 auto 模式:按 §18.2 探测规则选
log "auto-detect mode (IKEV2_NETWORK_MODE=auto)"
if [ "$HAS_PUBLIC_V6" = "true" ]; then
  SELECTED_MODE="host"
  SELECTED_REASON="OUT_IF has public IPv6 (2000::/3)"
elif [ "$HAS_PUBLIC_V4" = "true" ]; then
  SELECTED_MODE="bridge"
  SELECTED_REASON="OUT_IF has public IPv4 (no public IPv6)"
else
  SELECTED_MODE="ipvlan"
  SELECTED_REASON="OUT_IF has no public IP (will need ipvlan network for public access)"
fi
ok "selected network mode: ${SELECTED_MODE} (${SELECTED_REASON})"

# 3.4 ipvlan 模式:自动建网络
[ "$SELECTED_MODE" = "ipvlan" ] && ensure_ipvlan "$OUT_IF_DETECTED"

# 3.5 生成 override
write_override "$SELECTED_MODE"
ok "wrote ${OVERRIDE_FILE}"
echo
echo "Selected mode:    ${SELECTED_MODE}"
echo "Reason:           ${SELECTED_REASON}"
echo "Outbound if:      ${OUT_IF_DETECTED}"
echo "Public IPv6:      ${PUBLIC_V6:-<none>}"
echo "Public IPv4:      ${PUBLIC_V4:-<none>}"
echo
log "next: docker compose up -d   (or ./scripts/up.sh)"
