#!/bin/bash
# 容器入口：网络模式校验 → 网卡探测 → forwarding → MASQUERADE →
#           swanctl.conf 占位符替换 → charon → swanctl 首次加载 → exec Go
# 设计说明见 docs/architecture.md §3.1（启动数据流）与 §6.1（环境变量）
#
# v2-79 重大改动：§0 自动探测链 + 所有硬编码可选
#   - 优先用环境变量；没传就自动探测；自动探测失败才报 FATAL
#   - 用户部署时**可以什么都不传**（只要宿主有公网 IPv6 + 默认路由）
#
# v2.86-PR20 重大改动：网络模式从 host-only 扩展为 host / macvlan / ipvlan
#   - 模式规格与校验集中在 scripts/network-mode.sh（唯一数据源），本文件只 source
#   - host 模式行为**逐字节不变**；L2 两模式只是把「共享宿主 netns」换成「容器私有 netns」
#
# 退出码：
#   0  正常
#   10 IPv6 公网地址不存在（IPv6_ONLY=true 时）
#   11 forwarding 启用失败
#   12 网络模式配置错误 / charon 启动失败
#   13 swanctl --load-all 失败（多次重试后仍失败）
#   14 探测不到对外网卡（含 L2 模式下 IKEV2_OUT_IF 在容器内不存在）
#   15 iptables MASQUERADE 添加失败
#   16 swanctl.conf 占位符替换失败
set -euo pipefail

LOG_PREFIX="[entrypoint]"

# ---------- 网络模式规格表（v2.86-PR20）----------
# 只定义常量与函数，无副作用。找不到文件时降级为「只有 host 可用」，
# 保证老镜像 / 手工构建的环境不会因缺文件而启动失败。
NETMODE_LIB="/etc/ikev2-panel/scripts/network-mode.sh"
if [ -f "$NETMODE_LIB" ]; then
  # shellcheck disable=SC1090
  . "$NETMODE_LIB"
else
  echo "${LOG_PREFIX} FATAL: $NETMODE_LIB not found (image build incomplete)" >&2
  exit 12
fi

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
IKEV2_NETWORK_MODE="${IKEV2_NETWORK_MODE:-host}"   # v2.86-PR20:host | macvlan | ipvlan
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
  # v2.86-pr23o:加 `|| true` 兜底 —— grep 在没匹配时返 1,
  # `set -euo pipefail` 会让整个 $(...) 表达式失败 → entrypoint 立即 exit 1。
  # 历史 bug:旧 cert.conf(在 v2.86-pr23o 之前保存的)缺 server_cn/acme_email 字段,
  # → 容器秒死 → restart:unless-stopped 看到非 0 → 无限重启。
  # 修法双保险:① Go 端 CertConfig struct 不再用 omitempty(根因);
  #            ② bash 端 grep || true 兜底(老 cert.conf 也能启动)。
  cert_mode_conf=$(grep -E '"cert_mode"[[:space:]]*:' "$CERT_CONF" 2>/dev/null | sed -nE 's/.*"cert_mode"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/p' | head -1 || true)
  domain_conf=$(grep -E '"domain"[[:space:]]*:' "$CERT_CONF" 2>/dev/null | sed -nE 's/.*"domain"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/p' | head -1 || true)
  cn_conf=$(grep -E '"server_cn"[[:space:]]*:' "$CERT_CONF" 2>/dev/null | sed -nE 's/.*"server_cn"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/p' | head -1 || true)
  email_conf=$(grep -E '"acme_email"[[:space:]]*:' "$CERT_CONF" 2>/dev/null | sed -nE 's/.*"acme_email"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/p' | head -1 || true)

  # 用覆盖赋值的方式让 cert.conf 优先 env
  [ -n "$cert_mode_conf" ] && IKEV2_CERT_MODE="$cert_mode_conf"
  [ -n "$domain_conf" ] && IKEV2_DOMAIN="$domain_conf"
  [ -n "$cn_conf" ] && IKEV2_SERVER_CN="$cn_conf"
  [ -n "$email_conf" ] && IKEV2_ACME_EMAIL="$email_conf"

  # v2.86-PR19(A11):LE 模式下 ServerCN 强制等于 Domain。
  #
  # 背景:cert.conf 里 server_cn 绝大多数情况是空串(LE 模式下这个字段本来就没用),
  # 上面第 76 行 `[ -n "$cn_conf" ] && ...` 在空串时不执行 → IKEV2_SERVER_CN
  # 保留 compose/env 里的默认值 vpn.example.com。
  # 而 Go 端 main.go 的兜底是 `if cfg.ServerCN == ""` 条件式 —— 看到非空的
  # vpn.example.com 就不兜底了,于是 mobileconfig 里的
  # ServerCertificateCommonName = vpn.example.com,而证书 CN = vpn.rewind2023.cn。
  # iOS 用 ServerCertificateCommonName 匹配证书 CN/SAN,不匹配就静默失败,
  # 客户端只报 "an unexpected error occurred"。
  #
  # LE 签发时证书 CN 强制等于 domain(acme.sh 的 LE 模式不给自定义 CN 的余地),
  # 所以这里无条件覆盖不会破坏任何合法配置 —— LE 模式下"ServerCN 与 Domain
  # 不同"本来就是非法状态。
  if [ "${IKEV2_CERT_MODE:-}" = "letsencrypt" ] && [ -n "${IKEV2_DOMAIN:-}" ]; then
    IKEV2_SERVER_CN="${IKEV2_DOMAIN}"
  fi

  echo "${LOG_PREFIX} [cert.conf] applied: cert_mode=${IKEV2_CERT_MODE} domain=${IKEV2_DOMAIN:-<empty>} server_cn=${IKEV2_SERVER_CN:-<empty>}"
fi

# ---------- §-0.5: v2.86-PR13.3:面板运行时 subnet 配置(优先级最高,对称 cert.conf)----------
# 背景:subnet.conf 已经在 panelstate 里实现(v2.86-PR13.2),但 entrypoint §0.6 完全没读这个文件,
# 重启容器后 entrypoint 会用 env / auto 探测覆盖 swanctl.conf,丢失面板用户的设定。
# v2.86-PR13.3 把 subnet.conf 提到 entrypoint §-0.5 优先级最高(对称 cert.conf 模式):
#
#   1. /data/panel-state/subnet.conf (面板 UI 填的,运行时改,需重启容器生效)
#   2. IKEV2_VPN_SUBNET / IKEV2_VPN_SUBNET_V6 env (老用户兼容,启动期生效)
#   3. entrypoint §0.6 自动探测 (没填没传时)
#
# 注意:运行时(handler "保存并立即生效"按钮)走的 UpdatePoolsAndReload 路径不需要重启,
# 已经能立即改 swanctl.conf。本节只解决"重启容器后值不被 entrypoint 覆盖"。
SUBNET_CONF="/data/panel-state/subnet.conf"
SUBNET_CONF_USED=0
if [ -f "$SUBNET_CONF" ]; then
  # 用 grep + sed 简单提取(镜像没装 jq)。字段顺序不重要。
  subnet_v4_conf=$(grep -E '"ipv4_subnet"[[:space:]]*:' "$SUBNET_CONF" | sed -nE 's/.*"ipv4_subnet"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/p' | head -1)
  subnet_v6_conf=$(grep -E '"ipv6_subnet"[[:space:]]*:' "$SUBNET_CONF" | sed -nE 's/.*"ipv6_subnet"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/p' | head -1)
  if [ -n "$subnet_v4_conf" ]; then
    IKEV2_VPN_SUBNET="$subnet_v4_conf"
    # v6 允许为空(用户可能只在面板改了 v4);空时保留 env 的 v6,不动
    if [ -n "$subnet_v6_conf" ]; then
      IKEV2_VPN_SUBNET_V6="$subnet_v6_conf"
    fi
    SUBNET_CONF_USED=1
    echo "${LOG_PREFIX} [subnet.conf] applied: ipv4=${IKEV2_VPN_SUBNET} ipv6=${IKEV2_VPN_SUBNET_V6}"
  else
    echo "${LOG_PREFIX} WARN: $SUBNET_CONF exists but ipv4_subnet missing or invalid, fallback to env/auto" >&2
  fi
fi

