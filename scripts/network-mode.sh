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

# ---------- 共享 L2 网络（v2.95）----------
#
# 背景：L2 网络原本由 compose 自己创建，名字是 `<project>_ikev2-l2` ——
# 换个 project / 换个目录就会变成"另一个网络"，而 Docker 的 IPAM 是全局的：
# 同子网再建一个直接 "Pool overlaps"，同 parent 再建一个 ipvlan 更狠 ——
# "network di-xxx is already using parent interface ens18"（ipvlan 的 parent 独占）。
#
# 所以 L2 网络必须有一个**固定的、跨 project 共享的名字**。
#
# 共享能力按 driver 分（实测 Docker 28.5.2 / Compose v2.40.3，192.168.50.63）：
#
#   ipvlan  — ✅ 可共享。子设备共享 parent 的 MAC，多个子设备共存无碍。
#             实测：同一 ipvlan 网络上同时挂 2 个容器（不同 project），
#             各自拿到 192.168.50.92 / .93，互 ping 0% loss。
#             ⚠️ 但 **parent 独占**：同一张网卡上建不了第二条 ipvlan 网络
#             （换子网也不行）。
#
#   macvlan — ✅ 也可共享。同一 parent 上可以有多条 macvlan 网络，
#             同一条 macvlan 网络上也能挂多个容器（各拿独立 MAC）。
#             实测：3 个容器（不同 project）挂同一条 macvlan 网络，
#             拿到 .2 / .3 / .4，MAC 各不相同。
#             ⚠️ 但**子网不能重叠**（Docker IPAM 全局）：
#             "invalid pool request: Pool overlaps with other one on this address space"。
#
# ⚠️ 曾有一版结论写成「macvlan 不可共享」，那是**误判** ——
#    当时在 63 上看到 "failed to create the macvlan port: device or resource busy"，
#    实际原因是**宿主 ens18 本身不支持 macvlan 设备**（手工
#    `ip link add link ens18 type macvlan` 同样报 Device or resource busy），
#    与"共享"无关。换成能建 macvlan 设备的 parent 后，同一条网络挂
#    3 个容器完全正常。**判据要看清楚是"驱动限制"还是"宿主限制"。**
#
# 默认网络名 = 驱动名（不带任何 project 标识）：
#   ipvlan  → "ipvlan"
#   macvlan → "macvlan"
NET_SHARED_NETWORK_NAME_DEFAULT_IPVLAN="ipvlan"
NET_SHARED_NETWORK_NAME_DEFAULT_MACVLAN="macvlan"

# compose networks 段的 **key**（不是网络名）。
#
# 为什么需要这个常量（v2.97）：
#   compose 复用一条**已存在**的同名网络时，会校验网络上的 label
#   `com.docker.compose.network` 是否等于 networks 段的 key（实测 63，
#   Docker 28.5.2 / Compose v2.40.3）：
#     不匹配 → "network ipvlan was found but has incorrect label
#              com.docker.compose.network set to "" (expected: "ikev2-l2")"
#   所以 scripts/up.sh 建网时必须带上这个 label，
#   否则 up.sh 建的网**纯 docker compose 路径复用不了**。
#
# 值必须与 docker-compose.{macvlan,ipvlan}.yml 里 networks 段的 key 一致
# （两份文件都是 ikev2-l2）。改 networks 段的 key 时这里要同步改，
# tests/network_mode_test.sh 的 NM-64 会强制校验两处一致。
NET_SHARED_NETWORK_COMPOSE_KEY="ikev2-l2"

# 按模式取默认网络名。
#
# 为什么不用一个全局常量：默认名要跟驱动一致（用户要求「自动创建的时候的
# 名字也是 macvlan 或 ipvlan」），而模式在运行时才确定。
net_shared_network_default_name() {
  case "${1:-}" in
    "$NET_MODE_MACVLAN") echo "$NET_SHARED_NETWORK_NAME_DEFAULT_MACVLAN" ;;
    "$NET_MODE_IPVLAN")  echo "$NET_SHARED_NETWORK_NAME_DEFAULT_IPVLAN" ;;
    *)                  echo "" ;;
  esac
}

# 共享网络的 IPAM 归**建网者**所有。
#
# 为什么这条这么重要：手工 `docker network create` 时若不带 --subnet，
# Docker 会塞一个默认池 172.18.0.0/16 进去。之后任何 join 方想指定
# ipv4_address 都会被拒：
#   "user specified IP address is supported only when connecting to
#    networks with user configured subnets"
# 也就是说，**建网时必须显式带 --subnet + --gateway**，否则共享网络没法分配固定地址。
#
# （建网/查网在宿主侧做，见 scripts/up.sh 的 _l2_shared_network_ensure。）

