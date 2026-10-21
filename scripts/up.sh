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
#
# IKEV2_L2_NETWORK_NAME（v2.95）：共享网络名。它必须 export ——
# compose 的 networks 段用 ${IKEV2_L2_NETWORK_NAME:-<默认名>} 插值出网络名，
# 不 export 的话 compose 会退回默认值，而本脚本按 .env 的值去建网，
# 两边会建出**两条不同的网络**（一个叫 .env 里的名字，一个叫默认名），
# 表现为"建网成功但容器加入的是另一条网" —— 极难定位。
#
# 默认名随模式而变（macvlan → "macvlan"，ipvlan → "ipvlan"），
# 所以这里**不能**在读到空值时就补默认值 —— 那时模式还没归一化。
# 真正的默认值由后面的 _l2_shared_network_name() 在建网时按模式解析。
for _k in IKEV2_L2_PARENT IKEV2_L2_SUBNET IKEV2_L2_GATEWAY IKEV2_L2_IPV4 \
         IKEV2_L2_NETWORK_NAME; do
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
      IKEV2_L2_NETWORK_NAME) echo l2_netname ;;
    esac
  }
  for _k in IKEV2_L2_PARENT IKEV2_L2_SUBNET IKEV2_L2_GATEWAY IKEV2_L2_IPV4 \
           IKEV2_L2_NETWORK_NAME; do
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
    # 网络名必须 export（v2.95）：compose 的 networks 段用
    # ${IKEV2_L2_NETWORK_NAME:-ipvlan} 插值出网络名。
    # 不 export 的话，用户在 compose 顶部把 x-l2-netname 改成字面量
    # （如 &l2_netname "mynet"）时，up.sh 读到 "mynet" 而 compose 仍用
    # 默认的 "ipvlan" —— 两边加入**不同的网络**，且没有任何报错。
    [ "$_k" = "IKEV2_L2_NETWORK_NAME" ] && export IKEV2_L2_NETWORK_NAME
  done
  unset _anchor_key
fi

# 共享网络名是否**由用户显式指定**（v2.95）
#
# 这个标记决定「同名网络不存在时」的处置：
#   显式指定 → 不该被悄悄换成宿主上另一条参数兼容的网络（那是违背用户意图）
#   用默认值 → 可以自动接管（老用户升级 / 用户自建了别的名字的场景）
#
# 判定依据是「值不等于默认值」：无论它来自 shell env、.env 还是 compose 顶部
# anchor，只要用户写了非默认的名字，就是显式指定。
#
# 放在 anchor 回落**之后**算 —— 用户也可能在 compose 顶部写
# x-l2-netname: &l2_netname "mynet"（字面量），那同样是显式指定。
#
# 默认名随模式而变（macvlan → "macvlan"，ipvlan → "ipvlan"），
# 所以比的是**当前模式**的默认名，不是某个全局常量。
L2_NETWORK_NAME_EXPLICIT=0
if [ "$NETWORK_MODE" = "$NET_MODE_MACVLAN" ] || [ "$NETWORK_MODE" = "$NET_MODE_IPVLAN" ]; then
  L2_NETWORK_DEFAULT_NAME="$(net_shared_network_default_name "$NETWORK_MODE")"
  if [ -n "${IKEV2_L2_NETWORK_NAME:-}" ] \
     && [ "${IKEV2_L2_NETWORK_NAME}" != "$L2_NETWORK_DEFAULT_NAME" ]; then
    L2_NETWORK_NAME_EXPLICIT=1
  fi
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