# ---------- §-0.6: v2.86 批次 E:面板运行时「高级设置」(对称 cert.conf / subnet.conf)----------
# 背景:批次 E 把日志格式、审计保留天数、限速阈值、MSS clamp、DDoS 开关、
# nft 地址族这 6 项搬上面板。它们的消费者**全在 entrypoint 里**(nft 规则、
# /etc/cron.d),Go 进程运行期改了没人读 —— 必须让 entrypoint 也认 settings.json,
# 否则用户在面板里改完,重启容器后又被 env 覆盖回去,且没有任何提示。
#
# 优先级链(高 → 低),跟 §-1 / §-0.5 完全同构:
#   1. /data/panel-state/settings.json (面板 UI 填的,运行时改,需重启容器生效)
#   2. env(IKEV2_*)
#   3. 本节里的 `:-` 兜底
#
# 三点跟 §-1 的差别:
#   - log_level 不在这里处理:Go 进程自己的 logger 在 main.go 里构造,
#     且 config.SetLogLevel 已经支持运行期热改,不需要 entrypoint 参与。
#   - 数值项做了范围校验再采纳,越界只 WARN 不 exit —— 面板端已经拦过一次,
#     这里防的是人工编辑 settings.json 的情况,不该让容器起不来。
#   - 布尔项只认 "true"(其它一律当作没配),这跟 getbool 的宽松语义不同,
#     但这里宁严勿松:写错成 "flase" 时,保持"防护开启"比误关掉安全规则好。
PANEL_SETTINGS="/data/panel-state/settings.json"
if [ -f "$PANEL_SETTINGS" ]; then
  settings_get_str() {
    grep -E "\"$1\"[[:space:]]*:" "$PANEL_SETTINGS" 2>/dev/null \
      | sed -nE "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"([^\"]*)\".*/\1/p" | head -1 || true
  }
  settings_get_num() {
    grep -E "\"$1\"[[:space:]]*:" "$PANEL_SETTINGS" 2>/dev/null \
      | sed -nE "s/.*\"$1\"[[:space:]]*:[[:space:]]*([0-9]+).*/\1/p" | head -1 || true
  }
  settings_get_bool() {
    grep -E "\"$1\"[[:space:]]*:" "$PANEL_SETTINGS" 2>/dev/null \
      | sed -nE "s/.*\"$1\"[[:space:]]*:[[:space:]]*(true|false).*/\1/p" | head -1 || true
  }
  # 数值范围校验:非纯数字(空 / 负数 / 带小数)一律视为未配置
  settings_num_ok() {
    case "$1" in
      ''|*[!0-9]*) return 1 ;;
      *) return 0 ;;
    esac
  }

  _v="$(settings_get_str log_format)"
  if [ -n "$_v" ]; then IKEV2_LOG_FORMAT="$_v"; fi

  _v="$(settings_get_num audit_retention_days)"
  if settings_num_ok "$_v" && [ "$_v" -ge 1 ] && [ "$_v" -le 3650 ]; then
    IKEV2_AUDIT_RETENTION_DAYS="$_v"
  elif [ -n "$_v" ]; then
    echo "${LOG_PREFIX} WARN: settings.json audit_retention_days=${_v} out of [1,3650], keeping env" >&2
  fi

  _v="$(settings_get_num connlimit_per_ip)"
  if settings_num_ok "$_v" && [ "$_v" -ge 1 ] && [ "$_v" -le 10000 ]; then
    IKEV2_CONNLIMIT_PER_IP="$_v"
  elif [ -n "$_v" ]; then
    echo "${LOG_PREFIX} WARN: settings.json connlimit_per_ip=${_v} out of [1,10000], keeping env" >&2
  fi

  _v="$(settings_get_num hashlimit_per_min)"
  if settings_num_ok "$_v" && [ "$_v" -ge 1 ] && [ "$_v" -le 10000 ]; then
    IKEV2_HASHLIMIT_PER_MIN="$_v"
  elif [ -n "$_v" ]; then
    echo "${LOG_PREFIX} WARN: settings.json hashlimit_per_min=${_v} out of [1,10000], keeping env" >&2
  fi

  _v="$(settings_get_bool disable_mss_clamp)"
  if [ "$_v" = "true" ]; then IKEV2_DISABLE_MSS_CLAMP=true; fi

  _v="$(settings_get_bool disable_ddos_protection)"
  if [ "$_v" = "true" ]; then IKEV2_DISABLE_DDOS_PROTECTION=true; fi

  _v="$(settings_get_str nft_family)"
  if [ "$_v" = "v4" ] || [ "$_v" = "v6" ]; then IKEV2_FAMILY="$_v"; fi

  # 回传给 Go 进程:Go 侧 status API 要显示"当前生效值",
  # 而它只读 env,读不到上面这些 shell 局部赋值(见 §-0.6 结尾的 export)。
  export IKEV2_LOG_FORMAT IKEV2_AUDIT_RETENTION_DAYS IKEV2_CONNLIMIT_PER_IP \
         IKEV2_HASHLIMIT_PER_MIN IKEV2_DISABLE_MSS_CLAMP IKEV2_DISABLE_DDOS_PROTECTION \
         IKEV2_FAMILY

  echo "${LOG_PREFIX} [settings.json] applied: log_format=${IKEV2_LOG_FORMAT:-text} audit_days=${IKEV2_AUDIT_RETENTION_DAYS:-90} connlimit=${IKEV2_CONNLIMIT_PER_IP:-50} hashlimit=${IKEV2_HASHLIMIT_PER_MIN:-30} mss_clamp_disabled=${IKEV2_DISABLE_MSS_CLAMP:-false} ddos_disabled=${IKEV2_DISABLE_DDOS_PROTECTION:-false} family=${IKEV2_FAMILY:-auto}"
fi

# ====================================================================
# §0 自动探测链（v2-79）
# 任何"用户环境相关"的参数都不应该写死。探测顺序：环境变量 → 自动探测 → FATAL
# ====================================================================

# ---------- §0.0 网络模式归一化 + 校验（v2.86-PR20）----------
# 必须排在 §0.1 之前：OUT_IF 的语义随模式变化（宿主网卡名 vs 容器网卡名），
# §0.1 的存在性校验要用到模式信息。
#
# 校验逻辑全部在 scripts/network-mode.sh（唯一数据源），这里只负责：
#   1. 归一化（大小写 / 空白）并回写 IKEV2_NETWORK_MODE
#   2. 调用 net_mode_validate，失败 exit 12（语义与 v2.86-PR14 一致）
#   3. 打印模式摘要
IKEV2_NETWORK_MODE="$(net_mode_normalize "$IKEV2_NETWORK_MODE")"
if ! net_mode_validate "$IKEV2_NETWORK_MODE"; then
  echo "${LOG_PREFIX} FATAL: IKEV2_NETWORK_MODE=${IKEV2_NETWORK_MODE} 配置错误（详见上方 ERR）" >&2
  exit 12
fi

# 声明的模式 vs 实际 netns 是否一致（防御"改了 .env 但忘了重建容器"）
# 判据：host 模式必须有 DOCKER-USER 之外的特征 —— 直接比对 lo 之外的接口数
# 不可靠（多网卡宿主会误判），改用 /proc/self/ns/net 与宿主是否同 inode 的
# 近似手段代价太高；这里采用成本最低且足够可靠的方式：
#   L2 模式必然看不到宿主上常见的 docker0 桥，且必然有一块非 ethX 命名的
#   单网卡环境。真正强判据留给 §0.7（那里能拿到 OUT_IF 后再核对）。
detect_actual_netns_kind() {
  # 容器私有 netns 下，/proc/sys/net/ipv4/conf/all/rp_filter 的默认值与
  # 宿主一致，不可靠。改用最朴素的判据：是否存在 docker0 桥。
  if ip link show docker0 >/dev/null 2>&1; then
    echo "host-shared"
  else
    echo "private"
  fi
}