# 共享网络名合法性：Docker 网络名不允许空格 / 大写 / 特殊字符。
# 只拦真正会导致 daemon 报错的字符，不做过度限制。
net_shared_network_name_valid() {
  _n="${1:-}"
  [ -n "$_n" ] || return 1
  # 首字符必须是字母或数字
  case "$_n" in
    [0-9a-zA-Z]*) ;;
    *) return 1 ;;
  esac
  # 其余只允许字母数字与 . _ -
  case "$_n" in
    *[!0-9a-zA-Z._-]*) return 1 ;;
  esac
  return 0
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
      for _v in IKEV2_L2_PARENT IKEV2_L2_SUBNET IKEV2_L2_GATEWAY IKEV2_L2_IPV4 \
               IKEV2_MACVLAN_MODE IKEV2_IPVLAN_MODE IKEV2_IPVLAN_FLAG; do
        eval "_val=\${$_v:-}"
        if [ -n "$_val" ]; then
          echo "WARN: host 模式下忽略 $_v=$_val" >&2
        fi
      done
      if [ -n "${IKEV2_L2_NETWORK_NAME:-}" ]; then
        echo "WARN: host 模式下忽略 IKEV2_L2_NETWORK_NAME=${IKEV2_L2_NETWORK_NAME}" >&2
      fi
      if [ -n "${IKEV2_L2_EXTERNAL:-}" ]; then
        echo "WARN: host 模式下忽略 IKEV2_L2_EXTERNAL=${IKEV2_L2_EXTERNAL}" >&2
      fi
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

      # 固定容器 IPv4（可选）。留空 = Docker IPAM 自动分配，不校验。
      #
      # 校验放在这里而不是只靠 daemon 报错，是为了给出**指名变量**的提示 ——
      # daemon 只会说 "no configured subnet or ip-range contain the IP address"，
      # 不会告诉你是哪个变量填错了、应该填成什么样。
      if [ -n "${IKEV2_L2_IPV4:-}" ]; then
        _nm_valid_ipv4 "${IKEV2_L2_IPV4}" || {
          echo "ERR: IKEV2_L2_IPV4='${IKEV2_L2_IPV4}' 不是合法 IPv4 地址" >&2
          return 1
        }
        _nm_gw_in_subnet "${IKEV2_L2_IPV4}" "${IKEV2_L2_SUBNET}" || {
          echo "ERR: IKEV2_L2_IPV4='${IKEV2_L2_IPV4}' 不在 IKEV2_L2_SUBNET='${IKEV2_L2_SUBNET}' 内" >&2
          return 1
        }
        [ "${IKEV2_L2_IPV4}" != "${IKEV2_L2_GATEWAY}" ] || {
          echo "ERR: IKEV2_L2_IPV4='${IKEV2_L2_IPV4}' 不能与网关地址相同" >&2
          return 1
        }
      fi

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

      # 共享网络名（v2.95）。macvlan / ipvlan 都支持跨 project 共享。
      if [ -n "${IKEV2_L2_NETWORK_NAME:-}" ]; then
        net_shared_network_name_valid "${IKEV2_L2_NETWORK_NAME}" || {
          echo "ERR: IKEV2_L2_NETWORK_NAME='${IKEV2_L2_NETWORK_NAME}' 不是合法 Docker 网络名" >&2
          echo "     规则：首字符是字母或数字，其余只允许字母/数字/点/下划线/连字符" >&2
          return 1
        }
      fi

      # external 开关（v2.97）。compose 的 networks 段用它决定「谁建网」：
      #   false（默认）→ compose 自己建（纯 docker compose 路径可用）
      #   true          → compose 只加入（scripts/up.sh 建好并校验过后走这条）
      #
      # 非法值直接拒：compose 的插值是字符串塞进 YAML，值写错时
      # 报错信息会很难懂（"cannot unmarshal !!str `yes` into bool"），
      # 与其让用户去猜，不如在这里点名。
      case "${IKEV2_L2_EXTERNAL:-false}" in
        true|false) ;;
        *)
          echo "ERR: IKEV2_L2_EXTERNAL='${IKEV2_L2_EXTERNAL:-}' 非法；合法值: true | false" >&2
          echo "     false = 由 docker compose 自己建这条网络（默认，纯 compose 部署用这个）" >&2
          echo "     true  = 复用已存在的网络（scripts/up.sh 会自动设成 true）" >&2
          return 1
          ;;
      esac

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
# IPv6 的注入分两条通道（缺一不可）：
#
# 1) 容器内 /proc/sys 是 ro 挂载（实测 192.168.50.63）：
#       proc /proc/sys proc ro,nosuid,nodev,noexec,relatime
#    → 一切 sysctl 都只能靠 Docker 建容器时注入，entrypoint 无权自愈。
#
# 2) netns 级（本函数，= compose service 段 sysctls 块）：
#    v4/v6 forwarding（conf.all.*）+ default.accept_ra=2。
#    service 段 sysctls 只能写 conf.all / conf.default：写 IFNAME 占位符会被
#    Docker 以 unsafe procfs detected 拒绝，写 eth0 也被拒（must be supplied
#    using driver option ...）。conf.all.forwarding=1 由内核传播到所有接口。
#
# 3) 接口级（compose service networks 块的 driver_opts，endpoint sysctls）：
#    net.ipv6.conf.IFNAME.disable_ipv6=0,net.ipv6.conf.IFNAME.accept_ra=2
#    —— Docker 官方 macvlan/ipvlan 文档「Use router-assigned IPv6 addresses」
#    明确的唯一通道，IFNAME 由 Docker 挂端点时替换成真实网卡名。
#    ⚠️ 必须放在 **services.<服务>.networks.<网络>.driver_opts**（端点选项）。
#    放到 networks 段的 driver_opts（网络选项）会被静默忽略 ——
#    v2.86-PR20「endpoint sysctls 不生效」的实测结论即源于此，现予纠正。
#
# 4) 网络本身**不带 v6 子网、不开 enable_ipv6**：开的话 Docker 会从内置
#    ULA 池自动分一个 fd00::/64 段，容器 eth0 出现不路由的 ULA + 假默认路由。
#    IPv6 地址与默认路由一律来自上游路由器 RA（SLAAC），Docker 不经手。
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
# disable_ipv6 也在清单里：它由 endpoint sysctls（通道 3）保证，
# endpoint sysctls 若因 Docker 版本/放置位置问题没生效，这里会 FATAL exit 10。
net_mode_sysctl_checks() {
  cat <<'EOF'
net/ipv6/conf/IFNAME/disable_ipv6|0
net/ipv4/conf/IFNAME/forwarding|1
net/ipv6/conf/IFNAME/forwarding|1
net/ipv6/conf/IFNAME/accept_ra|2
EOF
}
