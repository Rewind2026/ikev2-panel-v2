#!/usr/bin/env bash
# up.sh - 一键启动容器（host / macvlan / ipvlan 三种网络模式）
#
# 为什么需要这个脚本:
#   - 自动 ./logs bind mount 目录准备(避免 docker compose up 因目录不存在失败)
#   - 按 IKEV2_NETWORK_MODE 选对应的 compose 文件(三个模式 = 三个独立文件)
#   - L2 模式的前置校验(必填变量 / v6 子网拦截)做在调 docker 之前
#
# v2.86-PR20:三个网络模式对应三个**独立完整**的 compose 文件,不用叠加:
#   host    : docker-compose.yml            (老用户 `docker compose up -d` 零改动)
#   macvlan : docker-compose.macvlan.yml
#   ipvlan  : docker-compose.ipvlan.yml
#
#   不需要 `!reset` / `!override` 标签 —— 三个文件各自完整,
#   所以**任何 Docker Compose 版本都能用**,不设版本下限。
#
# 用法:
#   ./scripts/up.sh                              # host(默认)
#   IKEV2_NETWORK_MODE=macvlan ./scripts/up.sh   # macvlan
#   IKEV2_NETWORK_MODE=ipvlan  ./scripts/up.sh   # ipvlan
#   ./scripts/up.sh --no-detect                  # 兼容老用户参数(已 no-op,本脚本消费,不会传给 docker compose)
#   ./scripts/up.sh --help                       # 看帮助
#
# 等价于（host 模式）:
#   mkdir -p logs && chmod 1777 logs && docker compose up -d
#
# 等价于（L2 模式）:
#   mkdir -p logs && chmod 1777 logs && docker compose -f docker-compose.macvlan.yml up -d

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "$PROJECT_DIR"

# ---------- 复用容器内同一份模式规格表 ----------
# network-mode.sh 是三种网络模式的**单一真源**：枚举、归一化、L2 变量校验、
# sysctl 清单都在那里，entrypoint.sh 用的就是它。
# v2.86-PR20 之前本脚本自己写了一份 require_l2_env，只查「三项非空 + 拦截 v6 子网」，
# 和 net_mode_validate 是同一件事的两份实现 —— 规则一改就漂移
# （例如 net_mode_validate 后来加的 parent!=lo、gateway 必须落在 subnet 内、
#  macvlan_mode=passthru 拦截，up.sh 全都不知道，只能等容器起来才 exit 12）。
# 该文件是纯 POSIX sh、零副作用，直接 source 即可。
# shellcheck source=scripts/network-mode.sh
. "${SCRIPT_DIR}/network-mode.sh"

# .env 里的 IKEV2_NETWORK_MODE 要能被本脚本读到（docker compose 自己也会读 .env，
# 但 up.sh 需要在选 compose 文件**之前**就知道模式）。
#
# 只 grep 出需要的键，不整体 source .env —— .env 的值可能带空格 / 特殊字符，
# 整体 source 有被当成 shell 代码执行的风险。
env_get() {
  [ -f ./.env ] || return 0
  # 取最后一个未注释的 KEY=VALUE 赋值（与 compose 的"后者覆盖前者"语义一致）
  sed -nE "s/^[[:space:]]*(export[[:space:]]+)?$1[[:space:]]*=[[:space:]]*(.*)$/\2/p" ./.env \
    | tail -n1 \
    | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' \
           -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'\$/\1/"
}

[ -n "${IKEV2_NETWORK_MODE:-}" ] || IKEV2_NETWORK_MODE="$(env_get IKEV2_NETWORK_MODE)"

# L2 变量沿用原样：读到就 export，让 docker compose 的 ${...} 插值
# 拿到与本脚本一致的取值。
for _k in IKEV2_L2_PARENT IKEV2_L2_SUBNET IKEV2_L2_GATEWAY IKEV2_L2_IPV4; do
  if [ -z "${!_k:-}" ]; then
    declare "$_k=$(env_get "$_k")"
    export "$_k"
  fi
done

