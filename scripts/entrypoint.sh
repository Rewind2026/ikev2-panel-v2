#!/bin/bash
# 容器入口：环境校验 → 网卡探测 → forwarding → MASQUERADE →
#           swanctl.conf 占位符替换 → charon → swanctl 首次加载 → exec Go
# 设计见 docs/design.md §1.5 + §8.4 + §11 (v2-79 自动探测)
#
# v2-79 重大改动：§0 自动探测链 + 所有硬编码可选
#   - 优先用环境变量；没传就自动探测；自动探测失败才报 FATAL
#   - 用户部署时**可以什么都不传**（只要宿主有公网 IPv6 + 默认路由）
#
# 退出码：
#   0  正常
#   10 IPv6 公网地址不存在（IPv6_ONLY=true 时）
#   11 forwarding 启用失败
#   12 charon 启动失败
#   13 swanctl --load-all 失败（多次重试后仍失败）
#   14 探测不到对外网卡
#   15 iptables MASQUERADE 添加失败
#   16 swanctl.conf 占位符替换失败
set -euo pipefail

LOG_PREFIX="[entrypoint]"

# v2.86-PR12.16:全面拥抱 nftables,不再用 iptables-nft 兼容壳
#   - 自定义表 ikev2 (ip) + ikev26 (ip6):docker daemon 的 iptables-restore 不会碰
#   - nft -f file.nft 一次原子提交,无 -I FORWARD N 位置坑
#   - 实测 docker daemon 重写 filter/nat 表 → 我们的 ikev2 表完全不受影响
# PR12.13 用 iptables-nft 写法虽然工作(7832/7710 packets),但仍混在 daemon 的
# filter/nat 表里,daemon 二次 iptables-restore 会清掉我们 -I FORWARD 1 的规则。
# PR12.14 试切 iptables-legacy → 双表分裂。PR12.16 切自定义 nft 表 → 优雅解决。
IPTABLES_CMD="${IPTABLES_CMD:-iptables}"
IP6TABLES_CMD="${IP6TABLES_CMD:-ip6tables}"
NFT_CMD="${NFT_CMD:-nft}"
NFT_TEMPLATE="/etc/ikev2-panel/scripts/ikev2.nft.template"

# ---------- 0. 加载环境变量默认值（v2-79：所有硬编码全部可选）----------
IKEV2_IPV6_ONLY="${IKEV2_IPV6_ONLY:-true}"
IKEV2_DATA_DIR="${IKEV2_DATA_DIR:-/data}"
IKEV2_OUT_IF="${IKEV2_OUT_IF:-}"                   # 留空 → §0 自动探测
IKEV2_SERVER_ADDR_V6="${IKEV2_SERVER_ADDR_V6:-}"   # 留空 → §1 自动探测
IKEV2_SERVER_ADDR_V4="${IKEV2_SERVER_ADDR_V4:-}"   # 留空 → §1 自动探测
IKEV2_SERVER_CN="${IKEV2_SERVER_CN:-}"             # 仅 LE 模式必填
IKEV2_NETWORK_MODE="${IKEV2_NETWORK_MODE:-auto}"   # v2-79 新增：auto/host/bridge/ipvlan
IKEV2_VPN_SUBNET="${IKEV2_VPN_SUBNET:-auto}"       # v2-79 新增：auto / 10.10.0.0/24 等

# v2-76：改回 EAP-MSCHAPv2 模式，per-user 密码由 Go 进程写到 /etc/swanctl/conf.d/。
# 容器层不需要任何共享密钥环境变量。

# ---------- §-1. v2.86-PR12.5:面板运行时证书配置（优先级最高）----------
# 背景：v2-79 设计里 CertMode / Domain / ServerCN / ACMEEmail 都只能从 env 读,
# 面板 UI 改完要重启 + 改 env。v2.86-PR12.5 引入 /data/panel-state/cert.conf,
# entrypoint 启动时优先从这里读,Go 进程也读(Go config.Load 同样的优先级链)。
#
# 优先级链(高 → 低):
#   1. /data/panel-state/cert.conf (面板 UI 填的,运行时改,需重启容器)
#   2. IKEV2_CERT_MODE / IKEV2_DOMAIN / IKEV2_SERVER_CN / IKEV2_ACME_EMAIL env
#
# 不强制要求 cert.conf 存在:文件不存在 → 静默降级到 env(用户没在面板改过,正常场景)。
# 文件存在但 JSON 损坏 → WARN + 降级到 env(避免启动失败)。
CERT_CONF="/data/panel-state/cert.conf"
if [ -f "$CERT_CONF" ]; then
  # 用 grep + sed 简单提取(镜像没装 jq)。字段顺序不重要。
  cert_mode_conf=$(grep -E '"cert_mode"[[:space:]]*:' "$CERT_CONF" | sed -nE 's/.*"cert_mode"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/p' | head -1)
  domain_conf=$(grep -E '"domain"[[:space:]]*:' "$CERT_CONF" | sed -nE 's/.*"domain"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/p' | head -1)
  cn_conf=$(grep -E '"server_cn"[[:space:]]*:' "$CERT_CONF" | sed -nE 's/.*"server_cn"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/p' | head -1)
  email_conf=$(grep -E '"acme_email"[[:space:]]*:' "$CERT_CONF" | sed -nE 's/.*"acme_email"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/p' | head -1)

  # 用覆盖赋值的方式让 cert.conf 优先 env
  [ -n "$cert_mode_conf" ] && IKEV2_CERT_MODE="$cert_mode_conf"
  [ -n "$domain_conf" ] && IKEV2_DOMAIN="$domain_conf"
  [ -n "$cn_conf" ] && IKEV2_SERVER_CN="$cn_conf"
  [ -n "$email_conf" ] && IKEV2_ACME_EMAIL="$email_conf"
  echo "${LOG_PREFIX} [cert.conf] applied: cert_mode=${IKEV2_CERT_MODE} domain=${IKEV2_DOMAIN:-<empty>} server_cn=${IKEV2_SERVER_CN:-<empty>}"
fi

# ====================================================================
# §0 自动探测链（v2-79）
# 任何"用户环境相关"的参数都不应该写死。探测顺序：环境变量 → 自动探测 → FATAL
# ====================================================================

# ---------- §0.1 探测对外网卡（优先用 IKEV2_OUT_IF）----------
detect_out_if() {
  # 1. 用户传了 → 验证存在即可
  if [ -n "$IKEV2_OUT_IF" ]; then
    if ip link show "$IKEV2_OUT_IF" >/dev/null 2>&1; then
      echo "$IKEV2_OUT_IF"
      return 0
    fi
    echo "${LOG_PREFIX} WARN: IKEV2_OUT_IF=$IKEV2_OUT_IF not found, falling back to auto-detect" >&2
  fi

  # 2. IPv4 默认路由 → 接口名（最稳）
  local v4_if
  v4_if=$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')
  if [ -n "$v4_if" ]; then
    echo "$v4_if"
    return 0
  fi

  # 3. IPv6 默认路由 → 接口名
  local v6_if
  v6_if=$(ip -6 route show default 2>/dev/null | awk '/default/ {print $5; exit}')
  if [ -n "$v6_if" ]; then
    echo "$v6_if"
    return 0
  fi

  # 4. 所有 UP 接口中第一个非 lo 的（last resort）
  ip -o link show up 2>/dev/null \
    | awk -F': ' '!/lo/ {print $2; exit}' \
    | tr -d ' '
}

OUT_IF="$(detect_out_if || true)"
if [ -z "$OUT_IF" ]; then
  echo "${LOG_PREFIX} FATAL: cannot detect outbound interface" >&2
  echo "${LOG_PREFIX} Set IKEV2_OUT_IF=eth0 (or your actual interface name)" >&2
  exit 14
fi
echo "${LOG_PREFIX} [auto-detect] Outbound interface: ${OUT_IF}"

# ---------- §0.2 探测 IPv6 公网地址（2000::/3，避开 ULA/Link-local）----------
detect_public_ipv6() {
  # /proc/net/if_inet6 是最稳定的方式（不依赖 iproute2）
  awk '$4=="01" || $4=="00" {print $6}' /proc/net/if_inet6 2>/dev/null | while read -r dev; do
    local hex
    hex=$(awk -v d="$dev" '$6==d && ($4=="01" || $4=="00") {print $1; exit}' /proc/net/if_inet6 2>/dev/null)
    [ -z "$hex" ] && continue
    # 32 hex chars → IPv6
    local ip
    ip=$(echo "$hex" | sed 's/\(....\)/\1:/g; s/:$//')
    # 只保留 2000::/3
    case "$ip" in
      2[0-9a-fA-F][0-9a-fA-F][0-9a-fA-F]:*) echo "$ip"; return 0 ;;
    esac
  done | head -n1
}

if [ -z "$IKEV2_SERVER_ADDR_V6" ] && [ "$IKEV2_IPV6_ONLY" = "true" ]; then
  IPV6_ADDR_DETECTED="$(detect_public_ipv6 || true)"
  if [ -n "$IPV6_ADDR_DETECTED" ]; then
    IKEV2_SERVER_ADDR_V6="$IPV6_ADDR_DETECTED"
    echo "${LOG_PREFIX} [auto-detect] Public IPv6: ${IKEV2_SERVER_ADDR_V6}"
  fi
fi

