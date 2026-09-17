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
export IKEV2_SERVER_ADDR_V6 IKEV2_SERVER_ADDR_V4 IKEV2_VPN_SUBNET VPN_SUBNET_BASE VPN_SERVER_VIP

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
    echo "${LOG_PREFIX} 提示 2：检查 docker daemon 是否有 CAP_SYS_ADMIN（host 网络模式必需）" >&2
    echo "${LOG_PREFIX} 提示 3：检查 privileged: true 是否给了容器（compose 文件）" >&2
    exit 11
  fi
fi
if ! enable_forwarding ipv4; then
  echo "${LOG_PREFIX} FATAL: net.ipv4.ip_forward=0 and cannot auto-enable" >&2
  echo "${LOG_PREFIX} 提示 1：sudo sysctl -w net.ipv4.ip_forward=1" >&2
  echo "${LOG_PREFIX} 提示 2：检查 docker daemon 是否有 CAP_SYS_ADMIN（host 网络模式必需）" >&2
  echo "${LOG_PREFIX} 提示 3：检查 privileged: true 是否给了容器（compose 文件）" >&2
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

    # 2. iptables FORWARD ACCEPT ipsec0 ↔ OUT_IF
    if iptables -C FORWARD -i ipsec0 -j ACCEPT 2>/dev/null; then
      echo "${LOG_PREFIX} FORWARD -i ipsec0 ACCEPT already present"
    else
      iptables -I FORWARD -i ipsec0 -j ACCEPT \
        && echo "${LOG_PREFIX} FORWARD -i ipsec0 ACCEPT added" \
        || echo "${LOG_PREFIX} WARN: failed to add FORWARD -i ipsec0" >&2
    fi
    if iptables -C FORWARD -o ipsec0 -j ACCEPT 2>/dev/null; then
      echo "${LOG_PREFIX} FORWARD -o ipsec0 ACCEPT already present"
    else
      iptables -I FORWARD -o ipsec0 -j ACCEPT \
        && echo "${LOG_PREFIX} FORWARD -o ipsec0 ACCEPT added" \
        || echo "${LOG_PREFIX} WARN: failed to add FORWARD -o ipsec0" >&2
    fi

    # 3. MASQUERADE：客户端 VPN_SUBNET 出向伪装成 OUT_IF 上的 IP（v2-79 用探测的子网）
    if iptables -t nat -C POSTROUTING -s "$IKEV2_VPN_SUBNET" -o "$OUT_IF" -j MASQUERADE 2>/dev/null; then
      echo "${LOG_PREFIX} MASQUERADE $IKEV2_VPN_SUBNET -> $OUT_IF already present"
    else
      iptables -t nat -A POSTROUTING -s "$IKEV2_VPN_SUBNET" -o "$OUT_IF" -j MASQUERADE \
        && echo "${LOG_PREFIX} MASQUERADE $IKEV2_VPN_SUBNET -> $OUT_IF added" \
        || echo "${LOG_PREFIX} WARN: failed to add MASQUERADE" >&2
    fi
  fi
fi

# ---------- 4. MASQUERADE（NAT 出向源地址伪装）----------
#  客户端 VPN 拨号后获得虚拟 IP（如 fd00::2 / 10.99.99.2），这个地址在公网不可路由。
#  出向流量的源 IP 必须是容器对外网卡地址（$OUT_IF 上的 IP），否则目标服务器回包失败。
#
#  规则：
#    - IPv6 模式（IKEV2_IPV6_ONLY=true）：仅 ip6tables MASQUERADE
#    - 双栈（IKEV2_IPV6_ONLY=false）：iptables + ip6tables 都要 MASQUERADE
#
#  我们不去 MASQUERADE 内网/loopback/链路本地地址，避免破坏 Docker 网络内部通信。
apply_masquerade() {
  local family="$1"   # "ipv4" | "ipv6"
  local iptables_bin
  if [ "$family" = "ipv6" ]; then
    iptables_bin="ip6tables"
  else
    iptables_bin="iptables"
  fi

  if ! command -v "$iptables_bin" >/dev/null 2>&1; then
    echo "${LOG_PREFIX} WARN: $iptables_bin not found, skipping ${family} MASQUERADE" >&2
    return 0
  fi

  # 幂等检查：避免重启容器时重复添加 MASQUERADE 规则
  if "$iptables_bin" -t nat -C POSTROUTING \
        -o "$OUT_IF" \
        -m addrtype ! --dst-type LOCAL \
        -j MASQUERADE 2>/dev/null; then
    echo "${LOG_PREFIX} ${family} MASQUERADE already present on ${OUT_IF}"
    return 0
  fi

  # 关键：使用 -m addrtype ! --dst-type LOCAL 而非多个 ! -d。
  # 原因：nf_tables 后端（Debian 12+ 默认）不支持多个 --destination 选项；
  # addrtype 还能自动覆盖 loopback/link-local/multicast/anycast 等所有本地地址，
  # 比手写排除更安全。
  if "$iptables_bin" -t nat -A POSTROUTING \
        -o "$OUT_IF" \
        -m addrtype ! --dst-type LOCAL \
        -j MASQUERADE 2>/dev/null; then
    echo "${LOG_PREFIX} ${family} MASQUERADE applied on ${OUT_IF}"
  else
    if [ "$family" = "ipv4" ]; then
      echo "${LOG_PREFIX} FATAL: iptables MASQUERADE failed" >&2
      exit 15
    else
      # IPv6 NAT 在某些宿主机（如 OpenVZ）内核不支持，仅警告
      echo "${LOG_PREFIX} WARN: ip6tables MASQUERADE failed (kernel may lack IPv6 NAT)" >&2
    fi
  fi
}