# v2.90 起，L2 那两份 compose 的推荐配置路径是「改文件顶部的 x-l2-* 三行」
# （零 .env）。env_get 只认 .env，所以这里补一个从 compose 顶部读的回落 ——
# 否则用户明明在 compose 里填好了，net_mode_validate 却报「缺少必填变量」。
#
# 只在本变量仍为空时才读，且**不 export**：compose 会自己插值，
# 这里 export 反而可能用一个被误解的值去覆盖 compose 本来能正确读到的。
#
# compose 顶部有三种写法，都要认（否则用户按 README 写完，up.sh 却说「缺变量」）：
#   1) x-l2-parent:  &l2_parent  "${IKEV2_L2_PARENT:-ens18}"   ← 推荐
#      取 :- 后面的默认值
#   2) x-l2-parent:  &l2_parent  "ens18"                      ← 裸字面量
#      取引号里的内容
#   3) x-l2-parent:  &l2_parent  ens18                         ← 裸值无引号
#      取 &anchor 之后的原文
#
# 注意顶层 key 用连字符（x-l2-parent），anchor 名用下划线（&l2_parent），
# 所以 key 要把 $2 里的下划线换成连字符。
compose_get() {
  _cf="./docker-compose.${1}.yml"
  [ -f "$_cf" ] || return 0
  _key="${2//_/-}"
  # 形态 1：${VAR:-默认值} → 取默认值
  _v="$(sed -nE "s/^x-${_key}:[[:space:]]*&${2}[[:space:]]+\"\\\$\{[A-Za-z_][A-Za-z0-9_]*:-([^}]*)\}\".*/\1/p" "$_cf" | head -n1)"
  if [ -n "$_v" ]; then
    printf '%s' "$_v"
    return 0
  fi
  # 形态 1 的变体：${VAR:-}（默认值留空）→ 返回空
  if grep -qE "^x-${_key}:[[:space:]]*&${2}[[:space:]]+\"\\\$\{[A-Za-z_][A-Za-z0-9_]*:-\}\"" "$_cf"; then
    return 0
  fi
  # 形态 2：引号里的字面量
  _v="$(sed -nE "s/^x-${_key}:[[:space:]]*&${2}[[:space:]]+\"([^\"]*)\".*/\1/p" "$_cf" | head -n1)"
  if [ -n "$_v" ]; then
    printf '%s' "$_v"
    return 0
  fi
  # 形态 3：裸值（无引号）
  _v="$(sed -nE "s/^x-${_key}:[[:space:]]*&${2}[[:space:]]+([^\"[:space:]].*)$/\1/p" "$_cf" \
    | head -n1 | sed -e 's/[[:space:]]*$//')"
  [ -n "$_v" ] && printf '%s' "$_v"
  return 0
}

# 模式专属开关只给 net_mode_validate 用，**不 export**：
# env_get 的解析规则和 compose 的 .env 解析规则不完全等价
# （行尾注释 `KEY=v # x` compose 会剥离、我们不会），export 出去反而可能
# 用一个被误解的值去覆盖 compose 本来能正确读到的 .env 值。
# 不 export 时它们只是普通 shell 变量，函数内照样可见。
for _k in IKEV2_MACVLAN_MODE IKEV2_IPVLAN_MODE IKEV2_IPVLAN_FLAG; do
  if [ -z "${!_k:-}" ]; then
    declare "$_k=$(env_get "$_k")"
  fi
done

# 归一化同样交给单一真源（大小写不敏感 + 去首尾空白 + 空值→host）
NETWORK_MODE="$(net_mode_normalize "${IKEV2_NETWORK_MODE:-}")"