# ---------- §0.3 探测 IPv4 公网地址（可选）----------
detect_public_ipv4() {
  # 优先取 OUT_IF 上的第一个 IPv4
  ip -4 addr show dev "$OUT_IF" 2>/dev/null \
    | awk '/inet / {print $2}' | cut -d/ -f1 \
    | grep -vE '^(127\.|0\.|169\.254\.|10\.|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.)' \
    | head -n1
}

if [ -z "$IKEV2_SERVER_ADDR_V4" ] && [ "$IKEV2_IPV6_ONLY" = "false" ]; then
  IPV4_ADDR_DETECTED="$(detect_public_ipv4 || true)"
  if [ -n "$IPV4_ADDR_DETECTED" ]; then
    IKEV2_SERVER_ADDR_V4="$IPV4_ADDR_DETECTED"
    echo "${LOG_PREFIX} [auto-detect] Public IPv4: ${IKEV2_SERVER_ADDR_V4}"
  fi
fi

# ---------- §0.4 探测 IPv4 默认网关 ----------
detect_v4_gateway() {
  ip -4 route show default 2>/dev/null | awk '/default/ {print $3; exit}'
}

V4_GW="$(detect_v4_gateway || true)"
[ -n "$V4_GW" ] && echo "${LOG_PREFIX} [auto-detect] IPv4 gateway: ${V4_GW}" || echo "${LOG_PREFIX} [auto-detect] no IPv4 gateway (IPv6-only host?)"

# ---------- §0.5 探测 LAN 网段（用来检测 VPN 虚拟 IP 段是否冲突）----------
detect_lan_subnets() {
  # 收集所有 IPv4 地址（排除 loopback / docker bridge），聚合成网段
  ip -4 -o addr show 2>/dev/null \
    | awk '$2 != "lo" {print $4}' \
    | grep -vE '^(127\.|0\.|169\.254\.)' \
    | cut -d/ -f1 \
    | while read -r ip; do
        # 简单聚合 /24（家用场景足够）
        echo "${ip%.*}.0/24"
      done | sort -u
}

LAN_SUBNETS="$(detect_lan_subnets || true)"
[ -n "$LAN_SUBNETS" ] && echo "${LOG_PREFIX} [auto-detect] LAN subnets: ${LAN_SUBNETS}"

# ---------- §0.5b 探测 IPv6 ULA/ULA-fallsite 防火墙段（v2-80）----------
# 背景：swanctl.conf pools 段 addrs 之前写死 fd00:1::/64（ULA 段）。
# 极少数用户家里的 LAN 正好就是 fd00:1::/64，撞了会路由不到。
# 现在按 IPv4 同样的"候选顺序"选第一个不冲突的 ULA 段。
#
# 检测范围：
#   - fd00::/8 全段（RFC 4193 ULA，CentOS/RHEL/openWRT 默认 IPv6 LAN 段）
#   - 顺便扫 fe80::/10 link-local 不算 LAN（每个接口自动有），忽略
#   - ::/3 全段不扫（公网段不在 LAN 上）
detect_lan_v6_ulas() {
  ip -6 -o addr show 2>/dev/null \
    | awk '$2 != "lo" {print $4}' \
    | grep -iE '^fd[0-9a-f]{2}:' \
    | cut -d/ -f1 \
    | while read -r ip; do
        # ULA 第三段（48-bit 中前 16 是 Global ID,后 32 是 Subnet ID）
        # 我们选 /64 prefix 的前 48 位作为冲突比较单位（避免 Subnet ID 误判）
        # 例如 fd00:1:2:3::1/64 → fd00:1::/48
        # 例如 fd00:abcd::/64 → fd00:abcd::/48
        echo "${ip%%:*:*:*}::/48"
      done | sort -u
}

LAN_V6_ULAS="$(detect_lan_v6_ulas || true)"
[ -n "$LAN_V6_ULAS" ] && echo "${LOG_PREFIX} [auto-detect] LAN IPv6 ULAs: ${LAN_V6_ULAS}"

# ---------- §0.6 自动选择不冲突的 VPN 虚拟 IP 段 ----------
# 候选顺序：10.10.0.0/24 → 10.13.0.0/24 → 10.17.0.0/24 → 10.42.0.0/24 → 10.66.0.0/24
# 选第一个不与 LAN_SUBNETS 重叠的
VPN_SUBNET_CANDIDATES=("10.10.0.0/24" "10.13.0.0/24" "10.17.0.0/24" "10.42.0.0/24" "10.66.0.0/24")

if [ "$IKEV2_VPN_SUBNET" = "auto" ]; then
  IKEV2_VPN_SUBNET=""
  for cand in "${VPN_SUBNET_CANDIDATES[@]}"; do
    # cand / LAN_SUBNETS 是否重叠？这里只做简单匹配（家用 LAN 都是 /24）
    cand_prefix="${cand%.*}"
    conflict=0
    for lan in $LAN_SUBNETS; do
      lan_prefix="${lan%.*}"
      if [ "$cand_prefix" = "$lan_prefix" ]; then
        conflict=1
        break
      fi
    done
    if [ "$conflict" = "0" ]; then
      IKEV2_VPN_SUBNET="$cand"
      break
    fi
  done
  if [ -z "$IKEV2_VPN_SUBNET" ]; then
    echo "${LOG_PREFIX} WARN: all VPN subnet candidates conflict with LAN, falling back to 10.10.0.0/24" >&2
    IKEV2_VPN_SUBNET="10.10.0.0/24"
  fi
  echo "${LOG_PREFIX} [auto-detect] VPN subnet: ${IKEV2_VPN_SUBNET} (LAN: ${LAN_SUBNETS:-none})"
else
  echo "${LOG_PREFIX} [user-set] VPN subnet: ${IKEV2_VPN_SUBNET}"
fi

# 把 subnet 转成首地址（x.x.x.1 给 server / virtual IP pool 第一位）
VPN_SUBNET_BASE="${IKEV2_VPN_SUBNET%.*}.0"
VPN_SERVER_VIP="${IKEV2_VPN_SUBNET%.*}.1"   # server 的 ipsec0 地址（强Swan 习惯）

# ---------- §0.6b 自动选择不冲突的 IPv6 ULA VPN pool 段（v2-80）----------
# 候选顺序：fd00:1::/64 → fd00:2::/64 → fd00:3::/64 → fd00:10::/64 → fd00:20::/64
# 比较单位是 /48（Global ID），避免 Subnet ID 干扰。
# 用户可以传 IKEV2_VPN_SUBNET_V6 指定（默认 auto）。
IKEV2_VPN_SUBNET_V6="${IKEV2_VPN_SUBNET_V6:-auto}"
VPN_V6_CANDIDATES=("fd00:1::/64" "fd00:2::/64" "fd00:3::/64" "fd00:10::/64" "fd00:20::/64")

if [ "$IKEV2_VPN_SUBNET_V6" = "auto" ]; then
  IKEV2_VPN_SUBNET_V6=""
  for cand in "${VPN_V6_CANDIDATES[@]}"; do
    # 候选的 /48（取前 3 段，fd00:1::/48）
    cand_prefix="$(echo "$cand" | sed -E 's|^([0-9a-f]+:[0-9a-f]+:[0-9a-f]+).*|\1::/48|')"
    conflict=0
    for lan in $LAN_V6_ULAS; do
      if [ "$cand_prefix" = "$lan" ]; then
        conflict=1
        break
      fi
    done
    if [ "$conflict" = "0" ]; then
      IKEV2_VPN_SUBNET_V6="$cand"
      break
    fi
  done
  if [ -z "$IKEV2_VPN_SUBNET_V6" ]; then
    echo "${LOG_PREFIX} WARN: all VPN v6 candidates conflict with LAN, falling back to fd00:1::/64" >&2
    IKEV2_VPN_SUBNET_V6="fd00:1::/64"
  fi
  echo "${LOG_PREFIX} [auto-detect] VPN v6 subnet: ${IKEV2_VPN_SUBNET_V6} (LAN ULA: ${LAN_V6_ULAS:-none})"
else
  echo "${LOG_PREFIX} [user-set] VPN v6 subnet: ${IKEV2_VPN_SUBNET_V6}"
fi

# ---------- §1. IPv6 公网地址校验（仅 IPv6_ONLY=true 时硬校验）----------
# v2-79：先看用户/自动探测的结果；都没有才 FATAL
IPV6_ADDR="$IKEV2_SERVER_ADDR_V6"
if [ "$IKEV2_IPV6_ONLY" = "true" ]; then
  if [ -z "$IPV6_ADDR" ]; then
    # 最后再试一次主动探测（覆盖 §0.2 失败场景）
    IPV6_ADDR="$(detect_public_ipv6 || true)"
    IKEV2_SERVER_ADDR_V6="$IPV6_ADDR"
  fi

  if [ -z "${IPV6_ADDR:-}" ]; then
    echo "${LOG_PREFIX} FATAL: no global IPv6 address found" >&2
    echo "${LOG_PREFIX} This project REQUIRES a public IPv6 address when IKEV2_IPV6_ONLY=true" >&2
    echo "${LOG_PREFIX} Configure IPv6 on the host network first, or set IKEV2_IPV6_ONLY=false" >&2
    exit 10
  fi
  echo "${LOG_PREFIX} Using IPv6 address: ${IPV6_ADDR}"
fi

# v2-79：把自动探测结果导出，供 §5 占位符替换使用
export IKEV2_SERVER_ADDR_V6 IKEV2_SERVER_ADDR_V4 IKEV2_VPN_SUBNET VPN_SUBNET_BASE VPN_SERVER_VIP IKEV2_VPN_SUBNET_V6

