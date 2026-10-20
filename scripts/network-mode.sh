#!/bin/sh
# network-mode.sh — 网络模式规格表（单一数据源）
#
# 由 entrypoint.sh source（容器内路径 /etc/ikev2-panel/scripts/network-mode.sh）。
# 本文件**只定义常量表与函数，不执行任何副作用**（不 exit、不写文件、不改全局状态）。
#
# 支持三种模式（v2.86-PR20 引入 macvlan / ipvlan，host 保持默认且零改动）：
#   host     — 容器共享宿主 netns；UDP 500/4500 与 TCP 8443 直接占用宿主端口
#   macvlan  — 容器独占 MAC，直挂物理二层，IPAM 分配 IPv4 + RA/SLAAC 拿 IPv6 GUA
#   ipvlan   — 容器共享 parent MAC，其余同 macvlan
#
# 设计说明见 docs/architecture.md §11.1。

# ---------- 合法模式枚举 ----------
NET_MODE_HOST="host"
NET_MODE_MACVLAN="macvlan"
NET_MODE_IPVLAN="ipvlan"

# ---------- 能力开关（供 entrypoint 分支判断）----------
# 用「返回码」而非全局变量：entrypoint.sh 是 `set -euo pipefail`，
# 任何非 0 返回都要显式处理，写成 `if net_mode_has_docker_user; then` 最安全。

# host 才需要 DOCKER-USER 链（docker daemon 只在宿主 netns 建这条链）
net_mode_has_docker_user() {
  [ "$1" = "$NET_MODE_HOST" ]
}

# 容器 /proc/sys 是否可写：host 共享宿主 netns 且 /proc/sys 为 ro 挂载 → 不可写；
# macvlan/ipvlan 有独立 netns → 可写，forwarding 自愈能真正生效
net_mode_procsys_rw() {
  [ "$1" != "$NET_MODE_HOST" ]
}

# 容器是否持有独立 IP（非宿主地址）
net_mode_has_own_ip() {
  [ "$1" != "$NET_MODE_HOST" ]
}

# DDoS 防护的目标链：
#   host    → DOCKER-USER（docker daemon 管的链，会被 daemon 重写 → 天然幂等）
#   L2 模式 → 容器私有 netns 自己的 INPUT 链（等价作用点，且不污染宿主）
net_mode_ddos_chain() {
  case "$1" in
    "$NET_MODE_HOST") echo "DOCKER-USER" ;;
    *)                echo "INPUT" ;;
  esac
}

# ---------- 模式归一化 ----------
# 大小写不敏感 + 去首尾空白；空值归一化为 host
net_mode_normalize() {
  _nm="$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]' | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
  [ -n "$_nm" ] || _nm="$NET_MODE_HOST"
  echo "$_nm"
}

# ---------- 各模式默认驱动参数（供日志与自检使用）----------
net_mode_driver_desc() {
  case "$1" in
    "$NET_MODE_HOST")
      echo "host-network (shares host netns)"
      ;;
    "$NET_MODE_MACVLAN")
      echo "macvlan_mode=${IKEV2_MACVLAN_MODE:-bridge}"
      ;;
    "$NET_MODE_IPVLAN")
      echo "ipvlan_mode=${IKEV2_IPVLAN_MODE:-l2} ipvlan_flag=${IKEV2_IPVLAN_FLAG:-bridge}"
      ;;
    *)
      echo "unknown"
      ;;
  esac
}

# ---------- IPv4 工具（纯 POSIX sh，不依赖 python/jq）----------

# 单段校验：纯数字、1-3 位、0-255、拒绝前导 0。
#
# 为什么显式拒绝前导 0（"010"）：POSIX sh 的 $((010)) 按**八进制**求值 = 8，
# 而 Docker 侧按十进制 = 10。同一份配置在两处算出不同的地址，
# 且不会报错，只表现为"网关莫名其妙不在子网内"。
# Go 1.17+ 的 net.ParseIP 同样拒绝前导 0，所以这里与 Docker 的判定一致 ——
# 早拒总好过让用户拿到一条语焉不详的 docker 报错。
_nm_valid_octet() {
  case "$1" in
    '' | *[!0-9]*)                  return 1 ;;
    0)                              return 0 ;;
    [1-9] | [1-9][0-9])             return 0 ;;
    1[0-9][0-9] | 2[0-4][0-9] | 25[0-5]) return 0 ;;
    *)                              return 1 ;;
  esac
}

# 严格 IPv4 点分十进制：恰好 4 段，每段 0-255。
# 段数不是 4 也会被挡下：IFS=. read -r a b c d 对 "1.2.3.4.5" 会把第 4 段
# 读成 "4.5"，交给 _nm_valid_octet 按非数字拒掉。
_nm_valid_ipv4() {
  case "$1" in
    *[!0-9.]*) return 1 ;;
  esac
  IFS=. read -r _nm_o1 _nm_o2 _nm_o3 _nm_o4 <<EOF