# L2 模式下，变量可能只写在 compose 顶部的 x-l2-* anchor 里（v2.90 起的
# 推荐路径，零 .env）。这时 shell 和 .env 里都是空的，net_mode_validate 会
# 误报「缺少必填变量」—— 所以在校验之前从对应 compose 顶部补读一次。
#
# 顺序刻意是 shell env → .env → compose 顶部字面量，与 Compose 自己的
# 插值优先级一致，不会出现「up.sh 认为是一个值、compose 用另一个值」。
if [ "$NETWORK_MODE" = "$NET_MODE_MACVLAN" ] || [ "$NETWORK_MODE" = "$NET_MODE_IPVLAN" ]; then
  _anchor_key() {
    case "$1" in
      IKEV2_L2_PARENT)  echo l2_parent ;;
      IKEV2_L2_SUBNET)  echo l2_subnet ;;
      IKEV2_L2_GATEWAY) echo l2_gateway ;;
      IKEV2_L2_IPV4)    echo l2_ipv4 ;;
    esac
  }
  for _k in IKEV2_L2_PARENT IKEV2_L2_SUBNET IKEV2_L2_GATEWAY IKEV2_L2_IPV4; do
    eval "_cur=\${$_k:-}"
    [ -n "$_cur" ] && continue
    _ak="$(_anchor_key "$_k")"
    _v="$(compose_get "$NETWORK_MODE" "$_ak")"
    # compose_get 已处理 ${VAR:-默认} / "字面量" / 裸值 三种形态。
    # 若取到的仍是插值表达式本身（如 ${VAR:?msg}），说明这个值没解析出来 ——
    # 交给 net_mode_validate 报「缺少必填变量」才是对的。
    case "$_v" in
      '${'*) continue ;;
    esac
    [ -n "$_v" ] && declare "$_k=$_v"
  done
  unset _anchor_key
fi

if [ -t 1 ]; then
  BLUE='\033[0;34m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; NC='\033[0m'
else
  BLUE=''; GREEN=''; YELLOW=''; RED=''; NC=''
fi
log()  { echo -e "${BLUE}[up.sh]${NC} $*" >&2; }
ok()   { echo -e "${GREEN}[up.sh] ✓${NC} $*" >&2; }
warn() { echo -e "${YELLOW}[up.sh] !${NC} $*" >&2; }
die()  { echo -e "${RED}[up.sh] ✗${NC} $*" >&2; exit 1; }

# ---------- 解析参数 ----------
# --no-detect 必须真的从参数里剔掉。
# v2.86-PR20 实测（Docker Compose v2.40.3）：只 warn 不剔除的话，
# 它会一路透传给 `docker compose up`，直接报 `unknown flag: --no-detect`，
# 整个启动失败 —— 正好和"兼容老参数"的意图相反。
# 用数组收集透传参数，而不是拼字符串：
# 拼字符串再 `exec docker compose up -d $ARGS` 会把 'xxx' 的引号原样传进去，
# docker 会把带引号的 '--force-recreate' 当成未知参数。
# 先把原始参数复制一份，遍历副本，这样循环里可以自由剔除。
ORIG_ARGS=("$@")
PASSTHRU_ARGS=()
for arg in "${ORIG_ARGS[@]}"; do
  case "$arg" in
    --no-detect) warn "--no-detect 已是 no-op(v2.86-PR14 删了网络模式自动探测),已忽略该参数" ;;
    -h|--help)
      cat <<EOF
用法: ./scripts/up.sh [选项]

选项:
  --no-detect    兼容老参数(已 no-op,会被本脚本消费,不会传给 docker compose)
  -h, --help     显示这个帮助

网络模式(读 .env 或环境变量 IKEV2_NETWORK_MODE)—— 三个模式 = 三个独立 compose 文件:
  host      docker-compose.yml            默认。容器共享宿主 netns,直占宿主 500/4500/8443
  macvlan   docker-compose.macvlan.yml   容器独占 MAC,直挂物理二层,500/4500 绑容器独立 IP
  ipvlan    docker-compose.ipvlan.yml    同 macvlan,但共享 parent MAC

  三个文件都是**完整**的,不叠加,所以任何 Docker Compose 版本都能用(无版本下限)。