# ---------- §0.1 探测对外网卡（优先用 IKEV2_OUT_IF）----------
detect_out_if() {
  # 1. 用户传了 → 验证存在即可
  if [ -n "$IKEV2_OUT_IF" ]; then
    if ip link show "$IKEV2_OUT_IF" >/dev/null 2>&1; then
      echo "$IKEV2_OUT_IF"
      return 0
    fi
    # v2.86-PR20：非 host 模式下，用户极可能沿用 host 时代的宿主网卡名
    # （如 ens18 / enp0s3）。这个值在容器 netns 内不存在，nft 模板会生成一条
    # 永不匹配的 oifname 规则 → MASQUERADE 静默失效 → 客户端能连 VPN 但没网。
    # 这是本次改造唯一会「启动成功但功能不可用」的静默故障，必须 FATAL。
    if net_mode_has_own_ip "$IKEV2_NETWORK_MODE"; then
      echo "${LOG_PREFIX} FATAL: IKEV2_OUT_IF=${IKEV2_OUT_IF} 在容器内不存在" >&2
      echo "${LOG_PREFIX}   当前网络模式: ${IKEV2_NETWORK_MODE}（容器有独立 netns，网卡名与宿主不同）" >&2
      echo "${LOG_PREFIX}   修复 1（推荐）: 从 .env 删掉 IKEV2_OUT_IF 这一行，让脚本自动探测（容器内通常是 eth0）" >&2
      echo "${LOG_PREFIX}   修复 2: 改成容器内的实际网卡名 —— docker exec <ctr> ip -br link 后填对应名字" >&2
      exit 14
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

# ⚠️ 必须回写 env（v2.86-PR20 审查修复）。
#
# 探测结果之前只是个 shell 局部变量，下游消费者拿不到：
#   1. scripts/ikev2-updown 由 charon 在 SA up/down 时拉起，读的是继承的
#      IKEV2_OUT_IF，兜底 `:-eth0`。宿主网卡叫 ens18/enp0s3 时 tc 全部失败，
#      而命令后面跟着 `|| true` → 日志照样打 "Applied 100Mbps"，
#      限速功能静默失效。
#   2. Go 侧 cmd/ikev2-panel/main.go 两处 os.Getenv("IKEV2_OUT_IF")
#      （DDNS Iface、ipv6watch Iface）同样拿不到。
#   3. internal/config/config.go 的 OutIf 默认值是 "eth0"，一样是错的。
# 回写之后三个下游与 entrypoint 自身看到的是同一个网卡，语义统一。
export IKEV2_OUT_IF="$OUT_IF"

# ---------- §0.2 探测 IPv6 公网地址（2000::/3，避开 ULA/Link-local）----------
#
# /proc/net/if_inet6 每行 6 个字段（实测 v2.86-PR20 复核）：
#   $1 addr(32hex)  $2 ifindex(dec)  $3 prefixlen(hex)
#   $4 scope(hex)   $5 flags(hex)    $6 devname
#
# ⚠️ v2.86-PR20 重写。原实现有**两个**独立缺陷，都只在 L2 模式暴露：
#
# 【缺陷 1】把 scope 当成 flags
#   原过滤条件是 `$4=="01" || $4=="00"`，$4 是 scope 而非 flags。
#   scope=00 恰好等价于 global scope，所以平时「看起来能用」，
#   但 `01` 根本不是合法 scope 值（合法值是 00/10/20/40/80/c0），
#   真正的地址状态位在 $5 —— 于是 tentative / dadfailed 从未被排除。
#   实测 flags 位（veth 上制造 DAD 中 / DAD 冲突两种状态交叉验证）：
#     80        稳定 global
#     82        nodad（跳过 DAD，稳定）
#     c0        DAD 进行中（tentative）    ← 原实现会误选
#     c8        DAD 冲突（dadfailed + tentative）← 原实现会误选
#     含 0x20   deprecated
#   L2 模式下 RA 触发的地址必然先经过 DAD，不过滤就会把一个「还不能用」的
#   地址写进 swanctl.conf 的 local_addrs → charon 绑定失败或客户端握手异常。
#
# 【缺陷 2】每张网卡只取第一条地址（影响更大）
#   原实现外层 `awk '...{print $6}'` 逐行打印**网卡名**（同一张网卡出现几次
#   就打印几次），内层 `awk -v d=$dev '$6==d && ...{print $1; exit}'` 的 exit
#   只取该网卡上的**第一条**匹配地址。
#   macvlan/ipvlan 的容器 eth0 上，Docker 分的 ULA（fd48:...）总是先于
#   SLAAC 拿到的 GUA 出现；内层拿到 ULA → case 2xxx: 匹配不上 → continue，
#   而外层下一轮喂的还是同一张网卡名 → 又拿到同一条 ULA → 永远走不到 GUA。
#   实测：同一份 if_inet6，把 ULA 行挪到 GUA 前面，原实现输出从
#   `2408:832e:...` 变成**空字符串**。也就是 L2 模式下 IKEV2_SERVER_ADDR_V6
#   经常探测不到，只能靠 .env 手工兜底 —— 正是本次要消灭的隐式行为。
#
# 判定方式刻意用 ip(8) 的权威文字标签（tentative/dadfailed/deprecated）
# 而不是硬编码 flags 位 —— 位含义随内核版本演进，文字标签是稳定契约。
# 顺带天然解决缺陷 2：ip -6 addr show 按地址逐行输出，不做「每网卡取第一条」。
# 输出格式（ip -6 addr show，每地址一行）:
#   inet6 2001:db8::2/64 scope global
#   inet6 2001:db8::1/64 scope global nodad
#   inet6 2001:db8::1/64 scope global dadfailed tentative
#   inet6 fe80::1/64 scope link proto kernel_ll
# $1=inet6 $2=cidr $3=scope $4=scope值 其后为可选状态标签
detect_public_ipv6() {
  ip -6 addr show 2>/dev/null \
    | awk '
        $1 == "inet6" {
          ip = $2
          sub(/\/.*/, "", ip)
          bad = 0
          for (i = 4; i <= NF; i++) {
            if ($i == "tentative" || $i == "dadfailed" || $i == "deprecated") bad = 1
          }
          if (bad) next
          # 只保留 global unicast 2000::/3，即 2000:: ~ 3fff::
          # （首 hextet 的首位是 2 或 3；fe80::/10 与 fd00::/8 天然不匹配）
          if (ip ~ /^[23][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F]:/) print ip
        }' \
    | head -n1
}

# v2.86-PR20：macvlan / ipvlan 模式下 IPv6 来自 RA + SLAAC，
# 新配的地址要过 DAD（Duplicate Address Detection），
# 在收敛前会被内核标记 tentative / dadfailed（见 §0.2 的 detect_public_ipv6）。
#
# 这里做有限次重试而不是一次即判死：L2 模式下 DAD 未收敛是**正常时序**，
# 不是配置错误。host 模式下地址早已收敛，保持单次探测（行为不变）。
#
# v2.86-PR20 实测修正（Docker 28.5.2，macvlan，192.168.50.63 parent=ens18）：
# 原来 3 次 × 2 秒 = 最多等 4 秒，**不够**。实测时间线：
#     t=0s   只有 Docker 自带的 ULA fd48:...::2（SLAAC 还没拿到 GUA）
#     t=4s   GUA 2408:832e:8ae:a170:... 才以 preferred 出现
# 原因是 RA 由上游路由器按周期发送（该网段 RA 间隔约 10s 级），
# 叠加 DAD 约 1s，最坏要等一个完整 RA 周期。
# 改成 6 次 × 3 秒 = 最多 15 秒，覆盖绝大多数 RA 周期。
# 这是启动期一次性等待，不影响运行期性能。
#
# ⚠️ 修好 §0.2 之后，这个循环的语义变了，要重新理解它「退出」的条件：
#   旧实现：只要网卡上**有任意一条** 2000::/3 地址就返回 ——
#           ULA 排在前面时反而返回空，拿到 GUA 后又太早返回（DAD 中）。
#           两个方向的错都有。
#   新实现：只有「存在一条 DAD 已收敛的 GUA」才返回 —— 这才是本循环
#           真正想等的那个状态。L2 模式下 RA 周期 ~10s + DAD ~1s，
#           6×3s=15s 的上限正好覆盖一个完整 RA 周期。
# 换句话说：循环从「可有可无的重试」变成了「L2 模式启动的关键路径」。
detect_public_ipv6_with_retry() {
  local tries=1 interval=2
  if net_mode_has_own_ip "$IKEV2_NETWORK_MODE"; then
    tries=6
    interval=3
  fi
  local i addr
  for i in $(seq 1 "$tries"); do
    addr="$(detect_public_ipv6 || true)"
    if [ -n "$addr" ]; then
      [ "$i" -gt 1 ] && echo "${LOG_PREFIX} [auto-detect] Public IPv6 converged after ~$(( (i-1)*interval ))s (RA/SLAAC/DAD)" >&2
      echo "$addr"
      return 0
    fi
    [ "$i" -lt "$tries" ] && sleep "$interval"
  done
  return 1
}

if [ -z "$IKEV2_SERVER_ADDR_V6" ] && [ "$IKEV2_IPV6_ONLY" = "true" ]; then
  IPV6_ADDR_DETECTED="$(detect_public_ipv6_with_retry || true)"
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

# v2.86-PR13.3:subnet.conf 已应用 → 跳过 auto-detect,保留面板用户的值。
# 候选检测只对 "auto" 字符串有意义;IKEV2_VPN_SUBNET 被 §-0.5 覆盖后是 "10.10.20.0/24"
# 这种具体值,本来就走不到 auto 分支,但加双保险避免脚本未来误改 env 路径时回归。
if [ "${SUBNET_CONF_USED}" = "1" ]; then
  echo "${LOG_PREFIX} [subnet.conf] skip auto-detect for v4 subnet (panel state wins)"
elif [ "$IKEV2_VPN_SUBNET" = "auto" ]; then
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

# v2.86-PR13.3:对称 v4,v6 同样在 subnet.conf 应用时跳过 auto-detect。
if [ "${SUBNET_CONF_USED}" = "1" ]; then
  echo "${LOG_PREFIX} [subnet.conf] skip auto-detect for v6 subnet (panel state wins)"
elif [ "$IKEV2_VPN_SUBNET_V6" = "auto" ]; then
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
    IPV6_ADDR="$(detect_public_ipv6_with_retry || true)"
    IKEV2_SERVER_ADDR_V6="$IPV6_ADDR"
  fi

  if [ -z "${IPV6_ADDR:-}" ]; then
    echo "${LOG_PREFIX} FATAL: no global IPv6 address found" >&2
    echo "${LOG_PREFIX} This project REQUIRES a public IPv6 address when IKEV2_IPV6_ONLY=true" >&2
    if net_mode_has_own_ip "$IKEV2_NETWORK_MODE"; then
      # L2 模式专属排障：IPv6 走 RA/SLAAC，失败原因基本收敛到这四条
      echo "${LOG_PREFIX} 当前模式=${IKEV2_NETWORK_MODE}，IPv6 依赖上游路由器的 RA + SLAAC，请逐条排查：" >&2
      echo "${LOG_PREFIX}   1) 上游路由器是否真的在发 RA（tcpdump -i ${IKEV2_L2_PARENT:-eth0} icmp6 and ip6[40]==134）" >&2
      echo "${LOG_PREFIX}   2) 容器网卡 disable_ipv6 是否为 0：docker exec <ctr> cat /proc/sys/net/ipv6/conf/${OUT_IF}/disable_ipv6" >&2
      echo "${LOG_PREFIX}   3) 容器网卡 accept_ra 是否为 2：docker exec <ctr> cat /proc/sys/net/ipv6/conf/${OUT_IF}/accept_ra" >&2
      echo "${LOG_PREFIX}      （本项目必开 IP forwarding，内核会把 accept_ra 抑制掉；L2 compose 文件已注入 net.ipv6.conf.default.accept_ra=2）" >&2
      echo "${LOG_PREFIX}   4) 宿主/云厂商是否拦截了来自新 MAC 的 RA（macvlan 特有，见 README 前置条件）" >&2
    else
      echo "${LOG_PREFIX} Configure IPv6 on the host network first, or set IKEV2_IPV6_ONLY=false" >&2
    fi
    exit 10
  fi
  echo "${LOG_PREFIX} Using IPv6 address: ${IPV6_ADDR}"
fi

# v2-79：把自动探测结果导出，供 §5 占位符替换使用
export IKEV2_SERVER_ADDR_V6 IKEV2_SERVER_ADDR_V4 IKEV2_VPN_SUBNET VPN_SUBNET_BASE VPN_SERVER_VIP IKEV2_VPN_SUBNET_V6

# ---------- §0.7 网络模式运行时确认（v2.86-PR20）----------
# 合法性与必填项的校验已在 §0.0 完成（net_mode_validate，失败 exit 12）。
# 本段负责两件事：
#   1. 打印模式摘要（可观测性：出问题时第一眼看这一行）
#   2. 「声明的模式 vs 实际 netns」交叉核对 —— 防御改了 .env 但忘了重建容器
#
# 删除了 v2.86-PR14 的硬编码 case，也删除了死变量 IKEV2_ACTUAL_NETWORK_MODE
# （导出后全库无消费者），改为写入 /run/ikev2-panel/netmode.env。
NETMODE_ENV_DIR="/run/ikev2-panel"
mkdir -p "$NETMODE_ENV_DIR"
{
  printf 'MODE=%s\n'        "$IKEV2_NETWORK_MODE"
  printf 'OUT_IF=%s\n'      "$OUT_IF"
  printf 'DRIVER=%s\n'     "$(net_mode_driver_desc "$IKEV2_NETWORK_MODE")"
  printf 'HAS_OWN_IP=%s\n'  "$(net_mode_has_own_ip "$IKEV2_NETWORK_MODE" && echo 1 || echo 0)"
  printf 'PROCSYS_RW=%s\n'  "$(net_mode_procsys_rw "$IKEV2_NETWORK_MODE" && echo 1 || echo 0)"
  printf 'DDOS_CHAIN=%s\n'  "$(net_mode_ddos_chain "$IKEV2_NETWORK_MODE")"
} > "$NETMODE_ENV_DIR/netmode.env"
chmod 644 "$NETMODE_ENV_DIR/netmode.env"

case "$IKEV2_NETWORK_MODE" in
  host)
    echo "${LOG_PREFIX} [network-mode] host (shares host netns; UDP 500/4500 + TCP 8443 on host ports)"
    ;;
  macvlan|ipvlan)
    echo "${LOG_PREFIX} [network-mode] ${IKEV2_NETWORK_MODE} ($(net_mode_driver_desc "$IKEV2_NETWORK_MODE"))"
    echo "${LOG_PREFIX} [network-mode] parent=${IKEV2_L2_PARENT} subnet=${IKEV2_L2_SUBNET} gateway=${IKEV2_L2_GATEWAY}"
    # 固定 IPv4（可选）。留空 = Docker IPAM 自动分配，这里回显实际拿到的地址，
    # 让「我配的固定地址到底生效没有」一眼可见，不用 docker inspect。
    if [ -n "${IKEV2_L2_IPV4:-}" ]; then
      echo "${LOG_PREFIX} [network-mode] static ipv4=${IKEV2_L2_IPV4}"
    else
      echo "${LOG_PREFIX} [network-mode] static ipv4=(auto-assigned by Docker IPAM)"
    fi
    echo "${LOG_PREFIX} [network-mode] container iface=${OUT_IF} (IKEV2_OUT_IF 语义为容器网卡名，不是宿主网卡名)"
    echo "${LOG_PREFIX} [network-mode] IPv6 via RA/SLAAC; injected sysctls: $(net_mode_sysctls | tr '\n' ' ')"
    # 交叉核对：容器私有 netns 下看不到宿主常见的 docker0 桥。
    # 反过来若声明 L2 却看到了 docker0，几乎必然是忘了重建容器。
    _actual_kind="$(detect_actual_netns_kind)"
    if [ "$_actual_kind" = "host-shared" ]; then
      echo "${LOG_PREFIX} WARN: declared ${IKEV2_NETWORK_MODE} but the container still shares the host netns (docker0 visible)." >&2
      echo "${LOG_PREFIX}   → 几乎必然是改了 IKEV2_NETWORK_MODE 但没重建容器。执行: docker compose down && ./scripts/up.sh" >&2
    fi
    unset _actual_kind
    ;;