$1
EOF
  if [ -z "$_nm_o1" ] || [ -z "$_nm_o2" ] || [ -z "$_nm_o3" ] || [ -z "$_nm_o4" ]; then
    return 1
  fi
  _nm_valid_octet "$_nm_o1" || return 1
  _nm_valid_octet "$_nm_o2" || return 1
  _nm_valid_octet "$_nm_o3" || return 1
  _nm_valid_octet "$_nm_o4" || return 1
  return 0
}

_nm_ip2int() {
  _nm_valid_ipv4 "$1" || return 1
  IFS=. read -r a b c d <<EOF
$1
EOF
  echo $(( (a << 24) + (b << 16) + (c << 8) + d ))
}

# 合法 IPv4 CIDR 且前缀 ≤ 30（/31 /32 无网关语义，不适合作为容器网段）
_nm_valid_ipv4_cidr() {
  case "$1" in
    *[!0-9./]*) return 1 ;;
  esac
  _nm_addr="${1%%/*}"
  _nm_pfx="${1##*/}"
  [ "$_nm_pfx" = "$1" ] && return 1
  case "$_nm_pfx" in '' | *[!0-9]*) return 1 ;; esac
  [ "$_nm_pfx" -le 0 ] 2>/dev/null && return 1
  [ "$_nm_pfx" -gt 30 ] 2>/dev/null && return 1
  _nm_valid_ipv4 "$_nm_addr" || return 1
  return 0
}

# gateway 是否落在 subnet 内
_nm_gw_in_subnet() {
  _nm_gw="$1"; _nm_sub="$2"
  _nm_ip2int "$_nm_gw" >/dev/null 2>&1 || return 1
  _nm_gwint="$(_nm_ip2int "$_nm_gw")"
  _nm_subint="$(_nm_ip2int "${_nm_sub%%/*}")"
  _nm_pfx="${_nm_sub##*/}"
  # mask = (2^32 - 1) << (32 - pfx)，用 64 位算术避免 32 位溢出
  _nm_mask=$(( (4294967295 << (32 - _nm_pfx)) & 4294967295 ))
  [ $(( _nm_gwint & _nm_mask )) -eq $(( _nm_subint & _nm_mask )) ]
}

# ---------- 模式合法性校验 ----------
# 返回 0 合法；非 0 时由调用方决定退出码（entrypoint 统一 exit 12）
net_mode_validate() {
  _m="$(net_mode_normalize "${1:-}")"

  case "$_m" in
    "$NET_MODE_HOST")
      # host 模式忽略 L2 专属变量（老 .env 残留不应导致启动失败，只提示）
      for _v in IKEV2_L2_PARENT IKEV2_L2_SUBNET IKEV2_L2_GATEWAY \
               IKEV2_MACVLAN_MODE IKEV2_IPVLAN_MODE IKEV2_IPVLAN_FLAG; do
        eval "_val=\${$_v:-}"
        if [ -n "$_val" ]; then
          echo "WARN: host 模式下忽略 $_v=$_val" >&2
        fi
      done
      return 0
      ;;

    "$NET_MODE_MACVLAN" | "$NET_MODE_IPVLAN")
      _missing=""
      [ -n "${IKEV2_L2_PARENT:-}" ]  || _missing="$_missing IKEV2_L2_PARENT"
      [ -n "${IKEV2_L2_SUBNET:-}" ]  || _missing="$_missing IKEV2_L2_SUBNET"
      [ -n "${IKEV2_L2_GATEWAY:-}" ] || _missing="$_missing IKEV2_L2_GATEWAY"
      if [ -n "$_missing" ]; then
        echo "ERR: IKEV2_NETWORK_MODE=$_m 缺少必填变量:$_missing" >&2
        echo "     IKEV2_L2_PARENT  = 宿主父接口名（如 eth0 / ens18）" >&2
        echo "     IKEV2_L2_SUBNET  = 宿主所在 IPv4 子网（如 203.0.113.0/24，仅 IPv4）" >&2
        echo "     IKEV2_L2_GATEWAY = 该子网的宿主网关" >&2
        return 1
      fi

      [ "${IKEV2_L2_PARENT:-}" != "lo" ] || {
        echo "ERR: IKEV2_L2_PARENT 不能是 lo" >&2
        return 1
      }

      # IKEV2_L2_SUBNET 只接受 IPv4。填 v6 子网会让 Docker IPAM 接管 IPv6
      # 并默认分配 ULA(fd00::/8)，公网不可达，与「SLAAC 拿 GUA」的设计冲突。
      case "${IKEV2_L2_SUBNET}" in
        *:*)
          echo "ERR: IKEV2_L2_SUBNET 只支持 IPv4（IPv6 走 RA/SLAAC 自动获取）" >&2
          return 1
          ;;
      esac

      _nm_valid_ipv4_cidr "${IKEV2_L2_SUBNET}" || {
        echo "ERR: IKEV2_L2_SUBNET='${IKEV2_L2_SUBNET}' 不是合法 IPv4 CIDR（前缀需 1-30）" >&2
        return 1
      }

      _nm_gw_in_subnet "${IKEV2_L2_GATEWAY}" "${IKEV2_L2_SUBNET}" || {
        echo "ERR: IKEV2_L2_GATEWAY='${IKEV2_L2_GATEWAY}' 不在 IKEV2_L2_SUBNET='${IKEV2_L2_SUBNET}' 内" >&2
        return 1
      }

      if [ "$_m" = "$NET_MODE_MACVLAN" ]; then
        case "${IKEV2_MACVLAN_MODE:-bridge}" in
          bridge|private|vepa) ;;
          passthru)
            echo "ERR: IKEV2_MACVLAN_MODE=passthru 不支持 IPAM，无法给容器分配地址" >&2
            echo "     请改用 bridge（默认）/ private / vepa" >&2
            return 1
            ;;
          *)
            echo "ERR: IKEV2_MACVLAN_MODE='${IKEV2_MACVLAN_MODE:-}' 非法；合法值: bridge | private | vepa" >&2
            return 1
            ;;
        esac
      fi

      if [ "$_m" = "$NET_MODE_IPVLAN" ]; then
        case "${IKEV2_IPVLAN_MODE:-l2}" in
          l2|l3|l3s) ;;
          *)
            echo "ERR: IKEV2_IPVLAN_MODE='${IKEV2_IPVLAN_MODE:-}' 非法；合法值: l2 | l3 | l3s" >&2
            return 1
            ;;
        esac
        # l3 / l3s 丢弃广播与组播，IKEv2 多用户并发场景不友好
        if [ "${IKEV2_IPVLAN_MODE:-l2}" != "l2" ]; then
          echo "WARN: IKEV2_IPVLAN_MODE=${IKEV2_IPVLAN_MODE} 会丢弃广播/组播，IKEv2 场景强烈建议 l2" >&2
        fi
        case "${IKEV2_IPVLAN_FLAG:-bridge}" in
          bridge|private|vepa) ;;
          *)
            echo "ERR: IKEV2_IPVLAN_FLAG='${IKEV2_IPVLAN_FLAG:-}' 非法；合法值: bridge | private | vepa" >&2
            return 1
            ;;
        esac
      fi

      return 0
      ;;

    bridge)
      echo "ERR: IKEV2_NETWORK_MODE=bridge 已于 v2.86-PR14 移除" >&2
      echo "     合法值: $NET_MODE_HOST | $NET_MODE_MACVLAN | $NET_MODE_IPVLAN" >&2
      return 1
      ;;

    *)
      echo "ERR: IKEV2_NETWORK_MODE='${1:-}' 非法；合法值: $NET_MODE_HOST | $NET_MODE_MACVLAN | $NET_MODE_IPVLAN" >&2
      return 1
      ;;
  esac
}

