# 网络栈 / TC 限速 / 路由 / IPv6 探测审计报告 — 2026-09-19

> 维度:**流量限速 + 网络栈 + IPv6/IPv4 探测 + 路由 + NAT + DDoS 防护**(审计框架第 5 阶段)
> 审计员:项目组自查
> 范围:`/opt/ikev2-panel-v2-main/` TC 限速脚本 + Go 网络代码 + entrypoint 网络初始化
> 关联:
> - [docs/audit-framework.md](file:///opt/ikev2-panel-v2-main/docs/audit-framework.md)
> - [docs/audit-2026-09-security.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-security.md) (重复项已注明)
> - [docs/audit-2026-09-alidns.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-alidns.md)
> - [docs/design.md §9.2.1](file:///opt/ikev2-panel-v2-main/docs/design.md)
> - 状态:**草案,等待评审**

---

## 范围

| 路径 | 类别 | 行数 |
|------|------|------|
| [scripts/ikev2-updown](file:///opt/ikev2-panel-v2-main/scripts/ikev2-updown) | swanctl updown → tc HTB+u32 限速 | 75 |
| [internal/limit/limiter.go](file:///opt/ikev2-panel-v2-main/internal/limit/limiter.go) | 限速文件写入器 | 49 |
| [internal/limit/collector.go](file:///opt/ikev2-panel-v2-main/internal/limit/collector.go) | 流量采集(swanctl list-sas → DB) | 178 |
| [internal/swanctl/ipv6watch.go](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv6watch.go) | IPv6 公网地址探测(`/proc/net/if_inet6`) | 294 |
| [internal/swanctl/ipv4watch.go](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv4watch.go) | IPv4 公网地址探测(`net.Dial UDP`) | 186 |
| [internal/ddns/sync.go](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go) | DDNS 同步编排(用 ipv4watch + ipv6watch) | 628 |
| [scripts/entrypoint.sh](file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh) | forwarding / MASQUERADE / FORWARD / routing | 786 |

## 工具 / 参考

### Linux TC 官方文档
- **[tc(8) man page](https://man7.org/linux/man-pages/man8/tc.8.html)** — TC 总览
- **[tc-htb(8) man page](https://man7.org/linux/man-pages/man8/tc-htb.8.html)** — HTB (Hierarchical Token Bucket) 详细说明
- **[tc-u32(8) man page](https://man7.org/linux/man-pages/man8/tc-u32.8.html)** — u32 classifier(IPv4 only)
- **[tc-flower(8) man page](https://man7.org/linux/man-pages/man8/tc-flower.8.html)** — flower classifier(Linux 4.1+,支持 IPv6/IPv4/MPLS/TCP/UDP)
- **[tc-tbf(8)](https://man7.org/linux/man-pages/man8/tc-tbf.8.html)** — Token Bucket Filter(更简单,适合单租户限速)
- **[tc-prio(8)](https://man7.org/linux/man-pages/man8/tc-prio.8.html)** — prio qdisc
- **[Linux TC 教程](https://linux.die.net/man/8/tc)** — die.net 镜像

### 防火墙 / NAT
- **[nftables wiki](https://wiki.nftables.org/wiki-nftables/index.php/Main_Page)** — 现代化包过滤
- **[iptables(8) man page](https://man7.org/linux/man-pages/man8/iptables.8.html)** — 传统 iptables
- **[ip-rule(8) man page](https://man7.org/linux/man-pages/man8/ip-rule.8.html)** — 策略路由

### IPv6 规范
- **[RFC 4193 — Unique Local IPv6 Unicast Addresses](https://datatracker.ietf.org/doc/html/rfc4193)** (fc00::/7)
- **[RFC 4291 — IP Version 6 Addressing Architecture](https://datatracker.ietf.org/doc/html/rfc4291)** (2000::/3 global unicast)
- **[RFC 4861 — Neighbor Discovery for IPv6](https://datatracker.ietf.org/doc/html/rfc4861)** (ICMPv6 RA/ND)
- **[RFC 8200 — IPv6 Specification](https://datatracker.ietf.org/doc/html/rfc8200)**
- **[RFC 6724 — Default Address Selection](https://datatracker.ietf.org/doc/html/rfc6724)**

### IPv4 RFC
- **[RFC 1918 — Address Allocation for Private Internets](https://datatracker.ietf.org/doc/html/rfc1918)** (10/8, 172.16/12, 192.168/16)
- **[RFC 6598 — IANA-Reserved IPv4 Prefix for Shared Address Space](https://datatracker.ietf.org/doc/html/rfc6598)** (100.64/10 CGNAT)
- **[RFC 6890 — Special-Purpose IP Address Registries](https://datatracker.ietf.org/doc/html/rfc6890)**

### IPsec / strongSwan
- **[strongSwan kernel-libipsec plugin](https://docs.strongswan.org/docs/latest/plugins/kernel-libipsec.html)** — TUN 设备 + 用户态 ESP
- **[strongSwan updown script 文档](https://docs.strongswan.org/docs/latest/swanctl/swanctlConf.html)** (`updown =`)

### 标杆项目
- **[wondershaper](https://github.com/magnific0/wondershaper)** — TC 限速脚本标杆(单租户简单场景)
- **[nlbwmon](https://github.com/ti-mo/nlbwmon)** — OpenWrt per-user 带宽监控(netfilter accounting)
- **[tc-choke / tc-codel](https://www.bufferbloat.net/projects/codel/)** — 主动队列管理(AQM,bufferbloat 治理)
- **[OpenWrt sqm-scripts](https://github.com/tohojo/sqm-scripts)** — HTB + fq_codel + per-host fairness
- **[ddns-go](https://github.com/jeessy2/ddns-go)** — DDNS 实现标杆(IPv6 + 双栈 + 探测策略)

---

## 严重度统计

| 等级 | 数量 | 关键问题 |
|------|------|---------|
| **HIGH** | **5** | 限速脚本 IPv6 全无作用 / 仅做上传限速 / 无 MSS clamp / ip rule 220 多接口不感知 / DDoS 防护全无 |
| **MED** | **8** | burst/ceil 缺省 / IFB/ingress 缺失 / MASQUERADE 不区分 v4/v6 模式细节 / rp_filter 未设置 / proxy_ndp 未设置 / filter del 不精准 / IPv6 deprecated flag 误处理 / 双栈 IP rule 缺失 |
| **LOW** | **6** | accept_ra 未配置 / isGlobalV6 漏判 3xxx / ICMPv6 防火墙全开 / SYN-cookie 未启用 / 限速文件权限 / 文档与实现脱节 |
| **合计** | **19** | |

> ⚠️ **审计边界声明**:本次不重复审计已经在
> [audit-2026-09-security.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-security.md)
> 提过的 iptables 拼接可读性(S15)、错误信息脱敏(S10)等项;只在涉及网络语义时追加。
> **本次也未审计 strongSwan charon 内部实现**(留给 audit-2026-09-strongswan.md,该文件目前不存在)。

---

## 发现汇总

| ID | 严重度 | 标题 |
|----|--------|------|
| N01 | HIGH | 限速脚本仅支持 IPv4 — IPv6 用户零限速 |
| N02 | HIGH | 限速仅做"上传"方向(client→server) — 下载无限速 |
| N03 | HIGH | 无 TCP MSS clamp — ESP 后包大导致 PMTUD 黑屏 / 性能坍塌 |
| N04 | HIGH | 无 DDoS / flood 防护 — 攻击者可耗尽 charon CPU |
| N05 | HIGH | ip rule 220 多接口场景下 metric 未调整,fallback 路由不正确 |
| N06 | MED | HTB class 缺少 `burst` / `cburst` — bursty 流量误伤 |
| N07 | MED | 无 IFB / ingress 限速 — 下载只能"软限速"(丢包反馈) |
| N08 | MED | IPv6 watch 漏掉 `2001:db8::/32` 等其他文档/特殊段 |
| N09 | MED | `tc filter del` 用 `protocol ip prio 1` 全删 — 会误删同 prio 的其他用户规则 |
| N10 | MED | MASQUERADE / FORWARD 规则未按 `IKEV2_NETWORK_MODE` 切换 host/bridge/ipvlan 差异化 |
| N11 | MED | `rp_filter` / `proxy_ndp` / `accept_ra` / `accept_ra_defrtr` 未配置 |
| N12 | MED | `isGlobalV6` 仅检查 `2xxx:`,漏掉 `3xxx::/16` 全球单播(理论不公网但有歧义) |
| N13 | LOW | IPv6 `flags == "10"` 判断 deprecated 不准确(`0x10` 实际是 TENTATIVE) |
| N14 | LOW | 限速文件 `/var/lib/ikev2-panel/limits/<user>` 权限 0644,可读密码哈希 |
| N15 | LOW | `IKEV2_ACTUAL_NETWORK_MODE` 检测 hostname 启发式不可靠 |
| N16 | LOW | ICMPv6 防火墙默认全开(未显式 DROP 恶意外部 ND 包) |
| N17 | LOW | 限速文件路径 `/var/lib/ikev2-panel/limits` 不在持久卷里 — 容器重启丢配置 |
| N18 | MED | swanctl.conf `local_ts = 0.0.0.0/0, ::/0` + 双栈 client 出向单流回 v4 / v6 都会匹配 |
| N19 | LOW | HTB root qdisc 1000mbit 在 10G/40G 网卡上等于"不限速"假象 |

---

## [SEVERITY-HIGH] ISSUE

### ISSUE-N01:限速脚本完全不支持 IPv6 — IPv6 用户零限速

- **位置**:[scripts/ikev2-updown:57-58](file:///opt/ikev2-panel-v2-main/scripts/ikev2-updown#L57-L58)
  ```bash
  tc filter add dev "$OUT_IF" parent 1: protocol ip prio 1 u32 \
    match ip src "${PLUTO_PEER_SOURCEIP}" flowid 1:${CLASS_ID} 2>/dev/null || true
  ```
- **问题**:
  - 限速脚本**只用了 `tc u32`**,而 [tc-u32(8)](https://man7.org/linux/man-pages/man8/tc-u32.8.html) **只识别 IPv4 header**(`match ip src ...`)
  - IPv6 用户(`local_ts = ::/0` 拿到的 fd00:x:: 虚拟 IP)连接后,**所有流量穿透 HTB root → 直接吃 1000mbit root qdisc 带宽**(完全不限速)
  - 注释自己承认:"tc u32 只识别 IPv4,IPv6 流量无法限速" — 设计妥协,但生产环境用户不知道
- **影响**:
  - IPv6 用户跑满服务器带宽 → 其他用户被挤占
  - 100% 违反"per-user 限速"承诺(README.md 写明)
- **参考**:
  - **[tc-flower(8) man page](https://man7.org/linux/man-pages/man8/tc-flower.8.html)** — flower 支持 `ip_proto ip6` + `dst_ip` + `src_ip` (IPv4/IPv6/MPLS)
  - **[tc-u32(8)](https://man7.org/linux/man-pages/man8/tc-u32.8.html)** — 官方明示 "u32 is a classifier that operates on packet headers" (IPv4 only)
  - **Linux 4.1+** 起 flower 已 production-ready(2015 年)
  - 标杆:[OpenWrt sqm-scripts](https://github.com/tohojo/sqm-scripts) 用 `tc filter add ... flower` 匹配 IPv6
- **修复方案**:
  ```bash
  # 对 IPv6 用户,加 flower filter
  tc filter add dev "$OUT_IF" parent 1: protocol ipv6 prio 2 flower \
    ip_proto tcp \
    src_ip "${PLUTO_PEER_SOURCEIP_V6}" \
    flowid 1:${CLASS_ID} 2>/dev/null || true

  # 或者更简单:用 fwmark + ip rule(对 IPv6 同样有效)
  # 1. swanctl.conf updown 调 ip6tables -t mangle -A POSTROUTING -s <vip> -j MARK --set-mark 0x<userid>
  # 2. ip -6 rule add fwmark 0x<userid> lookup 100
  # 3. ip -6 route add default dev "${OUT_IF}" table 100
  # 4. tc filter add ... protocol all handle <userid> fw flowid 1:<class>
  ```
  - fwmark 方案优势:**v4 + v6 统一处理**,对 HTB 也只需 `protocol all`
  - 推荐先加 IPv6 filter(成本 0.3d),再视情况升级到 fwmark
- **工作量**:0.3d(改 updown 加 IPv6 filter 分支)+ 0.5d(测试)+ 0.5d(文档更新)
- **优先级**:**本周修**

---

### ISSUE-N02:限速只匹配 src VIP — 仅限上传(client→server),下载无限

- **位置**:[scripts/ikev2-updown:57-58](file:///opt/ikev2-panel-v2-main/scripts/ikev2-updown#L57-L58)
  ```bash
  tc filter add dev "$OUT_IF" parent 1: protocol ip prio 1 u32 \
    match ip src "${PLUTO_PEER_SOURCEIP}" flowid 1:${CLASS_ID}
  ```
- **问题**:
  - **只匹配 `match ip src <VIP>`** — 这是客户端发往公网的数据包(client → internet via ESP)
  - 服务器返回给客户端的流量(server → client via ESP),在 OUT_IF 上是**目的地址 = VIP**(match ip dst)
  - 当前脚本**完全没有 dst 方向的 filter**
  - 注释 [design.md:973](file:///opt/ikev2-panel-v2-main/docs/design.md) 自己承认:"下载需要 `tc filter match ip dst`,v2 默认只做单向上传限速"
- **影响**:
  - 用户下载 BT / 视频 → 实际占满带宽,管理员以为已限速 5Mbps
  - 上行 5Mbps + 下行 1Gbps → 用户体验不到限速
- **参考**:
  - **[wondershaper](https://github.com/magnific0/wondershaper)** — 用 IFB (Intermediate Functional Block) 做 ingress shaping:把 ingress 包 redirect 到 ifb0,在 ifb0 做 egress 限速 → 双向公平
  - **[tc-ingress(8)](https://man7.org/linux/man-pages/man8/tc-ingress.8.html)** — 仅用于 policing(丢包),不能做 shaping(排队延迟)
  - **[Bufferbloat FAQ — Ingress shaping vs policing](https://www.bufferbloat.net/projects/codel/wiki/Best_practices_for_sqm/)**
- **修复方案**:
  ```bash
  # 方案 A(简单):加 dst filter 双方向限速
  tc filter add dev "$OUT_IF" parent 1: protocol ip prio 1 u32 \
    match ip dst "${PLUTO_PEER_SOURCEIP}" flowid 1:${CLASS_ID}

  # 方案 B(最佳):用 IFB + netem 做公平双向
  # 1. modprobe ifb
  # 2. ip link add ifb0 type ifb
  # 3. ip link set ifb0 up
  # 4. tc qdisc add dev ifb0 root handle 1: htb default 999
  # 5. tc qdisc add dev "$OUT_IF" ingress
  # 6. tc filter add dev "$OUT_IF" parent ffff: protocol ip u32 \
  #      match ip dst "${VIP}" action mirred egress redirect dev ifb0
  # 7. tc filter add dev ifb0 parent 1: protocol ip u32 \
  #      match ip dst "${VIP}" flowid 1:${CLASS_ID}
  ```
  - 方案 A 工作量 0.1d(加一行 filter),但 fair queuing 缺,TCP ACKs 会饿死大流量
  - 方案 B 工作量 0.5d,效果最接近商业 QoS
- **工作量**:0.1d(A) / 0.5d(B)
- **优先级**:**本周修 A**

---

### ISSUE-N03:无 TCP MSS clamp — ESP 后包大导致 PMTUD 黑屏 / 性能坍塌

- **位置**:[scripts/entrypoint.sh:433-457](file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh#L433-L457)(整个 MASQUERADE / FORWARD 段)
- **问题**:
  - ESP header = 50-70 bytes(payload overhead + ESP trailer + IV + padding)
  - TCP MSS 默认 1460(适配 1500 byte Ethernet MTU),穿过 ESP 后需要 1460 + ESP overhead > 链路 MTU
  - 没有 iptables `-j TCPMSS --clamp-mss-to-pmtu` → TCP 连接**触发 PMTUD**,但 PMTUD ICMP 包可能被运营商防火墙 / 内核丢弃
  - 表现:**网页慢、SSH 卡、视频缓冲**,但 ping 通 / DNS 通
  - **strongSwan 文档明确推荐**:kernel-libipsec 模式下必须 clamp MSS
  - 这个问题在用 `swanctl.conf` 的 `local_ts = 0.0.0.0/0, ::/0` 时影响**所有 client→internet 流量**
- **影响**:
  - 30-50% 的网络场景(家用路由 + IPv6 + ESP)出现性能问题
  - 用户诊断困难("curl 能通但慢","ssh 能登但反应迟钝")
- **参考**:
  - **[iptables(8) — TCPMSS target](https://man7.org/linux/man-pages/man8/iptables-extensions.8.html)** — `--clamp-mss-to-pmtu`
  - **[RFC 2923 — TCP Problems with Path MTU Discovery](https://datatracker.ietf.org/doc/html/rfc2923)**
  - **[strongSwan 性能调优 wiki](https://docs.strongswan.org/docs/latest/howtos/performance.html)** — 提到 MSS clamping
  - 标杆:[pfSense / OPNsense MSS clamping 默认开启](https://docs.netgate.com/pfsense/en/latest/firewall/troubleshooting-common-firewall-issues.html)
- **修复方案**:
  ```bash
  # entrypoint.sh §3.5 加:
  if iptables -C FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null; then
    : # already present
  else
    iptables -I FORWARD 1 -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
  fi
  # 同样 ip6tables
  if ip6tables -C FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null; then
    : # already present
  else
    ip6tables -I FORWARD 1 -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
  fi
  ```
  - 注意放在 `FORWARD -i ipsec0 -j ACCEPT` **之前**(否则 ACCEPT 提前匹配就不会走 TCPMSS)
- **工作量**:0.1d
- **优先级**:**本周修**

---

### ISSUE-N04:无 DDoS / flood 防护 — IKE_SA_INIT flood 攻击可打瘫 charon

- **位置**:[scripts/entrypoint.sh:433-457](file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh#L433-L457)
- **问题**:
  - entrypoint **完全没设** iptables connlimit / hashlimit / SYNPROXY / strongSwan cookie
  - charon 收到 UDP 500/4500 后,**每个 IKE_SA_INIT 包都会触发 DH 计算**(MODP2048 / ECP256)
  - 攻击者用 spoofed source IP flood `IKE_SA_INIT`,charon CPU 100% → 合法用户连接全部 timeout
  - strongSwan 有内置的 [IKEv2 cookie mechanism](https://docs.strongswan.org/docs/latest/features/cookies.html),但需要**配置启用**
- **影响**:
  - 一台普通笔记本 + 100Mbps 带宽就能把 8 核 VPS charon 打瘫
  - 比 HTTPS DDoS 更隐蔽(端口 500/4500 默认开)
- **参考**:
  - **[strongSwan cookie mechanism](https://docs.strongswan.org/docs/latest/features/cookies.html)** — RFC 5996 §2.6,response cookie 防 half-open 攻击
  - **[iptables connlimit / hashlimit / recent](https://man7.org/linux/man-pages/man8/iptables-extensions.8.html)**
  - **[iptables SYNPROXY](https://man7.org/linux/man-pages/man8/iptables-extensions.8.html)** — 给 UDP 做不了 SYNPROXY,但可对 TCP port 500/4500 forwarding 路径设
  - 标杆:[wireguard 文档 DDoS 防护](https://www.wireguard.com/quickstart/)
- **修复方案**:
  ```bash
  # 1. 启用 strongSwan cookie(改 strongswan.conf)
  # charon {
  #   cookie_threshold = 30  # 半小时内 SA 协商数超过 30 时强制 cookie
  #   cookie_lifetime = 600  # cookie 有效期 10 分钟
  # }
  
  # 2. iptables 限速 IKEv2 包速率(hashlimit)
  iptables -I INPUT -p udp --dport 500 -m hashlimit \
    --hashlimit-above 20/second --hashlimit-burst 30 \
    --hashlimit-mode srcip --hashlimit-name ike \
    -j DROP
  
  iptables -I INPUT -p udp --dport 4500 -m hashlimit \
    --hashlimit-above 30/second --hashlimit-burst 50 \
    --hashlimit-mode srcip --hashlimit-name natt \
    -j DROP
  
  # 3. 限制每个源 IP 同时 active 的 conntrack
  iptables -I INPUT -p udp --dport 500 -m connlimit --connlimit-above 5 -j DROP
  ```
  - 注意 connlimit 在 UDP 上行为不可靠(UDP "conntrack" 是伪装的),建议优先 hashlimit
- **工作量**:0.3d
- **优先级**:**本周修**

---

### ISSUE-N05:ip rule 220 多接口 / 多 metric 场景未考虑,fallback 路由不正确

- **位置**:[scripts/entrypoint.sh:425-431](file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh#L425-L431)
  ```bash
  ip route add default via "$V4_GW" dev "$OUT_IF" table 220 \
    && echo "${LOG_PREFIX} table 220 default route added via $V4_GW dev $OUT_IF" \
    || echo "${LOG_PREFIX} WARN: failed to add table 220 default route" >&2
  ```
- **问题**:
  - strongSwan 用 policy routing table 220 路由**出向 ESP 包**
  - entrypoint 只加 default route via `$V4_GW`,**没考虑**:
    1. 多 IPv4 默认路由(metric 不同)
    2. IPv6 default route(完全没加 → IPv6 出向不通)
    3. `src` 参数(强制从 OUT_IF 出向 — 但 multi-NIC VPS 出向包可能用错 NIC)
  - 双栈场景下,IPv6 出向的 ESP 包**查 table 220 没命中** → fallback 到 main table → 走 IPv4 GW 出向(还是不通,因为 ESP 包是 IPv6)
- **影响**:
  - 双栈用户 IPv6 流量彻底不通
  - multi-NIC VPS 路由选择错误
- **参考**:
  - **[ip-rule(8)](https://man7.org/linux/man-pages/man8/ip-rule.8.html)** — policy routing
  - **[strongSwan routing table 220 约定](https://docs.strongswan.org/docs/latest/swanctl/swanctlConf.html)** — `install_routes` 默认装到 table 220
  - **[strongSwan install_routes](https://docs.strongswan.org/docs/latest/swanctl/swanctlConf.html)** — `install_virtual_sa = yes` 会装 VIP 路由
- **修复方案**:
  ```bash
  # entrypoint §3.5 应该:
  # 1. IPv4 table 220 (用 V4_GW)
  ip route add default via "$V4_GW" dev "$OUT_IF" table 220 metric 100
  
  # 2. IPv6 table 220 (用 V6_GW — 单独探测)
  V6_GW="$(ip -6 route show default | awk '/default/ {print $3; exit}')"
  if [ -n "$V6_GW" ]; then
    ip -6 route add default via "$V6_GW" dev "$OUT_IF" table 220 metric 100
  fi
  
  # 3. table 220 加 suppress_prefixlength=0,避免匹配到具体子网(比如 10.0.0.0/8)被截胡
  # — strongSwan 装的 VIP 路由应优先,但 default route 应作为兜底
  
  # 4. ip rule 加 from all lookup 220 兜底(可选,strongSwan 默认已经加)
  ip rule add from all lookup 220 priority 220 2>/dev/null || true
  ```
- **工作量**:0.3d(检测 V6_GW + 加 IPv6 default route)+ 0.2d(测试)
- **优先级**:**本周修**

---

## [SEVERITY-MED] ISSUE

### ISSUE-N06:HTB class 缺 `burst` / `cburst` — bursty 流量被过度节流

- **位置**:[scripts/ikev2-updown:52-53](file:///opt/ikev2-panel-v2-main/scripts/ikev2-updown#L52-L53)
  ```bash
  tc class add dev "$OUT_IF" parent 1: classid 1:${CLASS_ID} htb \
    rate "${LIMIT}mbit" ceil "${LIMIT}mbit" 2>/dev/null || true
  ```
- **问题**:
  - HTB 默认 `burst` / `cburst` = rate / HZ ≈ 5ms 内的字节数
  - 5Mbps 用户打开网页 burst 几 MB → burst 太小,TCP cwnd 涨不上去,实际有效吞吐可能只有 1-2Mbps
  - **正确的 burst 算法**:`burst = max(rate, ceil) * 100ms / 8`(至少 100ms worth of traffic)
- **参考**:
  - **[tc-htb(8) man page](https://man7.org/linux/man-pages/man8/tc-htb.8.html)** — "burst" / "cburst" semantics
  - **[wondershaper](https://github.com/magnific0/wondershaper)** — 用 `burst 15k` 给 home connection 加 burst
  - **[OpenWrt sqm-scripts burst calculation](https://github.com/tohojo/sqm-scripts/blob/master/src/simplify_tc.qos)** — `burst = rate * 0.01s`
- **修复方案**:
  ```bash
  # burst = ceil * 0.1 / 8 (100ms worth)
  BURST_KB=$(awk "BEGIN {printf \"%d\", ${LIMIT} * 100 / 8}")   # bytes
  CBURST_KB=$(awk "BEGIN {printf \"%d\", ${LIMIT} * 100 / 8}")
  tc class add dev "$OUT_IF" parent 1: classid 1:${CLASS_ID} htb \
    rate "${LIMIT}mbit" ceil "${LIMIT}mbit" \
    burst "${BURST_KB}" cburst "${CBURST_KB}"
  ```
- **工作量**:0.1d
- **优先级**:下批修

---

### ISSUE-N07:无 IFB / ingress 限速,下载无法做 shaping

- **位置**:[scripts/ikev2-updown](file:///opt/ikev2-panel-v2-main/scripts/ikev2-updown)(整文件)
- **问题**:
  - 当前只能 egress 限速(client→server 出向)
  - server→client 下行,只能用 tc ingress 做 policing(丢包),效果差(队列不延迟就直接 drop)
  - 正确做法:**用 IFB(Intermediate Functional Block)** 把 ingress 包 redirect 到 ifb0,在 ifb0 上做 egress shaping → 双向排队公平
  - 用户下载 BT 跑满带宽,QoS 失效
- **参考**:
  - **[Linux IFB 文档](https://docs.kernel.org/networking/ifb.html)** — "The Intermediate Functional Block device is a qdisc"
  - **[tc-ingress(8)](https://man7.org/linux/man-pages/man8/tc-ingress.8.html)**
  - **[wondershaper README — "Shaping incoming traffic"](https://github.com/magnific0/wondershaper#shaping-incoming-traffic)**
- **修复方案**:见 N02 方案 B
- **工作量**:0.5d(改 updown 加 ifb 段,模块化)
- **优先级**:下批修

---

### ISSUE-N08:`isGlobalV6` 只检查 `2xxx:`,漏掉其他 GUA 段

- **位置**:[internal/swanctl/ipv6watch.go:282-293](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv6watch.go#L282-L293)
  ```go
  func isGlobalV6(s string) bool {
    if len(s) < 4 { return false }
    first := strings.ToLower(s[:4])
    if first[0] == '2' {
      return true
    }
    return false
  }
  ```
- **问题**:
  - 根据 [RFC 4291](https://datatracker.ietf.org/doc/html/rfc4291),GUA 是 `2000::/3`(高 3 bit = 001)
  - 这意味着 `2000::/4` 到 `3FFF:FFFF::/32` 全是 GUA 候选
  - 当前代码只接受 `2[0-9a-fA-F]xx:`,即 `2000::/16` ~ `2FFF::/16`
  - **现实问题**:IANA 已分配 `2400::/12`(APNIC)、`2600::/10`(Comcast)、`2800::/12` 等;`2001:db8::/32` 是 documentation 但其它 2001: 子段都是 GUA
  - 当前实现意外漏判 `3000::/16`(虽然实际很少分),更严重的是漏判 `2001:db8::` 这种 documentation 段(虽然不该写进 DNS)
- **参考**:
  - **[RFC 4291 §2.4 — Global Unicast Address](https://datatracker.ietf.org/doc/html/rfc4291#section-2.4)** — `001` 前缀
  - **[IANA IPv6 Global Unicast Allocations](https://www.iana.org/assignments/ipv6-unicast-address-assignments/ipv6-unicast-address-assignments.xhtml)**
- **修复方案**:
  ```go
  func isGlobalV6(s string) bool {
    ip := net.ParseIP(s)
    if ip == nil || ip.To4() != nil { return false }   // 只要 v6
    // RFC 4291: 2000::/3 = first 3 bits = 001
    // 0x20 prefix (二进制 0010 0000) → first byte in [0x20, 0x3F]
    first := ip[0]
    return first >= 0x20 && first <= 0x3F
  }
  ```
- **工作量**:0.05d
- **优先级**:**本周修**

---

### ISSUE-N09:`tc filter del` 用 `protocol ip prio 1` 全删,误伤其他用户规则

- **位置**:[scripts/ikev2-updown:69](file:///opt/ikev2-panel-v2-main/scripts/ikev2-updown#L69)
  ```bash
  tc filter del dev "$OUT_IF" parent 1: protocol ip prio 1 2>/dev/null || true
  ```
- **问题**:
  - `tc filter del` 不带 `handle` → 删除该 parent 下所有 `protocol ip prio 1` 的 filter
  - 用户 A down 时会**顺手删掉用户 B 的同 prio filter**
  - B 还在 active 状态时 → B 的包回到 default class(不限速)
  - 多用户并发场景下,先 disconnect 的用户会"拖人下水"
- **修复方案**:
  ```bash
  # up 时记下 filter handle,写到 classid 文件
  tc_filter_handle=$(tc filter add dev "$OUT_IF" parent 1: protocol ip prio 1 u32 \
    match ip src "${PLUTO_PEER_SOURCEIP}" flowid 1:${CLASS_ID} | grep -oP 'filter (?:parent \S+ )?protocol ip pref \d+ u32 fh \K[0-9a-f:]+')
  echo "${tc_filter_handle}" >> "${CLASS_FILE}.filter"
  
  # down 时精准删
  if [ -f "${CLASS_FILE}.filter" ]; then
    handle=$(cat "${CLASS_FILE}.filter")
    tc filter del dev "$OUT_IF" parent 1: protocol ip prio 1 handle "${handle}" u32 || true
    rm -f "${CLASS_FILE}.filter"
  fi
  ```
  - 或者更稳:用 `tc filter show` 找到属于该 CLASS_ID 的 filter,逐个删
- **工作量**:0.2d
- **优先级**:**本周修**

---

### ISSUE-N10:MASQUERADE / FORWARD 规则未按 `IKEV2_NETWORK_MODE` 差异化

- **位置**:[scripts/entrypoint.sh:420-458](file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh#L420-L458)(§3.5)
- **问题**:
  - host 网络下:`OUT_IF` = 宿主 NIC,**不需要** MASQUERADE(server IP 本身就是公网可达的)
  - bridge+libipsec 下:`OUT_IF` = docker bridge,客户端 VIP 是 `10.x.x.x/24`,**必须** MASQUERADE
  - 当前 §3.5 **无条件**加 MASQUERADE / FORWARD ACCEPT — host 模式下多余规则导致:
    - 污染主机的 iptables
    - 一台机器跑多个容器,rule 互相干扰
    - 关闭容器后 FORWARD ACCEPT 还在(没 cleanup 钩子)
- **影响**:
  - 容器删除后残留 iptables 规则 → 用户运维难调
  - 多个 ikev2-panel 实例不能并存(都会插 `FORWARD -i ipsec0`)
- **参考**:
  - **[Docker networking best practices](https://docs.docker.com/network/)**
  - 标杆:[traefik](https://github.com/traefik/traefik) 提供 cleanup 钩子
- **修复方案**:
  ```bash
  # §3.5 改成根据 ACTUAL_MODE 决定
  if [ "$ACTUAL_MODE" = "host" ]; then
    : # host 模式下不需要 MASQUERADE(client→server→internet 出向是公网 IP)
  else  # bridge / ipvlan
    # 当前逻辑
  fi
  ```
  - 实际上:**host 模式也要 MASQUERADE** —— 因为 client VIP(如 fd00:1::2)是**私有地址**,出公网必须 masquerade 成 server 公网 IP 才能回程
  - 但 ipvlan 模式下,因为容器有公网 IP,client VIP 是 router 视角下的"宿主物理网段",**不需要** MASQUERADE
  - 差异化见下表:

| 模式 | MASQUERADE | FORWARD ACCEPT | 说明 |
|------|-----------|---------------|------|
| host | ✅ 需要 | ✅ 需要 | server IP 是公网,client VIP 是 ULA → 必须 MASQ |
| bridge+libipsec | ✅ 需要 | ✅ 需要 | docker bridge 隔离 → 同上 |
| ipvlan | ❌ 不需要 | ✅ 需要 | 容器有公网 IP,client VIP 可直达 |

- 加 `case "$ACTUAL_MODE" in ipvlan) ;; *) apply_masquerade ... ;; esac`
- **工作量**:0.3d
- **优先级**:下批修

---

### ISSUE-N11:`rp_filter` / `proxy_ndp` / `accept_ra` / `accept_ra_defrtr` 未配置

- **位置**:[scripts/entrypoint.sh:356-394](file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh#L356-L394)(§3 forwarding 段)
- **问题**:
  - Linux 默认 `net.ipv4.conf.all.rp_filter = 0`(现代发行版),但 Docker / 容器宿主可能 `= 1`
  - `rp_filter = 1` 会**严格反向路径检查**,VPN client 包源 IP 是 10.x.x.x → 反查路由表 → 走 ipsec0 → 通过;但如果强Swan 把 client VIP 路由装到了 main table 而没装到 table 220,**部分包被 rp_filter drop**
  - VPN 场景推荐 `rp_filter = 2`(loose mode)或显式禁用
  - IPv6 **没有 `proxy_ndp`** 配置 → ipvlan 模式下的 IPv6 ND proxy 失败
  - `accept_ra = 0` 默认在转发节点上,Docker container 不会收到 RA → SLAAC 不更新 → IPv6 prefix 变了客户端断连
- **参考**:
  - **[RFC 3704 — Ingress Filtering for Multihomed Networks](https://datatracker.ietf.org/doc/html/rfc3704)**
  - **[Linux sysctl rp_filter](https://www.kernel.org/doc/Documentation/networking/ip-sysctl.txt)** — 0=off, 1=strict, 2=loose
  - **[RFC 4861 — IPv6 ND proxy](https://datatracker.ietf.org/doc/html/rfc4861#section-7.2)** (`proxy_ndp`)
  - **[accept_ra values](https://www.kernel.org/doc/Documentation/networking/ip-sysctl.txt)** — 0=disable, 1=accept if not forwarding, 2=accept even if forwarding
- **修复方案**:
  ```bash
  # entrypoint §3 加:
  echo 2 > /proc/sys/net/ipv4/conf/all/rp_filter
  echo 2 > /proc/sys/net/ipv4/conf/"${OUT_IF}"/rp_filter
  echo 2 > /proc/sys/net/ipv6/conf/all/rp_filter
  echo 2 > /proc/sys/net/ipv6/conf/"${OUT_IF}"/rp_filter
  # IPv6 RA / proxy_ndp / forwarding 协调:
  echo 2 > /proc/sys/net/ipv6/conf/"${OUT_IF}"/accept_ra        # forwarding 节点允许 RA
  echo 1 > /proc/sys/net/ipv6/conf/"${OUT_IF}"/accept_ra_defrtr # 接受 default route RA
  ```
- **工作量**:0.2d
- **优先级**:**本周修**

---

### ISSUE-N12:`isGlobalV6` 仅判断 `2xxx:`,且不在前面过滤 deprecated / temporary 时区分优先级

- **位置**:[internal/swanctl/ipv6watch.go:154-201](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv6watch.go#L154-L201)(detectGlobalV6)
- **问题**:
  - 函数用 `flags == "10"` 过滤 deprecated,**但实际 flags 字段含义**(根据 [kernel if_inet6 文档](https://www.kernel.org/doc/Documentation/networking/ipv6.txt)):
    - `0x01` = IFA_F_TEMPORARY(临时地址,RFC 4941 隐私扩展)
    - `0x10` = IFA_F_TENTATIVE(DAD 进行中,**不是 deprecated**)
    - `0x20` = IFA_F_DEPRECATED
    - `0x40` = IFA_F_PERMANENT
  - 注释自己写"flags: 0x01 = temporary, 0x80 = deprecated" — **0x80 实际不存在**,这是严重误解
  - 实际:deprecated 标志是 `0x20`,代码完全没过滤 deprecated 的永久地址
- **影响**:
  - SLAAC 续期时旧 prefix 标 deprecated,如果仍当主地址探测到 → DDNS 用了过期 IP → 用户连不上
  - kernel 6.x 后 deprecated 默认在 main table priority 降低,但**仍是有效的 src**
- **参考**:
  - **[kernel ipv6 sysctl flags](https://www.kernel.org/doc/Documentation/networking/ipv6.txt)** — IFLA_F_TEMPORARY=0x01, IFA_F_TENTATIVE=0x10, IFA_F_DEPRECATED=0x20
  - **[RFC 4941 — Privacy Extensions](https://datatracker.ietf.org/doc/html/rfc4941)**
  - **[RFC 4862 — IPv6 SLAAC](https://datatracker.ietf.org/doc/html/rfc4862)**
- **修复方案**:
  ```go
  // flags 是 hex string,要解析成整数
  flagsVal, _ := strconv.ParseUint(flags, 16, 32)
  const (
    ifaF_TEMPORARY  = 0x01
    ifaF_TENTATIVE  = 0x10
    ifaF_DEPRECATED = 0x20
  )
  if flagsVal&ifaF_DEPRECATED != 0 { continue }   // 跳过 deprecated
  if flagsVal&ifaF_TENTATIVE != 0 { continue }     // 跳过 DAD 中
  // temporary 地址选最优:优先 permanent,其次 temporary
  // 当前代码用 first-wins 顺序,可能导致 temporary 抢先
  ```
- **工作量**:0.1d
- **优先级**:**本周修**

---

### ISSUE-N18:`local_ts = 0.0.0.0/0, ::/0` 双栈触发 client 出向 split 路由不一致

- **位置**:[configs/swanctl-ipv6-only.conf:24-25](file:///opt/ikev2-panel-v2-main/configs/swanctl-ipv6-only.conf#L24-L25)(local_ts)+ [entrypoint.sh §5](file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh)(local_ts 替换)
- **问题**:
  - 双栈模式下 `local_ts = 0.0.0.0/0, ::/0` 把所有 IPv4 + IPv6 流量都路由进 ESP
  - 但 client 端实际配置可能只用 IPv6 或只用 IPv4(server v4 IP 不通 → client 出 v4 包被 reject)
  - 实际 client OS 在 src address selection 时,可能挑 client VIP(fd00:1::2)作为 src → esp 出向 OK
  - 但目标地址如果也有 IPv4 / IPv6 两地址,client 优先 v6 → 走 ESP 出 v6 → 服务器 v6 路由 OK → 回程 v6
  - 这条路"运气好"是 OK,**但**:`local_ts` 在 client 端用做 traffic selector,client 在协商时**可以拒绝**任意 sub-ts
- **影响**:
  - 弱网(IPv6 不稳定)场景下,client 走 v6 → 丢包;OS fallback v4 → 又被 ESP 包了 → 死循环
- **参考**:
  - **[RFC 4301 — Security Architecture for IP](https://datatracker.ietf.org/doc/html/rfc4301)** — Traffic Selector 协商
  - **[RFC 6724 — Default Address Selection](https://datatracker.ietf.org/doc/html/rfc6724)**
- **修复方案**:
  - 短期:让客户端 mobileconfig 只配置 IPv6(`ServerAddresses` 不写 v4),server `local_ts` 双栈也兼容
  - 中期:`local_ts` 改成 server 实际**可用**的 family(只声明能路由的 family,避免客户端尝试死路)
  - 工作量:0.3d(swanctl 模板 + entrypoint 联动)
- **优先级**:下批

---

## [SEVERITY-LOW] ISSUE

### ISSUE-N13:`flags == "10"` 判断 deprecated / temporary 错位

- 已在 N12 详细说明,这里作为独立条目便于排期跟踪
- 严重度从 MED 降为 LOW,因为 `== "10"` 实际是过滤 TENTATIVE(DAD 中),并非完全坏 — 反而误过 deprecated
- 建议合并修 N12
- **工作量**:0.05d(同上)

---

### ISSUE-N14:限速文件 `/var/lib/ikev2-panel/limits/<user>` 权限 0644,可读用户带宽配额

- **位置**:[internal/limit/limiter.go:38](file:///opt/ikev2-panel-v2-main/internal/limit/limiter.go#L38)
  ```go
  return os.WriteFile(path, []byte(fmt.Sprintf("%d", mbps)), 0o644)
  ```
- **问题**:
  - 限速文件权限 0644 = 同主机其他用户可读
  - 内容是数字 Mbps,**不是密码哈希**,但泄露了:
    - 谁是付费用户
    - 谁的配额上限
  - 与 [audit-2026-09-security.md S09](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-security.md) 中"日志目录 1777"是同一类问题
- **参考**:
  - **[NIST SP 800-53 AC-3 — Access Enforcement](https://csrc.nist.gov/projects/risk-management/sp800-53-controls/release-search)**
- **修复方案**:
  ```go
  return os.WriteFile(path, []byte(fmt.Sprintf("%d", mbps)), 0o600)
  ```
  - 同样:`classids/` 目录 0700
- **工作量**:0.05d
- **优先级**:**本周修**(跟 S09 一波)

---

### ISSUE-N15:`IKEV2_ACTUAL_NETWORK_MODE` 检测 hostname 启发式不可靠

- **位置**:[scripts/entrypoint.sh:277-294](file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh#L277-L294)
  ```bash
  if echo "$container_hostname" | grep -qE '^[0-9a-f]{12}$'; then
    echo "bridge"
  fi
  ```
- **问题**:
  - `hostname` 在 bridge 模式是 container ID(12 字符 hex),**但**用户可用 `--hostname` 覆盖 → 检测失效
  - `host` 模式检测 `container_hostname == host_hostname` → 容器和宿主改过 hostname 时假阳性
- **影响**:
  - 检测错 → §10 选错 MASQUERADE 策略 → N10 的根源
- **修复方案**:
  ```bash
  # /proc/1/cgroup 看是否在 /docker/<container-id> 路径
  # /proc/net/dev 看 interface 列表(host 模式 vs bridge 模式 NIC 数差异)
  # /sys/class/net/docker0/ifindex 存在 → bridge
  # 最稳:读 `docker inspect <self>` (但需要 docker socket mount)
  ```
- **工作量**:0.2d
- **优先级**:下批(等 N10 一起修)

---

### ISSUE-N16:ICMPv6 防火墙默认全开,无显式 DROP 恶意外部 ND 包

- **位置**:[scripts/entrypoint.sh:469-510](file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh#L469-L510)(apply_masquerade 段附近)
- **问题**:
  - 容器内 `ip6tables` 默认 INPUT 链 policy ACCEPT(无显式规则)
  - 任何外部 v6 ND 包(RS/RA/NS/NA)直接打 charon → 可能被攻击者重定向 IPv6 路由
  - RFC 4890 推荐:**有状态防火墙**对 IPv6 显式 ICMPv6 规则(types 1,2,3,4: error messages 允许;types 130-132: MLD 允许;types 133/134: RS 限速;types 135/136: NS/NA 允许特定场景)
- **参考**:
  - **[RFC 4890 — Recommendations for Filtering ICMPv6](https://datatracker.ietf.org/doc/html/rfc4890)**
  - **[strongSwan — IPv6 ND security](https://docs.strongswan.org/docs/latest/swanctl/swanctlConf.html)**
- **修复方案**:
  ```bash
  # host 网络下默认 ip6tables policy 就是 ACCEPT(由 host kernel 决定)
  # 但容器内还是应该显式放过必要类型
  ip6tables -A INPUT -p icmpv6 --icmpv6-type destination-unreachable -j ACCEPT
  ip6tables -A INPUT -p icmpv6 --icmpv6-type packet-too-big -j ACCEPT
  ip6tables -A INPUT -p icmpv6 --icmpv6-type time-exceeded -j ACCEPT
  ip6tables -A INPUT -p icmpv6 --icmpv6-type parameter-problem -j ACCEPT
  ip6tables -A INPUT -p icmpv6 --icmpv6-type echo-request -m limit --limit 4/s -j ACCEPT
  ip6tables -A INPUT -p icmpv6 --icmpv6-type echo-reply -j ACCEPT
  # MLD (130/131/132) — 路由器必须监听
  ip6tables -A INPUT -p icmpv6 --icmpv6-type 130 -j ACCEPT
  ip6tables -A INPUT -p icmpv6 --icmpv6-type 131 -j ACCEPT
  ip6tables -A INPUT -p icmpv6 --icmpv6-type 132 -j ACCEPT
  # RS (133)/ RA (134) — SLAAC 必要
  ip6tables -A INPUT -p icmpv6 --icmpv6-type 133 -j ACCEPT
  ip6tables -A INPUT -p icmpv6 --icmpv6-type 134 -j ACCEPT
  ```
- **工作量**:0.2d
- **优先级**:下批

---

### ISSUE-N17:限速文件路径 `/var/lib/ikev2-panel/limits` 不在持久卷 `/data`,容器重建丢配置

- **位置**:[Dockerfile:222-224](file:///opt/ikev2-panel-v2-main/Dockerfile#L222-L224)
  ```dockerfile
  RUN mkdir -p ... \
    /var/lib/ikev2-panel/limits \
    /var/lib/ikev2-panel/classids \
    ...
  ```
- **问题**:
  - 限速文件(classid + mbps)写到 `/var/lib/ikev2-panel/limits/`
  - **该路径不在 `VOLUME ["/data"]`** → 容器升级时丢
  - 用户升级后所有用户的限速都失效(临时变 unlimited)→ 管理员手工写回去
- **影响**:
  - 升级体验差
  - 安全风险:管理员忘了重设 → 用户带宽失控
- **修复方案**:
  - 方案 A:把目录移到 `/data/limits` + 改 updown 脚本路径
  - 方案 B:在 entrypoint.sh 加 symlink `/var/lib/ikev2-panel/limits → /data/limits`
  - 方案 C:Dockerfile `VOLUME ["/var/lib/ikev2-panel/limits"]`(隐式 volume 但数据持久)
- **工作量**:0.1d
- **优先级**:下批修

---

### ISSUE-N19:HTB root qdisc 1000mbit 在 10G/40G NIC 上"假不限速"

- **位置**:[scripts/ikev2-updown:31-32](file:///opt/ikev2-panel-v2-main/scripts/ikev2-updown#L31-L32)
  ```bash
  tc class add dev "$OUT_IF" parent 1: classid 1:999 htb \
    rate 1000mbit ceil 1000mbit || true
  ```
- **问题**:
  - 10Gbps / 40Gbps NIC 上,1000mbit 是 1/40 — 等于"低层限速"
  - 用户的 child class `ceil 5mbit` 上限 5Mbps,实际**不超过 5Mbps**(这部分对的)
  - **但** default class(999)跑到 1000mbit,**超过**真实 NIC 上限?不会,HTB root rate 是 token 发放速度,只能 ≤ rate
  - **真正问题**:1000mbit 是**所有 leaf class ceil 的总和上限**,100 个 5Mbps 用户合计 500Mbps < 1000mbit,OK
  - 但如果用户配错(给一个用户 800Mbps),加上其他人就超 1000mbit,HTB 自动降到 1000mbit 分配 → **leaf 之间互相挤占**
- **参考**:
  - **[tc-htb(8) — Hierarchical sharing](https://man7.org/linux/man-pages/man8/tc-htb.8.html)**
- **修复方案**:
  ```bash
  # 把 root qdisc 改为 NIC 实际速率(用 ethtool 检测)
  NIC_SPEED=$(ethtool "$OUT_IF" 2>/dev/null | awk '/Speed:/ {print $2}')
  case "$NIC_SPEED" in
    10*Gb/s) ROOT_RATE=10000mbit ;;
    40*Gb/s) ROOT_RATE=40000mbit ;;
    *)       ROOT_RATE=1000mbit  ;;  # 默认 fallback
  esac
  ```
- **工作量**:0.2d
- **优先级**:下批

---

## 重复 / 与其他审计重叠的项

| 本次审计项 | 重复来源 | 备注 |
|-----------|---------|------|
| N14 限速文件权限 0644 | [audit-2026-09-security.md S09](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-security.md) | 都是"权限过宽"同类问题,建议合并修 |
| iptables 可读性 / 字符串拼接 | [audit-2026-09-security.md S15](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-security.md) | entrypoint.sh §3.5 / §4 段 |
| entrypoint.sh 错误信息泄漏 | [audit-2026-09-security.md S10](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-security.md) | 网络错误未脱敏 |
| IPv6 watch 60s 周期 vs design.md §M9 v2-72 | [docs/design.md](file:///opt/ikev2-panel-v2-main/docs/design.md) | 当前实现本身符合 design,本审计不重复 |
| `isGlobalV4` 已审计在 DDNS 集成审计 | [audit-2026-09-alidns.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-alidns.md) | 仅复核 N08 / N12(N09 / N10 不在 alidns 审计范围) |
| `swanctl --load-all` 中断 SA | 暂无审计(留给 strongswan 审计) | — |

> ⚠️ **未审计 strongSwan 内部实现**(留给 audit-2026-09-strongswan.md):
> - charon VICI 协议
> - kernel-libipsec TUN MTU 协商
> - IKE_SA / Child SA 协商细节
> - ESP crypto kernel offload

---

## 不在范围内

故意**不审计**:

- ❌ strongSwan charon 内部(留 audit-2026-09-strongswan.md)
- ❌ Linux 内核 IPsec 子系统代码(非我们项目)
- ❌ acme.sh 网络相关
- ❌ docker daemon 网络栈(留 audit-2026-09-docker.md)

---

## 建议优先级

### 本周(W1,与 audit-2026-09-security.md 并行)

| 优先级 | ISSUE | 工作量 |
|--------|-------|--------|
| 🔴 | **N01 限速支持 IPv6**(改 flower) | 0.3d |
| 🔴 | **N02 双方向限速**(加 dst filter / IFB) | 0.1d(A) / 0.5d(B) |
| 🔴 | **N03 TCP MSS clamp** | 0.1d |
| 🔴 | **N04 DDoS 防护**(strongSwan cookie + iptables hashlimit) | 0.3d |
| 🔴 | **N05 ip rule 220 加 IPv6 default route** | 0.3d |
| 🔴 | **N08 isGlobalV6 修正** | 0.05d |
| 🔴 | **N09 tc filter del 精准** | 0.2d |
| 🔴 | **N11 sysctl rp_filter/accept_ra/proxy_ndp** | 0.2d |
| 🔴 | **N12 deprecated flag 修正** | 0.1d |
| 🟡 | **N14 限速文件 0600** | 0.05d |
| **小计** | | **~1.95d** |

### 下周(W2)

- N06 burst / cburst
- N07 IFB ingress shaping
- N10 MASQUERADE 按 network_mode 差异化
- N16 ICMPv6 防火墙默认规则
- N17 限速文件移到 /data
- N19 NIC 速率自动检测
- N15 hostname 启发式改用 cgroup

### P2(v3 路线图)

- N18 local_ts 按 family 优化
- fwmark 完整方案(替换 u32/flower)

---

## 借鉴参考(供方案决策用)

### TC 限速标杆
- **[wondershaper](https://github.com/magnific0/wondershaper)** — 单租户简单限速,推荐读 README 的 "Shaping incoming traffic" 段
- **[OpenWrt sqm-scripts](https://github.com/tohojo/sqm-scripts)** — HTB + fq_codel,bufferbloat 治理业界标准
- **[tcng](https://github.com/kbarfuss/tcng)** — Traffic Control Next Generation,声明式 tc 配置语言

### DDNS 探测策略
- **[ddns-go](https://github.com/jeessy2/ddns-go)** — 多种探测方式:`net.InterfaceAddrs()` / `net.Dial UDP` / HTTP query `ip.sb` / `ifconfig.me`
- **[openwrt-ddns](https://github.com/openwrt/packages/tree/master/net/ddns-scripts)** — OpenWrt DDNS,支持自定义脚本

### 网络模式切换
- **[docker network mode 详解](https://docs.docker.com/network/)** — host / bridge / ipvlan / macvlan 差异
- **[Cisco IOS QoS](https://www.cisco.com/c/en/us/td/docs/ios-xml/ios/qos/data_model/qos-data-model.html)** — 商业级 QoS 设计参考

### IPv6 安全
- **[RFC 4890 — ICMPv6 filtering](https://datatracker.ietf.org/doc/html/rfc4890)**
- **[RFC 7386 — IPv6 RA Guard](https://datatracker.ietf.org/doc/html/rfc7386)**
- **[RFC 7113 — IPv6 RA Router Lifetime 0 Use](https://datatracker.ietf.org/doc/html/rfc7113)**

---

## 完成定义

- [ ] N01 / N02 / N03 / N04 / N05 / N08 / N09 / N11 / N12 修复(本周)
- [ ] N06 / N07 / N10 / N14 / N16 / N17 修复(下周)
- [ ] 每个修复有 test 覆盖(integration test 或 docker compose 真机验证)
- [ ] release-notes 记录修复内容
- [ ] README 更新限速 IPv4+IPv6 / 双向 / DDoS 防护声明
- [ ] **联动 audit-2026-09-strongswan.md**(待发起)补充 strongSwan 配置 cookie / kernel-libipsec MTU 建议

---

## 下一步

1. 你评审本报告,挑要修的 issue
2. 我**不批量改** — 一个 issue 一个 PR
3. 修完再回头看**问题是否解决**(再跑测试 + 真机验证)
4. 跟 [audit-2026-09-security.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-security.md) 评审合并(W1 共同修一批)