esac

# ---------- 3. 检查 IP forwarding（v2-79 三层兜底：compose sysctls → entrypoint 自愈 → FATAL）----------
#  必需：容器作为 VPN 路由器，VPN 客户端流量要从容器转发到公网。
#  IPv6：net.ipv6.conf.all.forwarding=1
#  IPv4：net.ipv4.ip_forward=1
#
# 网络模式差异（v2.86-PR20）：
#   host    : 容器与宿主共享 netns，且 /proc/sys 是 ro 挂载 → 自愈**不生效**，
#             forwarding 必须由宿主保证（v2.86-PR19 A16 实测结论，见下）
#   macvlan / ipvlan : 容器有独立 netns，/proc/sys 可写 → 自愈**能真正生效**，
#             这是 L2 模式相对 host 模式的一个实质改善
#
# 镜像里没有 sysctl 二进制（debian-slim），所以这里用 echo 直写 /proc/sys。
#
# v2-79.2 增强：自愈成功后**同时把值持久化到宿主 /etc/sysctl.d/99-ikev2.conf**。
# 实现：docker-compose.yml 把宿主 /etc/sysctl.d bind mount 到 /host-sysctl.d（rw）。
# 这样：
#   - 当前会话立即生效（写 /proc/sys）
#   - 宿主重启 → systemd-sysctl.service 加载 /etc/sysctl.d/*.conf → 持久保留
#   - 下次部署到新机器，第一次启动也会自愈 + 持久化
# 无 99-ikev2.conf 也不要紧：自愈成功后才写，不会覆盖宿主已有配置。
#
# v2.86-PR20：L2 模式下容器是独立 netns，写宿主 /etc/sysctl.d 只会改**宿主**
# 的 forwarding，对容器 netns 无效（sysctl 的 forwarding 是 per-netns 的）。
# 所以 L2 模式跳过持久化，避免给宿主写一条"看起来在起作用、实际只作用于
# 另一个 netns"的配置造成误判。
persist_sysctl() {
  local key="$1" value="$2"   # key = 完整 sysctl 名（如 net.ipv6.conf.all.forwarding）
  local host_dir="/host-sysctl.d"
  local host_conf="${host_dir}/99-ikev2.conf"

  if net_mode_has_own_ip "$IKEV2_NETWORK_MODE"; then
    # 容器私有 netns：写宿主 sysctl.d 对本容器无效，不做（避免误导）
    return 0
  fi

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
      if net_mode_has_own_ip "$IKEV2_NETWORK_MODE"; then
        echo "${LOG_PREFIX} [auto-heal] ${path} set to 1 (container net ns, L2 mode)" >&2
      else
        echo "${LOG_PREFIX} [auto-heal] ${path} set to 1 (host net ns, effective on host)" >&2
      fi
      persist_sysctl "$sysctl_name" "1"
      return 0
    fi
  fi
  return 1
}

# v2.86-PR19(A16):这里曾经包在 `if [ "$IKEV2_IPV6_ONLY" != "true" ]` 里,
# 只有双栈模式才自愈 IPv6 forwarding。**这是错的**,现改为无条件尝试。
#
# 为什么错(192.168.50.63 实测,IPV6_ONLY=true 即默认配置):
#   实机 /proc/sys/net/ipv6/conf/*/forwarding 全线为 0,而且因为
#   `IPV6_ONLY=true` 走了跳过分支,enable_forwarding ipv6 **一次都没被调用过**。
#
# 但 IPv6 内层流量其实是通的(设计如此):
#   local_ts = 0.0.0.0/0, ::/0   ← §5 恒定双栈,不受 IPV6_ONLY 影响
#   addrs    = 10.10.0.0/24, fd00:1::/64  ← 客户端会拿到 ULA IPv6
#   dns      = 1.1.1.1, 8.8.8.8, 2606:4700:4700::1111, ... ← 已推 IPv6 DNS
#   nft ip6 ikev26 masquerade + MSS / DOCKER-USER ip6tables ipsec0 ACCEPT 全部就位
# 也就是说"IPv6 转发链路"只差 forwarding 这一个开关 —— 而它偏偏被条件跳过了。
#
# 后果(不是"完全断网",所以很容易被漏掉):
#   iOS/Android 会优先用 ULA IPv6 发包 → 内核 forwarding=0 直接丢弃(无 ICMP)
#   → Happy Eyeballs 超时后回退 IPv4 → 用户看到"能上网",只是慢一拍。
#   已经推给客户端的 IPv6 DNS 服务器则完全不可达。
#   192.168.50.63 上 nft table ip6 ikev26 所有 counter 恒为 0,与该分析一致。
#
# 与 IPV6_ONLY 的关系:v2.86-PR12.10 起 local_ts 恒为双栈,IPV6_ONLY
# 只控制"监听/拨号用哪个网络栈"(IKE 协商走 IPv6),**不影响 ESP 内层协议族**。
# 内层协议族既然不受它影响,转发开关就不该受它控制 —— 原条件分支与该前提自相矛盾。
#
# ⚠️ 为什么 IPv6 失败只 WARN 不 FATAL,而 IPv4 失败 FATAL(2026-10-13 实测修正):
#   宿主上 /proc/sys 是 **ro** 挂载:
#     $ grep ' /proc/sys ' /proc/mounts
#     proc /proc/sys proc ro,nosuid,nodev,noexec,relatime 0 0
#   → enable_forwarding 的 `echo 1 > $path` 在容器内**永远失败**,
#     NET_ADMIN 也救不了(ro 是挂载属性,不是 capability)。
#   → 也就是说 v4 / v6 的"自愈"从来没真正成功过。IPv4 之所以一路绿灯,
#     只是因为宿主本来就开着 ip_forward=1,走的是函数开头的"已开直接返回"。
#
#   所以:
#     - IPv4=0 是**致命**的:没有任何转发 → 所有客户端都出不了网,
#       且这是本项目的核心功能,必须启动即失败。
#     - IPv6=0 只是**降级**:客户端会 IPv4 回退,IPv4 主链路完好,面板和
#       VPN 都能正常使用。此时 FATAL 会让整个面板起不来,代价远大于收益。
#       正确做法是明确告警 + 给出宿主侧修复命令,让运维自己决定。
if [ -e /proc/sys/net/ipv6/conf/all/forwarding ]; then
  if ! enable_forwarding ipv6; then
    echo "${LOG_PREFIX} WARN: net.ipv6.conf.all.forwarding=0 and cannot auto-enable — IPv6 内层转发不可用,客户端会回退 IPv4" >&2
    echo "${LOG_PREFIX}   影响：客户端拿到 fd00:1:: 地址后会先试 IPv6,被内核静默丢弃后回退 IPv4(能上网但慢一拍);已推送的 IPv6 DNS 不可达" >&2
    if net_mode_has_own_ip "$IKEV2_NETWORK_MODE"; then
      # L2 模式：容器有独立 netns，/proc/sys 可写，失败说明容器真的没 NET_ADMIN
      echo "${LOG_PREFIX}   当前模式=${IKEV2_NETWORK_MODE}，容器 netns 内 /proc/sys 应当可写" >&2
      echo "${LOG_PREFIX}   排查：docker inspect ikev2-panel --format '{{.HostConfig.CapAdd}}' 应含 NET_ADMIN" >&2
    else
      echo "${LOG_PREFIX}   宿主侧修复：sudo sysctl -w net.ipv6.conf.all.forwarding=1" >&2
      echo "${LOG_PREFIX}   持久化    ：echo 'net.ipv6.conf.all.forwarding=1' | sudo tee /etc/sysctl.d/99-ikev2.conf" >&2
      echo "${LOG_PREFIX}   若 /proc/sys 是 ro 挂载(容器内常见),NET_ADMIN 也无法写,只能从宿主执行上面两条" >&2
    fi
  fi
