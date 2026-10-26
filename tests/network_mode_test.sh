#!/usr/bin/env bash
# tests/network_mode_test.sh — scripts/network-mode.sh 的 L1 单元测试
#
# 纯 bash + POSIX sh，不依赖 bats / docker / 网络，可在 CI 直接跑。
# 对应技术方案 §9.1 ① 的 NM-01 ~ NM-19。
#
# 用法:
#   ./tests/network_mode_test.sh
#   bash tests/network_mode_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NETMODE_SH="${SCRIPT_DIR}/../scripts/network-mode.sh"
ENTRYPOINT_SH="${SCRIPT_DIR}/../scripts/entrypoint.sh"

PASS=0
FAIL=0
FAILED_IDS=()

_red()   { printf '\033[0;31m%s\033[0m' "$1"; }
_green() { printf '\033[0;32m%s\033[0m' "$1"; }

ok()   { PASS=$((PASS+1)); printf '  %s %s\n' "$(_green '✓')" "$1"; }
bad()  { FAIL=$((FAIL+1)); FAILED_IDS+=("$1"); printf '  %s %s — %s\n' "$(_red '✗')" "$1" "${2:-}"; }

[ -f "$NETMODE_SH" ] || { echo "FATAL: $NETMODE_SH not found"; exit 1; }

# 在干净子 shell 里跑 net_mode_validate，捕获 stdout+stderr 与返回码。
# 参数形如 MODE=macvlan IKEV2_L2_PARENT=eth0 ...
# 未列出的 IKEV2_* 全部 unset，保证每个用例互不污染。
validate_rc() {
  (
    unset IKEV2_L2_PARENT IKEV2_L2_SUBNET IKEV2_L2_GATEWAY IKEV2_L2_IPV4
    unset IKEV2_L2_NETWORK_NAME IKEV2_L2_EXTERNAL
    unset IKEV2_MACVLAN_MODE IKEV2_IPVLAN_MODE IKEV2_IPVLAN_FLAG
    unset IKEV2_L2_IPV6_SYSCTL IKEV2_SERVER_ADDR_V6
    while [ "$#" -gt 0 ]; do
      case "$1" in
        MODE=*)      MODE="${1#MODE=}" ;;
        IKEV2_*)     export "$1" ;;
      esac
      shift
    done
    # shellcheck disable=SC1090
    . "$NETMODE_SH"
    net_mode_validate "$MODE"
  ) >/tmp/nm_test_out.$$ 2>&1
  RC=$?
  OUT="$(cat /tmp/nm_test_out.$$)"
  rm -f /tmp/nm_test_out.$$
  return 0
}

echo "== network-mode.sh 单元测试 =="

# ---------- NM-01 host 无 L2 变量 ----------
validate_rc "MODE=host"
if [ "$RC" -eq 0 ] && [ -z "$OUT" ]; then
  ok "NM-01 host 无 L2 变量 → 0，无输出"
else
  bad "NM-01 host 无 L2 变量" "rc=$RC out=$OUT"
fi

# ---------- NM-02 空值归一化 ----------
NORM="$(MODE='' bash -c ". '$NETMODE_SH'; net_mode_normalize ''")"
if [ "$NORM" = "host" ]; then
  ok "NM-02 空值归一化为 host"
else
  bad "NM-02 空值归一化为 host" "got=$NORM"
fi

# ---------- NM-03 host + 遗留 L2 变量 → 0 + WARN ----------
validate_rc "MODE=host" "IKEV2_L2_PARENT=eth0"
if [ "$RC" -eq 0 ] && printf '%s' "$OUT" | grep -q "WARN: host 模式下忽略 IKEV2_L2_PARENT"; then
  ok "NM-03 host + 残留 IKEV2_L2_PARENT → 0 + WARN"
else
  bad "NM-03 host + 残留 IKEV2_L2_PARENT" "rc=$RC out=$OUT"
fi

# ---------- NM-04 bridge → 1 ----------
validate_rc "MODE=bridge"
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "host | macvlan | ipvlan"; then
  ok "NM-04 bridge → 非 0，文案列出三个合法值"
else
  bad "NM-04 bridge" "rc=$RC out=$OUT"
fi

# ---------- NM-05 大写归一化 ----------
NORM="$(MODE='  MACVLAN  ' bash -c ". '$NETMODE_SH'; net_mode_normalize \"\$MODE\"")"
if [ "$NORM" = "macvlan" ]; then
  ok "NM-05 大写+空白归一化"
else
  bad "NM-05 大写+空白归一化" "got=$NORM"
fi

# ---------- NM-06 缺 PARENT ----------
validate_rc "MODE=macvlan" "IKEV2_L2_SUBNET=203.0.113.0/24" "IKEV2_L2_GATEWAY=203.0.113.1"
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "IKEV2_L2_PARENT"; then
  ok "NM-06 macvlan 缺 PARENT → 非 0，指出变量名"
else
  bad "NM-06 macvlan 缺 PARENT" "rc=$RC out=$OUT"
fi

# ---------- NM-07 缺 SUBNET ----------
validate_rc "MODE=macvlan" "IKEV2_L2_PARENT=eth0" "IKEV2_L2_GATEWAY=203.0.113.1"
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "IKEV2_L2_SUBNET"; then
  ok "NM-07 macvlan 缺 SUBNET → 非 0"
else
  bad "NM-07 macvlan 缺 SUBNET" "rc=$RC out=$OUT"
fi

# ---------- NM-08 缺 GATEWAY ----------
validate_rc "MODE=macvlan" "IKEV2_L2_PARENT=eth0" "IKEV2_L2_SUBNET=203.0.113.0/24"
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "IKEV2_L2_GATEWAY"; then
  ok "NM-08 macvlan 缺 GATEWAY → 非 0"
else
  bad "NM-08 macvlan 缺 GATEWAY" "rc=$RC out=$OUT"
fi

# ---------- NM-09 gateway 不在 subnet 内 ----------
validate_rc "MODE=macvlan" "IKEV2_L2_PARENT=eth0" "IKEV2_L2_SUBNET=203.0.113.0/24" "IKEV2_L2_GATEWAY=10.0.0.1"
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "不在"; then
  ok "NM-09 gateway 不在 subnet 内 → 非 0"
else
  bad "NM-09 gateway 不在 subnet 内" "rc=$RC out=$OUT"
fi

# ---------- NM-09b gateway 在 subnet 内 → 0 ----------
validate_rc "MODE=macvlan" "IKEV2_L2_PARENT=eth0" "IKEV2_L2_SUBNET=203.0.113.0/24" "IKEV2_L2_GATEWAY=203.0.113.254"
if [ "$RC" -eq 0 ] && [ -z "$OUT" ]; then
  ok "NM-09b gateway 在 subnet 内 → 0，无输出"
else
  bad "NM-09b gateway 在 subnet 内" "rc=$RC out=$OUT"
fi

# ---------- NM-10 macvlan passthru ----------
validate_rc "MODE=macvlan" "IKEV2_L2_PARENT=eth0" "IKEV2_L2_SUBNET=203.0.113.0/24" \
                  "IKEV2_L2_GATEWAY=203.0.113.1" "IKEV2_MACVLAN_MODE=passthru"
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "passthru"; then
  ok "NM-10 macvlan passthru → 非 0"
else
  bad "NM-10 macvlan passthru" "rc=$RC out=$OUT"
fi

# ---------- NM-11 ipvlan l3 → 0 + WARN ----------
validate_rc "MODE=ipvlan" "IKEV2_L2_PARENT=eth0" "IKEV2_L2_SUBNET=203.0.113.0/24" \
                 "IKEV2_L2_GATEWAY=203.0.113.1" "IKEV2_IPVLAN_MODE=l3"
if [ "$RC" -eq 0 ] && printf '%s' "$OUT" | grep -q "WARN"; then
  ok "NM-11 ipvlan l3 → 0 + WARN"
else
  bad "NM-11 ipvlan l3" "rc=$RC out=$OUT"
fi

# ---------- NM-12 ipvlan 非法 mode ----------
validate_rc "MODE=ipvlan" "IKEV2_L2_PARENT=eth0" "IKEV2_L2_SUBNET=203.0.113.0/24" \
                 "IKEV2_L2_GATEWAY=203.0.113.1" "IKEV2_IPVLAN_MODE=bogus"
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "l2 | l3 | l3s"; then
  ok "NM-12 ipvlan 非法 mode → 非 0，列出合法值"
else
  bad "NM-12 ipvlan 非法 mode" "rc=$RC out=$OUT"
fi

# ---------- NM-13 ipvlan 三项齐全 → 0 ----------
validate_rc "MODE=ipvlan" "IKEV2_L2_PARENT=eth0" "IKEV2_L2_SUBNET=203.0.113.0/24" \
                 "IKEV2_L2_GATEWAY=203.0.113.1" "IKEV2_IPVLAN_FLAG=bridge"
if [ "$RC" -eq 0 ] && [ -z "$OUT" ]; then
  ok "NM-13 ipvlan 三项齐全 → 0，无输出"
else
  bad "NM-13 ipvlan 三项齐全" "rc=$RC out=$OUT"
fi

# ---------- NM-14 parent=lo ----------
validate_rc "MODE=macvlan" "IKEV2_L2_PARENT=lo" "IKEV2_L2_SUBNET=203.0.113.0/24" "IKEV2_L2_GATEWAY=203.0.113.1"
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "不能是 lo"; then
  ok "NM-14 parent=lo → 非 0"
else
  bad "NM-14 parent=lo" "rc=$RC out=$OUT"
fi

# ---------- NM-15 v6 子网被拒 ----------
validate_rc "MODE=macvlan" "IKEV2_L2_PARENT=eth0" "IKEV2_L2_SUBNET=2001:db8::/64" "IKEV2_L2_GATEWAY=203.0.113.1"
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "只支持 IPv4"; then
  ok "NM-15 v6 子网被拒"
else
  bad "NM-15 v6 子网被拒" "rc=$RC out=$OUT"
fi

# ---------- NM-16 /31 前缀被拒 ----------
validate_rc "MODE=macvlan" "IKEV2_L2_PARENT=eth0" "IKEV2_L2_SUBNET=203.0.113.0/31" "IKEV2_L2_GATEWAY=203.0.113.1"
if [ "$RC" -ne 0 ]; then
  ok "NM-16 /31 前缀被拒"
