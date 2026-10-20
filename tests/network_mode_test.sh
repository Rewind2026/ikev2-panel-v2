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
    unset IKEV2_L2_PARENT IKEV2_L2_SUBNET IKEV2_L2_GATEWAY
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
# v2.86-PR20 实测后重写：注入点从 driver option 改成 service 段 sysctls，
# 取值也从 IFNAME/disable_ipv6 改成 all/default.accept_ra。
SYSC="$(bash -c ". '$NETMODE_SH'; net_mode_sysctls")"
if printf '%s' "$SYSC" | grep -q "net.ipv4.conf.all.forwarding=1" \
   && printf '%s' "$SYSC" | grep -q "net.ipv6.conf.all.forwarding=1" \
   && printf '%s' "$SYSC" | grep -q "net.ipv6.conf.default.accept_ra=2"; then
  ok "NM-22 net_mode_sysctls 含 v4/v6 forwarding + default.accept_ra=2"
else
  bad "NM-22 net_mode_sysctls" "got=$SYSC"
fi

# 回读清单必须覆盖全部三条，且用 IFNAME 占位（运行时替换成真实网卡名）
CHK="$(bash -c ". '$NETMODE_SH'; net_mode_sysctl_checks")"
if printf '%s' "$CHK" | grep -q "net/ipv4/conf/IFNAME/forwarding|1" \
   && printf '%s' "$CHK" | grep -q "net/ipv6/conf/IFNAME/forwarding|1" \
   && printf '%s' "$CHK" | grep -q "net/ipv6/conf/IFNAME/accept_ra|2"; then
  ok "NM-37 net_mode_sysctl_checks 覆盖三条回读项"
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

# ---------- L2 sysctl 注入点与 compose 文件一致 ----------
# v2.86-PR20 实测结论（Docker 28.5.2，192.168.50.63）：
#   - networks.<name>.options 不是合法 Compose 字段，config 直接报错
#   - driver option com.docker.network.endpoint.sysctls 两种格式都不生效
#   - 唯一可用通道是 service 段 sysctls:，且只能写 conf.all / conf.default
# 这三条守护就是为了防止有人改回那两条死路。
for p in "$MACV" "$IPVL"; do
  f="$(basename "$p")"
  # 1) 不得再出现非法字段 / 无效驱动选项
  if sed -e 's/[[:space:]]*#.*$//' "$p" | grep -q "com.docker.network.endpoint.sysctls"; then
    bad "NM-38 ${f} 不含 endpoint.sysctls 驱动选项" "found in a non-comment line"
  else
    ok "NM-38 ${f} 不含 endpoint.sysctls 驱动选项"
  fi
  # 2) service 段 sysctls 三条齐全（值必须与 net_mode_sysctls() 一致）
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
  # 3) networks 段必须 enable_ipv6，否则 disable_ipv6=1 收不到 RA
  if sed -e 's/[[:space:]]*#.*$//' "$p" | grep -qE '^[[:space:]]*enable_ipv6:[[:space:]]*true[[:space:]]*$'; then
    ok "NM-28 ${f} networks 段 enable_ipv6: true"
  else
    bad "NM-28 ${f} networks 段 enable_ipv6: true" "not found"
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
for p in "$BASE" "$MACV" "$IPVL"; do
  f="$(basename "$p")"
  if grep -qE '^[[:space:]]*name:[[:space:]]*ikev2-panel-data[[:space:]]*$' "$p"; then
    ok "NM-31 ${f} 顶层卷名锁定 ikev2-panel-data"
  else
    bad "NM-31 ${f} 顶层卷名锁定 ikev2-panel-data" "not found"
  fi
  if grep -qE '^[[:space:]]*external:' "$p"; then
    bad "NM-31 ${f} 无 external:" "found one"
  else
    ok "NM-31 ${f} 无 external:"
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

# ---------- 模板里不该再有 %IKEV2_OUT_IF% 占位符（P1-3 回归）----------
# sed 是死代码的前提是模板里没这个占位符；哪天有人加回来，这条会红。
if grep -q '%IKEV2_OUT_IF%' "${SCRIPT_DIR}/../configs/swanctl-ipv6-only.conf"; then
  bad "NM-49 模板无 %IKEV2_OUT_IF% 占位符" "模板里又出现了这个占位符"
elif code_only "$ENTRY_SH" | grep -q 's|%IKEV2_OUT_IF%|'; then
  bad "NM-49 模板无 %IKEV2_OUT_IF% 占位符" "entrypoint 里仍有对应的死 sed"
else
  ok "NM-49 %IKEV2_OUT_IF% 占位符与死 sed 均已移除"
fi

# ---------- up.sh 必须复用 network-mode.sh，而不是自带一份校验 ----------
# P1：up.sh 曾自带 require_l2_env，与 net_mode_validate 是同一件事的两份实现，
# 规则一改就漂移。这条守住「单一真源」。
if code_only "$UP_SH" | grep -q 'network-mode\.sh' \
   && grep -q 'net_mode_validate' "$UP_SH"; then
  ok "NM-50 up.sh source network-mode.sh 并调用 net_mode_validate"
else
  bad "NM-50 up.sh 复用 network-mode.sh" \
      "source=$(code_only "$UP_SH" | grep -c 'network-mode\.sh') validate=$(grep -c 'net_mode_validate' "$UP_SH")"
fi

# 旧的那份重复实现必须已经删干净（否则两份逻辑又会各自漂移）
if code_only "$UP_SH" | grep -q 'require_l2_env'; then
  bad "NM-51 up.sh 已删除重复的 require_l2_env" "仍能找到该函数"
else
  ok "NM-51 up.sh 已删除重复的 require_l2_env"
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

echo
echo "----------------------------------------"
printf 'passed: %s   failed: %s\n' "$(_green "$PASS")" "$([ "$FAIL" -gt 0 ] && _red "$FAIL" || echo 0)"
if [ "$FAIL" -gt 0 ]; then
  printf 'failed ids: %s\n' "${FAILED_IDS[*]}"
  exit 1
fi
echo "all tests passed"