else
  echo "${LOG_PREFIX} WARN: kernel built without IPv6, skipping net.ipv6.conf.all.forwarding" >&2
fi
if ! enable_forwarding ipv4; then
  echo "${LOG_PREFIX} FATAL: net.ipv4.ip_forward=0 and cannot auto-enable" >&2
  if net_mode_has_own_ip "$IKEV2_NETWORK_MODE"; then
    echo "${LOG_PREFIX} 当前模式=${IKEV2_NETWORK_MODE}，容器 netns 内 /proc/sys 应当可写" >&2
    echo "${LOG_PREFIX} 排查 1：docker inspect ikev2-panel --format '{{.HostConfig.CapAdd}}' 应含 NET_ADMIN" >&2
    echo "${LOG_PREFIX} 排查 2：docker exec ikev2-panel sh -c 'echo 1 > /proc/sys/net/ipv4/ip_forward' 看是否 Permission denied" >&2
  else
    echo "${LOG_PREFIX} 提示 1：sudo sysctl -w net.ipv4.ip_forward=1" >&2
    echo "${LOG_PREFIX} 提示 2：echo 'net.ipv4.ip_forward=1' | sudo tee /etc/sysctl.d/99-ikev2.conf（宿主侧持久化）" >&2
    echo "${LOG_PREFIX} 提示 3：若 /proc/sys 是 ro 挂载(容器内常见),NET_ADMIN 也无法写,只能从宿主执行上面两条" >&2
  fi
  echo "${LOG_PREFIX} 提示 4：IPv4 forwarding=0 时所有客户端都出不了网,这里不降级" >&2
  exit 11
fi
echo "${LOG_PREFIX} IP forwarding OK (v4=$(cat /proc/sys/net/ipv4/ip_forward 2>/dev/null), v6=$(cat /proc/sys/net/ipv6/conf/all/forwarding 2>/dev/null))"

# ---------- 3.2 SLAAC 状态复核（仅 L2 模式，v2.86-PR20）----------
# 为什么必须在开 forwarding **之后**复核：
#   内核 accept_ra 的功能性默认是「forwarding 关闭时启用、开启时禁用」。
#   §3 刚把 forwarding 打开 → 内核会走 addrconf_forward_change()，
#   把处于**默认态**的接口 accept_ra 强制刷成 0。
#   L2 compose 用 sysctls 写了 net.ipv6.conf.default.accept_ra=2（显式值不是
#   默认态，内核不会再改写），但那是"应该成立"，不是"一定成立"——
#   一旦用户改了 compose 文件、漏了 enable_ipv6、或换了不同 Docker 版本，
#   就会静默失效，表现为"客户端能连 VPN 但没有 IPv6 出口"。
#   这里显式回读一次：被改写就 FATAL，给出可执行的修复动作，而不是让用户
#   在 §1 拿到一个含糊的 "no global IPv6 address"。
#
# 期望值取自 net_mode_sysctl_checks()，与 compose 的 sysctls 块同源。
# 注意 disable_ipv6 不在复核列表里：它由 networks 段的 enable_ipv6: true 保证
# （实测不开就是 1，且没有任何 sysctl 能把它从 service 段改回来），
# 真出问题会表现为下面的 accept_ra=2 仍然拿不到 GUA，由地址探测兜住。
if net_mode_has_own_ip "$IKEV2_NETWORK_MODE" && [ -e "/proc/sys/net/ipv6/conf/${OUT_IF}/accept_ra" ]; then
  _slaac_bad=""
  _slaac_shown=""
  # 用 here-doc + 重定向而不是管道：管道会把 while 放进子 shell，
  # 循环内的赋值传不回来，"有不符合项"就会被静默当成通过。
  while IFS='|' read -r _sc_path _sc_want; do
    [ -n "$_sc_path" ] || continue
    _sc_real="${_sc_path//IFNAME/$OUT_IF}"
    _sc_got="$(cat "/proc/sys/${_sc_real}" 2>/dev/null || echo '?')"
    _slaac_shown="${_slaac_shown} ${_sc_real}=${_sc_got}"
    [ "$_sc_got" = "$_sc_want" ] || _slaac_bad="${_slaac_bad} ${_sc_real}=${_sc_got}(期望 ${_sc_want})"
  done <<EOF
$(net_mode_sysctl_checks)
EOF

  if [ -z "$_slaac_bad" ]; then
    echo "${LOG_PREFIX} [slaac] ${OUT_IF}:${_slaac_shown} — RA/SLAAC 可用"
  else
    echo "${LOG_PREFIX} FATAL: ${OUT_IF} sysctl 不符合预期 → IPv6 SLAAC 不可用" >&2
    echo "${LOG_PREFIX}   不符项：${_slaac_bad}" >&2
    echo "${LOG_PREFIX}   期望（net_mode_sysctl_checks）：" >&2
    net_mode_sysctl_checks | sed 's/^/     /' >&2
    echo "${LOG_PREFIX}   常见原因：L2 compose 文件被改动，或 networks 段漏了 enable_ipv6: true" >&2
    echo "${LOG_PREFIX}   排查：docker inspect ikev2-panel --format '{{.HostConfig.Sysctls}}'" >&2
    echo "${LOG_PREFIX}   修复：docker compose -f docker-compose.${IKEV2_NETWORK_MODE}.yml down && docker compose -f docker-compose.${IKEV2_NETWORK_MODE}.yml up -d" >&2
    exit 10
  fi
  unset _slaac_bad _slaac_shown _sc_path _sc_want _sc_real _sc_got
fi

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

    # 1b. table 220 必须同时带**直连网段**路由,否则同网段回包会绕网关。
    #
    # 背景（v2.92 实测,192.168.50.63 + ipvlan + 固定 IP 192.168.50.66）:
    #   面板 8443 从局域网访问时,TCP 能建连但 TLS 握手必被 RST,浏览器打不开。
    #   抓包定位:RST 的源 MAC 是**网关**的 MAC(不是容器的),且
    #   SYN-ACK / ServerHello 的源 MAC 同样被换成网关的 —— 说明
    #   容器发往同网段的对端回包,**先经过了网关**。
    #
    # 根因:strongSwan charon 的 kernel-netlink 插件会装一条
    #   `ip rule add priority 220 from all lookup 220`
    # 把**所有**出向流量的查表强制到 table 220。而 table 220 里只有
    #   default via <网关>
    # 这一条 —— 同网段的 192.168.50.0/24 在 table 220 里查不到直连路由,
    # 于是**回包也被 default 带走,绕网关出去**。
    # 绕网关 = 源 MAC 被换成网关的 + 网关对该会话做了不对称路径处理
    # → 注入 RST → 浏览器侧表现为"连上了但立刻被重置"。
    #
    # 为什么 host 模式没事:host 模式下容器共享宿主 netns,宿主有
    # `ip rule 10: from <宿主IP> lookup route_ens18` 这条更优先的规则,
    # 源地址是宿主 IP 的流量走 route_ens18 表,压根不进 table 220。
    # L2 模式容器是独立 netns,没有这条规则,只能靠 table 220 自己带直连路由。
    #
    # 修法:把 main 表里 dev $OUT_IF 的直连(scope link)路由**复制**到 table 220。
    #   - 同网段回包 → table 220 命中直连路由 → 源 MAC 是容器自己的 → 不绕网关 → 正常
    #   - 出公网     → table 220 仍走 default via 网关 → XFRM 出向行为完全不变
    # 注意:不能用 `ip route show ... dev $OUT_IF` —— 带了 dev 过滤后
    # 输出里**不再有 dev 字段**,后面就没法判断这条路由属于哪个接口了。
    # 所以这里先取全部 scope link 路由,再用 awk 按 dev 过滤。
    _t220_pfx_list="$(ip -4 route show table main scope link 2>/dev/null \
      | awk -v want="$OUT_IF" '{for (i=1;i<=NF;i++) if ($i=="dev" && $(i+1)==want) {print $1; exit}}' \
      || true)"
    if [ -n "$_t220_pfx_list" ]; then
      while read -r _lr_pfx; do
        [ -n "$_lr_pfx" ] || continue
        # scope link 表示"直连,不经网关" —— 抄进 table 220 后同网段回包不再绕网关
        ip route replace "$_lr_pfx" dev "$OUT_IF" scope link table 220 2>/dev/null \
          && echo "${LOG_PREFIX} table 220 direct route added: ${_lr_pfx} dev ${OUT_IF} scope link" \
          || echo "${LOG_PREFIX} WARN: failed to add table 220 direct route ${_lr_pfx}" >&2
      done <<EOF