# 检测容器内是否有公网 IPv4 地址（决定是否做 IPv4 MASQUERADE）
HAS_IPV4_PUBLIC=false
if ip -4 addr show dev "$OUT_IF" 2>/dev/null | awk '/inet / {print $2}' | grep -vE '^(127\.|0\.|169\.254\.|10\.|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.)' | head -1 | grep -q .; then
  HAS_IPV4_PUBLIC=true
fi

if [ "$IKEV2_IPV6_ONLY" = "true" ]; then
  # 强制 IPv6 模式：仅 IPv6 MASQUERADE
  apply_masquerade "ipv6"
else
  # 双栈 / IPv4 模式：两者都做（如果容器有公网 IPv4）
  if [ "$HAS_IPV4_PUBLIC" = "true" ]; then
    apply_masquerade "ipv4"
  else
    echo "${LOG_PREFIX} WARN: no public IPv4 on ${OUT_IF}, skipping ipv4 MASQUERADE" >&2
  fi
  # 始终尝试 IPv6 MASQUERADE（如果容器有 IPv6）
  if [ -n "$IPV6_ADDR" ] || ip -6 addr show dev "$OUT_IF" 2>/dev/null | grep -q 'inet6 .* global'; then
    apply_masquerade "ipv6"
  fi
fi

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

  # ---- 1. local_addrs ----
  if [ -n "${IKEV2_SERVER_ADDR_V6:-}" ]; then
    sed -i "s|%IKEV2_LOCAL_ADDRS_DIRECTIVE%|local_addrs = ${IKEV2_SERVER_ADDR_V6}|g" /etc/swanctl/swanctl.conf
    echo "${LOG_PREFIX} local_addrs = ${IKEV2_SERVER_ADDR_V6} (auto-detected v6)"
  elif [ -n "${IKEV2_SERVER_ADDR_V4:-}" ]; then
    sed -i "s|%IKEV2_LOCAL_ADDRS_DIRECTIVE%|local_addrs = ${IKEV2_SERVER_ADDR_V4}|g" /etc/swanctl/swanctl.conf
    echo "${LOG_PREFIX} local_addrs = ${IKEV2_SERVER_ADDR_V4} (auto-detected v4)"
  else
    # 都没有 → 留空（strongSwan 默认监听所有接口 500/4500）
    sed -i "s|%IKEV2_LOCAL_ADDRS_DIRECTIVE%|# local_addrs omitted: listen on all interfaces|g" /etc/swanctl/swanctl.conf
    echo "${LOG_PREFIX} local_addrs omitted (listen on all interfaces)"
  fi

  # ---- 2. local_ts ----
  if [ "$IKEV2_IPV6_ONLY" = "true" ]; then
    sed -i "s|%IKEV2_LOCAL_TS%|::/0|g" /etc/swanctl/swanctl.conf
  else
    sed -i "s|%IKEV2_LOCAL_TS%|0.0.0.0/0, ::/0|g" /etc/swanctl/swanctl.conf
  fi

  # ---- 2.5 VPN 子网（v2-79 占位符化）----
  # pools 段的 addrs 用探测到的 IKEV2_VPN_SUBNET（§0.6 自动选不冲突的段）
  sed -i "s|%IKEV2_VPN_SUBNET%|${IKEV2_VPN_SUBNET}|g" /etc/swanctl/swanctl.conf

  # ---- 3. CN / cert 文件名 / 其他 ----
  sed -i "s|%IKEV2_SERVER_CN%|${IKEV2_SERVER_CN:-vpn.example.com}|g" /etc/swanctl/swanctl.conf
  sed -i "s|%IKEV2_OUT_IF%|${OUT_IF}|g" /etc/swanctl/swanctl.conf

  # 证书文件名：LE 模式用 $DOMAIN.pem，自签模式用 server.cert.pem
  if [ "${IKEV2_CERT_MODE:-self-signed}" = "letsencrypt" ] && [ -n "${IKEV2_DOMAIN:-}" ]; then
    sed -i "s|%IKEV2_SERVER_CERT_FILE%|${IKEV2_DOMAIN}.pem|g" /etc/swanctl/swanctl.conf
  else
    sed -i "s|%IKEV2_SERVER_CERT_FILE%|server.cert.pem|g" /etc/swanctl/swanctl.conf
  fi
  PLACEHOLDER_OK=1