# ---------- L2 sysctl 单一真源 ----------
# docker-compose.{macvlan,ipvlan}.yml 的 service 段 sysctls 块与本函数必须同值。
#
# 实测结论（Docker 28.5.2，macvlan/ipvlan，192.168.50.63 parent=ens18）：
#
# 1) 容器内 /proc/sys 是 ro 挂载，NET_ADMIN 也写不了
#       proc /proc/sys proc ro,nosuid,nodev,noexec,relatime
#    → 只能靠 Docker 建容器时注入，entrypoint 无权自愈。
#
# 2) driver option `com.docker.network.endpoint.sysctls` 在本场景**不生效**：
#    逗号串与 JSON 两种格式实测都是 v4fwd=1 v6fwd=0 ra=1 gua=0，
#    即只有 v4 forwarding 被动生效（那是 netns 默认值，不是我们注入的）。
#    且 networks.<name>.options 根本不是合法 Compose 字段，`docker compose config`
#    会直接报 additional properties 'options' not allowed。
#    → 唯一可用通道是 service 段 `sysctls:`。
#
# 3) service 段 sysctls 里 IFNAME 占位符不可用（Docker 报 unsafe procfs detected），
#    直接写 eth0 也被拒（must be supplied using driver option ...）。
#    → 只能写 conf.all / conf.default，由内核或 netns 新建接口时生效。
#
# 4) 必须配合 networks 段的 enable_ipv6: true，否则 Docker 建端点时把
#    disable_ipv6 置 1，收不到 RA（实测 dis6=1 gua=0）。
net_mode_sysctls() {
  cat <<'EOF'
net.ipv4.conf.all.forwarding=1
net.ipv6.conf.all.forwarding=1
net.ipv6.conf.default.accept_ra=2
EOF
}

# entrypoint 的运行时回读用：<sysctl 名>|<期望值>
# 逐条比对，不符合预期就 FATAL，绝不静默降级。
#
# 为什么 accept_ra 查 conf.eth0 而不是 conf.default：
#   default 只决定"此后新建接口"的取值，容器 eth0 建好后就固定在 eth0 上了。
#   回读必须读实际承载业务的那块网卡，否则校验的是一个没人用的值。
net_mode_sysctl_checks() {
  cat <<'EOF'
net/ipv4/conf/IFNAME/forwarding|1
net/ipv6/conf/IFNAME/forwarding|1
net/ipv6/conf/IFNAME/accept_ra|2
EOF
}