$_t220_pfx_list
EOF
      unset _t220_pfx_list _lr_pfx
    else
      echo "${LOG_PREFIX} WARN: no scope-link route on $OUT_IF, table 220 may lack direct route" >&2
    fi

    # 2. v2.86-PR12.16:nft 自定义表 ikev2 / ikev26 一次性原子提交
    #    替换原来的 iptables -I FORWARD / -t nat -A POSTROUTING 零散操作
    #    docker daemon 重写 filter/nat 表不会碰自定义表,根治 PR12.13 二次 iptables-restore 挤掉规则
    #
    # v2.86-PR19(A14):失败不再 exit 15,改为降级。
    #
    # 原来 `nft -f` 失败就 exit 15 → restart: unless-stopped 无限重启
    # (实测 RestartCount 一路涨到 9,面板完全起不来)。
    # 但这张表只负责"VPN 客户端的转发 + NAT",失败不该让整个面板不可用 ——
    # 面板网页、用户管理、证书签发、DDNS、审计都不依赖它。
    #
    # 关于"iifname 指向的接口必须存在"这个假设:实测**不成立**。
    # 我一度怀疑是时序问题(nft 规则在 charon 启动前应用,而 ipsec0 由 charon
    # 创建),专门写了"轮询等 ipsec0 出现"的逻辑,然后在 192.168.50.63 上
    # 用 NET_ADMIN 容器实测:
    #   iifname "nosuchif0" accept            → 加载成功
    #   oifname "nosuchif0" accept            → 加载成功
    #   含 ipsec0 引用的完整模板(接口全不存在) → 加载成功
    #   表已存在时 delete + nft -f             → 加载成功
    #   表已存在时直接叠加 nft -f              → 加载成功
    # 所以那次"Operation not supported"另有原因(排查期间我手工往宿主 nft
    # 加载过带重复 chain 的表,污染了环境),**不是模板也不是时序**。
    # 等待逻辑已撤掉 —— charon 在 §6 才启动,§3.5 等它出现会白等 30 秒。
    #
    # 保留的改动只有一条:失败降级 + 清半成品表,让 §7.9 的 retry 循环去补。
    if [ -f "$NFT_TEMPLATE" ]; then
      NFT_RUNTIME="/run/ikev2-panel/ikev2.nft"
      mkdir -p /run/ikev2-panel
      # 标记「本次启动确实要装这张表」。§7.9 的完整性校验循环据此决定
      # 是真去补救，还是压根不该跑（见 §7.9 开头说明）。
      NFT_EXPECTED=1
      sed -e "s|@OUT_IF4@|${OUT_IF}|g" \
          -e "s|@OUT_IF6@|${OUT_IF}|g" \
          -e "s|@VPN_SUBNET4@|${IKEV2_VPN_SUBNET}|g" \
          -e "s|@VPN_SUBNET6@|${IKEV2_VPN_SUBNET_V6:-fd00::/64}|g" \
          "$NFT_TEMPLATE" > "$NFT_RUNTIME"

      if $NFT_CMD -f "$NFT_RUNTIME" 2>&1; then
        echo "${LOG_PREFIX} nft ikev2/ikev26 ruleset applied (atomic via $NFT_RUNTIME)"
      else
        # v2.86-PR19(A14):不再 exit 15。
        #
        # 原来失败就 exit 15 → restart: unless-stopped 无限重启。
        # 但 nft 规则只影响"VPN 客户端的转发/NAT",失败不该让整个面板不可用
        # —— 面板网页、用户管理、证书签发、DDNS、审计都不依赖这张表。
        # 降级为:删掉半成品表 + 写告警,让 §7.9 的 retry 循环去补。
        echo "${LOG_PREFIX} ERROR: nft -f failed, degrading (retry loop will re-apply)" >&2
        $NFT_CMD delete table ip ikev2  2>/dev/null || true
        $NFT_CMD delete table ip6 ikev26 2>/dev/null || true
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

# ---------- 4.6. DDoS 防护（v2.86-PR9 修正；v2.86-PR20 按模式选链）----------
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
#      次日容器启动时 §4.7 cleanup-on-startup 会清 DOCKER-USER 里残留的旧规则
#
# v2.86-PR20 模式差异：
#   host    → 目标链 DOCKER-USER（共享宿主 netns，docker daemon 建这条链）
#   macvlan / ipvlan → 目标链 INPUT（容器私有 netns，docker daemon 不在容器内建链；
#                     走容器自己的 INPUT 链，同样覆盖到入站 UDP 500/4500，
#                     且规则随容器销毁而消失，天然无残留）
#   两种模式都用同一个 §4.7 清理函数，按 net_mode_ddos_chain 取链名。
#
# 设计权衡：
#   - connlimit：每 IP 最大 50 并发 SA（家用 1-50 人，每人 ≤ 2 设备 = 100 并发上限）
#   - hashlimit：每分钟最多 30 次新 SA 协商
#   - 关键：connlimit / hashlimit 必须放在 ACCEPT 之前，否则无效
#     DOCKER-USER 链里 RETURN 走默认行为，DROP 走我们自己规则；放最前即可
#   - 1-50 人规模下，新 SA 速率阈值 30/min 不会误伤真实用户
#
# v2.86-PR9：默认阈值，可通过 IKEV2_CONNLIMIT_PER_IP / IKEV2_HASHLIMIT_PER_MIN 调。

DDOS_CHAIN="$(net_mode_ddos_chain "$IKEV2_NETWORK_MODE")"

apply_ddos_protection() {
  local iptables_bin="$1"
  local chain="$DDOS_CHAIN"
  if ! command -v "$iptables_bin" >/dev/null 2>&1; then
    echo "${LOG_PREFIX} WARN: $iptables_bin not found, skipping DDoS protection" >&2
    return 0
  fi

  # host 模式下 DOCKER-USER 链可能不存在（docker daemon 不一定建）
  # 不存在 → 跳过（host 网络本身没 docker 网桥,这条规则意义不大）
  if [ "$chain" = "DOCKER-USER" ] && ! "$iptables_bin" -nL "$chain" >/dev/null 2>&1; then
    echo "${LOG_PREFIX} $iptables_bin DOCKER-USER chain not present (host net?), skipping DDoS protection"
    return 0
  fi

  local CONNLIMIT_PER_IP="${IKEV2_CONNLIMIT_PER_IP:-50}"
  local HASHLIMIT_PER_MIN="${IKEV2_HASHLIMIT_PER_MIN:-30}"

  # 幂等：检查规则是否已存在（用 comment 锚点定位我们自己的规则）
  # 不用 -C 因为 connlimit/hashlimit 的全规则匹配会因为 comment 不同漏判
  if "$iptables_bin" -nL "$chain" 2>/dev/null | grep -q "v2.86-PR9 connlimit per-IP SA cap"; then
    echo "${LOG_PREFIX} $iptables_bin connlimit (${chain}) already present"
  else
    "$iptables_bin" -I "$chain" 1 -p udp --dport 500 \
      -m connlimit --connlimit-above "$CONNLIMIT_PER_IP" --connlimit-mask 32 \
      -m comment --comment "v2.86-PR9 connlimit per-IP SA cap" \
      -j DROP \
      && echo "${LOG_PREFIX} $iptables_bin connlimit (UDP/500, ${chain}) added" \
      || echo "${LOG_PREFIX} WARN: failed to add $iptables_bin connlimit" >&2
  fi

  if "$iptables_bin" -nL "$chain" 2>/dev/null | grep -q "v2.86-PR9 new-SA rate per IP"; then
    echo "${LOG_PREFIX} $iptables_bin hashlimit (${chain}) already present"
  else
    "$iptables_bin" -I "$chain" 2 -p udp --dport 500 \
      -m hashlimit --hashlimit-above "${HASHLIMIT_PER_MIN}/min" --hashlimit-burst 10 \
      --hashlimit-mode srcip --hashlimit-name ikev2_ddos \
      -m comment --comment "v2.86-PR9 new-SA rate per IP" \
      -j DROP \
      && echo "${LOG_PREFIX} $iptables_bin hashlimit (UDP/500, ${chain}) added" \
      || echo "${LOG_PREFIX} WARN: failed to add $iptables_bin hashlimit" >&2
  fi
}