else
  bad "NM-16 /31 前缀被拒" "rc=$RC out=$OUT"
fi

# ---------- 能力开关 ----------
cap() {
  (
    # shellcheck disable=SC1090
    . "$NETMODE_SH"
    "$1" "$2"
  ) >/dev/null 2>&1
  echo $?
}

if [ "$(cap net_mode_has_docker_user host)" = "0" ] \
   && [ "$(cap net_mode_has_docker_user macvlan)" = "1" ] \
   && [ "$(cap net_mode_has_docker_user ipvlan)" = "1" ]; then
  ok "NM-17 net_mode_has_docker_user：仅 host 为真"
else
  bad "NM-17 net_mode_has_docker_user" \
      "host=$(cap net_mode_has_docker_user host) mac=$(cap net_mode_has_docker_user macvlan) ipv=$(cap net_mode_has_docker_user ipvlan)"
fi

if [ "$(cap net_mode_procsys_rw host)" = "1" ] \
   && [ "$(cap net_mode_procsys_rw macvlan)" = "0" ] \
   && [ "$(cap net_mode_procsys_rw ipvlan)" = "0" ]; then
  ok "NM-18 net_mode_procsys_rw：仅 L2 为真"
else
  bad "NM-18 net_mode_procsys_rw" \
      "host=$(cap net_mode_procsys_rw host) mac=$(cap net_mode_procsys_rw macvlan)"
fi

if [ "$(cap net_mode_has_own_ip host)" = "1" ] \
   && [ "$(cap net_mode_has_own_ip macvlan)" = "0" ] \
   && [ "$(cap net_mode_has_own_ip ipvlan)" = "0" ]; then
  ok "NM-19 net_mode_has_own_ip：仅 L2 为真"
else
  bad "NM-19 net_mode_has_own_ip" \
      "host=$(cap net_mode_has_own_ip host) mac=$(cap net_mode_has_own_ip macvlan)"
fi

# ---------- DDoS 目标链 ----------
DDOS_HOST="$(bash -c ". '$NETMODE_SH'; net_mode_ddos_chain host")"
DDOS_MAC="$(bash -c ". '$NETMODE_SH'; net_mode_ddos_chain macvlan")"
DDOS_IPV="$(bash -c ". '$NETMODE_SH'; net_mode_ddos_chain ipvlan")"
if [ "$DDOS_HOST" = "DOCKER-USER" ] && [ "$DDOS_MAC" = "INPUT" ] && [ "$DDOS_IPV" = "INPUT" ]; then
  ok "NM-21 net_mode_ddos_chain：host=DOCKER-USER，L2=INPUT"
else
  bad "NM-21 net_mode_ddos_chain" "host=$DDOS_HOST mac=$DDOS_MAC ipv=$DDOS_IPV"
fi

# ---------- L2 sysctl 单一真源（最关键的一条）----------
# netns 级：service 段 sysctls（forwarding + default.accept_ra）；
# 接口级：endpoint sysctls（见 NM-38）。两条通道缺一不可。
SYSC="$(bash -c ". '$NETMODE_SH'; net_mode_sysctls")"
if printf '%s' "$SYSC" | grep -q "net.ipv4.conf.all.forwarding=1" \
   && printf '%s' "$SYSC" | grep -q "net.ipv6.conf.all.forwarding=1" \
   && printf '%s' "$SYSC" | grep -q "net.ipv6.conf.default.accept_ra=2"; then
  ok "NM-22 net_mode_sysctls 含 v4/v6 forwarding + default.accept_ra=2"
else
  bad "NM-22 net_mode_sysctls" "got=$SYSC"
fi

# 回读清单必须覆盖全部四条，且用 IFNAME 占位（运行时替换成真实网卡名）
CHK="$(bash -c ". '$NETMODE_SH'; net_mode_sysctl_checks")"
if printf '%s' "$CHK" | grep -q "net/ipv6/conf/IFNAME/disable_ipv6|0" \
   && printf '%s' "$CHK" | grep -q "net/ipv4/conf/IFNAME/forwarding|1" \
   && printf '%s' "$CHK" | grep -q "net/ipv6/conf/IFNAME/forwarding|1" \
   && printf '%s' "$CHK" | grep -q "net/ipv6/conf/IFNAME/accept_ra|2"; then
  ok "NM-37 net_mode_sysctl_checks 覆盖四条回读项（含 disable_ipv6）"
else
  bad "NM-37 net_mode_sysctl_checks" "got=$CHK"
fi

# ---------- 三个 compose 文件的网络声明形态 ----------
# v2.86-PR20 改为「三模式 = 三个独立完整文件」，不再用 !reset。
BASE="${SCRIPT_DIR}/../docker-compose.yml"
MACV="${SCRIPT_DIR}/../docker-compose.macvlan.yml"
IPVL="${SCRIPT_DIR}/../docker-compose.ipvlan.yml"

# host：network_mode: host（老用户 docker compose up -d 零改动）
if grep -qE '^[[:space:]]*network_mode:[[:space:]]*host[[:space:]]*$' "$BASE"; then
  ok "NM-24 docker-compose.yml 保留 network_mode: host"
else
  bad "NM-24 docker-compose.yml 保留 network_mode: host" "not found"
fi

# L2 两个文件：挂 networks，且绝不能有 network_mode（两者互斥）
for p in "$MACV" "$IPVL"; do
  f="$(basename "$p")"
  if grep -qE '^[[:space:]]*networks:[[:space:]]*$' "$p"; then
    ok "NM-26 ${f} 挂 networks"
  else
    bad "NM-26 ${f} 挂 networks" "not found"
  fi
  if sed -e 's/[[:space:]]*#.*$//' "$p" | grep -qE '^[[:space:]]*network_mode:'; then
    bad "NM-26 ${f} 不含 network_mode" "found one (与 networks 互斥)"
  else
    ok "NM-26 ${f} 不含 network_mode"
  fi
  if grep -q "ikev2-l2" "$p"; then
    ok "NM-26 ${f} 引用 ikev2-l2 网络"
  else
    bad "NM-26 ${f} 引用 ikev2-l2 网络" "not found"
  fi
done

# ---------- 三个文件都不含 !reset / !override（独立文件不需要）----------
# 只看非注释行 —— 注释里提到这两个标签是正常的（说明为什么不用它们）
for p in "$BASE" "$MACV" "$IPVL"; do
  f="$(basename "$p")"
  if sed -e 's/[[:space:]]*#.*$//' "$p" | grep -qE '!(reset|override)'; then
    bad "NM-25 ${f} 不含 !reset/!override" "found one in a non-comment line"
  else
    ok "NM-25 ${f} 不含 !reset/!override"
  fi
done

# ---------- L2 IPv6 注入点与 compose 文件一致（RA/SLAAC 官方模式）----------
# Docker 官方 macvlan/ipvlan 文档「Use router-assigned IPv6 addresses」：
#   - 网络不带 v6 子网（不开 enable_ipv6）→ Docker 建端点时把 disable_ipv6 置 1
#   - 接口级 sysctl 只能走 endpoint driver_opts（service networks 块）：
#     com.docker.network.endpoint.sysctls=IFNAME.disable_ipv6=0,IFNAME.accept_ra=2
#   - 端点级选项必须在 services.<服务>.networks.<网络>.driver_opts，
#     放到 networks 段会被静默忽略（v2.86-PR20 实测误判的根源）
#
# ⚠️ 本测试开了 pipefail（见文件头），「剥注释后 grep -q」必须先存变量再 grep
#    （sed|grep -q 会在命中时让上游 sed 吃 SIGPIPE，时好时坏，见 NM-49 处说明）。
MACV_CODE="$(sed -e 's/[[:space:]]*#.*$//' "$MACV")"
IPVL_CODE="$(sed -e 's/[[:space:]]*#.*$//' "$IPVL")"
for _pair in "macvlan:$MACV_CODE" "ipvlan:$IPVL_CODE"; do
  _drv="${_pair%%:*}"
  _code="${_pair#*:}"
  _name_map="macvlan:docker-compose.macvlan.yml ipvlan:docker-compose.ipvlan.yml"
  f="${_name_map##*$_drv:}"; f="${f%% *}"
  # 1) service networks 块必须带 endpoint sysctls（非注释行）
  if printf '%s' "$_code" | grep -q "com.docker.network.endpoint.sysctls"; then
    ok "NM-38 ${f} service networks 带 endpoint.sysctls"
  else
    bad "NM-38 ${f} service networks 带 endpoint.sysctls" "not found"
  fi
  # 2) endpoint sysctls 必须同时打开 disable_ipv6=0 与 accept_ra=2（IFNAME 占位）
  if printf '%s' "$_code" \
     | grep -q 'com.docker.network.endpoint.sysctls:.*IFNAME.disable_ipv6=0,net.ipv6.conf.IFNAME.accept_ra=2'; then
    ok "NM-38 ${f} endpoint.sysctls 含 disable_ipv6=0 + accept_ra=2"
  else
    bad "NM-38 ${f} endpoint.sysctls 含 disable_ipv6=0 + accept_ra=2" "not found"
  fi
  # 3) networks 段绝不能开 enable_ipv6 —— 开了 Docker 会自动塞 ULA 池，
  #    容器 eth0 出现不路由的 ULA + 假默认路由，与「RA 路由器下发」冲突
  if printf '%s' "$_code" | grep -qE '^[[:space:]]*enable_ipv6:[[:space:]]*'; then
    bad "NM-28 ${f} networks 段不含 enable_ipv6" "found one（IPv6 必须走 RA/SLAAC，不能开 enable_ipv6）"
  else
    ok "NM-28 ${f} networks 段不含 enable_ipv6"
  fi
done
unset MACV_CODE IPVL_CODE

for p in "$MACV" "$IPVL"; do
  f="$(basename "$p")"
  # service 段 sysctls 三条齐全（值必须与 net_mode_sysctls() 一致）
  missing=""
  grep -qE '^[[:space:]]*net\.ipv4\.conf\.all\.forwarding:[[:space:]]*"1"[[:space:]]*$' "$p" \
    || missing="$missing net.ipv4.conf.all.forwarding=1"
  grep -qE '^[[:space:]]*net\.ipv6\.conf\.all\.forwarding:[[:space:]]*"1"[[:space:]]*$' "$p" \
    || missing="$missing net.ipv6.conf.all.forwarding=1"
  grep -qE '^[[:space:]]*net\.ipv6\.conf\.default\.accept_ra:[[:space:]]*"2"[[:space:]]*$' "$p" \
    || missing="$missing net.ipv6.conf.default.accept_ra=2"
  if [ -z "$missing" ]; then
    ok "NM-23 ${f} service 段 sysctls 三条齐全"
  else
    bad "NM-23 ${f} service 段 sysctls 三条齐全" "missing:$missing"
  fi