macvlan / ipvlan 需要提供三项（两种方式任选）：
  1) 直接改 compose 文件顶部的 x-l2-* 三行（推荐，零 .env）：
       x-l2-parent:  &l2_parent  "eth0"
       x-l2-subnet:  &l2_subnet  "192.168.1.0/24"
       x-l2-gateway: &l2_gateway "192.168.1.1"
     environment 段与 networks 段引用同一个 anchor，不会只改一边。
     ⚠️ 保留 \${IKEV2_L2_XXX:-} 外壳（把值填在 :- 后面），
        这样 .env 用户仍能覆盖；直接写裸值会让 .env 永久失效。
  2) 或在 .env 里提供（优先级 shell env > .env > compose 字面量）：
       IKEV2_L2_PARENT=eth0
       IKEV2_L2_SUBNET=192.168.1.0/24
       IKEV2_L2_GATEWAY=192.168.1.1
     (IKEV2_L2_SUBNET 只支持 IPv4,v6 走 RA/SLAAC)

  可选：固定容器 IPv4（留空 = Docker 自动分配）
       x-l2-ipv4:    &l2_ipv4    "192.168.1.10"
     必须落在 subnet 内、且不等于网关、且宿主上没有被占用
     （撞上宿主已用地址会让宿主入站连接全断，up.sh 会在启动前拦）。

  查真实值:
    ip -o -4 route show default | head -1    # dev 后面=父接口,第三列=网关
    ip -o -4 addr show <父接口>               # inet 列的 CIDR=子网
EOF
      exit 0
      ;;
    *)
      # 其余参数原样透传给 docker compose
      PASSTHRU_ARGS+=("$arg")
      ;;
  esac
done

# ---------- 模式合法性校验 + 按网络模式选 compose 文件 ----------
# host 分支必须保持 COMPOSE_FILES 为空，展开后命令退化为改造前的
# `docker compose up -d "$@"`（向后兼容的硬要求）。
COMPOSE_FILES=""

# 校验复用 network-mode.sh 的 net_mode_validate，不再维护第二份实现。
# 它把 ERR/WARN 打到 stderr、用返回码表示结果；这里只翻译退出语义。
# 收益：up.sh 在 docker 启动**之前**就能报出与容器内 entrypoint 完全一致的
# 错误（parent=lo / gateway 不落在 subnet 内 / macvlan_mode=passthru /
# 网关缺失 / v6 子网 ...），不必等容器起来再 exit 12。
# host 模式下它对 .env 残留的 L2 变量打 WARN，与 entrypoint 行为一致。
if ! net_mode_validate "$NETWORK_MODE"; then
  die "IKEV2_NETWORK_MODE=${NETWORK_MODE} 配置不合法（详见上方 ERR 行）"
fi

case "$NETWORK_MODE" in
  host)
    COMPOSE_FILES=""
    ;;
  macvlan|ipvlan)
    [ -f "./docker-compose.${NETWORK_MODE}.yml" ] \
      || die "docker-compose.${NETWORK_MODE}.yml 不存在（请先 git pull 更新项目文件）"
    COMPOSE_FILES="-f docker-compose.${NETWORK_MODE}.yml"
    warn "网络模式: ${NETWORK_MODE}（parent=${IKEV2_L2_PARENT} subnet=${IKEV2_L2_SUBNET} gateway=${IKEV2_L2_GATEWAY}）"
    if [ -n "${IKEV2_L2_IPV4:-}" ]; then
      warn "容器固定 IPv4: ${IKEV2_L2_IPV4}"
    else
      warn "容器固定 IPv4: (未设置，由 Docker 自动分配)"
    fi
    warn "切换网络模式必须重建容器，本次会自动 recreate"
    ;;
  *)
    # 兜底：net_mode_validate 当前只对 host/macvlan/ipvlan 返回 0。
    # 万一 network-mode.sh 以后加了新模式而这里没跟上，静默落到上面的
    # host 分支会「启动成功但网络模式不对」，所以显式拦下来。
    die "IKEV2_NETWORK_MODE=${NETWORK_MODE} 尚未被 scripts/up.sh 支持（compose 文件缺失），请检查 scripts/network-mode.sh 与本脚本是否同步"
    ;;
esac