# ---------- 4.5c. §4.7.1 启动清理（必须早于 §4.6 加规则）----------
# 顺序很关键：这里清的是"上一轮容器留下的"锚点规则。放在 §4.6 之后的话，
# 刚加进去的规则会被自己刚跑完的清理按同样的 comment 锚点删掉 —— 表现为
# entrypoint 日志打印 "connlimit ... added"，重启后 DOCKER-USER 却是空的。
# 背景与设计权衡见 §4.7（退出时 trap 与本函数对称）。
cleanup_ikev2_iptables_rules() {
  local iptables_bin="$1"
  local chain="$DDOS_CHAIN"
  if ! command -v "$iptables_bin" >/dev/null 2>&1; then
    return 0
  fi
  # 1) 清目标链里带 v2.86-PR9 锚点的规则(DDoS 防护)
  if "$iptables_bin" -nL "$chain" >/dev/null 2>&1; then
    local line_nums
    line_nums=$("$iptables_bin" -nL "$chain" --line-numbers 2>/dev/null \
      | awk '/v2.86-PR9 connlimit per-IP SA cap/ {print $1}' \
      | tac)
    for n in $line_nums; do
      "$iptables_bin" -D "$chain" "$n" 2>/dev/null || true
    done
    line_nums=$("$iptables_bin" -nL "$chain" --line-numbers 2>/dev/null \
      | awk '/v2.86-PR9 new-SA rate per IP/ {print $1}' \
      | tac)
    for n in $line_nums; do
      "$iptables_bin" -D "$chain" "$n" 2>/dev/null || true
    done
  fi
}

if command -v "$NFT_CMD" >/dev/null 2>&1; then
  $NFT_CMD delete table ip ikev2  2>/dev/null || true
  $NFT_CMD delete table ip6 ikev26 2>/dev/null || true
fi
cleanup_ikev2_iptables_rules "iptables" 2>/dev/null || true
cleanup_ikev2_iptables_rules "ip6tables" 2>/dev/null || true

if [ "${IKEV2_DISABLE_DDOS_PROTECTION:-false}" = "true" ]; then
  echo "${LOG_PREFIX} DDoS protection disabled (IKEV2_DISABLE_DDOS_PROTECTION=true)"
else
  if [ "$IKEV2_IPV6_ONLY" = "false" ]; then
    apply_ddos_protection "iptables"
  fi
  apply_ddos_protection "ip6tables"
fi

# ---------- 4.7. 启动时清理残留 + 退出时 trap 清理 ----------
# 目的:解决"容器 restart 循环 + iptables 规则残留"的污染问题。
#
# v2.86-PR12.16 重大改动:不再清理 iptables FORWARD / nat / mangle 链(我们的规则
# 全在自定义 nft 表 ikev2 / ikev26 里,docker daemon 不碰 → 无残留风险)。
# 只需清 §4.6 DDoS 加的链规则(host 模式是 DOCKER-USER,daemon 会重写 → 我们的
# connlimit/hashlimit 会残留 → 必须显式清)。
#
# v2.86-PR20:L2 模式下目标链是容器自己的 INPUT,容器销毁即消失,理论上无残留;
# 但 docker exec 里手工加过规则、或 kill -9 后 netns 被复用的极端情况仍需清理,
# 所以统一按 DDOS_CHAIN 清理,行为对称。
#
# 两层防护:
#   1. **启动时清理**(§4.5c,在 §4.6 加规则之前跑):清目标链里属于本项目的旧规则
#      (v2.86-PR9 注释锚点)+ nft delete table ip ikev2 / ip6 ikev26(双保险,kill -9 兜底)
#      - 处理 docker kill -9 场景(trap 没机会跑,下次启动补清)
#   2. **退出时 trap**(这里):docker stop SIGTERM / docker kill 优雅退出时清规则 + nft
#      - 兼顾正常 docker-compose down / restart

# ---------- 4.7b. 退出时 trap（优雅退出兜底）----------
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
  # v2.86-PR19(A11)修复:local.id 必须与证书 CN 一致,否则 EAP 阶段必失败。
  #
  # 实机故障(192.168.50.63,LE 模式,domain=vpn.rewind2023.cn):
  #   certs = vpn.rewind2023.cn.pem   ← 证书文件名按 IKEV2_DOMAIN 替换,正确
  #   id    = vpn.example.com         ← 但 CN 走的是另一个变量,落到默认值
  # iOS 连上后卡在 IKE_AUTH,charon.log 反复打:
  #     EAP-Identity request configured, but not supported
  #     initiating EAP_MSCHAPV2 method
  #     authentication of 'vpn.example.com' (myself) with RSA signature successful
  # 然后 half open IKE_SA 超时销毁,客户端报 "user authentication failed"。
  #
  # 原因:strongSwan 拿 local.id 去跟证书做匹配,id=vpn.example.com 而证书
  # CN=vpn.rewind2023.cn → 匹配不上,EAP 阶段直接断。LE 模式下证书 CN 就是
  # IKEV2_DOMAIN(LE 签发时强制要求),所以 local.id 必须取它。
  #
  # 自签模式保持用 IKEV2_SERVER_CN(自签证书的 CN 由面板设置决定)。
  if [ "${IKEV2_CERT_MODE:-self-signed}" = "letsencrypt" ] && [ -n "${IKEV2_DOMAIN:-}" ]; then
    SWAN_LOCAL_ID="${IKEV2_DOMAIN}"
  else
    SWAN_LOCAL_ID="${IKEV2_SERVER_CN:-vpn.example.com}"
  fi
  sed -i "s|%IKEV2_SERVER_CN%|${SWAN_LOCAL_ID}|g" "$SWAN_TMP_CONF"
  echo "${LOG_PREFIX} local.id = ${SWAN_LOCAL_ID} (cert CN)"
  # v2.86-PR20:这里原先还有一行 `sed -i "s|%IKEV2_OUT_IF%|${OUT_IF}|g"`，
  # 已删除 —— 模板 configs/swanctl-ipv6-only.conf 里从来没有这个占位符
  # （实测 grep 命中 0 次），是个从不生效的空转 sed。
  # OUT_IF 真正的传递方式是环境变量：§0.1 export IKEV2_OUT_IF，
  # 由 ikev2-updown / DDNS / ipv6watch 读取，swanctl.conf 不需要它。

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
# 设计说明见 docs/architecture.md §4.6（cert 包）
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

  # v2.86-pr23o 修复:acme.sh --install --home /opt/acme.sh 装的是 runtime 精简版,
  # 不含 dnsapi/deploy/notify。Dockerfile L350-352 用 cp -a 装到 /opt/acme.sh/,
  # 但 /usr/local/bin/acme.sh 是单文件 copy,_SCRIPT_HOME=/usr/local/bin,
  # _findHook 查 $_SCRIPT_HOME/dnsapi/ 找不到 → "Cannot find DNS API hook for: dns_ali"。
  # 运行时兜底:在 /usr/local/bin/ 下创建 dnsapi/deploy/notify symlink 指向 /opt/acme.sh/ 对应目录。
  # 老镜像(rebuild 之前)也立刻能用,无需 rebuild。
  for _sub in dnsapi deploy notify; do
    if [ -d "/opt/acme.sh/${_sub}" ] && [ ! -e "/usr/local/bin/${_sub}" ]; then
      ln -sf "/opt/acme.sh/${_sub}" "/usr/local/bin/${_sub}"
      echo "${LOG_PREFIX} LE: linked /usr/local/bin/${_sub} -> /opt/acme.sh/${_sub}"
    fi
  done

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
    # v2.86-PR19(A10):chmod 失败不能终止启动。
    # 实机(192.168.50.63):文件属主可能是 10001(面板 uid)而非 root,
    # 此时 chmod 需要 CAP_FOWNER;cap 配齐后正常,但仍不应让 set -e 杀掉
    # 整个启动流程 —— 凭证已经写进去了,权限不理想最多是"同 uid 可读",
    # 远好过面板起不来。Dockerfile/compose 已补 FOWNER,这里是兜底。
    chmod 600 "${ACME_CONF}" 2>/dev/null \
      || echo "${LOG_PREFIX} WARN: chmod 600 ${ACME_CONF} failed — 凭证已写入,但权限可能偏宽" >&2
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
    # v2.86-PR19(A10):同 account.conf,chmod 失败不致命(见上文)。
    # 证书本身已由 acme.sh 以 0600 写出,这里的 chmod 只是收尾加固。
    chmod 644 "${LE_FULLCHAIN}" 2>/dev/null \
      || echo "${LOG_PREFIX} WARN: chmod 644 ${LE_FULLCHAIN} failed, using acme.sh default mode" >&2
    chmod 600 "${LE_PRIVKEY}" 2>/dev/null \
      || echo "${LOG_PREFIX} WARN: chmod 600 ${LE_PRIVKEY} failed, using acme.sh default mode" >&2
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
  #
  # v2.86-PR19(A11):剥掉链尾的根证书,只保留 leaf + intermediate。
  #
  # 背景(2026-10-12,192.168.50.63 + iOS 17 实测):
  # LE 从 2025 起默认签发的 fullchain 是 **3 张**:
  #     leaf(CN=vpn.rewind2023.cn) → "Let's Encrypt YR2" → "ISRG Root YR"
  # 而 "ISRG Root YR" 本身是根证书(由 ISRG Root X1 交叉签发)。
  #
  # RFC 5246 §7.4.2/§7.4.1:发送方**不应**把信任锚(根证书)包含在链里,
  # 客户端本地已预置 iOS 的系统根证书。strongSwan 拿到 3 张会原样全发出去,
  # iOS 的路径构建引擎对"根证书由另一个根证书签发"(YR → X1 交叉签发)
  # 存在已知的歧义拒绝路径 —— 实测表现为 IKE_AUTH 阶段客户端静默断开。
  #
  # strongSwan 官方的做法是只 load leaf + 中间 CA,把根留给客户端的系统信任库。
  # 所以这里在拷贝前统一剥掉链尾的根,只留前 N-1 张(至少 2 张:leaf+intermediate)。
  LE_CHAIN_FIXED="$(mktemp)"
  if openssl crl2pkcs7 -nocrl -certfile "${LE_FULLCHAIN}" 2>/dev/null | \
     openssl pkcs7 -print_certs -outform PEM > /dev/null 2>&1; then
    total_certs=$(grep -c "BEGIN CERTIFICATE" "${LE_FULLCHAIN}" 2>/dev/null || echo 1)
    if [ "${total_certs}" -gt 2 ]; then
      # 逐张拆出前 N-1 张(去掉最后一张根)
      awk -v keep=$((total_certs - 1)) -v out="${LE_CHAIN_FIXED}" '
        /-----BEGIN CERTIFICATE-----/ { n++ }
        n <= keep { print >> out }
      ' "${LE_FULLCHAIN}"
      if grep -q "BEGIN CERTIFICATE" "${LE_CHAIN_FIXED}" 2>/dev/null; then
        cp "${LE_CHAIN_FIXED}" "${LE_FULLCHAIN}"
        echo "${LOG_PREFIX} LE: 剥掉链尾根证书,保留 $((total_certs - 1)) 张(原 ${total_certs} 张)—— iOS 不接受带根的 fullchain"
      else
        echo "${LOG_PREFIX} WARN: 剥根失败,保留原 fullchain(${total_certs} 张)"
      fi
    else
      echo "${LOG_PREFIX} LE: fullchain 已是 ${total_certs} 张(leaf+intermediate),无需剥根"
    fi
  else
    echo "${LOG_PREFIX} WARN: 无法解析 fullchain,跳过剥根(保持原样)"
  fi
  rm -f "${LE_CHAIN_FIXED}"

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
  # v2.86-PR19(A12):检查自定义 nft 表 ikev2 (ip) + ikev26 (ip6) 是否存在
  # `nft list table` 失败返回非 0 = 表不存在
  if ! command -v "$NFT_CMD" >/dev/null 2>&1; then
    return 1
  fi
  $NFT_CMD list table ip ikev2  >/dev/null 2>&1 || return 1
  $NFT_CMD list table ip6 ikev26 >/dev/null 2>&1 || return 1
  return 0
}