done

# host 文件绝不能有 service 段 sysctls —— runc 会以
# "not allowed in host network namespace" 拒绝，容器直接起不来。
if sed -e 's/[[:space:]]*#.*$//' "$BASE" | grep -qE '^[[:space:]]*sysctls:[[:space:]]*$'; then
  bad "NM-39 docker-compose.yml 无 service 段 sysctls" "found (host 模式下 runc 会拒绝)"
else
  ok "NM-39 docker-compose.yml 无 service 段 sysctls"
fi

# ================================================================
# 三文件同步守护（v2.86-PR20）
#
# 三个 compose 文件是**完整副本**而不是 override，所以「改一处漏两处」
# 是这套方案唯一的结构性风险。后果不是报错，而是：
#   - 卷名不一致 → 某个模式下静默新建空卷 → panel.db / LE 证书全丢
#     （v2-72 与 PR19 已各真实踩过一次，见 compose 注释里的事故记录）
#   - capabilities 不一致 → 某个模式下静默缺 FOWNER → chmod 失败 → 无限重启
#   - environment 不一致 → 某个模式下变量丢失 → 行为悄悄不同
#
# 所以这里逐项比对三份文件的关键配置。比对前先剥掉注释与空行，
# 只看真正生效的配置行。
# ================================================================

# 抽取指定 key 的值。标量（image: x）取同行内容；块（cap_add:）取缩进更深的所有行。
# 用缩进判定块的结束：块的起始行缩进为 keyind，之后所有缩进 > keyind 的行
# 都属于该块，遇到缩进 <= keyind 的非注释行即结束。
# （不能用 "下一个 ^[a-z_]+: 就结束" 这种简化规则 —— 会跨块抓到相邻配置，
#   比如 cap_drop 的块会一路吃到 cap_add。）
extract_block() {
  # $1=文件 $2=key（如 "cap_add"）
  awk -v key="$2" '
    function ind(s,   n) { n = match(s, /[^ ]/); return n == 0 ? -1 : n - 1 }
    {
      cur = ind($0)
      if (inblk) {
        if (cur == -1) next                              # 块内空行
        if ($0 ~ /^[[:space:]]*#/) next                  # 块内注释
        if (cur <= keyind) { inblk = 0 }                # 缩进回退 = 块结束
        else {
          line = $0
          gsub(/^[[:space:]]+/, "", line)
          gsub(/[[:space:]]+/, " ", line)
          sub(/[[:space:]]*#.*$/, "", line)              # 去行尾注释，只比生效内容
          gsub(/[[:space:]]+$/, "", line)
          if (line != "") print line
          next
        }
      }
      # 先判「key 后面同行还有值」= 标量
      if ($0 ~ ("^[[:space:]]*" key ":[[:space:]]*[^[:space:]]")) {
        line = $0
        sub("^[[:space:]]*" key ":[[:space:]]*", "", line)
        gsub(/[[:space:]]*#.*$/, "", line)               # 去行尾注释
        gsub(/[[:space:]]+$/, "", line)
        if (line != "") print line
        next
      }
      # 否则 = 块起始
      if ($0 ~ ("^[[:space:]]*" key ":[[:space:]]*$")) {
        match($0, /[^ ]/); keyind = RSTART - 1
        inblk = 1
      }
    }
  ' "$1"
}

# ---------- NM-29 image / container_name / restart 三份一致 ----------
for key in "image" "container_name" "restart" "pull_policy"; do
  a="$(extract_block "$BASE" "$key")"
  b="$(extract_block "$MACV" "$key")"
  c="$(extract_block "$IPVL" "$key")"
  if [ -n "$a" ] && [ "$a" = "$b" ] && [ "$a" = "$c" ]; then
    ok "NM-29 三份 ${key} 一致（${a}）"
  else
    bad "NM-29 三份 ${key} 一致" "host='${a}' macvlan='${b}' ipvlan='${c}'"
  fi
done

# ---------- NM-30 capabilities 三份一致（漏 FOWNER 会导致无限重启）----------
for key in "cap_add" "cap_drop" "devices" "security_opt"; do
  a="$(extract_block "$BASE" "$key" | sort | tr '\n' ' ')"
  b="$(extract_block "$MACV" "$key" | sort | tr '\n' ' ')"
  c="$(extract_block "$IPVL" "$key" | sort | tr '\n' ' ')"
  if [ -n "$a" ] && [ "$a" = "$b" ] && [ "$a" = "$c" ]; then
    ok "NM-30 三份 ${key} 一致（${a}）"
  else
    bad "NM-30 三份 ${key} 一致" "host=[${a}] macvlan=[${b}] ipvlan=[${c}]"
  fi
done

# ---------- NM-31 顶层 volumes（含 name: ikev2-panel-data）三份一致 ----------
# 这是最重要的一项：卷名漂移 = 静默丢 panel.db / LE 证书
#
# ⚠️ external: 的检查只针对 **volumes 段**（v2.95 起 networks 段合法使用
# external: true —— 共享 L2 网络由宿主侧管理，compose 不建也不删）。
# 之前这条是全文件 grep，v2.95 加了 networks 的 external 后误报。
# 卷上的 external 依然必须禁：external volume 要求宿主先手建这个卷，
# 否则 docker compose up 直接失败，老用户升级即挂。
for p in "$BASE" "$MACV" "$IPVL"; do
  f="$(basename "$p")"
  if grep -qE '^[[:space:]]*name:[[:space:]]*ikev2-panel-data[[:space:]]*$' "$p"; then
    ok "NM-31 ${f} 顶层卷名锁定 ikev2-panel-data"
  else
    bad "NM-31 ${f} 顶层卷名锁定 ikev2-panel-data" "not found"
  fi
  # 只看 volumes 段：从 volumes: 到下一个顶层 key（networks:）为止
  _vol_ext="$(sed -n '/^volumes:/,/^networks:/p' "$p" | grep -cE '^[[:space:]]*external:' || true)"
  if [ "${_vol_ext:-0}" -eq 0 ]; then
    ok "NM-31 ${f} volumes 段无 external:"
  else
    bad "NM-31 ${f} volumes 段无 external:" "found ${_vol_ext}"
  fi
done

# ---------- NM-32 services.volumes 三份一致（长语法三挂载）----------
for p in "$BASE" "$MACV" "$IPVL"; do
  f="$(basename "$p")"
  miss=""
  grep -q 'source: ikev2-data'   "$p" || miss="$miss /data"
  grep -q 'target: /data'        "$p" || miss="$miss /data:target"
  grep -q './logs:/var/log:rw'   "$p" || miss="$miss logs"
  grep -q '/etc/sysctl.d:/host-sysctl.d:rw' "$p" || miss="$miss sysctl.d"
  if [ -z "$miss" ]; then
    ok "NM-32 ${f} 三条挂载齐全"
  else
    bad "NM-32 ${f} 三条挂载齐全" "missing:${miss}"
  fi
done

# ---------- NM-33 tmpfs 三份一致 + tun/no-new-privileges 在位 ----------
a="$(extract_block "$BASE" "tmpfs" | sort | tr '\n' ' ')"
b="$(extract_block "$MACV" "tmpfs" | sort | tr '\n' ' ')"
c="$(extract_block "$IPVL" "tmpfs" | sort | tr '\n' ' ')"
if [ -n "$a" ] && [ "$a" = "$b" ] && [ "$a" = "$c" ]; then
  ok "NM-33 三份 tmpfs 一致（$(printf '%s' "$a" | wc -w | tr -d ' ') 条）"
else
  bad "NM-33 三份 tmpfs 一致" "host=[${a}] macvlan=[${b}] ipvlan=[${c}]"
fi

# ---------- NM-34 environment 键集合三份一致（只比键名，不比值）----------
# 值里带模式默认值（IKEV2_NETWORK_MODE）与模式专属变量，允许不同；
# 但**键的集合**必须一致，漏一个键意味着某个模式下变量根本没透传。
# environment 块的键是 6 空格缩进的 KEY: value，块结束靠缩进回退到 4 空格。
env_keys() {
  awk '
    function ind(s,   n) { n = match(s, /[^ ]/); return n == 0 ? -1 : n - 1 }
    {
      cur = ind($0)
      if (inblk) {
        if (cur == -1) next
        if ($0 ~ /^[[:space:]]*#/) next
        if (cur <= keyind) { inblk = 0 }
        else {
          line = $0
          sub(/^[[:space:]]+/, "", line)
          # 只取 KEY: 形式的行（environment 下全是这种；list 形式不会出现）
          if (line ~ /^[A-Za-z_][A-Za-z0-9_]*:/) {
            sub(/:.*$/, "", line)
            print line
          }
          next
        }
      }
      if ($0 ~ /^    environment:[[:space:]]*$/) { inblk = 1; keyind = 4 }
    }
  ' "$1" | sort -u
}
ka="$(env_keys "$BASE")"
kb="$(env_keys "$MACV")"
kc="$(env_keys "$IPVL")"
# 先各自断言非空，防止「三份都空」被误判为一致
for pair in "host:$ka" "macvlan:$kb" "ipvlan:$kc"; do
  name="${pair%%:*}"; keys="${pair#*:}"
  if [ -n "$keys" ]; then
    ok "NM-34 ${name} environment 键非空（$(printf '%s\n' "$keys" | wc -l | tr -d ' ') 个）"
  else
    bad "NM-34 ${name} environment 键非空" "extractor matched nothing"
  fi
done
if [ -n "$ka" ] && [ "$ka" = "$kb" ] && [ "$ka" = "$kc" ]; then
  ok "NM-34 三份 environment 键集合一致"
else
  bad "NM-34 三份 environment 键集合一致" \
      "仅host有: $(comm -23 <(printf '%s\n' "$ka") <(printf '%s\n' "$kb"; printf '%s\n' "$kc") | sort -u | tr '\n' ' ') | 仅macvlan有: $(comm -23 <(printf '%s\n' "$kb") <(printf '%s\n' "$ka"; printf '%s\n' "$kc") | sort -u | tr '\n' ' ')"
fi

# ---------- NM-35 关键 environment 变量三份都在 ----------
for v in IKEV2_DATA_DIR IKEV2_SERVER_CN IKEV2_OUT_IF IKEV2_NETWORK_MODE \
         IKEV2_L2_PARENT IKEV2_L2_SUBNET IKEV2_L2_GATEWAY IKEV2_SERVER_ADDR_V6 \
         IKEV2_VPN_SUBNET IKEV2_CERT_MODE IKEV2_DOMAIN Ali_Key Ali_Secret; do
  miss=""
  for p in "$BASE" "$MACV" "$IPVL"; do
    grep -qE "^[[:space:]]*${v}:" "$p" || miss="$miss $(basename "$p")"
  done
  if [ -z "$miss" ]; then
    ok "NM-35 ${v} 三份透传"
  else
    bad "NM-35 ${v} 三份透传" "missing in:${miss}"
  fi
done

# ---------- NM-36 IKEV2_NETWORK_MODE 必须硬编码，且与文件自身模式一致 ----------
# 这一项防的是「声明与实际 netns 不一致」。
#
# 早期写法是 ${IKEV2_NETWORK_MODE:-macvlan}，看起来安全，实际有洞：
# `.env` 里残留的 IKEV2_NETWORK_MODE=host 会**覆盖**默认值，于是
#   「容器实际在 macvlan netns 里（compose 文件说了算）+ entrypoint 按 host 分支跑」
# 后果：DDoS 去找 macvlan 容器里不存在的 DOCKER-USER 链（静默跳过防护）、
#       persist_sysctl 误写宿主的 /etc/sysctl.d/99-ikev2.conf、
#       L2 的 SLAAC 运行时复核整个被跳过（RA 被丢弃也无人察觉）。
#
# 网络由**文件**决定，变量必须服从文件。所以三份都必须是硬编码字面量，
# 且各自等于自己的模式名。
declare -A EXPECT_MODE=(
  ["docker-compose.yml"]="host"
  ["docker-compose.macvlan.yml"]="macvlan"
  ["docker-compose.ipvlan.yml"]="ipvlan"
)
for p in "$BASE" "$MACV" "$IPVL"; do
  f="$(basename "$p")"
  want="${EXPECT_MODE[$f]}"
  raw="$(grep -E '^[[:space:]]*IKEV2_NETWORK_MODE:' "$p" | head -1 | sed -e 's/^[[:space:]]*IKEV2_NETWORK_MODE:[[:space:]]*//' -e 's/[[:space:]]*$//')"
  # 1) 必须是纯字面量，不含 ${...} 插值
  if printf '%s' "$raw" | grep -q '\${'; then
    bad "NM-36 ${f} IKEV2_NETWORK_MODE 硬编码为 ${want}" "found variable interpolation: ${raw}"
  # 2) 字面量值必须等于本文件对应的模式
  elif [ "$raw" = "\"$want\"" ] || [ "$raw" = "'$want'" ]; then
    ok "NM-36 ${f} IKEV2_NETWORK_MODE 硬编码为 ${want}"
  else
    bad "NM-36 ${f} IKEV2_NETWORK_MODE 硬编码为 ${want}" "got ${raw}"
  fi
done

# ---------- NM-57 L2 三变量的单一真源：anchor，environment 与 networks 同源 ----------
#
# 背景（真实故障）：同一个 L2 变量在 compose 文件里出现两次，走两条
# **互相独立**的插值路径 ——
#   - networks 段的 parent/subnet/gateway → 插值结果给 Docker daemon 建网
#   - environment 段的同名三项           → 插值结果作为容器 env 传进去
# entrypoint 读的是**容器里**那一份。
#
# 两段只要语法不同（一边 `:-` 一边 `:?`、或一边有默认值一边没有），
# 就会出现这个故障：
#   compose 插值通过 → 容器创建成功 → 容器里三个变量是**空串**
#   → entrypoint net_mode_validate 判「缺少必填变量」→ exit 12
#   → restart: unless-stopped 无限重启，日志反复刷同一行 ERR，
#     根因（变量压根没传进容器）从日志上完全看不出来。
#
# v2.89 的修法是把两段都写成 `:?`（缺失即在 up 阶段报错）。
# 但那逼用户额外准备一个 .env —— 而「拉到项目 → 编辑 compose → up」
# 才是 compose 的主用法，那条路上很多人根本不会碰 .env。
#
# v2.90 改成 anchor：值在文件顶部的 x-l2-* 定义一次，
# environment 段和 networks 段引用**同一个 anchor**。
# 「两段不一致」这个故障模式被结构性消除 —— 不存在两套语法可比。
# 用户只改顶部三行字面量即可，零 .env。
#
# 这一项锁三件事：
#   1. L2 两份的 environment 段三项用 `*anchor` 而不是 `${...}`
#   2. L2 两份的 networks 段三项用**同一个** anchor
#   3. host 那份保持 `${...:-}`（host 模式不需要这三个变量）
#
# 另外锁住「anchor 定义存在且带 ${VAR:-默认}」——
# 少了 `${VAR:-}` 就变成硬编码，.env 用户失去覆盖能力。

_l2_anchor_name() {
  case "$1" in
    IKEV2_L2_PARENT)  echo l2_parent ;;
    IKEV2_L2_SUBNET)  echo l2_subnet ;;
    IKEV2_L2_GATEWAY) echo l2_gateway ;;
    IKEV2_L2_IPV4)    echo l2_ipv4 ;;
  esac
}