# ---------- §0.7 网络模式校验（v2-80）----------
# 背景：
#   docker compose 启动前就要决定 network_mode (host/bridge/ipvlan),
#   entrypoint 在容器内运行无法热切换。
#   所以探测必须由 scripts/auto-network.sh 在宿主上跑一遍,
#   生成 docker-compose.override.yml,再 docker compose up -d。
#
# entrypoint 这层只能做"校验 + 提示":
#   - 检测实际拿到的网络模式 (host network 下 hostname 等于宿主 hostname,
#     bridge 下 hostname 是 container-id)
#   - 跟用户声明的 IKEV2_NETWORK_MODE 不匹配 → WARN 提示跑 ./scripts/up.sh
#   - 这不是 FATAL:老用户直接 docker compose up -d 也跑得起来 (默认 host)
detect_network_mode() {
  local container_hostname
  container_hostname=$(hostname 2>/dev/null || echo "")
  local host_hostname
  host_hostname=$(cat /etc/hostname 2>/dev/null || echo "")

  # 简单启发:host 网络下 hostname == 宿主 hostname
  if [ -n "$container_hostname" ] && [ "$container_hostname" = "$host_hostname" ]; then
    echo "host"
    return 0
  fi
  # bridge / ipvlan 模式:hostname 是容器 ID (12 字符 hex)
  if echo "$container_hostname" | grep -qE '^[0-9a-f]{12}$'; then
    echo "bridge"
    return 0
  fi
  echo "unknown"
}

ACTUAL_MODE="$(detect_network_mode)"
if [ "$IKEV2_NETWORK_MODE" = "auto" ]; then
  echo "${LOG_PREFIX} [auto-detect] network mode: ${ACTUAL_MODE} (host: docker-compose.yml override = auto-generated by ./scripts/up.sh)"
else
  if [ "$ACTUAL_MODE" != "unknown" ] && [ "$ACTUAL_MODE" != "$IKEV2_NETWORK_MODE" ]; then
    echo "${LOG_PREFIX} WARN: declared IKEV2_NETWORK_MODE=${IKEV2_NETWORK_MODE} but actual=${ACTUAL_MODE}" >&2
    echo "${LOG_PREFIX}   说明:docker-compose.yml 默认 host 网络;override 由 ./scripts/up.sh 生成" >&2
    echo "${LOG_PREFIX}   解决:跑 ./scripts/up.sh 让脚本自动重生成 override (会清掉旧的 docker-compose.override.yml)" >&2
  else
    echo "${LOG_PREFIX} [network-mode] ${ACTUAL_MODE} (declared=${IKEV2_NETWORK_MODE})"
  fi
fi
# 把 actual 模式也 export,下游 §3 / §4 可以根据 mode 决定是否需要 sysctls 等
export IKEV2_ACTUAL_NETWORK_MODE="$ACTUAL_MODE"

# ---------- 3. 检查 IP forwarding（v2-79 三层兜底：compose sysctls → entrypoint 自愈 → FATAL）----------
#  必需：容器作为 VPN 路由器，VPN 客户端流量要从容器转发到公网。
#  IPv6：net.ipv6.conf.all.forwarding=1
#  IPv4：net.ipv4.ip_forward=1
#
# 三层兜底（设计见 docs §15.x "forwarding 自愈"）：
#   1) docker-compose.yml 的 sysctls: —— docker daemon 起容器时改宿主内核
#      （依赖 docker daemon 权限，多数发行版默认就有 CAP_SYS_ADMIN）
#   2) entrypoint 自愈 —— 上一步失败时，host 模式下 privileged: true 会让
#      容器内 /proc/sys 可写，直接 echo 1 即可（host 模式下容器和宿主共享 net ns）
#   3) FATAL —— 自愈也失败（说明宿主机 sysctl 是真 read-only），报清晰错误
#
# 镜像里没有 sysctl 二进制（debian-slim），所以这里用 echo 直写 /proc/sys。
# 区分 host vs bridge：bridge 模式容器内 /proc/sys 是 RO，写不进去——失败正常，
# 不影响功能（bridge 模式下根本不需要 forwarding key）。
#
# v2-79.2 增强：自愈成功后**同时把值持久化到宿主 /etc/sysctl.d/99-ikev2.conf**。
# 实现：docker-compose.yml 把宿主 /etc/sysctl.d bind mount 到 /host-sysctl.d（rw）。
# 这样：
#   - 当前会话立即生效（写 /proc/sys）
#   - 宿主重启 → systemd-sysctl.service 加载 /etc/sysctl.d/*.conf → 持久保留
#   - 下次部署到新机器，第一次启动也会自愈 + 持久化
# 无 99-ikev2.conf 也不要紧：自愈成功后才写，不会覆盖宿主已有配置。
persist_sysctl() {
  local key="$1" value="$2"   # key = 完整 sysctl 名（如 net.ipv6.conf.all.forwarding）
  local host_dir="/host-sysctl.d"
  local host_conf="${host_dir}/99-ikev2.conf"
  # 没挂载宿主 /etc/sysctl.d → 跳过持久化（容器内 sysctl 仍然生效，但宿主重启会丢）
  [ -d "$host_dir" ] || { echo "${LOG_PREFIX} [persist] $host_dir not mounted, skipping host-side persist"; return 0; }
  [ -w "$host_dir" ] || { echo "${LOG_PREFIX} [persist] $host_dir not writable, skipping host-side persist"; return 0; }

  # 已有配置文件 → 增量追加缺失的 key（不覆盖用户已写的内容）
  if [ -f "$host_conf" ]; then
    if grep -qE "^[[:space:]]*${key//./\\.}\b" "$host_conf"; then
      echo "${LOG_PREFIX} [persist] $host_conf already has $key entry, not touching"
      return 0
    fi
  fi

  cat >> "$host_conf" <<EOF
${key} = ${value}
EOF
  echo "${LOG_PREFIX} [persist] wrote ${key}=${value} to $host_conf (survives host reboot)"
}

enable_forwarding() {
  local key="$1"   # ipv6 | ipv4
  local path=""
  local sysctl_name=""
  [ "$key" = "ipv6" ] && path=/proc/sys/net/ipv6/conf/all/forwarding && sysctl_name="net.ipv6.conf.all.forwarding"
  [ "$key" = "ipv4" ] && path=/proc/sys/net/ipv4/ip_forward && sysctl_name="net.ipv4.ip_forward"

  # 已开 → 直接返回
  if [ -r "$path" ] && [ "$(cat "$path" 2>/dev/null)" = "1" ]; then
    return 0
  fi

  # 没开 / 不存在（系统自检）→ 尝试自愈
  if [ -w "$path" ] 2>/dev/null; then
    if echo 1 > "$path" 2>/dev/null; then
      echo "${LOG_PREFIX} [auto-heal] ${path} set to 1 (host net ns, effective on host)" >&2
      persist_sysctl "$sysctl_name" "1"
      return 0
    fi
  fi
  return 1
}

if [ "$IKEV2_IPV6_ONLY" != "true" ] || [ "$IKEV2_NETWORK_MODE" != "bridge" ]; then
  if ! enable_forwarding ipv6; then
    echo "${LOG_PREFIX} FATAL: net.ipv6.conf.all.forwarding=0 and cannot auto-enable" >&2
    echo "${LOG_PREFIX} 提示 1：sudo sysctl -w net.ipv6.conf.all.forwarding=1" >&2
    echo "${LOG_PREFIX} 提示 2：检查 docker compose 是否给了 NET_ADMIN capability（v2.86-PR11 后已不再需要 privileged）" >&2
    echo "${LOG_PREFIX} 提示 3：docker daemon 自身需要 CAP_SYS_ADMIN 写 /proc/sys/net" >&2
    exit 11
  fi
fi
if ! enable_forwarding ipv4; then
  echo "${LOG_PREFIX} FATAL: net.ipv4.ip_forward=0 and cannot auto-enable" >&2
  echo "${LOG_PREFIX} 提示 1：sudo sysctl -w net.ipv4.ip_forward=1" >&2
  echo "${LOG_PREFIX} 提示 2：检查 docker compose 是否给了 NET_ADMIN capability（v2.86-PR11 后已不再需要 privileged）" >&2
  echo "${LOG_PREFIX} 提示 3：docker daemon 自身需要 CAP_SYS_ADMIN 写 /proc/sys/net" >&2
  exit 11
fi
echo "${LOG_PREFIX} IP forwarding OK (v4=$(cat /proc/sys/net/ipv4/ip_forward 2>/dev/null), v6=$(cat /proc/sys/net/ipv6/conf/all/forwarding 2>/dev/null))"