# ---------------------------------------------------------------------------
# v2.86-PR19(A12):DOCKER-USER ACCEPT ——VPN 转发修复
#
# 实机故障(192.168.50.63, 2026-10-12):
#   CHILD_SA 建立成功,in 4.86MB/27984pkt,但客户端完全无法访问网络。
#   nft ikev2.mangle_forward (priority mangle=-150) counter=1424pkt  ← 包进了forward hook
#   nft ikev2.postrouting `ip saddr 10.10.0.0/24 masquerade` counter=0  ← 却被 NAT 不到
#   iptables -L FORWARD -n -v:policy DROP 6048 packets
#
# 根因(踩了一个很隐蔽的 nft 语义坑):
#
#   宿主 netns 里Docker daemon 建了 `table ip filter`,其 FORWARD chain 是
#       type filter hook forward priority filter; policy drop;
#   我们在 nft 模板里写的也是
#       type filter hook forward priority 0(等价 filter); policy accept;
#       iifname "ipsec0" accept
#
#   **nft 的 accept 判定在 base chain 里不是终态。** 它只终止"当前这条
#   base chain"的求值,包**继续流向同 hook 上优先级更大的下一条 base chain**。
#   所以哪怕我们把 priority 调到 -10 抢在 docker 前面并且 accept,包也会
#   继续走 priority filter(0) → DOCKER-USER(空链) → DOCKER-FORWARD →
#   无 accept 规则 → 命中 docker 的 policy drop 被丢弃。
#   实测印证:DOCKER-USER counter 6208 = FORWARD 链总 counter 6208,
#   policy drop 6048 —— 6208 个包全部走到了链尾被丢。
#
#   (我最初以为"priority 抢在前面 + accept"就能解决,加了 priority -10 后
#    依然不通 —— 因为 nft accept 根本不终止跨 chain 的遍历。)
#
# 修复:改用 **iptables -I DOCKER-USER**,这是 iptables 层规则,
#       -j ACCEPT 是**真终态**(终止 iptables 链遍历 + 跳过其余 FORWARD 规则
#       + 不套用链尾 policy)。DOCKER-USER 也是 Docker 官方文档指定的
#       "用户自定义规则"插入点,不会被 docker 自己清理。
#
# 幂等:先 -C 检查再 -I,避免重复累积。
#
# v2.86-PR20:仅 host 模式需要这一步。
#   host    : 容器共享宿主 netns,docker daemon 的 FORWARD policy=drop 会拦下
#             ipsec0 转发流量,必须往 DOCKER-USER 插 ACCEPT。
#   L2 模式 : 容器私有 netns,FORWARD 链 policy=accept(容器自己的 netns 里
#             docker daemon 不插规则),nft ikev2 表里的 accept 已经是终态,
#             **不需要**也不能往 DOCKER-USER 插 —— 容器内根本没有这条链。
# ---------------------------------------------------------------------------
ensure_docker_user_accept() {
  # L2 模式:私有 netns 里没有 DOCKER-USER 链,直接返回(不是错误)
  if ! net_mode_has_docker_user "$IKEV2_NETWORK_MODE"; then
    return 0
  fi
  _ipt="$(command -v iptables || true)"
  _ip6t="$(command -v ip6tables || true)"
  # -i ipsec0:客户端解封后的包进入宿主机转发
  # -o ipsec0:服务端回给客户端的包(回程也要放行,否则单向可达)
  if [ -n "$_ipt" ]; then
    $_ipt -C DOCKER-USER -i ipsec0 -j ACCEPT 2>/dev/null || \
      $_ipt -I DOCKER-USER 1 -i ipsec0 -j ACCEPT 2>/dev/null || true
    $_ipt -C DOCKER-USER -o ipsec0 -j ACCEPT 2>/dev/null || \
      $_ipt -I DOCKER-USER 1 -o ipsec0 -j ACCEPT 2>/dev/null || true
  fi
  if [ -n "$_ip6t" ]; then
    $_ip6t -C DOCKER-USER -i ipsec0 -j ACCEPT 2>/dev/null || \
      $_ip6t -I DOCKER-USER 1 -i ipsec0 -j ACCEPT 2>/dev/null || true
    $_ip6t -C DOCKER-USER -o ipsec0 -j ACCEPT 2>/dev/null || \
      $_ip6t -I DOCKER-USER 1 -o ipsec0 -j ACCEPT 2>/dev/null || true
  fi
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

# v2.86-PR20 审查修复：先看 §3.5 到底有没有打算装这张表。
#
# §3.5 的 nft 装载被两层条件包住：
#     if [ "$IKEV2_IPV6_ONLY" = "true" ]; then
#       if [ -z "$V4_GW" ]; then ...skip... else ...装表... fi
#     fi
# 所以以下两种配置下 **从头到尾就不该有这张表**：
#   - IKEV2_IPV6_ONLY=false（双栈 / 纯 IPv4 部署）
#   - IKEV2_IPV6_ONLY=true 但宿主没有 IPv4 默认路由（纯 IPv6 主机，很常见）
#
# 而本循环原来是无条件执行的。check_ikev2_rules 永远返回非 0 →
# 跑满 phase1+phase2 = 10×1s + 50×5s = **260 秒**才退出，
# 还会写一个 iptables-warn.flag 假告警，让面板 banner 报
# 「VPN 客户端无法访问公网」—— 而实际是这条路径压根不适用。
# 表现为纯 IPv6 主机启动卡 4 分钟，是很典型的「首次部署以为挂了」。
#
# NFT_EXPECTED 由 §3.5 在真正进入装载分支时置 1。
if [ "${NFT_EXPECTED:-0}" != "1" ]; then
  echo "${LOG_PREFIX} nft ikev2/ikev26 not applicable (IPV6_ONLY=${IKEV2_IPV6_ONLY}, V4_GW=${V4_GW:-none}), skipping integrity check"
else
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
fi

# ---------- 7b. v2.86-PR19(A12):DOCKER-USER ACCEPT（仅 host 模式）----------
# nft 自定义表的 accept 不终止跨 chain 遍历(见 ensure_docker_user_accept 注释),
# 必须额外往 iptables 的 DOCKER-USER 链插 ACCEPT 才能真正放行 ipsec0 转发。
# 放在规则校验循环之后 —— DOCKER-USER 链由 docker daemon 创建,首次启动时
# 可能还没就绪,这里已确保前面的循环至少跑过一轮。
#
# v2.86-PR20:L2 模式下这一步整体跳过(见函数内注释)。此时容器私有 netns 的
# FORWARD 链 policy=accept,nft ikev2/ikev26 表里的 iifname ipsec0 accept
# 就是终态,不需要任何 iptables hack。
if net_mode_has_docker_user "$IKEV2_NETWORK_MODE"; then
  ensure_docker_user_accept
  if command -v iptables >/dev/null 2>&1 && iptables -C DOCKER-USER -i ipsec0 -j ACCEPT 2>/dev/null; then
    echo "${LOG_PREFIX} DOCKER-USER ACCEPT rules present (ipsec0 in/out, v4+v6)"
  else
    echo "${LOG_PREFIX} WARN: failed to install DOCKER-USER ACCEPT; VPN clients may not reach the internet" >&2
  fi
else
  echo "${LOG_PREFIX} [network-mode] ${IKEV2_NETWORK_MODE}: skipping DOCKER-USER ACCEPT (private netns, FORWARD policy=accept)"
fi

# ---------- 8. exec Go 进程（接管 PID 1，收到信号时优雅关闭）----------
echo "${LOG_PREFIX} Starting ikev2-panel (Go)..."
exec /usr/local/bin/ikev2-panel