for v in IKEV2_L2_PARENT IKEV2_L2_SUBNET IKEV2_L2_GATEWAY IKEV2_L2_IPV4; do
  a="$(_l2_anchor_name "$v")"
  for p in "$MACV" "$IPVL"; do
    f="$(basename "$p")"

    # 1. environment 段必须是 *anchor
    raw="$(grep -E "^[[:space:]]+${v}:" "$p" | head -1)"
    if printf '%s' "$raw" | grep -q "\*${a}\b"; then
      ok "NM-57 ${f} environment ${v} 引用 *${a}"
    else
      bad "NM-57 ${f} environment ${v} 引用 *${a}" "got: ${raw}"
    fi

    # 2. networks 段（parent / subnet / gateway / ipv4_address）必须是同一个 anchor
    case "$v" in
      IKEV2_L2_PARENT)  key="parent" ;;
      IKEV2_L2_SUBNET)  key="subnet" ;;
      IKEV2_L2_GATEWAY) key="gateway" ;;
      IKEV2_L2_IPV4)    key="ipv4_address" ;;
    esac
    raw="$(grep -E "^[[:space:]]+-?[[:space:]]*${key}:" "$p" | head -1)"
    if printf '%s' "$raw" | grep -q "\*${a}\b"; then
      ok "NM-57 ${f} networks ${key} 引用 *${a}（与 environment 同源）"
    else
      bad "NM-57 ${f} networks ${key} 引用 *${a}" "got: ${raw}"
    fi

    # 3. anchor 定义存在，且值是 ${VAR:-默认}（保留 .env 覆盖能力）
    #    注意：顶层 key 用连字符（x-l2-parent），anchor 名用下划线（&l2_parent）
    raw="$(grep -E "^x-${a//_/-}:" "$p" | head -1)"
    if [ -z "$raw" ]; then
      bad "NM-57 ${f} 顶部有 x-${a} 定义" "缺失"
    elif printf '%s' "$raw" | grep -q '&'"${a}"'\b' \
         && printf '%s' "$raw" | grep -q '\${'"${v}"':-'; then
      ok "NM-57 ${f} x-${a} 定义为 \${${v}:-默认}（.env 可覆盖）"
    else
      bad "NM-57 ${f} x-${a} 定义为 \${${v}:-默认}" "got: ${raw}"
    fi
  done

  # 4. host 那份保持 ${...:-}（host 模式不需要这三个变量）
  raw="$(grep -E "^[[:space:]]+${v}:" "$BASE" | head -1)"
  if printf '%s' "$raw" | grep -q ':-' && ! printf '%s' "$raw" | grep -q ':?'; then
    ok "NM-57 docker-compose.yml ${v} 保持 :- 可选（host 模式不需要）"
  else
    bad "NM-57 docker-compose.yml ${v} 保持 :- 可选" "got: ${raw}"
  fi
done

# ---------- NM-58 L2 的 service 段 networks 必须用长语法 ----------
#
# 固定容器 IPv4 属于 **endpoint 级**配置，只能写在 service 段的
#   networks:
#     ikev2-l2:
#       ipv4_address: x.x.x.x
# 短语法（`- ikev2-l2`）根本表达不了 ipv4_address —— 强行塞进顶层
# networks 段的 ipam 里则会被 Docker **静默忽略**（ipam 配的是
# subnet/gateway，不是容器自己的地址），用户填了地址却发现没生效，
# 且没有任何报错。
#
# 这一项锁两件事：
#   1. service 段 networks 必须是长语法（形如 `ikev2-l2:` 后跟缩进块）
#   2. 长语法块里必须有 ipv4_address，且引用 *l2_ipv4
for p in "$MACV" "$IPVL"; do
  f="$(basename "$p")"
  if grep -qE '^[[:space:]]+-[[:space:]]+ikev2-l2[[:space:]]*$' "$p"; then
    bad "NM-58 ${f} service 段 networks 用长语法" "仍是短语法（- ikev2-l2），无法表达 ipv4_address"
  else
    ok "NM-58 ${f} service 段 networks 用长语法"
  fi
  raw="$(grep -E "^[[:space:]]+ipv4_address:" "$p" | head -1)"
  if printf '%s' "$raw" | grep -q '\*l2_ipv4\b'; then
    ok "NM-58 ${f} service 段 ipv4_address 引用 *l2_ipv4"
  else
    bad "NM-58 ${f} service 段 ipv4_address 引用 *l2_ipv4" "got: ${raw}"
  fi
done

# ---------- NM-59 固定容器 IPv4（IKEV2_L2_IPV4）的校验语义 ----------
#
# 固定 IPv4 是**可选**的：留空 = Docker IPAM 自动分配，不能因此判失败。
# 填了才校验，且校验点跟 daemon 一致（daemon 也会拒不在子网内的地址）：
#   - 必须是合法 IPv4
#   - 必须落在 IKEV2_L2_SUBNET 内
#   - 不能与网关相同
_L2BASE="IKEV2_L2_PARENT=eth0 IKEV2_L2_SUBNET=192.168.50.0/24 IKEV2_L2_GATEWAY=192.168.50.1"

# 留空 → 通过
validate_rc "MODE=ipvlan" $_L2BASE
if [ "$RC" -eq 0 ]; then
  ok "NM-59 留空 IKEV2_L2_IPV4 → 通过（自动分配）"
else
  bad "NM-59 留空 IKEV2_L2_IPV4 → 通过" "rc=$RC out=$OUT"
fi

# 合法地址 → 通过
validate_rc "MODE=ipvlan" $_L2BASE "IKEV2_L2_IPV4=192.168.50.77"
if [ "$RC" -eq 0 ]; then
  ok "NM-59 合法固定 IP → 通过"
else
  bad "NM-59 合法固定 IP → 通过" "rc=$RC out=$OUT"
fi

# 不在子网内 → 拒绝，且报错要指名变量
validate_rc "MODE=ipvlan" $_L2BASE "IKEV2_L2_IPV4=10.99.99.5"
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "IKEV2_L2_IPV4"; then
  ok "NM-59 固定 IP 不在子网内 → 拒绝且指名变量"
else
  bad "NM-59 固定 IP 不在子网内 → 拒绝且指名变量" "rc=$RC out=$OUT"
fi

# 与网关相同 → 拒绝
validate_rc "MODE=ipvlan" $_L2BASE "IKEV2_L2_IPV4=192.168.50.1"
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "IKEV2_L2_IPV4"; then
  ok "NM-59 固定 IP 与网关相同 → 拒绝"
else
  bad "NM-59 固定 IP 与网关相同 → 拒绝" "rc=$RC out=$OUT"
fi

# 非法 IPv4 → 拒绝
validate_rc "MODE=ipvlan" $_L2BASE "IKEV2_L2_IPV4=999.1.1.1"
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "IKEV2_L2_IPV4"; then
  ok "NM-59 固定 IP 非法 → 拒绝"
else
  bad "NM-59 固定 IP 非法 → 拒绝" "rc=$RC out=$OUT"
fi

# host 模式下只 WARN，不拒绝
validate_rc "MODE=host" "IKEV2_L2_IPV4=192.168.50.77"
if [ "$RC" -eq 0 ] && printf '%s' "$OUT" | grep -q "WARN.*IKEV2_L2_IPV4"; then
  ok "NM-59 host 模式忽略固定 IP 并 WARN"
else
  bad "NM-59 host 模式忽略固定 IP 并 WARN" "rc=$RC out=$OUT"
fi

# ---------- 非法模式 ----------
validate_rc "MODE=foobar"
if [ "$RC" -ne 0 ]; then
  ok "NM-27 任意非法值 → 非 0"
else
  bad "NM-27 任意非法值" "rc=0"
fi

# ================================================================
# detect_public_ipv6 的地址状态过滤（v2.86-PR20 P0-1 回归）
#
# entrypoint.sh 有 1200+ 行且带大量顶层副作用（装包、写文件、起进程），
# 没法整体 source 进测试。这里用 awk 把函数体**原样抠出来** eval，
# 并用一个假 `ip` 命令喂 fixture —— 测的是仓库里那段真实代码，不是复制品。
#
# 为什么必须用 fixture 而不是读宿主机的 /proc/net/if_inet6：
# 宿主机上不会同时存在 stable / tentative / dadfailed 三种状态的 GUA 地址，
# 且 /proc/net/if_inet6 只有 flags 十六进制（要靠位运算还原），
# 而函数现在刻意消费 `ip -6 addr show` 的**文字标签**。
# 直接构造文本才能覆盖「同一台机器上 RA 刚到、多个地址并存」这个真实场景。
# ================================================================
ENTRY_SH="${SCRIPT_DIR}/../scripts/entrypoint.sh"
UP_SH="${SCRIPT_DIR}/../scripts/up.sh"
FIXTURE_DIR="$(mktemp -d)"
trap 'rm -rf "$FIXTURE_DIR"' EXIT

# 抠出 detect_public_ipv6 函数定义（从 `^detect_public_ipv6() {` 到第一个独立的 `}`）
DP6_SRC="$(awk '/^detect_public_ipv6\(\) \{/{f=1} f{print} f&&/^\}$/{exit}' "$ENTRY_SH")"

# 跑一次 detect_public_ipv6。$1 = `ip -6 addr show` 的输出文本。
# 用一个覆盖 PATH 的假 `ip` 把 fixture 喂进去，函数体本身不动。
dp6() {
  local fixture_text="$1"
  mkdir -p "$FIXTURE_DIR/bin"
  printf '#!/bin/sh\ncat <<'"'"'IPFIXTURE_EOF'"'"'\n%s\nIPFIXTURE_EOF\n' "$fixture_text" \
    > "$FIXTURE_DIR/bin/ip"
  chmod +x "$FIXTURE_DIR/bin/ip"
  env -i PATH="$FIXTURE_DIR/bin:/usr/bin:/bin" bash -c '
    eval "$1"
    detect_public_ipv6
  ' _ "$DP6_SRC" 2>/dev/null
}

if [ -z "$DP6_SRC" ]; then
  bad "NM-40 从 entrypoint.sh 抽取 detect_public_ipv6" "抽取失败"
else
  ok "NM-40 从 entrypoint.sh 抽取 detect_public_ipv6"

  # ---------- 核心回归：tentative 不能被当成结果 ----------
  # 旧实现读错字段（把 scope 当 flags），DAD 中的地址从来没被排除过。
  # 下面这条 fixture 就是那个现场：stable 地址 + 一个 DAD 中的地址。
  got="$(dp6 'inet6 2001:db8::1/64 scope global tentative
inet6 2001:db8::2/64 scope global')"
  if [ "$got" = "2001:db8::2" ]; then
    ok "NM-41 tentative 地址被跳过，返回 stable 那个"
  else
    bad "NM-41 tentative 过滤" "got='${got}' want=2001:db8::2"
  fi

  # DAD 冲突（dadfailed + tentative）：同样不可用
  got="$(dp6 'inet6 2001:db8::1/64 scope global dadfailed tentative')"
  if [ -z "$got" ]; then
    ok "NM-42 dadfailed 地址被跳过（无可用地址 → 空）"
  else
    bad "NM-42 dadfailed 过滤" "got='${got}' want=''"
  fi

  # deprecated：地址还在但已过期，不该作为 local_addrs
  got="$(dp6 'inet6 2001:db8::1/64 scope global deprecated
inet6 2001:db8::2/64 scope global')"
  if [ "$got" = "2001:db8::2" ]; then
    ok "NM-43 deprecated 地址被跳过"
  else
    bad "NM-43 deprecated 过滤" "got='${got}' want=2001:db8::2"
  fi

  # nodad：跳过 DAD 但地址是稳定的，必须被接受
  # （nodad 只说明「没做 DAD」，不等于 tentative）
  got="$(dp6 'inet6 2001:db8::1/64 scope global nodad')"
  if [ "$got" = "2001:db8::1" ]; then
    ok "NM-44 nodad 地址被接受（稳定态，非 tentative）"
  else
    bad "NM-44 nodad 接受" "got='${got}' want=2001:db8::1"
  fi

  # 2000::/3 边界：3xxx: 属于同一段，必须收；fe80::/10 与 fd00::/8 必须排除
  got="$(dp6 'inet6 3fff::1/64 scope global')"
  if [ "$got" = "3fff::1" ]; then
    ok "NM-45 2000::/3 上界 3fff:: 被接受"
  else
    bad "NM-45 3fff:: 边界" "got='${got}' want=3fff::1"
  fi

  got="$(dp6 'inet6 fe80::1/64 scope link
inet6 fd48:1234::2/64 scope global
inet6 ::1/128 scope host')"
  if [ -z "$got" ]; then
    ok "NM-46 link-local / ULA / loopback 全被排除"
  else
    bad "NM-46 非 GUA 过滤" "got='${got}' want=''"
  fi

  # dynamic：SLAAC 正常地址的常见标签，不能被误伤
  got="$(dp6 'inet6 2001:db8::5/64 scope global dynamic')"
  if [ "$got" = "2001:db8::5" ]; then
    ok "NM-47 dynamic 标签（SLAAC 正常地址）被接受"
  else
    bad "NM-47 dynamic 接受" "got='${got}' want=2001:db8::5"
  fi

  # ---------- ULA 不能遮蔽 GUA（P0-1 缺陷 2 回归）----------
  # macvlan/ipvlan 容器 eth0 上，Docker 分的 ULA 必然先于 SLAAC 的 GUA 出现。
  # 旧实现内层 awk 的 exit 只取「该网卡的第一条地址」，拿到 ULA 就再也走不到
  # GUA → IKEV2_SERVER_ADDR_V6 探测为空。实测把 ULA 行挪到 GUA 前面，
  # 旧实现输出从 2408:832e:... 变成空字符串。
  got="$(dp6 'inet6 fd48:1234::2/64 scope global
inet6 2408:832e:8a5:be0:8781:3c48:cf95:22d/64 scope global dynamic')"
  if [ "$got" = "2408:832e:8a5:be0:8781:3c48:cf95:22d" ]; then
    ok "NM-52 ULA 排在前面时不遮蔽 GUA"
  else
    bad "NM-52 ULA 遮蔽 GUA" "got='${got}' want=2408:832e:8a5:be0:8781:3c48:cf95:22d"
  fi
fi

# ---------- 入口契约：探测结果必须回写 env（P0-2 回归）----------
# P0-2：OUT_IF 之前只是 shell 局部变量，下游 ikev2-updown / DDNS /
# ipv6watch / config.OutIf 全都拿不到 → tc 限速静默失效。
if grep -qE '^[[:space:]]*export IKEV2_OUT_IF="\$OUT_IF"' "$ENTRY_SH"; then
  ok "NM-48 entrypoint export IKEV2_OUT_IF"
else
  bad "NM-48 entrypoint export IKEV2_OUT_IF" "未找到 export 语句"
fi

# 剥掉整行注释后再比对：下面几条守卫的关键词（require_l2_env、
# %IKEV2_OUT_IF%）在解释性注释里会被有意提到，只看可执行代码。
code_only() { sed -e 's/^[[:space:]]*#.*$//' "$1"; }

# 下面所有「剥注释后 grep -q」的判据都走 *_code 变量而不是管道：
# 本测试开了 `set -o pipefail`，`grep -q` 命中即退出会让上游 sed 收到
# SIGPIPE（141），整条管道被判失败。文件一大就从「稳定通过」变成「时好时坏」。
_entry_code="$(code_only "$ENTRY_SH")"
_up_code="$(code_only "$UP_SH")"

# ---------- 模板里不该再有 %IKEV2_OUT_IF% 占位符（P1-3 回归）----------
# sed 是死代码的前提是模板里没这个占位符；哪天有人加回来，这条会红。
if grep -q '%IKEV2_OUT_IF%' "${SCRIPT_DIR}/../configs/swanctl-ipv6-only.conf"; then
  bad "NM-49 模板无 %IKEV2_OUT_IF% 占位符" "模板里又出现了这个占位符"
elif printf '%s' "$_entry_code" | grep -q 's|%IKEV2_OUT_IF%|'; then
  bad "NM-49 模板无 %IKEV2_OUT_IF% 占位符" "entrypoint 里仍有对应的死 sed"
else
  ok "NM-49 %IKEV2_OUT_IF% 占位符与死 sed 均已移除"
fi

# ---------- up.sh 必须复用 network-mode.sh，而不是自带一份校验 ----------
# P1：up.sh 曾自带 require_l2_env，与 net_mode_validate 是同一件事的两份实现，
# 规则一改就漂移。这条守住「单一真源」。
#
# 用前面已经算好的 $_up_code（见 code_only 定义处的说明）：
# 本测试开了 `set -o pipefail`，`grep -q` 命中即退出会让上游 sed 收到
# SIGPIPE（141），整条管道被判失败。up.sh 变大后这个竞态从「稳定通过」
# 变成「时好时坏」—— 实测 284 行时同一判据连跑 5 次得到 FFP PF。
if printf '%s' "$_up_code" | grep -q 'network-mode\.sh' \
   && printf '%s' "$_up_code" | grep -q 'net_mode_validate'; then
  ok "NM-50 up.sh source network-mode.sh 并调用 net_mode_validate"
else
  bad "NM-50 up.sh 复用 network-mode.sh" \
      "source=$(printf '%s' "$_up_code" | grep -c 'network-mode\.sh') validate=$(printf '%s' "$_up_code" | grep -c 'net_mode_validate')"
fi

# 旧的那份重复实现必须已经删干净（否则两份逻辑又会各自漂移）
if printf '%s' "$_up_code" | grep -q 'require_l2_env'; then
  bad "NM-51 up.sh 已删除重复的 require_l2_env" "仍能找到该函数"
else
  ok "NM-51 up.sh 已删除重复的 require_l2_env"
fi

# ---------- NM-60 up.sh 的 compose_get 必须认得 compose 顶部的三种写法 ----------
#
# 真实故障（v2.90 实机发现）：up.sh 的 compose_get 只会取
#   x-l2-parent:  &l2_parent  "eth0"
# 这种**裸字面量**。但 compose 文件里实际是
#   x-l2-parent:  &l2_parent  "${IKEV2_L2_PARENT:-ens18}"
# 于是 compose_get 取到的是插值表达式本身，up.sh 认为「变量还是空的」，
# 直接报「缺少必填变量 IKEV2_L2_PARENT」—— 而用户明明在 compose 里填好了。
# 结果：用户照 README 改完 compose，跑 up.sh 却被拒。
#
# 这一项把 compose_get 从 up.sh 里抠出来单独跑，三种写法都要取对。
CG_DIR="$(mktemp -d)"
cat > "$CG_DIR/docker-compose.ipvlan.yml" <<'CGEOF'
x-l2-parent:  &l2_parent  "${IKEV2_L2_PARENT:-ens18}"
x-l2-subnet:  &l2_subnet  "192.168.50.0/24"
x-l2-gateway: &l2_gateway 192.168.50.1
x-l2-ipv4:    &l2_ipv4    "${IKEV2_L2_IPV4:-192.168.50.77}"
CGEOF
cat > "$CG_DIR/docker-compose.macvlan.yml" <<'CGEOF'
x-l2-parent:  &l2_parent  "${IKEV2_L2_PARENT:-}"
CGEOF

# 把 compose_get 从 up.sh 抠出来（到下一个顶层 } 为止）
_CG_SRC="$(awk '/^compose_get\(\) \{/{f=1} f{print} f&&/^\}$/{exit}' "$UP_SH")"
if [ -z "$_CG_SRC" ]; then
  bad "NM-60 up.sh 存在 compose_get 函数" "抠不出来"
else
  ok "NM-60 up.sh 存在 compose_get 函数"
  # 在临时目录里跑，compose_get 读的是相对路径 ./docker-compose.<mode>.yml
  _cg() { # $1=mode $2=anchor名
    ( cd "$CG_DIR" && eval "$_CG_SRC"; compose_get "$1" "$2" )
  }
  if [ "$(_cg ipvlan l2_parent)" = "ens18" ]; then
    ok "NM-60 compose_get 取到 \${VAR:-默认} 里的默认值"
  else
    bad "NM-60 compose_get 取 \${VAR:-默认} 的默认值" "got='$(_cg ipvlan l2_parent)' want=ens18"
  fi
  if [ "$(_cg ipvlan l2_subnet)" = "192.168.50.0/24" ]; then
    ok "NM-60 compose_get 取到裸字面量"
  else
    bad "NM-60 compose_get 取裸字面量" "got='$(_cg ipvlan l2_subnet)' want=192.168.50.0/24"
  fi
  if [ "$(_cg ipvlan l2_gateway)" = "192.168.50.1" ]; then
    ok "NM-60 compose_get 取到无引号裸值"
  else
    bad "NM-60 compose_get 取无引号裸值" "got='$(_cg ipvlan l2_gateway)' want=192.168.50.1"
  fi
  if [ "$(_cg ipvlan l2_ipv4)" = "192.168.50.77" ]; then
    ok "NM-60 compose_get 取到固定 IPv4 的默认值"
  else
    bad "NM-60 compose_get 取固定 IPv4 的默认值" "got='$(_cg ipvlan l2_ipv4)' want=192.168.50.77"
  fi
  # 默认值留空的形态 → 必须返回空（而不是把 ${VAR:-} 当成值）
  if [ -z "$(_cg macvlan l2_parent)" ]; then
    ok "NM-60 compose_get 对 \${VAR:-}（默认留空）返回空"
  else
    bad "NM-60 compose_get 对 \${VAR:-} 返回空" "got='$(_cg macvlan l2_parent)' want=(空)"
  fi
fi
rm -rf "$CG_DIR"

# ---------- NM-61 table 220 必须带直连(scope link)路由 ----------
#
# 真实故障（v2.92 实机,192.168.50.63 + ipvlan + 固定 IP 192.168.50.66）:
#   面板 8443 从局域网访问时 TCP 能建连但 TLS 握手必被 RST,浏览器打不开。
#   抓包:RST 的源 MAC 是**网关**的 MAC,连 SYN-ACK 的源 MAC 也是网关的
#        —— 说明容器发往同网段对端的回包**绕了网关**。
#
# 根因:strongSwan charon 装 `ip rule 220: from all lookup 220`,
#   把所有出向流量的查表强制到 table 220;而 table 220 里只有
#   `default via <网关>`,同网段直连路由缺失 → 回包也被 default 带走。
#   绕网关 → 源 MAC 被替换 + 网关按不对称路径处理 → 注入 RST。
#
# 修法:entrypoint.sh §3.5 在加 default 的同时,把 main 表里
#   dev $OUT_IF 的 scope link 直连路由复制进 table 220。
#
# 本项锁死两条不变量:
#   1. entrypoint.sh 里必须有「把 scope link 路由抄进 table 220」这段逻辑
#   2. 那段逻辑必须用 scope link(直连),不能是普通 dev 路由
if grep -qE 'scope link table 220' "$ENTRYPOINT_SH" 2>/dev/null; then
  ok "NM-61 entrypoint.sh 把 scope link 直连路由写入 table 220"
else
  bad "NM-61 entrypoint.sh 把 scope link 直连路由写入 table 220" "缺少该逻辑（L2 模式回包会绕网关 → TLS 握手被 RST）"
fi

# 必须同时存在 default via 网关那条(出公网仍要走它) —— 修 NM-61 不能把 default 拆掉
if grep -qE 'ip route add default via .* table 220' "$ENTRYPOINT_SH" 2>/dev/null; then
  ok "NM-61 entrypoint.sh 保留 table 220 的 default via 网关路由"
else
  bad "NM-61 entrypoint.sh 保留 table 220 的 default via 网关路由" "缺少（出公网会断）"
fi

# 抄直连路由时不能带 dev 过滤 —— `ip route show ... dev X` 的输出里没有 dev 字段，
# 照着解析会拿到空结果（真机上就是这样踩的坑）。
if grep -qE 'ip -4 route show table main scope link dev' "$ENTRYPOINT_SH" 2>/dev/null; then
  bad "NM-61 抄直连路由时未用 dev 过滤" "带了 dev 过滤 → 输出无 dev 字段 → 解析为空"
else
  ok "NM-61 抄直连路由时未用 dev 过滤（避免输出丢 dev 字段）"
fi

# ---------- IPv4 段校验：每段必须 0-255 且无前导 0 ----------
# 修 _nm_valid_ipv4_cidr 之前的漏洞：只查字符集与段数，
# "999.0.113.0/24" 这类超范围值能一路通过校验，直到 docker 建网段才报错。
v4_rc() {
  (
    # shellcheck disable=SC1090
    . "$NETMODE_SH"
    _nm_valid_ipv4 "$1"
  ) >/dev/null 2>&1
}

v4_expect() { # $1=ip  $2=0|1(期望返回码)
  v4_rc "$1"
  local got=$?
  if [ "$got" -eq "$2" ]; then
    ok "NM-5$3 _nm_valid_ipv4 '$1' → rc=$2"
  else
    bad "NM-5$3 _nm_valid_ipv4 '$1'" "rc=$got want=$2"
  fi
}

echo
echo "-- IPv4 段校验（NM-53 ~ NM-55）"
v4_expect "192.168.50.1"    0 3
v4_expect "0.0.0.0"        0 3
v4_expect "255.255.255.255" 0 3
v4_expect "192.168.50.256" 1 3
v4_expect "999.0.113.0"    1 3
v4_expect "192.168.50"     1 3
v4_expect "1.2.3.4.5"      1 3
# 前导 0：POSIX sh 按八进制求值而 Docker 按十进制，两边会算出不同地址
v4_expect "010.0.0.1"      1 3
v4_expect "192.168.050.1"  1 3
v4_expect ""                1 3

cidr_rc() {
  (
    # shellcheck disable=SC1090
    . "$NETMODE_SH"
    _nm_valid_ipv4_cidr "$1"
  ) >/dev/null 2>&1
}

cidr_expect() { # $1=cidr  $2=0|1  $3=编号后缀
  cidr_rc "$1"
  local got=$?
  if [ "$got" -eq "$2" ]; then
    ok "NM-5$3 _nm_valid_ipv4_cidr '$1' → rc=$2"
  else
    bad "NM-5$3 _nm_valid_ipv4_cidr '$1'" "rc=$got want=$2"
  fi
}

cidr_expect "192.168.50.0/24"   0 4
cidr_expect "192.168.50.0/30"   0 4
cidr_expect "192.168.50.0/31"   1 4
cidr_expect "192.168.50.0/0"    1 4
cidr_expect "192.168.50.0"      1 4
cidr_expect "999.0.113.0/24"    1 4
cidr_expect "192.168.50.256/24" 1 4

echo
echo "-- 超范围 IPv4 必须被 net_mode_validate 拦下（NM-55）"
# 注意匹配的是 SUBNET 自己的报错文案，而不是字符串 "IKEV2_L2_SUBNET" ——
# 网关那条的报错里也含这个变量名（"GATEWAY 不在 IKEV2_L2_SUBNET 内"），
# 宽匹配会让本用例在两道关都失效时仍然通过。
validate_rc "MODE=macvlan" IKEV2_L2_PARENT=eth0 \
            IKEV2_L2_SUBNET=999.0.113.0/24 IKEV2_L2_GATEWAY=999.0.113.1
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q 'SUBNET=.*不是合法 IPv4 CIDR'; then
  ok "NM-55 网段地址超 255 → SUBNET 校验拒下"
else
  bad "NM-55 超范围 SUBNET 被 SUBNET 校验拒下" "rc=$RC out=$OUT"
fi

# 前导 0 的子网要**因为 SUBNET 校验**被拒，而不是碰巧被后面的网关检查兜住。
# 同理必须匹配 SUBNET 自己的报错文案，否则去掉前导 0 那道关后本用例仍会通过
# （网关检查会因八进制/十进制算出不同网络而先报错），守护就形同虚设。
validate_rc "MODE=macvlan" IKEV2_L2_PARENT=eth0 \
            IKEV2_L2_SUBNET=192.168.050.0/24 IKEV2_L2_GATEWAY=192.168.50.1
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q 'SUBNET=.*不是合法 IPv4 CIDR'; then
  ok "NM-56 前导 0 的子网由 SUBNET 校验拒下（八进制/十进制歧义）"
else
  bad "NM-56 前导 0 子网被 SUBNET 校验拒下" "rc=$RC out=$OUT"
fi

# ---------- NM-62 共享 L2 网络：网络名校验（v2.95 / v2.96）----------
echo "-- 共享 L2 网络名校验（NM-62）"

# 合法网络名必须放行（ipvlan 与 macvlan 两种模式都要放行 —— v2.96 起两者都共享）
for _m in ipvlan macvlan; do
  validate_rc "MODE=$_m" IKEV2_L2_PARENT=eth0 \
              IKEV2_L2_SUBNET=192.168.50.0/24 IKEV2_L2_GATEWAY=192.168.50.1 \
              IKEV2_L2_NETWORK_NAME="$_m"
  if [ "$RC" -eq 0 ]; then
    ok "NM-62 合法网络名 '$_m' 放行（$_m 模式）"
  else
    bad "NM-62 合法网络名 '$_m' 放行（$_m 模式）" "rc=$RC out=$OUT"
  fi
done

validate_rc "MODE=ipvlan" IKEV2_L2_PARENT=eth0 \
            IKEV2_L2_SUBNET=192.168.50.0/24 IKEV2_L2_GATEWAY=192.168.50.1 \
            IKEV2_L2_NETWORK_NAME=my-l2.net_1
if [ "$RC" -eq 0 ]; then
  ok "NM-62 合法网络名 'my-l2.net_1' 放行"
else
  bad "NM-62 合法网络名 'my-l2.net_1' 放行" "rc=$RC out=$OUT"
fi

# 非法网络名必须被拒（daemon 会直接报 network name is invalid）
for _m in ipvlan macvlan; do
  validate_rc "MODE=$_m" IKEV2_L2_PARENT=eth0 \
              IKEV2_L2_SUBNET=192.168.50.0/24 IKEV2_L2_GATEWAY=192.168.50.1 \
              IKEV2_L2_NETWORK_NAME=-badstart
  if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q 'NETWORK_NAME'; then
    ok "NM-62 非法网络名 '-badstart' 被拒（$_m 模式）"
  else
    bad "NM-62 非法网络名 '-badstart' 被拒（$_m 模式）" "rc=$RC out=$OUT"
  fi
done

validate_rc "MODE=ipvlan" IKEV2_L2_PARENT=eth0 \
            IKEV2_L2_SUBNET=192.168.50.0/24 IKEV2_L2_GATEWAY=192.168.50.1 \
            "IKEV2_L2_NETWORK_NAME=has space"
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q 'NETWORK_NAME'; then
  ok "NM-62 非法网络名 'has space' 被拒"
else
  bad "NM-62 非法网络名 'has space' 被拒" "rc=$RC out=$OUT"
fi

# host 模式只 WARN 不拒（老 .env 残留不应导致启动失败）
validate_rc "MODE=host" IKEV2_L2_NETWORK_NAME=ipvlan
if [ "$RC" -eq 0 ] && printf '%s' "$OUT" | grep -q 'WARN.*NETWORK_NAME'; then
  ok "NM-62 host 模式忽略网络名（只 WARN）"
else
  bad "NM-62 host 模式忽略网络名（只 WARN）" "rc=$RC out=$OUT"
fi

# ---------- NM-63 共享 L2 网络：compose 与 up.sh 的接线（v2.95 / v2.96）----------
echo "-- 共享 L2 网络接线（NM-63）"

# 1) 两份 compose 的 networks 段都必须有 name:（固定名，跨 project 共享的锚点）
for _pair in "ipvlan:$IPVL" "macvlan:$MACV"; do
  _m="${_pair%%:*}"; _f="${_pair#*:}"
  if grep -qE '^[[:space:]]+name:[[:space:]]+\*l2_netname[[:space:]]*$' "$_f"; then
    ok "NM-63 $_m networks 段 name 引用 *l2_netname"
  else
    bad "NM-63 $_m networks 段 name 引用 *l2_netname" "not found"
  fi
done

# 2) 顶部必须有 x-l2-netname anchor，且是 ${IKEV2_L2_NETWORK_NAME:-默认}
for _pair in "ipvlan:$IPVL" "macvlan:$MACV"; do
  _m="${_pair%%:*}"; _f="${_pair#*:}"
  _raw="$(grep -E '^x-l2-netname:' "$_f" | head -1)"
  if printf '%s' "$_raw" | grep -q '&l2_netname' \
     && printf '%s' "$_raw" | grep -q '\${IKEV2_L2_NETWORK_NAME:-'; then
    ok "NM-63 $_m 顶部 x-l2-netname 定义为 \${IKEV2_L2_NETWORK_NAME:-默认}"
  else
    bad "NM-63 $_m 顶部 x-l2-netname 定义为 \${IKEV2_L2_NETWORK_NAME:-默认}" "got: ${_raw}"
  fi
done

# 3) 两份 compose 的 networks 段都必须有 external 插值开关（v2.97）
#
#    external: "${IKEV2_L2_EXTERNAL:-false}"
#
#    语义（63 实测，Docker 28.5.2 / Compose v2.40.3）：
#      false（默认）→ compose 自己建这条网络（纯 compose 路径可用）
#      true          → compose 只加入，不创建也不删除（up.sh 路径）
#    所以这里**不能**再写死 external: true —— 那样纯 compose 路径直接失败：
#      "network ipvlan declared as external, but could not be found"
for _pair in "ipvlan:$IPVL" "macvlan:$MACV"; do
  _m="${_pair%%:*}"; _f="${_pair#*:}"
  if sed -n '/^networks:/,$p' "$_f" \
     | grep -qE '^[[:space:]]+external:[[:space:]]+"\$\{IKEV2_L2_EXTERNAL:-false\}"[[:space:]]*$'; then
    ok "NM-63 $_m networks 段 external 为插值开关 \${IKEV2_L2_EXTERNAL:-false}"
  else
    bad "NM-63 $_m networks 段 external 为插值开关 \${IKEV2_L2_EXTERNAL:-false}" \
        "not found"
  fi
done

# 4) up.sh 必须有「不存在则创建、存在则校验」的共享网络逻辑
if grep -q '_l2_shared_network_ensure' "$SCRIPT_DIR/../scripts/up.sh"; then
  ok "NM-63 up.sh 有 _l2_shared_network_ensure"
else
  bad "NM-63 up.sh 有 _l2_shared_network_ensure" "not found"
fi

# 5) up.sh 必须能按 parent+子网接管既有的同驱动网络（老用户升级 / 用户自建）
if grep -q '_l2_find_compatible' "$SCRIPT_DIR/../scripts/up.sh"; then
  ok "NM-63 up.sh 有 _l2_find_compatible（接管既有网络）"
else
  bad "NM-63 up.sh 有 _l2_find_compatible" "not found"
fi

# 6) up.sh 必须 export IKEV2_L2_NETWORK_NAME —— 否则 up.sh 与 compose 会加入不同的网络
if grep -q 'export IKEV2_L2_NETWORK_NAME' "$SCRIPT_DIR/../scripts/up.sh"; then
  ok "NM-63 up.sh export IKEV2_L2_NETWORK_NAME（与 compose 同一条网络）"
else
  bad "NM-63 up.sh export IKEV2_L2_NETWORK_NAME" "not found"
fi

# 7) 默认名两处必须一致：network-mode.sh 的常量 vs compose 顶部默认值
#    （v2.96：macvlan → "macvlan"，ipvlan → "ipvlan"）
_nm_ip="$(grep -E '^NET_SHARED_NETWORK_NAME_DEFAULT_IPVLAN=' "$NETMODE_SH" | head -1 | cut -d= -f2- | tr -d '\"')"
_nm_mac="$(grep -E '^NET_SHARED_NETWORK_NAME_DEFAULT_MACVLAN=' "$NETMODE_SH" | head -1 | cut -d= -f2- | tr -d '\"')"
_cmp_ip="$(sed -nE 's/^x-l2-netname:.*\$\{IKEV2_L2_NETWORK_NAME:-([^}]*)\}.*/\1/p' "$IPVL" | head -1)"
_cmp_mac="$(sed -nE 's/^x-l2-netname:.*\$\{IKEV2_L2_NETWORK_NAME:-([^}]*)\}.*/\1/p' "$MACV" | head -1)"
if [ -n "$_nm_ip" ] && [ "$_nm_ip" = "$_cmp_ip" ]; then
  ok "NM-63 ipvlan 默认网络名两处一致（$_nm_ip）"
else
  bad "NM-63 ipvlan 默认网络名两处一致" "network-mode.sh=${_nm_ip} compose=${_cmp_ip}"
fi
if [ -n "$_nm_mac" ] && [ "$_nm_mac" = "$_cmp_mac" ]; then
  ok "NM-63 macvlan 默认网络名两处一致（$_nm_mac）"
else
  bad "NM-63 macvlan 默认网络名两处一致" "network-mode.sh=${_nm_mac} compose=${_cmp_mac}"
fi

# 8) 默认名必须就是驱动名本身（不带任何 project 标识）
if [ "$_nm_ip" = "ipvlan" ] && [ "$_nm_mac" = "macvlan" ]; then
  ok "NM-63 默认网络名就是驱动名（ipvlan / macvlan）"
else
  bad "NM-63 默认网络名就是驱动名" "ipvlan=${_nm_ip} macvlan=${_nm_mac}"
fi

# 9) network-mode.sh 必须提供按模式取默认名的函数
if grep -q 'net_shared_network_default_name()' "$NETMODE_SH"; then
  ok "NM-63 network-mode.sh 有 net_shared_network_default_name（按模式取默认名）"
else
  bad "NM-63 network-mode.sh 有 net_shared_network_default_name" "not found"
fi

# 10) up.sh 必须对 macvlan 也调用共享网络逻辑（v2.96：两种驱动都共享）
if grep -q '"macvlan" \\' "$SCRIPT_DIR/../scripts/up.sh"; then
  ok "NM-63 up.sh 对 macvlan 也调用 _l2_shared_network_ensure"
else
  bad "NM-63 up.sh 对 macvlan 也调用 _l2_shared_network_ensure" "not found"
fi

# 11) parent 独占预检只对 ipvlan 生效（macvlan 的 parent 不独占）
if grep -q 'if \[ "\$_driver" = "ipvlan" \]; then' "$SCRIPT_DIR/../scripts/up.sh"; then
  ok "NM-63 parent 独占预检只对 ipvlan 生效"
else
  bad "NM-63 parent 独占预检只对 ipvlan 生效" "not found"
fi

# ---------- NM-64 纯 compose 路径可用的三件套（v2.97）----------
#
# v2.96 的设计前提是「用户跑 up.sh，up.sh 建网 + compose 以 external 加入」。
# v2.97 修正：真实部署场景是**只跑 compose**（Docker 管理器 GUI），
# 所以建网责任必须能交还给 compose。三件套缺一不可：
#
#   ① networks 段的 external 是插值开关（NM-63 第 3 条已查）
#   ② 顶部三项必填变量用嵌套 :? 形态 → 留空时 compose 在**拉镜像前**
#      就用中文报错（不是英文的 "required variable ... is missing a value"）
#   ③ up.sh 建网时带 com.docker.compose.network label → 两条路径建出来的
#      网络可以互相复用（否则纯 compose 复用 up.sh 建的网会报
#      "incorrect label com.docker.compose.network set to "" (expected: ikev2-l2)"）
echo "-- 纯 compose 路径接线（NM-64）"

# 1) 两份 compose 顶部三项必须是嵌套 :? 形态（中文报错）
for _pair in "ipvlan:$IPVL" "macvlan:$MACV"; do
  _m="${_pair%%:*}"; _f="${_pair#*:}"
  for _v in PARENT SUBNET GATEWAY; do
    _raw="$(grep -E "^x-l2-$(echo "$_v" | tr 'A-Z' 'a-z'):" "$_f" | head -1)"
    if printf '%s' "$_raw" | grep -qE '\$\{IKEV2_L2_[A-Z]+:-\$\{IKEV2_L2_[A-Z]+:\?.+\}\}'; then
      ok "NM-64 $_m x-l2-$(echo "$_v" | tr 'A-Z' 'a-z') 用嵌套 :? 必填（留空中文报错）"
    else
      bad "NM-64 $_m x-l2-$(echo "$_v" | tr 'A-Z' 'a-z') 用嵌套 :? 必填" "got: ${_raw}"
    fi
  done
done

# 2) network-mode.sh 的 compose key 常量必须与两份 compose 的 networks 段 key 一致
#
#    compose 复用已存在的同名网络时，会校验网络上的 label
#    com.docker.compose.network 是否等于 networks 段的 **key**。
#    key 写错 → up.sh 建出来的网纯 compose 路径复用不了。
_nm_key="$(grep -E '^NET_SHARED_NETWORK_COMPOSE_KEY=' "$NETMODE_SH" | head -1 | cut -d= -f2- | tr -d '"'"'" | tr -d ' ')"
if [ -z "$_nm_key" ]; then
  bad "NM-64 network-mode.sh 有 NET_SHARED_NETWORK_COMPOSE_KEY" "not found"
else
  ok "NM-64 network-mode.sh 有 NET_SHARED_NETWORK_COMPOSE_KEY（$_nm_key）"
  for _pair in "ipvlan:$IPVL" "macvlan:$MACV"; do
    _m="${_pair%%:*}"; _f="${_pair#*:}"
    _ck="$(sed -n '/^networks:/,$p' "$_f" | grep -E '^[[:space:]]+[A-Za-z0-9_-]+:[[:space:]]*$' | head -1 | tr -d ' :')"
    if [ "$_ck" = "$_nm_key" ]; then
      ok "NM-64 $_m networks 段 key 与常量一致（$_ck）"
    else
      bad "NM-64 $_m networks 段 key 与常量一致" "compose=${_ck} const=${_nm_key}"
    fi
  done
fi

# 3) up.sh 建网必须带 com.docker.compose.network label
if grep -q -- '--label "com.docker.compose.network=' "$SCRIPT_DIR/../scripts/up.sh"; then
  ok "NM-64 up.sh 建网带 com.docker.compose.network label（两条路径互通）"
else
  bad "NM-64 up.sh 建网带 com.docker.compose.network label" "not found"
fi

# 4) up.sh 必须强制 external=true（否则 compose 抢已建好的网络 / 报 label 错）
if grep -q 'IKEV2_L2_EXTERNAL=true' "$SCRIPT_DIR/../scripts/up.sh" \
   && grep -q 'export IKEV2_L2_EXTERNAL' "$SCRIPT_DIR/../scripts/up.sh"; then
  ok "NM-64 up.sh 强制并 export IKEV2_L2_EXTERNAL=true"
else
  bad "NM-64 up.sh 强制并 export IKEV2_L2_EXTERNAL=true" "not found"
fi

# 5) network-mode.sh 必须校验 IKEV2_L2_EXTERNAL 只能是 true / false
if grep -q 'IKEV2_L2_EXTERNAL' "$NETMODE_SH" \
   && grep -qE 'true\|false' "$NETMODE_SH"; then
  ok "NM-64 network-mode.sh 校验 IKEV2_L2_EXTERNAL 合法值"
else
  bad "NM-64 network-mode.sh 校验 IKEV2_L2_EXTERNAL 合法值" "not found"
fi

# 6) host 模式必须忽略 IKEV2_L2_EXTERNAL（只 WARN，不失败）
validate_rc "MODE=host" "IKEV2_L2_EXTERNAL=true"
if [ "$RC" -eq 0 ] && printf '%s' "$OUT" | grep -q "WARN.*IKEV2_L2_EXTERNAL"; then
  ok "NM-64 host 模式忽略 IKEV2_L2_EXTERNAL（只 WARN）"
else
  bad "NM-64 host 模式忽略 IKEV2_L2_EXTERNAL（只 WARN）" "rc=$RC out=$OUT"
fi

# 7) L2 模式 + 非法 IKEV2_L2_EXTERNAL → 必须报错（不是静默忽略）
validate_rc "MODE=ipvlan" "IKEV2_L2_PARENT=eth0" "IKEV2_L2_SUBNET=192.168.1.0/24" \
            "IKEV2_L2_GATEWAY=192.168.1.1" "IKEV2_L2_EXTERNAL=maybe"
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "IKEV2_L2_EXTERNAL"; then
  ok "NM-64 L2 模式 + 非法 IKEV2_L2_EXTERNAL → 报错"
else
  bad "NM-64 L2 模式 + 非法 IKEV2_L2_EXTERNAL → 报错" "rc=$RC out=$OUT"
fi

# 8) L2 模式 + 合法 IKEV2_L2_EXTERNAL=true → 必须通过
validate_rc "MODE=ipvlan" "IKEV2_L2_PARENT=eth0" "IKEV2_L2_SUBNET=192.168.1.0/24" \
            "IKEV2_L2_GATEWAY=192.168.1.1" "IKEV2_L2_EXTERNAL=true"
if [ "$RC" -eq 0 ]; then
  ok "NM-64 L2 模式 + IKEV2_L2_EXTERNAL=true → 通过"
else
  bad "NM-64 L2 模式 + IKEV2_L2_EXTERNAL=true → 通过" "rc=$RC out=$OUT"
fi

echo
echo "----------------------------------------"
printf 'passed: %s   failed: %s\n' "$(_green "$PASS")" "$([ "$FAIL" -gt 0 ] && _red "$FAIL" || echo 0)"
if [ "$FAIL" -gt 0 ]; then
  printf 'failed ids: %s\n' "${FAILED_IDS[*]}"
  exit 1
fi
echo "all tests passed"