# ---------- 3.5. 修 IPv4 出向路由 + FORWARD ACCEPT（v2-78 + v2-79 自动化）----------
# 背景：v2-76 拨号成功后客户端能拿到 IP 但无法访问公网。
# 根因（家用路由器 IPv6 出公网被防火墙挡）：
#   - iPhone 通过 IPv6 拨号，server 把客户端流量按 children{local_ts=0.0.0.0/0, ::/0}
#     劫持进 ipsec0
#   - 出向走 XFRM 路径，policy routing table 220：strongSwan 默认只放客户端 VIP 的直连路由
#     (`10.10.0.1 dev ipsec0 proto static`)，**没有 default route** → 出向包被丢
#   - 即使有 default route：docker 在 host network 模式下让 iptables FORWARD 默认 DROP，
#     没有 ACCEPT 规则 → 转发也丢
#   - 即使能转发：内网 VPN_SUBNET 客户端源 IP 在公网不可路由，需要 MASQUERADE
#   - 所以 ipv4 转发需要三件事一起做：table 220 default + FORWARD ACCEPT + MASQUERADE
#
# v2-79 改动：
#   - VPN_SUBNET 不再硬编码 10.10.0.0/24，改用 §0.6 自动选择的 IKEV2_VPN_SUBNET
#   - V4_GW 也用 §0.4 自动探测的结果（不需要重复跑 ip route）
#
# 为什么用 v4 而不是 v6：用户的家用路由器对 LAN→WAN 的 IPv6 出向有防火墙限制
# （curl -6 ipv6.google.com timeout），但 IPv4 出向正常 → 实际只能用 v4 出公网。
# 这是 v2-78 临时绕过：长期方案应该改 swanctl children 让出向只走 v4，避免 dual-stack
# 部分通的部分不通。
#
# 全部幂等：用 `-C` / `show | grep -q` 检查是否已存在，存在则跳过；
# 重启容器 / 重建镜像 都不会重复添加。
if [ "$IKEV2_IPV6_ONLY" = "true" ]; then
  if [ -z "$V4_GW" ]; then
    echo "${LOG_PREFIX} WARN: no ipv4 default route, skipping v4 forward setup" >&2
  else
    # 1. table 220 加 default route（XFRM 出向路由表）
    if ip route show table 220 2>/dev/null | grep -q '^default'; then
      echo "${LOG_PREFIX} table 220 default route already present"
    else
      ip route add default via "$V4_GW" dev "$OUT_IF" table 220 \
        && echo "${LOG_PREFIX} table 220 default route added via $V4_GW dev $OUT_IF" \
        || echo "${LOG_PREFIX} WARN: failed to add table 220 default route" >&2
    fi

    # 2. v2.86-PR12.16:nft 自定义表 ikev2 / ikev26 一次性原子提交
    #    替换原来的 iptables -I FORWARD / -t nat -A POSTROUTING 零散操作
    #    docker daemon 重写 filter/nat 表不会碰自定义表,根治 PR12.13 二次 iptables-restore 挤掉规则
    if [ -f "$NFT_TEMPLATE" ]; then
      NFT_RUNTIME="/run/ikev2-panel/ikev2.nft"
      mkdir -p /run/ikev2-panel
      sed -e "s|@OUT_IF4@|${OUT_IF}|g" \
          -e "s|@OUT_IF6@|${OUT_IF}|g" \
          -e "s|@VPN_SUBNET4@|${IKEV2_VPN_SUBNET}|g" \
          -e "s|@VPN_SUBNET6@|${IKEV2_VPN_SUBNET_V6:-fd00::/64}|g" \
          "$NFT_TEMPLATE" > "$NFT_RUNTIME"
      if $NFT_CMD -f "$NFT_RUNTIME" 2>&1; then
        echo "${LOG_PREFIX} nft ikev2/ikev26 ruleset applied (atomic via $NFT_RUNTIME)"
      else
        echo "${LOG_PREFIX} FATAL: nft -f failed" >&2
        exit 15
      fi
    else
      echo "${LOG_PREFIX} FATAL: $NFT_TEMPLATE not found" >&2
      exit 15
    fi
    # export 给 §7.9 retry 用
    export NFT_RUNTIME
  fi
fi

# ---------- 4. MASQUERADE（v2.86-PR12.16 改由 nft 自定义表 ikev2/ikev26 统一处理）----------
# 客户端 VPN 拨号后获得虚拟 IP（如 fd00::2 / 10.99.99.2），这个地址在公网不可路由。
# 出向流量的源 IP 必须是容器对外网卡地址（$OUT_IF 上的 IP），否则目标服务器回包失败。
#
# v2.86-PR12.16 重大改动：MASQUERADE 规则已在 §3.5 的 nft 自定义表 ikev2/ikev26
# 里用 `oifname @OUT_IF@ fib daddr type != local counter masquerade` 统一加好。
# 这里的 apply_masquerade / HAS_IPV4_PUBLIC / 各种分支全部废弃,只保留 family 检测
# 写到 /run/ikev2-panel/family.env (供 ikev2-updown 用)。
#
# 不再 MASQUERADE 内网/loopback/链路本地地址 → 由 nft `fib daddr type != local` 处理,
# 比 iptables `! --dst-type LOCAL` 更严格(fib daddr 查 fib 路由表,包括 throw/blackhole)。

# 检测容器内是否有公网 IPv4 地址
HAS_IPV4_PUBLIC=false
if ip -4 addr show dev "$OUT_IF" 2>/dev/null | awk '/inet / {print $2}' | grep -vE '^(127\.|0\.|169\.254\.|10\.|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.)' | head -1 | grep -q .; then
  HAS_IPV4_PUBLIC=true
fi

echo "${LOG_PREFIX} MASQUERADE delegated to nft table ikev2/ikev26 (HAS_IPV4_PUBLIC=${HAS_IPV4_PUBLIC}, IPv6_ONLY=${IKEV2_IPV6_ONLY})"

# ---------- 4.4. TCP MSS clamp（v2.86-PR12.16 改由 nft 自定义表统一处理）----------
# 背景同 PR12.13: VPN 隧道 MTU ~1400,客户端原始 MSS=1460,MTU<1420 时大包被丢。
# v2.86-PR12.16:改用 nft 的 `tcp option maxseg size set rt mtu` 在 ikev2/ikev26 表
# mangle_forward / mangle_output chain 里加。`rt mtu` 让 nft 查 fib 路由表取路径 MTU,
# 比 iptables `--clamp-mss-to-pmtu` 更准确(后者查的是 conntrack 缓存)。
#
# 关闭开关:IKEV2_DISABLE_MSS_CLAMP=true (罕见:某些内网环境 MSS=1460 反而对)
if [ "${IKEV2_DISABLE_MSS_CLAMP:-false}" = "true" ]; then
  echo "${LOG_PREFIX} MSS clamp disabled (IKEV2_DISABLE_MSS_CLAMP=true)"
else
  echo "${LOG_PREFIX} MSS clamp delegated to nft table ikev2/ikev26 (mangle_forward + mangle_output)"
fi

# ---------- 4.5. 写 family.env（v2.85-PR7:updown 脚本限速按 family 路由）----------
# 背景：
#   ikev2-updown 由 swanctl 调用，swanctl 不传业务环境变量，只传 PLUTO_*
#   所以 family（ipv4 / ipv6 / dual）必须写到文件，updown 脚本 source。
#
# 决定规则：
#   - IKEV2_FAMILY 显式传了 → 直接用（运维手动覆盖用）
#   - IKEV2_IPV6_ONLY=true → ipv6（v2 默认）
#   - IKEV2_IPV6_ONLY=false → 容器有 IPv4 又有 IPv6 → dual；只有 v4 → ipv4
#
# 写入 /run/ikev2-panel/family.env（FAMILY=<value> 一行）。
# v2.86-PR11 后 root filesystem 只读,/etc/ikev2-panel/scripts/ 必须保留
# (COPY 自镜像,renew-cert.sh / audit-retention.sh),所以 family.env 改放 /run/
# (tmpfs,运行时算,容器重启重算)。
mkdir -p /run/ikev2-panel
if [ -n "${IKEV2_FAMILY:-}" ]; then
  FAMILY="$IKEV2_FAMILY"
elif [ "$IKEV2_IPV6_ONLY" = "true" ]; then
  FAMILY="ipv6"
else
  if [ "$HAS_IPV4_PUBLIC" = "true" ] && ([ -n "$IPV6_ADDR" ] || ip -6 addr show dev "$OUT_IF" 2>/dev/null | grep -q 'inet6 .* global'); then
    FAMILY="dual"
  else
    FAMILY="ipv4"
  fi
fi
printf 'FAMILY=%s\n' "$FAMILY" > /run/ikev2-panel/family.env
chmod 644 /run/ikev2-panel/family.env
echo "${LOG_PREFIX} family.env written: FAMILY=${FAMILY}"