# ---------- 共享 L2 网络：不存在则创建，存在则校验（v2.95 / v2.96）----------
#
# 为什么需要这一段：L2 模式的网络是**跨 project 共享**的
# （compose 里 external: true），compose 自己不建网络 —— 所以建网的责任
# 落到了本脚本上。
#
# 三件事：
#   1. 网络不存在 → 用与 compose 同一份参数建出来
#   2. 网络已存在 → 逐项比对参数，不符就报错并**列出差异**
#   3. 不符时绝不自动删改 —— 那条网络可能是别的项目正在用的
#
# macvlan 与 ipvlan **都走这套**（v2.96）：实测同一条 macvlan 网络上可以挂
# 多个容器、可以跨 project，IP 与 MAC 各不相同 —— macvlan 同样支持共享。
#
# 两者唯一的差别是 **parent 独占性**（实测，Docker 28.5.2）：
#   ipvlan  — parent 独占。同一张网卡上建不了第二条 ipvlan 网络（换子网也不行）。
#   macvlan — parent **不**独占。同一张网卡上可以有多条 macvlan 网络
#             （子网不重叠即可）。
# 所以 ipvlan 需要额外的 parent 冲突预检，macvlan 不需要。

# 从 docker network inspect 读一个字段（取不到输出空串）
#
# 一律用 `with ... {{.}}{{end}}` 包裹：Go template 的 index 在 key 不存在时
# 返回 nil，裸写会输出 "<no value>"，把网络名拼进 URL 之类的字符串时很难看。
_l2_net_field() {
  docker network inspect "$1" --format "$2" 2>/dev/null | head -n1
}

# 共享网络名：用户指定的优先，否则用**当前模式的默认名**
# （macvlan → "macvlan"，ipvlan → "ipvlan"，都不带 project 前缀）。
_l2_shared_network_name() {
  _d="$(net_shared_network_default_name "$NETWORK_MODE")"
  printf '%s' "${IKEV2_L2_NETWORK_NAME:-$_d}"
}

# 列出宿主机上所有指定 driver 的网络名（每个一行）
_l2_list_networks_by_driver() {
  docker network ls --filter "driver=$1" --format '{{.Name}}' 2>/dev/null
}

# driver 对应的 driver_opts 键名（ipvlan 有两个，macvlan 只有一个）
#
#   ipvlan  → ipvlan_mode（l2/l3/l3s） + ipvlan_flag（bridge/private/vepa）
#   macvlan → macvlan_mode（bridge/private/vepa），没有独立的 flag
_l2_mode_opt_key() {
  case "$1" in
    ipvlan)  echo ipvlan_mode ;;
    macvlan) echo macvlan_mode ;;
    *)       echo "" ;;
  esac
}
_l2_flag_opt_key() {
  case "$1" in
    ipvlan)  echo ipvlan_flag ;;
    *)       echo "" ;;
  esac
}