# ---------- 固定 IPv4 与宿主现有地址的冲突检查（部署前）----------
#
# 为什么必须在**部署前**拦：docker daemon 只会拒「不在子网内」和
# 「网络自己已占用」的地址，**不会**知道宿主某个接口上已经有这个 IP。
#
# 实测（192.168.50.63，Docker 28.5.2，2026-10-21）：
#   固定 IP 填成宿主自己的 192.168.50.63 →
#     容器正常创建、正常启动，宿主 ARP 表被抢，
#     宿主的 SSH 连接直接 "Connection reset by peer"，
#     网关 ping 通但所有入站连接断掉 —— 宿主网络被打挂。
#
# 也就是说，**填错固定 IP 的代价可能不止是容器起不来，而是宿主失联**。
# daemon 拦不住，就只能由 up.sh 在 up 之前拦。
#
# 检查方式：列出宿主所有接口的 IPv4 地址，逐个比对。
_l2_ipv4_conflict_check() {
  [ -n "${IKEV2_L2_IPV4:-}" ] || return 0
  command -v ip >/dev/null 2>&1 || return 0
  _hits="$(ip -o -4 addr show 2>/dev/null \
    | awk '{for (i=1;i<=NF;i++) if ($i=="inet") {split($(i+1),a,"/"); print a[1]}}' \
    | grep -Fx "${IKEV2_L2_IPV4}" || true)"
  [ -z "$_hits" ] && return 0
  echo "ERR: IKEV2_L2_IPV4=${IKEV2_L2_IPV4} 与宿主已有地址冲突：" >&2
  echo "     宿主网卡上已经有这个 IP，容器启动后会抢走它（ARP 冲突），" >&2
  echo "     导致宿主入站连接全部断开（实测 SSH 直接被 reset）。" >&2
  echo "     请换一个宿主未占用的地址，或把 x-l2-ipv4 留空让 Docker 自动分配。" >&2
  return 1
}
if [ "$NETWORK_MODE" = "$NET_MODE_MACVLAN" ] || [ "$NETWORK_MODE" = "$NET_MODE_IPVLAN" ]; then
  _l2_ipv4_conflict_check \
    || die "固定 IPv4 与宿主地址冲突，拒绝启动（详见上方 ERR 行）"
fi

# ---------- 准备 ./logs bind mount 目标(v2.86-PR13.4)----------
# docker-compose 把容器内 /var/log bind 到宿主 ./logs。
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
# 把实际执行的命令原样打出来 —— 三个模式三个文件，用户最容易搞不清
# "up.sh 到底跑了什么"，打出来就没有黑盒感。
#
# 用 PASSTHRU_ARGS 数组而不是 "$@"：
#   "$@" 里还留着已被消费的 --no-detect，直接透传会让 docker compose 报
#   unknown flag（Docker Compose v2.40.3 实测）。
#   exec 时用 "${PASSTHRU_ARGS[@]}" 保持数组元素边界，
#   不会把 '--force-recreate' 连引号一起传下去。
#   set -u 下空数组直接展开 "${arr[@]}" 会报 unbound variable，
#   所以先判断长度，零个就直接不带参数。
run_compose() {
  if [ -n "$COMPOSE_FILES" ]; then
    if [ "${#PASSTHRU_ARGS[@]}" -eq 0 ]; then
      log "running: docker compose ${COMPOSE_FILES} up -d"
      # shellcheck disable=SC2086
      exec docker compose ${COMPOSE_FILES} up -d
    fi
    log "running: docker compose ${COMPOSE_FILES} up -d ${PASSTHRU_ARGS[*]}"
    # shellcheck disable=SC2086
    exec docker compose ${COMPOSE_FILES} up -d "${PASSTHRU_ARGS[@]}"
  fi

  if [ "${#PASSTHRU_ARGS[@]}" -eq 0 ]; then
    log "running: docker compose up -d"
    exec docker compose up -d
  fi
  log "running: docker compose up -d ${PASSTHRU_ARGS[*]}"
  exec docker compose up -d "${PASSTHRU_ARGS[@]}"
}

run_compose