# ---------- 4.5b. /etc/swanctl 从模板恢复（v2.86-PR11.1）----------
# 背景：v2.86-PR11 read_only + tmpfs 挂 /etc/swanctl 后,镜像里
# /etc/swanctl/swanctl.conf + conf.d/ + x509/ private/ ... 在容器启动时被
# 空 tmpfs 覆盖。镜像里我们把模板备份到 /etc/swanctl.dist/(只读),这里挂载后
# 第一次启动时检测 + 恢复。后续证书 / 用户 conf 由 §7.5 / Go 进程写,无需再处理。
SWAN_DIST="/etc/swanctl.dist"
if [ -d "$SWAN_DIST" ]; then
  # 1) swanctl.conf（主配置）
  if [ ! -f /etc/swanctl/swanctl.conf ]; then
    cp -a "$SWAN_DIST"/swanctl.conf /etc/swanctl/swanctl.conf
    echo "${LOG_PREFIX} swanctl.conf restored from $SWAN_DIST (tmpfs overlay)"
  fi
  # 2) conf.d + 所有子目录（charon 启动要扫这些）
  for sub in conf.d x509 x509ca x509ocsp x509aa x509ac x509crl private rsa ecdsa pkcs8 pkcs12 pubkey; do
    [ -d "$SWAN_DIST/$sub" ] || continue
    mkdir -p "/etc/swanctl/$sub"
    # rsync 风格：dist 有但目标没有就补（dist 里的空目录也建）
    for f in "$SWAN_DIST/$sub"/*; do
      [ -e "$f" ] || continue
      bn="$(basename "$f")"
      [ ! -e "/etc/swanctl/$sub/$bn" ] && cp -a "$f" "/etc/swanctl/$sub/$bn"
    done
  done
fi

# v2.86-PR12.8:删除了原本的 §4.5c (acme.sh account.key 持久化 + 启动/退出同步) +
# §4.5d (trap 退出落盘) + §4.5b-bis (dnsapi 恢复)。理由:
#   - docker-compose.yml 不再 read_only,/opt/acme.sh 不再 tmpfs
#   - acme.sh 直接在镜像层 /opt/acme.sh (cp 进去的 dnsapi/ 自然暴露) 写
#   - account.json / ca/ 直接落 /data/acme-sh (命名卷,天然持久化)
#   - 容器重启 = /data 还在 = account 复用 = 不会触发 LE rate-limit
# 跟 nginx-proxy/acme-companion / mailcow 等成熟项目做法一致。

# ---------- 4.6. DDoS 防护（v2.86-PR9 修正：改用 DOCKER-USER + trap 兜底）----------
# 背景：
#   - 审计 Top-10 #10 + 2024 多起 IKE_SA_INIT flood 把家用 VPS charon 拍挂
#   - v2.86-PR9 原始方案：往 INPUT 链 -I connlimit/hashlimit
#   - **副作用**：容器 down 后宿主机 INPUT 链残留（docker daemon 不会清容器加的 INPUT 规则）
#     → restart 10 次 = 10 条死规则叠加
#
# 修正（v2.86-PR9-fix）：
#   1. 改用 DOCKER-USER 链（Docker 官方推荐 hook 点，见 [docker docs][oneuptime]）
#      - DOCKER-USER 是 docker daemon 自己建的 chain，daemon restart 时会清
#      - 容器 down → DOCKER-USER 还在但规则被 daemon 重新生成时刷掉 → **天然幂等**
#      - 不用动 INPUT/FORWARD，宿主防火墙不被污染
#   2. 加 trap EXIT/TERM/INT 兜底：即便 docker kill -9 让 trap 拿不到，
#      下次容器启动时 §4.7 cleanup-on-startup 会清 DOCKER-USER 里残留的旧规则
#
# 设计权衡：
#   - connlimit：每 IP 最大 50 并发 SA（家用 1-50 人，每人 ≤ 2 设备 = 100 并发上限）
#   - hashlimit：每分钟最多 30 次新 SA 协商
#   - 关键：connlimit / hashlimit 必须放在 ACCEPT 之前，否则无效
#     DOCKER-USER 链里 RETURN 走默认行为，DROP 走我们自己规则；放最前即可
#   - 1-50 人规模下，新 SA 速率阈值 30/min 不会误伤真实用户
#
# v2.86-PR9：默认阈值，可通过 IKEV2_CONNLIMIT_PER_IP / IKEV2_HASHLIMIT_PER_MIN 调。

apply_ddos_protection() {
  local iptables_bin="$1"
  if ! command -v "$iptables_bin" >/dev/null 2>&1; then
    echo "${LOG_PREFIX} WARN: $iptables_bin not found, skipping DDoS protection" >&2
    return 0
  fi

  # DOCKER-USER 链可能不存在（host 网络模式下 docker daemon 不一定建）
  # 不存在 → 跳过（host 网络本身没 docker 网桥,这条规则意义不大）
  if ! "$iptables_bin" -nL DOCKER-USER >/dev/null 2>&1; then
    echo "${LOG_PREFIX} $iptables_bin DOCKER-USER chain not present (host net?), skipping DDoS protection"
    return 0
  fi

  local CONNLIMIT_PER_IP="${IKEV2_CONNLIMIT_PER_IP:-50}"
  local HASHLIMIT_PER_MIN="${IKEV2_HASHLIMIT_PER_MIN:-30}"

  # 幂等：检查规则是否已存在（用 comment 锚点定位我们自己的规则）
  # 不用 -C 因为 connlimit/hashlimit 的全规则匹配会因为 comment 不同漏判
  if "$iptables_bin" -nL DOCKER-USER 2>/dev/null | grep -q "v2.86-PR9 connlimit per-IP SA cap"; then
    echo "${LOG_PREFIX} $iptables_bin connlimit (DOCKER-USER) already present"
  else
    "$iptables_bin" -I DOCKER-USER 1 -p udp --dport 500 \
      -m connlimit --connlimit-above "$CONNLIMIT_PER_IP" --connlimit-mask 32 \
      -m comment --comment "v2.86-PR9 connlimit per-IP SA cap" \
      -j DROP \
      && echo "${LOG_PREFIX} $iptables_bin connlimit (UDP/500, DOCKER-USER) added" \
      || echo "${LOG_PREFIX} WARN: failed to add $iptables_bin connlimit" >&2
  fi

  if "$iptables_bin" -nL DOCKER-USER 2>/dev/null | grep -q "v2.86-PR9 new-SA rate per IP"; then
    echo "${LOG_PREFIX} $iptables_bin hashlimit (DOCKER-USER) already present"
  else
    "$iptables_bin" -I DOCKER-USER 2 -p udp --dport 500 \
      -m hashlimit --hashlimit-above "${HASHLIMIT_PER_MIN}/min" --hashlimit-burst 10 \
      --hashlimit-mode srcip --hashlimit-name ikev2_ddos \
      -m comment --comment "v2.86-PR9 new-SA rate per IP" \
      -j DROP \
      && echo "${LOG_PREFIX} $iptables_bin hashlimit (UDP/500, DOCKER-USER) added" \
      || echo "${LOG_PREFIX} WARN: failed to add $iptables_bin hashlimit" >&2
  fi
}

if [ "${IKEV2_DISABLE_DDOS_PROTECTION:-false}" = "true" ]; then
  echo "${LOG_PREFIX} DDoS protection disabled (IKEV2_DISABLE_DDOS_PROTECTION=true)"
else
  if [ "$IKEV2_IPV6_ONLY" = "false" ]; then
    apply_ddos_protection "iptables"
  fi
  apply_ddos_protection "ip6tables"
fi

# ---------- 4.7. 启动时清理残留 + 退出时 trap 清理 ----------
# 目的:解决"容器 restart 循环 + iptables INPUT 残留"的污染问题。
#
# v2.86-PR12.16 重大改动:不再清理 iptables FORWARD / nat / mangle 链(我们的规则
# 全在自定义 nft 表 ikev2 / ikev26 里,docker daemon 不碰 → 无残留风险)。
# 只需清 §4.6 DDoS 加的 DOCKER-USER 规则(DOCKER-USER 是 daemon 管的链,会被
# daemon 重写 → 我们的 connlimit/hashlimit 会残留 → 必须显式清)。
#
# 两层防护:
#   1. **启动时清理**:清 DOCKER-USER 链里属于本项目的旧规则(v2.86-PR9 注释锚点)
#      + nft delete table ip ikev2 / ip6 ikev26(双保险,kill -9 兜底)
#      - 处理 docker kill -9 场景(trap 没机会跑,下次启动补清)
#   2. **退出时 trap**:docker stop SIGTERM / docker kill 优雅退出时清 DOCKER-USER + nft
#      - 兼顾正常 docker-compose down / restart

cleanup_ikev2_iptables_rules() {
  local iptables_bin="$1"
  if ! command -v "$iptables_bin" >/dev/null 2>&1; then
    return 0
  fi
  # 1) 清 DOCKER-USER 链里带 v2.86-PR9 锚点的规则(DDoS 防护)
  if "$iptables_bin" -nL DOCKER-USER >/dev/null 2>&1; then
    local line_nums
    line_nums=$("$iptables_bin" -nL DOCKER-USER --line-numbers 2>/dev/null \
      | awk '/v2.86-PR9 connlimit per-IP SA cap/ {print $1}' \
      | tac)
    for n in $line_nums; do
      "$iptables_bin" -D DOCKER-USER "$n" 2>/dev/null || true
    done
    line_nums=$("$iptables_bin" -nL DOCKER-USER --line-numbers 2>/dev/null \
      | awk '/v2.86-PR9 new-SA rate per IP/ {print $1}' \
      | tac)
    for n in $line_nums; do
      "$iptables_bin" -D DOCKER-USER "$n" 2>/dev/null || true
    done
  fi
}

# §4.7.1 启动时清理(kill -9 兜底)
#   - 删自定义 nft 表(双保险,kill -9 残留也清掉)
#   - 清 DOCKER-USER 残留的 §4.6 DDoS 规则
if command -v "$NFT_CMD" >/dev/null 2>&1; then
  $NFT_CMD delete table ip ikev2  2>/dev/null || true
  $NFT_CMD delete table ip6 ikev26 2>/dev/null || true
fi
cleanup_ikev2_iptables_rules "iptables" 2>/dev/null || true
cleanup_ikev2_iptables_rules "ip6tables" 2>/dev/null || true

# §4.7.2 退出时 trap(优雅退出兜底)
# 注意:trap 的命令会在脚本任意退出点触发,包括 §12 exec 后的子进程继承。
# 用函数包一层,避免 trap 影响 charon / Go 进程启动。
_on_exit_cleanup() {
  if command -v "$NFT_CMD" >/dev/null 2>&1; then
    $NFT_CMD delete table ip ikev2  2>/dev/null || true
    $NFT_CMD delete table ip6 ikev26 2>/dev/null || true
  fi
  cleanup_ikev2_iptables_rules "iptables" 2>/dev/null || true
  cleanup_ikev2_iptables_rules "ip6tables" 2>/dev/null || true
}
trap _on_exit_cleanup EXIT TERM INT

# ---------- 5. 替换 swanctl.conf 中的占位符 ----------
#  模板里有 %IKEV2_LOCAL_ADDRS_DIRECTIVE% / %IKEV2_LOCAL_TS% / %IKEV2_SERVER_CN%
#  / %IKEV2_SERVER_CERT_FILE% 等占位符，必须替换成实际值，否则 charon 无法解析。
#
# v2-79 关键改动：local_addrs 不再硬编码
#   - 有公网 IP（v6 或 v4）→ 写 `local_addrs = <ip>`
#   - 都没有 → 删掉整行（strongSwan 默认监听所有接口的 500/4500）
# local_ts 同样：IPv6_ONLY 时只 ::/0；双栈时 0.0.0.0/0, ::/0
PLACEHOLDER_OK=0
if [ -f /etc/swanctl/swanctl.conf ]; then
  echo "${LOG_PREFIX} Replacing placeholders in /etc/swanctl/swanctl.conf..."

  # v2.86-PR11:root filesystem 只读,sed -i 在 /etc/ 写临时文件失败。
  # 复制到 /tmp 处理完再 cat 回去(in-place 不能创建临时文件,但 cat 重定向可以)。
  SWAN_TMP_CONF=$(mktemp /tmp/swanctl.conf.XXXXXX)
  cp /etc/swanctl/swanctl.conf "$SWAN_TMP_CONF"

  # ---- 1. local_addrs ----
  if [ -n "${IKEV2_SERVER_ADDR_V6:-}" ]; then
    sed -i "s|%IKEV2_LOCAL_ADDRS_DIRECTIVE%|local_addrs = ${IKEV2_SERVER_ADDR_V6}|g" "$SWAN_TMP_CONF"
    echo "${LOG_PREFIX} local_addrs = ${IKEV2_SERVER_ADDR_V6} (auto-detected v6)"
  elif [ -n "${IKEV2_SERVER_ADDR_V4:-}" ]; then
    sed -i "s|%IKEV2_LOCAL_ADDRS_DIRECTIVE%|local_addrs = ${IKEV2_SERVER_ADDR_V4}|g" "$SWAN_TMP_CONF"
    echo "${LOG_PREFIX} local_addrs = ${IKEV2_SERVER_ADDR_V4} (auto-detected v4)"
  else
    # 都没有 → 留空（strongSwan 默认监听所有接口 500/4500）
    sed -i "s|%IKEV2_LOCAL_ADDRS_DIRECTIVE%|# local_addrs omitted: listen on all interfaces|g" "$SWAN_TMP_CONF"
    echo "${LOG_PREFIX} local_addrs omitted (listen on all interfaces)"
  fi

  # ---- 2. local_ts ----
  # v2.86-PR12.10 修复:之前 IPv6_ONLY=true 写 local_ts=::/0,导致 iOS 客户端拿到
  # IPv4 虚拟 IP(10.10.0.1)后,发的 IPv4 流量不匹配 TS(::/0 是 IPv6 only)→ ESP
  # 内核 libipsec DROP → "VPN 连上但上不了网"。修法:无论 IPv6_ONLY 还是双栈,都写
  # 双栈 TS 0.0.0.0/0, ::/0,因为 ESP tunnel 本身可以承载 IPv4 inner packet;
  # IPv6_ONLY 只控制监听/拨号用的网络栈(IKE 协商走 IPv6),不影响 ESP 内层协议族。
  # v2.86-PR12.13:合并双分支(内容相同,见 PR12.10 注释)。
  sed -i "s|%IKEV2_LOCAL_TS%|0.0.0.0/0, ::/0|g" "$SWAN_TMP_CONF"

  # ---- 2.5 VPN 子网（v2-79 占位符化）----
  # pools 段的 addrs 用探测到的 IKEV2_VPN_SUBNET（§0.6 自动选不冲突的段）
  sed -i "s|%IKEV2_VPN_SUBNET%|${IKEV2_VPN_SUBNET}|g" "$SWAN_TMP_CONF"
  # v2-80：IPv6 ULA pool（§0.6b 自动选不冲突的 fd00:x::/64，替换原来的 fd00:1::/64）
  sed -i "s|%IKEV2_VPN_SUBNET_V6%|${IKEV2_VPN_SUBNET_V6}|g" "$SWAN_TMP_CONF"

  # ---- 3. CN / cert 文件名 / 其他 ----
  sed -i "s|%IKEV2_SERVER_CN%|${IKEV2_SERVER_CN:-vpn.example.com}|g" "$SWAN_TMP_CONF"
  sed -i "s|%IKEV2_OUT_IF%|${OUT_IF}|g" "$SWAN_TMP_CONF"

  # 证书文件名：LE 模式用 $DOMAIN.pem，自签模式用 server.cert.pem
  if [ "${IKEV2_CERT_MODE:-self-signed}" = "letsencrypt" ] && [ -n "${IKEV2_DOMAIN:-}" ]; then
    sed -i "s|%IKEV2_SERVER_CERT_FILE%|${IKEV2_DOMAIN}.pem|g" "$SWAN_TMP_CONF"
  else
    sed -i "s|%IKEV2_SERVER_CERT_FILE%|server.cert.pem|g" "$SWAN_TMP_CONF"
  fi

  # 把处理后的 conf 写回原位置(cat 重定向可以,临时文件创建不行)
  cat "$SWAN_TMP_CONF" > /etc/swanctl/swanctl.conf
  rm -f "$SWAN_TMP_CONF"
  PLACEHOLDER_OK=1
else
  echo "${LOG_PREFIX} WARN: /etc/swanctl/swanctl.conf not found, skipping placeholder replacement" >&2
  PLACEHOLDER_OK=1  # 不致命，swanctl 会用其他 conf
fi

# ---------- 6. 启动 charon（strongSwan IKE 守护进程）----------
# v2.86-PR10:不再用 `ipsec start --nofork`(依赖 stroke plugin,已删)。
# strongSwan 6.0+ 推荐直接调 charon 二进制 + 配置强Swan VICI。
echo "${LOG_PREFIX} Starting charon..."
/usr/lib/ipsec/charon >/var/log/charon.log 2>&1 &
CHARON_BG_PID=$!

# 等待 charon 就绪（最多 10 秒）
CHARON_READY=0
for i in $(seq 1 20); do
  if swanctl --stats >/dev/null 2>&1; then
    echo "${LOG_PREFIX} charon ready (took ${i}*0.5s)"
    CHARON_READY=1
    break
  fi
  sleep 0.5
done

if [ "$CHARON_READY" != "1" ]; then
  echo "${LOG_PREFIX} FATAL: charon failed to start" >&2
  echo "${LOG_PREFIX} Last 30 lines of /var/log/charon.log:" >&2
  tail -n 30 /var/log/charon.log >&2 || true
  exit 12
fi

# ---------- 7. 加载 swanctl 连接配置 ----------
# 为什么必须：charon 启动后**默认不会自动加载** /etc/swanctl/swanctl.conf，
# 不调 swanctl --load-all → swanctl --list-conns 是空的 → iOS 发 IKE 包来
# charon 回 NO_PROPOSAL_CHOSEN → 用户看到"连不上"。
#
# commit 25 设计意图是让 Go 进程通过 VICI 加载，但实际没实现（cmd/.../main.go
# 只 load-creds，没调 VICI load-conns），所以这里必须显式加载，否则 server
# 永远没 connection。
#
# 加载时机：必须等到证书安装到 /etc/swanctl/x509/ 之后。LE 模式在 §7.5 装好，
# 自签模式 Go 进程启动后会装。所以这里先 load 一次（conf 里 certs 是相对路径
# 时也能找到 LE cert），Go 进程装完自签 cert 后重启 charon 或再调一次 VICI。
#
# 为简化：先 load 一次让 LE 模式能用；自签模式等 Go 进程启动后由 VICI 接管。
echo "${LOG_PREFIX} Loading swanctl connections..."
if ! swanctl --load-all 2>&1 | tail -10; then
  echo "${LOG_PREFIX} WARN: swanctl --load-all failed (will retry after Go installs certs)" >&2
fi

# ---------- 7.5. Let's Encrypt 模式证书准备 ----------
# 设计见 docs/design.md §1.4.1
#
# 触发条件：IKEV2_CERT_MODE=letsencrypt
# 流程：
#   1. 检查 /data/le/fullchain.pem 是否存在
#      - 不存在 → 调 acme.sh --issue 首次签发 → acme.sh --install-cert 写到 /data/le/
#      - 存在 → 直接跳过（容器重建、升级等情况）
#   2. 复制 /data/le/fullchain.pem → /etc/swanctl/x509/$DOMAIN.pem
#   3. 复制 /data/le/privkey.pem → /etc/swanctl/private/$DOMAIN.key
#   4. 启动 cron 守护（容器内 cron 每天跑 renew-cert.sh）
#   5. 把 /data/le/ 也写一份 LAST_RENEW_FAILED 文件占位（容器首次启动后由 renew-cert 管理）
#
# 凭证注入（不写进镜像）：
#   - $Ali_Key / $Ali_Secret 从 docker compose env 传入
#   - 写到 /opt/acme.sh/account.conf（容器内、0600 权限）
#   - 容器销毁后凭证即丢失；下次启动需重新提供（避免泄漏风险）

if [ "${IKEV2_CERT_MODE:-self-signed}" = "letsencrypt" ]; then
  if [ -z "${IKEV2_DOMAIN:-}" ]; then
    echo "${LOG_PREFIX} FATAL: IKEV2_DOMAIN is required for LE mode" >&2
    exit 17
  fi

  LE_CERT_DIR="/data/le"
  LE_FULLCHAIN="${LE_CERT_DIR}/fullchain.pem"
  LE_PRIVKEY="${LE_CERT_DIR}/privkey.pem"
  # v2.86-PR12.8:acme.sh --home 指向 /data/acme-sh (命名卷,天然持久化)。
  # 之前用 /opt/acme.sh 时 account.json / ca/ 写到镜像层 overlay upper,
  # docker compose down 后丢失 → 容器重启重新注册 → 触发 LE rate-limit。
  # 改成 /data/acme-sh 后,account.json / ca/ / 域名证书目录全在命名卷里,
  # 跟 nginx-proxy/acme-companion 的 /etc/acme.sh 命名卷用法一致。
  # dnsapi/deploy/notify 是 acme.sh 二进制的"伴生文件",acme.sh 通过
  # $(dirname "$0") 找,跟 --home 无关——所以放 /opt/acme.sh (镜像层) 不受影响。
  ACME_HOME="/data/acme-sh"
  ACME_CONF="${ACME_HOME}/account.conf"
  SWAN_X509="/etc/swanctl/x509/${IKEV2_DOMAIN}.pem"
  SWAN_KEY="/etc/swanctl/private/${IKEV2_DOMAIN}.key"

  mkdir -p "${LE_CERT_DIR}" "${ACME_HOME}"

  # 注入阿里云 DNS API 凭证（仅 LE 模式 + 阿里云 dns_ali 插件）
  # v2-83:优先级链
  #   1. /data/panel-state/aliyun.creds (面板 UI 填的,运行时可改)
  #   2. $IKEV2_ALIYUN_KEY_ID / $IKEV2_ALIYUN_KEY_SECRET (新统一命名 env)
  #   3. $ALIYUN_ACCESS_KEY_ID / $ALIYUN_ACCESS_KEY_SECRET (v2-82 legacy DDNS)
  #   4. $Ali_Key / $Ali_Secret (acme.sh 命名,legacy)
  PANEL_CREDS="/data/panel-state/aliyun.creds"
  PANEL_KEY_ID=""
  PANEL_KEY_SECRET=""
  if [ -f "${PANEL_CREDS}" ]; then
    # 解析 JSON — 镜像里没装 jq,用 sed/grep 简单提取
    #   格式: { "key_id": "...", "key_secret": "..." }
    #   字段值是字符串,可能有转义 \" \\;但 AccessKey 是字母数字无转义
    PANEL_KEY_ID=$(sed -n 's/.*"key_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${PANEL_CREDS}" 2>/dev/null | head -1)
    PANEL_KEY_SECRET=$(sed -n 's/.*"key_secret"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${PANEL_CREDS}" 2>/dev/null | head -1)
    if [ -n "${PANEL_KEY_ID}" ] && [ -n "${PANEL_KEY_SECRET}" ]; then
      export Ali_Key="${PANEL_KEY_ID}"
      export Ali_Secret="${PANEL_KEY_SECRET}"
      echo "${LOG_PREFIX} LE: aliyun DNS API credentials loaded from ${PANEL_CREDS} (panel UI)"
    else
      echo "${LOG_PREFIX} WARN: ${PANEL_CREDS} exists but key_id/key_secret missing or invalid" >&2
    fi
  fi

  # 面板凭证未提供时,fallback 到 env (优先级链 2-4)
  if [ -z "${Ali_Key:-}" ] || [ -z "${Ali_Secret:-}" ]; then
    if [ -n "${IKEV2_ALIYUN_KEY_ID:-}" ] && [ -n "${IKEV2_ALIYUN_KEY_SECRET:-}" ]; then
      export Ali_Key="${IKEV2_ALIYUN_KEY_ID}"
      export Ali_Secret="${IKEV2_ALIYUN_KEY_SECRET}"
      echo "${LOG_PREFIX} LE: aliyun credentials from IKEV2_ALIYUN_KEY_* env (v2-83 unified naming)"
    elif [ -n "${ALIYUN_ACCESS_KEY_ID:-}" ] && [ -n "${ALIYUN_ACCESS_KEY_SECRET:-}" ]; then
      export Ali_Key="${ALIYUN_ACCESS_KEY_ID}"
      export Ali_Secret="${ALIYUN_ACCESS_KEY_SECRET}"
      echo "${LOG_PREFIX} LE: aliyun credentials from ALIYUN_ACCESS_KEY_* env (v2-82 legacy, will be removed in v3.0.0)" >&2
    elif [ -n "${Ali_Key:-}" ] && [ -n "${Ali_Secret:-}" ]; then
      echo "${LOG_PREFIX} LE: aliyun credentials from Ali_Key/Ali_Secret env (acme.sh native naming, legacy)"
    fi
  fi

  # 注入到 acme.sh account.conf(无论来源)
  if [ -n "${Ali_Key:-}" ] && [ -n "${Ali_Secret:-}" ]; then
    printf 'export Ali_Key="%s"\nexport Ali_Secret="%s"\n' \
      "${Ali_Key}" "${Ali_Secret}" > "${ACME_CONF}"
    chmod 600 "${ACME_CONF}"
    echo "${LOG_PREFIX} LE: aliyun DNS API credentials installed to ${ACME_CONF}"
  fi

  # DDNS 凭证也注入 (DDNS 在 Go 进程内通过 panelstate.aliyun.creds 直读,这里只是兼容
  # 老用户用 env ALIYUN_ACCESS_KEY_* 的场景——如果 panelstate 文件不存在,Go 进程 fallback 到 env)
  export IKEV2_ALIYUN_KEY_ID="${Ali_Key:-}"
  export IKEV2_ALIYUN_KEY_SECRET="${Ali_Secret:-}"

  # 首次签发
  if [ ! -f "${LE_FULLCHAIN}" ] || [ ! -f "${LE_PRIVKEY}" ]; then
    echo "${LOG_PREFIX} LE: first run, issuing certificate for ${IKEV2_DOMAIN}..."
    if ! acme.sh --issue --dns dns_ali \
         -d "${IKEV2_DOMAIN}" \
         --keylength 2048 \
         --server letsencrypt \
         --home "${ACME_HOME}" \
         --config-home "${ACME_HOME}" 2>&1 | tail -20; then
      echo "${LOG_PREFIX} FATAL: acme.sh --issue failed (see logs above)" >&2
      echo "${LOG_PREFIX} 提示：检查 Ali_Key/Ali_Secret 是否正确；域名 ${IKEV2_DOMAIN} 是否在阿里云 DNS 解析" >&2
      exit 18
    fi

    # 安装证书到 /data/le/（持久化卷）
    if ! acme.sh --install-cert -d "${IKEV2_DOMAIN}" \
         --fullchain-file "${LE_FULLCHAIN}" \
         --key-file       "${LE_PRIVKEY}" \
         --reloadcmd      "/usr/local/bin/ikev2-reload.sh" \
         --home "${ACME_HOME}" \
         --config-home "${ACME_HOME}"; then
      echo "${LOG_PREFIX} FATAL: acme.sh --install-cert failed" >&2
      exit 19
    fi
    chmod 644 "${LE_FULLCHAIN}"
    chmod 600 "${LE_PRIVKEY}"
    echo "${LOG_PREFIX} LE: cert issued and stored at ${LE_CERT_DIR}"
  else
    echo "${LOG_PREFIX} LE: existing cert found at ${LE_CERT_DIR}, skipping issue"
  fi

  # v2.86-PR12.4:cert / key 改 atomic + 0o600(key)
  # v2.86-PR12.1 helper(atomic_rename) 在 §6 之前的 LE 块里没定义,
  # 这里内联实现。功能等价:write tmp + rename。
  atomic_rename_inline() {
    local src="$1" dst="$2" mode="$3"
    if [ ! -f "$src" ]; then
      return 0
    fi
    local tmp="${dst}.tmp.$$"
    install -m "$mode" "$src" "$tmp"
    mv -f "$tmp" "$dst"
  }

  # 复制到 strongSwan 默认查找路径(v2.86-PR12.4 atomic + key 0600)
  atomic_rename_inline "${LE_FULLCHAIN}" "${SWAN_X509}" "644"
  atomic_rename_inline "${LE_PRIVKEY}"  "${SWAN_KEY}" "600"
  echo "${LOG_PREFIX} LE: cert installed to ${SWAN_X509}"

  # v2.86-PR12.3:renew cron 随机化 + 多次重试
  # 原因:LE 续签 cron 全球都在固定时间跑,容易触发 LE 速率配额限流。
  # 做法:每天随机 02:00~04:00 之间一个时间点跑 1 次,加 12h 一次重试。
  # 2 次/天足够覆盖 cert < 30 天的场景(< 30 天立即续,> 30 天跳过)
  CRON_FILE="/etc/cron.d/ikev2-le-renew"
  # $RANDOM 是 bash 内建 0-32767 伪随机
  RAND_HOUR=$((2 + RANDOM % 3))     # 2 / 3 / 4
  RAND_MIN=$((RANDOM % 60))          # 0-59
  cat > "${CRON_FILE}" <<EOF
# ikev2 panel LE cert renew (v2.86-PR12.3,自动生成,随机时间)
# 每天 ${RAND_HOUR}:$(printf '%02d' $RAND_MIN) 跑一次 + 12h 后重试
${RAND_MIN} ${RAND_HOUR} * * * root /etc/ikev2-panel/scripts/renew-cert.sh ${IKEV2_DOMAIN} > /var/log/ikev2-renew.log 2>&1
${RAND_MIN} $(((RAND_HOUR + 12) % 24)) * * * root /etc/ikev2-panel/scripts/renew-cert.sh ${IKEV2_DOMAIN} > /var/log/ikev2-renew-retry.log 2>&1
EOF
  chmod 644 "${CRON_FILE}"
  echo "${LOG_PREFIX} LE: cron entry written (random ${RAND_HOUR}:$(printf '%02d' $RAND_MIN) + 12h retry)"

  # 启动 cron 守护（容器内）
  # 注：cron 在容器里 PID 不是 1 也能跑——只要 init 系统允许 fork
  if command -v cron >/dev/null 2>&1; then
    service cron start 2>&1 | tail -3 || cron 2>&1 | tail -3
    echo "${LOG_PREFIX} LE: cron daemon started"
  else
    echo "${LOG_PREFIX} WARN: cron not found, LE renew will not auto-run" >&2
  fi
fi

# ---------- 7.7. audit_log retention cron（Phase 5 A）----------
# 设计: 跟 LE 续签 cron 同一文件,但 LE 不存在时单独写一份。
# 每天 04:00 跑,默认保留 90 天,可通过 IKEV2_AUDIT_RETENTION_DAYS 覆盖。
#
# 这里不依赖 LE 模式:即使自签模式也有审计,长期跑也要 retention。
# 同样写 /etc/cron.d/ 兼容 cron 守护;如果上面已经启动了 cron 守护,直接复用。
AUDIT_CRON_FILE="/etc/cron.d/ikev2-audit-retention"
cat > "${AUDIT_CRON_FILE}" <<EOF
# ikev2 panel audit retention (Phase 5 A, 自动生成, 请勿手工改)
# 每天 04:00 删 ${IKEV2_AUDIT_RETENTION_DAYS:-90} 天前的 audit_log
0 4 * * * root /etc/ikev2-panel/scripts/audit-retention.sh > /var/log/ikev2-audit-retention.log 2>&1
EOF
chmod 644 "${AUDIT_CRON_FILE}"
echo "${LOG_PREFIX} audit retention cron entry written to ${AUDIT_CRON_FILE}"

# 如果 LE cron 没启动 cron daemon(自签模式),这里补启一次。
# service cron start 是幂等的(已经起过会报 warning,不阻塞)。
if command -v cron >/dev/null 2>&1; then
  service cron start 2>&1 | tail -2 || true
  echo "${LOG_PREFIX} cron daemon running (for audit retention + LE renew)"
else
  echo "${LOG_PREFIX} WARN: cron not found, audit retention will NOT auto-run"
fi

# ---------- 7.6. 在证书装好后再 load 一次 swanctl ----------
# §7 提前 load 会因为 LE cert 还没复制到 /etc/swanctl/x509/ 而失败（见 commit 25 后日志）。
# LE 模式 cert 在 §7.5 复制，自签模式 cert 在 Go 进程启动后写出来。
# 不管哪种模式，**§7.5 之后**、**Go 进程启动之前**这一刻，两种 cert 都已经在磁盘上：
#   - LE 模式：§7.5 复制完成
#   - 自签模式：charon 启动前 entrypoint 没复制，但 Go 进程会用 VICI load-creds 单独加载，
#     entrypoint 这里跳过 load-all（Go 进程启动后会接管 charon 配置）
# 但简单起见我们都尝试一次 load-all：LE 模式成功，自签模式 cert 找不到是预期失败，不致命。
if [ -f "${SWAN_X509:-}" ]; then
  echo "${LOG_PREFIX} Loading swanctl connections (post-cert)..."
  swanctl --load-all 2>&1 | tail -8 || echo "${LOG_PREFIX} WARN: load-all failed (Go process will retry via VICI)"
fi

# ---------- 7.9. v2.86-PR12.17:nft 自定义表 ikev2/ikev26 完整性校验 + 重建 ----------
# 背景:PR12.13 用 iptables-nft 写在 docker daemon 自己的 filter/nat 表里,daemon
# 异步 iptables-restore 会挤掉规则 → 必须 retry。PR12.16 改用自定义 nft 表
# ikev2 / ikev26,daemon 根本不会碰 → 理论上不需要 retry。但保留 retry 作为
# 防御性编程:极端场景(daemon 切换 iptables=false / nft 服务重启 / 命名空间被
# 重置)下 nft 表可能丢,需要 retry 重新提交。
#
# v2.86-PR12.17 修复:re_add 改为 "先 delete stale 表再 nft -f",处理表存在但
# 内容残缺的情况(docker 极少见,但 nft 跨 netns 时表会变 stale → 单纯 nft -f
# 会跟旧表冲突报错)。
#
# phase1(快重试):1s × 10 次,覆盖 nft 服务冷启动延迟
# phase2(慢重试):5s × 50 次,覆盖 daemon reload / 容器断网重连
# 失败时(总 260 秒)写 ERROR 日志 + sentinel 文件。
IPTABLES_RETRY_PHASE1=10
IPTABLES_RETRY_PHASE2=50
IPTABLES_RETRY=0
IPTABLES_PHASE2=0
IPTABLES_HEALTHZ_DIR="/run/ikev2-panel"
mkdir -p "$IPTABLES_HEALTHZ_DIR" 2>/dev/null || true

check_ikev2_rules() {
  # v2.86-PR12.16:检查自定义 nft 表 ikev2 (ip) + ikev26 (ip6) 是否存在
  # `nft list table` 失败返回非 0 = 表不存在
  if ! command -v "$NFT_CMD" >/dev/null 2>&1; then
    return 1
  fi
  $NFT_CMD list table ip ikev2  >/dev/null 2>&1 || return 1
  $NFT_CMD list table ip6 ikev26 >/dev/null 2>&1 || return 1
  return 0
}

re_add_ikev2_rules() {
  # v2.86-PR12.17:先清 stale 表 → 再 nft -f 原子重建
  # 单 nft -f 在 stale 表存在时会因为 chain 已存在报错 → 必须先 delete
  if [ ! -f "${NFT_RUNTIME:-/run/ikev2-panel/ikev2.nft}" ]; then
    echo "${LOG_PREFIX} re_add: NFT_RUNTIME missing, skip" >&2
    return 1
  fi
  if ! command -v "$NFT_CMD" >/dev/null 2>&1; then
    echo "${LOG_PREFIX} re_add: nft not found" >&2
    return 1
  fi
  # 删 stale 表(允许失败 — 表不存在时 nft delete table 也返回非 0)
  $NFT_CMD delete table ip ikev2  2>/dev/null || true
  $NFT_CMD delete table ip6 ikev26 2>/dev/null || true
  # 原子重建
  if $NFT_CMD -f "${NFT_RUNTIME}" 2>/dev/null; then
    return 0
  fi
  echo "${LOG_PREFIX} re_add: nft -f failed" >&2
  return 1
}

while true; do
  if check_ikev2_rules; then
    echo "${LOG_PREFIX} nft ikev2/ikev26 ruleset verified after $IPTABLES_RETRY retries"
    rm -f "$IPTABLES_HEALTHZ_DIR/iptables-warn.flag" 2>/dev/null || true
    break
  fi
  IPTABLES_RETRY=$((IPTABLES_RETRY + 1))
  if [ "$IPTABLES_PHASE2" = "0" ] && [ "$IPTABLES_RETRY" -ge "$IPTABLES_RETRY_PHASE1" ]; then
    IPTABLES_PHASE2=1
    echo "${LOG_PREFIX} phase1 retry exhausted, entering slow retry phase (5s interval, max ${IPTABLES_RETRY_PHASE2} more)" >&2
  fi
  if [ "$IPTABLES_RETRY" -ge $((IPTABLES_RETRY_PHASE1 + IPTABLES_RETRY_PHASE2)) ]; then
    echo "${LOG_PREFIX} ERROR: nft ikev2/ikev26 ruleset not stable after $IPTABLES_RETRY retries" >&2
    echo "${LOG_PREFIX}   → VPN SA 协商可能正常,但客户端无法访问公网(转发被 DROP)" >&2
    echo "${LOG_PREFIX}   → 可疑根因:nft 服务异常 / docker 切换 iptables=false" >&2
    echo "${LOG_PREFIX}   → 排查: docker exec <ctr> nft list table ip ikev2" >&2
    # sentinel 文件,Go 进程 healthz 检测后 banner 提示
    printf 'iptables_unstable=true retries=%s timestamp=%s\n' \
      "$IPTABLES_RETRY" "$(date -u +%FT%TZ)" \
      > "$IPTABLES_HEALTHZ_DIR/iptables-warn.flag" 2>/dev/null || true
    break
  fi
  echo "${LOG_PREFIX} nft ruleset missing, re-applying (retry $IPTABLES_RETRY)"
  if ! re_add_ikev2_rules; then
    echo "${LOG_PREFIX}   re_add_ikev2_rules returned non-zero, will retry" >&2
  fi
  if [ "$IPTABLES_PHASE2" = "1" ]; then sleep 5; else sleep 1; fi
done

# ---------- 8. exec Go 进程（接管 PID 1，收到信号时优雅关闭）----------
echo "${LOG_PREFIX} Starting ikev2-panel (Go)..."
exec /usr/local/bin/ikev2-panel