# 在宿主机上找一条「parent 与子网都对得上」的既有同驱动网络。
#
# 为什么需要这个（向后兼容 + 用户诉求）：
#   1. 老用户升级前已经有 `<project>_ikev2-l2` 这条网络了。新版默认名是
#      驱动名（"ipvlan" / "macvlan"），若不去找就会去新建 →
#      ipvlan 会撞 "already using parent interface"，老用户升级即失败。
#      所以要**自动接管**那条已有网络。
#   2. 用户的诉求是「我先建了一条网络，当前项目和其他项目都加入它」。
#      名字可能不是默认名（他可能叫别的），所以按 parent + 子网去认。
#
# 判据含 mode / flag —— l2 与 l3 的广播组播行为不同，flag（bridge/private/vepa）
# 决定容器之间能否互访。参数不同就不能当成"同一条网络"接管。
#
# 找到就返回网络名，找不到返回空串。
_l2_find_compatible() {
  _driver="$1"
  _want_parent="$2"; _want_subnet="$3"; _want_gateway="$4"
  _want_mode="$5"; _want_flag="$6"
  _mk="$(_l2_mode_opt_key "$_driver")"
  _fk="$(_l2_flag_opt_key "$_driver")"
  for _n in $(_l2_list_networks_by_driver "$_driver"); do
    [ -n "$_n" ] || continue
    _p="$(_l2_net_field "$_n" '{{with index .Options "parent"}}{{.}}{{end}}')"
    _s="$(_l2_net_field "$_n" '{{with (index .IPAM.Config 0).Subnet}}{{.}}{{end}}')"
    _g="$(_l2_net_field "$_n" '{{with (index .IPAM.Config 0).Gateway}}{{.}}{{end}}')"
    _m="$(_l2_net_field "$_n" "{{with index .Options \"$_mk\"}}{{.}}{{end}}")"
    _fl=""
    [ -n "$_fk" ] && _fl="$(_l2_net_field "$_n" "{{with index .Options \"$_fk\"}}{{.}}{{end}}")"
    if [ "$_p" = "$_want_parent" ] && [ "$_s" = "$_want_subnet" ] \
       && [ "$_g" = "$_want_gateway" ] && [ "$_m" = "$_want_mode" ] \
       && [ "$_fl" = "$_want_flag" ]; then
      printf '%s' "$_n"
      return 0
    fi
  done
  return 1
}

# 判断宿主上是否已存在同名的 L2 网络，并核对关键参数。
#
# 返回码：
#   0 = 可用（不存在，或存在且参数一致）
#   1 = 参数不一致（已打印差异）
_l2_shared_network_verify() {
  _name="$1"
  _driver="$2"          # ipvlan / macvlan
  _parent="$3"
  _subnet="$4"
  _gateway="$5"
  _mode="$6"            # ipvlan_mode 或 macvlan_mode
  _flag="$7"            # ipvlan_flag（macvlan 不用，传空）

  if ! docker network inspect "$_name" >/dev/null 2>&1; then
    return 0
  fi

  _a_driver="$(_l2_net_field "$_name" '{{.Driver}}')"
  _a_parent="$(_l2_net_field "$_name" '{{index .Options "parent"}}')"
  _a_mode="$(_l2_net_field "$_name" "{{index .Options \"$(_l2_mode_opt_key "$_driver")\"}}")"
  _a_flag=""
  if [ -n "$(_l2_flag_opt_key "$_driver")" ]; then
    _a_flag="$(_l2_net_field "$_name" "{{index .Options \"$(_l2_flag_opt_key "$_driver")\"}}")"
  fi
  # IPAM 的第一个 config 段就是 IPv4 段
  _a_subnet="$(_l2_net_field "$_name" '{{(index .IPAM.Config 0).Subnet}}')"
  _a_gateway="$(_l2_net_field "$_name" '{{(index .IPAM.Config 0).Gateway}}')"
  _a_v6="$(_l2_net_field "$_name" '{{.EnableIPv6}}')"

  _diff=""
  [ "$_a_driver" = "$_driver" ]   || _diff="$_diff"$'\n'"     驱动:             期望=$_driver          实际=$_a_driver"
  [ "$_a_parent" = "$_parent" ]   || _diff="$_diff"$'\n'"     父接口:           期望=$_parent          实际=$_a_parent"
  [ "$_a_subnet" = "$_subnet" ]   || _diff="$_diff"$'\n'"     子网:             期望=$_subnet          实际=$_a_subnet"
  [ "$_a_gateway" = "$_gateway" ] || _diff="$_diff"$'\n'"     网关:             期望=$_gateway        实际=$_a_gateway"
  [ "$_a_mode" = "$_mode" ]       || _diff="$_diff"$'\n'"     $(_l2_mode_opt_key "$_driver"): 期望=$_mode  实际=$_a_mode"
  [ -z "$_flag" ] || { [ "$_a_flag" = "$_flag" ] || _diff="$_diff"$'\n'"     $(_l2_flag_opt_key "$_driver"): 期望=$_flag  实际=$_a_flag"; }
  [ "$_a_v6" = "true" ]           || _diff="$_diff"$'\n'"     IPv6:             期望=enable_ipv6=true  实际=EnableIPv6=$_a_v6"

  if [ -n "$_diff" ]; then
    echo "ERR: 已存在的网络 '$_name' 与当前配置不匹配：" >&2
    printf '%s\n' "$_diff" | sed 's/^/       /' >&2
    echo "     这条网络可能是别的项目正在用的（共享网络的 IPAM 归建网者所有）。" >&2
    echo "     处置二选一：" >&2
    echo "       1) 把当前配置的参数改成与它一致（推荐，不影响别人）" >&2
    echo "       2) 换一条自己的网络：设置 IKEV2_L2_NETWORK_NAME=<别的名字>" >&2
    return 1
  fi
  return 0
}

# 预检 + 必要时创建共享网络
_l2_shared_network_ensure() {
  _driver="$1"; _parent="$2"; _subnet="$3"; _gateway="$4"; _mode="$5"; _flag="$6"
  _name="$(_l2_shared_network_name)"

  # 情况 1：同名网络已存在 → 直接复用（但要先校参数）
  if docker network inspect "$_name" >/dev/null 2>&1; then
    ok "共享网络 '$_name' 已存在（复用，不修改）"
    _l2_shared_network_verify "$_name" "$_driver" "$_parent" "$_subnet" "$_gateway" "$_mode" "$_flag" \
      || die "共享网络 '$_name' 参数不匹配，拒绝启动（详见上方 ERR 行）"
    return 0
  fi

  # 情况 2：同名不存在 → 找宿主机上「parent + 子网都对得上」的既有同驱动网络。
  #   这一步同时覆盖两种真实诉求：
  #     · 老用户升级：老网络叫 <project>_ikev2-l2，parent/子网一致 → 自动接管
  #     · 用户自建：用户先建了一条叫别的名字的网络 → 自动加入它
  #
  # ⚠️ 只有**用默认名**（用户没显式指定）时才自动接管。
  #    用户显式写了网络名 = 明确要求用那一条，不该被悄悄换成另一条。
  #    显式指定但那条不存在、而另有一条参数兼容的 → 报清楚让用户选。
  if _found="$(_l2_find_compatible "$_driver" "$_parent" "$_subnet" "$_gateway" "$_mode" "$_flag")" \
     && [ -n "$_found" ]; then
    if [ "$L2_NETWORK_NAME_EXPLICIT" = "0" ]; then
      ok "宿主机上已有兼容的 $_driver 网络 '$_found'（parent/子网/模式一致）→ 直接复用"
      warn "  提示：可用 IKEV2_L2_NETWORK_NAME=$_found 显式指定，避免以后误判"
      # compose 必须加入**同一条**网络。compose 的网络名来自 networks 段的
      # ${IKEV2_L2_NETWORK_NAME:-<驱动名>}，所以这里把变量指向实际复用的那条。
      export IKEV2_L2_NETWORK_NAME="$_found"
      return 0
    fi
    echo >&2
    echo "ERR: 你指定的共享网络 '$_name' 不存在" >&2
    echo "     但宿主机上已有一条参数兼容的 $_driver 网络 '$_found'（parent/子网/模式一致）。" >&2
    echo "     处置二选一：" >&2
    echo "       1) 用它：设置 IKEV2_L2_NETWORK_NAME=$_found" >&2
    echo "       2) 坚持用 '$_name'：先删掉 '$_found'（docker network rm $_found）" >&2
    die "指定的共享网络不存在"
  fi

  # 情况 3：parent 上已有别的同驱动网络（子网不同）→ 明确报冲突。
  #   ⚠️ 这个检查**只对 ipvlan 成立** —— 实测 macvlan 的 parent **不**独占，
  #      同一张网卡上可以有多条 macvlan 网络（子网不重叠即可）。
  #      ipvlan 的 parent 是独占的，同一个网卡上建不了第二条 ipvlan 网络，
  #      与其让 daemon 报一句英文，不如这里说清楚。
  if [ "$_driver" = "ipvlan" ]; then
    for _n in $(_l2_list_networks_by_driver ipvlan); do
      [ -n "$_n" ] || continue
      _p="$(_l2_net_field "$_n" '{{with index .Options "parent"}}{{.}}{{end}}')"
      [ "$_p" = "$_parent" ] || continue
      _s="$(_l2_net_field "$_n" '{{with (index .IPAM.Config 0).Subnet}}{{.}}{{end}}')"
      _g="$(_l2_net_field "$_n" '{{with (index .IPAM.Config 0).Gateway}}{{.}}{{end}}')"
      echo >&2
      echo "ERR: parent 接口 $_parent 上已有一条 ipvlan 网络 '$_n'（subnet=$_s gateway=$_g）" >&2
      echo "     ipvlan 的 parent 是**独占**的，同一张网卡上建不了第二条 ipvlan 网络。" >&2
      echo "     处置二选一：" >&2
      echo "       1) 复用它：把配置改成它的 subnet/gateway，" >&2
      echo "          或设置 IKEV2_L2_NETWORK_NAME=$_n" >&2
      echo "       2) 换一张网卡做 parent" >&2
      die "ipvlan 网络与宿主既有网络冲突"
    done
  fi

  log "共享网络 '$_name' 不存在 → 创建（driver=$_driver parent=$_parent subnet=$_subnet）"
  # --subnet / --gateway 必须显式给：不给的话 Docker 会塞默认池 172.18.0.0/16，
  # 之后任何 join 方指定 ipv4_address 都会被拒
  # （"user specified IP address is supported only when connecting to networks
  #   with user configured subnets"）。
  #
  # --ipv6 让 Docker 自动补一个 v6 网段并打开 enable_ipv6 ——
  # 容器端点建好时 disable_ipv6 才会是 0，接口才收得到 RA（拿 GUA）。
  _cmd=(docker network create
        --driver "$_driver"
        --opt "parent=$_parent"
        --opt "$(_l2_mode_opt_key "$_driver")=$_mode")
  [ -n "$_flag" ] && _cmd+=(--opt "$(_l2_flag_opt_key "$_driver")=$_flag")
  _cmd+=(--subnet "$_subnet" --gateway "$_gateway" --ipv6 "$_name")

  if ! "${_cmd[@]}"; then
    echo >&2
    echo "ERR: 创建共享网络 '$_name' 失败。常见原因：" >&2
    if [ "$_driver" = "ipvlan" ]; then
      echo "     · parent 接口 $_parent 上已有一条 ipvlan 网络（ipvlan 的 parent 独占）" >&2
      echo "       → 复用那条：把 IKEV2_L2_NETWORK_NAME 设成它的名字" >&2
    fi
    echo "     · 同子网已存在别的 L2 网络（Docker IPAM 全局，不允许重叠）" >&2
    echo "       → 复用它：把 IKEV2_L2_NETWORK_NAME 设成它的名字，或把子网改成不重叠的" >&2
    echo "     · 父接口名写错（用 ip -o -4 route show default 查真实值）" >&2
    die "共享网络创建失败"
  fi
  ok "共享网络 '$_name' 已创建"

  # 建完再校一次 —— 建网参数若被 daemon 调整过（比如 v6 段），这里能发现
  _l2_shared_network_verify "$_name" "$_driver" "$_parent" "$_subnet" "$_gateway" "$_mode" "$_flag" \
    || die "新建的共享网络 '$_name' 参数不符合预期（详见上方 ERR 行）"
}

if [ "$NETWORK_MODE" = "$NET_MODE_IPVLAN" ]; then
  _l2_shared_network_ensure \
    "ipvlan" \
    "${IKEV2_L2_PARENT}" "${IKEV2_L2_SUBNET}" "${IKEV2_L2_GATEWAY}" \
    "${IKEV2_IPVLAN_MODE:-l2}" "${IKEV2_IPVLAN_FLAG:-bridge}"
elif [ "$NETWORK_MODE" = "$NET_MODE_MACVLAN" ]; then
  # macvlan 同样支持跨 project 共享同一条网络（v2.96）——
  # 实测同一条 macvlan 网络上可挂多个容器，各拿独立 MAC 与 IP。
  # macvlan 没有独立的 flag 参数，mode 就是 macvlan_mode（bridge/private/vepa）。
  _l2_shared_network_ensure \
    "macvlan" \
    "${IKEV2_L2_PARENT}" "${IKEV2_L2_SUBNET}" "${IKEV2_L2_GATEWAY}" \
    "${IKEV2_MACVLAN_MODE:-bridge}" ""
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