else
  echo "${LOG_PREFIX} WARN: /etc/swanctl/swanctl.conf not found, skipping placeholder replacement" >&2
  PLACEHOLDER_OK=1  # 不致命，swanctl 会用其他 conf
fi

# ---------- 6. 启动 charon（strongSwan IKE 守护进程）----------
echo "${LOG_PREFIX} Starting charon..."
ipsec start --nofork >/var/log/charon.log 2>&1 &
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
#   - 写到 /root/.acme.sh/account.conf（容器内、0600 权限）
#   - 容器销毁后凭证即丢失；下次启动需重新提供（避免泄漏风险）

if [ "${IKEV2_CERT_MODE:-self-signed}" = "letsencrypt" ]; then
  if [ -z "${IKEV2_DOMAIN:-}" ]; then
    echo "${LOG_PREFIX} FATAL: IKEV2_DOMAIN is required for LE mode" >&2
    exit 17
  fi

  LE_CERT_DIR="/data/le"
  LE_FULLCHAIN="${LE_CERT_DIR}/fullchain.pem"
  LE_PRIVKEY="${LE_CERT_DIR}/privkey.pem"
  ACME_HOME="/root/.acme.sh"
  ACME_CONF="${ACME_HOME}/account.conf"
  SWAN_X509="/etc/swanctl/x509/${IKEV2_DOMAIN}.pem"
  SWAN_KEY="/etc/swanctl/private/${IKEV2_DOMAIN}.key"

  mkdir -p "${LE_CERT_DIR}"

  # 注入阿里云 DNS API 凭证（仅 LE 模式 + 阿里云 dns_ali 插件）
  if [ -n "${Ali_Key:-}" ] && [ -n "${Ali_Secret:-}" ]; then
    printf 'export Ali_Key="%s"\nexport Ali_Secret="%s"\n' \
      "${Ali_Key}" "${Ali_Secret}" > "${ACME_CONF}"
    chmod 600 "${ACME_CONF}"
    echo "${LOG_PREFIX} LE: aliyun DNS API credentials installed"
  fi

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

  # 复制到 strongSwan 默认查找路径
  install -m 644 "${LE_FULLCHAIN}" "${SWAN_X509}"
  install -m 600 "${LE_PRIVKEY}"  "${SWAN_KEY}"
  echo "${LOG_PREFIX} LE: cert installed to ${SWAN_X509}"

  # 注册 cron 任务：每天凌晨 03:30 跑 renew-cert.sh
  CRON_FILE="/etc/cron.d/ikev2-le-renew"
  cat > "${CRON_FILE}" <<EOF
# ikev2 panel LE cert renew（自动生成，请勿手工改）
30 3 * * * root /etc/ikev2-panel/scripts/renew-cert.sh ${IKEV2_DOMAIN} > /var/log/ikev2-renew.log 2>&1
EOF
  chmod 644 "${CRON_FILE}"
  echo "${LOG_PREFIX} LE: cron entry written to ${CRON_FILE}"

  # 启动 cron 守护（容器内）
  # 注：cron 在容器里 PID 不是 1 也能跑——只要 init 系统允许 fork
  if command -v cron >/dev/null 2>&1; then
    service cron start 2>&1 | tail -3 || cron 2>&1 | tail -3
    echo "${LOG_PREFIX} LE: cron daemon started"
  else
    echo "${LOG_PREFIX} WARN: cron not found, LE renew will not auto-run" >&2
  fi
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

# ---------- 8. exec Go 进程（接管 PID 1，收到信号时优雅关闭）----------
echo "${LOG_PREFIX} Starting ikev2-panel (Go)..."
exec /usr/local/bin/ikev2-panel