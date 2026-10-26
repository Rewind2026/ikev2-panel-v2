# v2.98 — IPv6 修复：去掉 ULA 假地址，L2 网络改走上游路由器 RA/SLAAC 下发

发布日期：2026-10-26
镜像：`ghcr.io/rewind2026/ikev2-panel-v2:v2.98`（linux/amd64）

> 完整变更清单见 [CHANGELOG.md](CHANGELOG.md) 的 v2.98 小节。
> 与 v2.97 不同：**这次 entrypoint 变了**（SLAAC 回读复核加第四条），所以要用
> `:v2.98` / `:2.98` / 重新 pull 的 `:latest`——旧镜像没有新的复核逻辑。

---

## 一句话

**用户诉求：「compose 创建的 ipvlan 和 macvlan 网络 IPv6 部分外部访问不了，
需要把 IPv6 改成 RA 路由器下发那种才行。」**

现在 L2 网络（macvlan / ipvlan）不再让 Docker 自造 ULA 段，容器 IPv6 由
**上游路由器 RA/SLAAC 下发**——和宿主同一前缀、同一条 `proto ra` 默认路由，
外部可以直接访问容器的 v6 地址。

---

## 之前为什么访问不了

v2.97 及之前的 compose 写了 `enable_ipv6: true`，但 `ipam.config` 只配了
IPv4。Docker 看到网络要 IPv6 又没给段，就从内置 ULA 池（fd00::/8）自动挑
一段分给网络。容器于是拿到：

1. 一个 `fd00::/8` 地址——上游路由器根本没有这条路由，出网即黑洞
   （v2.97 发布说明里那个「GUA」`fd82:f5a0:6f67::2`，fd 开头其实就是 ULA）；
2. 一条指向**不存在网关**的假默认路由。

所以容器 ping 不通任何真实 v6 网关，外部也访问不了容器的 v6 地址。

---

## 现在的姿势（Docker 官方「Use router-assigned IPv6 addresses」）

网络**不带** v6 子网、不写 `enable_ipv6`；Docker（≥ 26.0.2，
CVE-2024-32473 修复后）建端点时会把 `net.ipv6.conf.<if>.disable_ipv6`
置 1，需要用 **endpoint 级 sysctl** 翻回来：

```yaml
services:
  ikev2-panel:
    networks:
      ikev2-l2:
        ipv4_address: *l2_ipv4
        driver_opts:
          com.docker.network.endpoint.sysctls: "net.ipv6.conf.IFNAME.disable_ipv6=0,net.ipv6.conf.IFNAME.accept_ra=2"
```

两个关键点（都踩过坑）：

| 关键点 | 原因 |
|--------|------|
| 必须写在 **service 段 networks 块**的 `driver_opts`（端点级） | 写到文件末尾 `networks.<name>.driver_opts`（网络级）会被**静默忽略**——v2.86-PR20「endpoint sysctls 不生效」的错误结论就是把网络级和端点级搞混了 |
| `accept_ra=2` | 容器必开 IP forwarding，内核（RFC 4861）在 forwarding 开启时默认丢弃 RA，必须显式写 2 |

service 段 `sysctls`（netns 级）继续兜底 IPv4/IPv6 forwarding 与
`conf.default.accept_ra`；接口级四条期望值由 entrypoint 启动时回读复核，
不对就 FATAL 并提示检查 endpoint sysctls。

---

## ⚠️ 行为变更

| 项 | 之前（v2.97） | 现在（v2.98） |
|---|---|---|
| 容器 IPv6 地址 | Docker 内置 ULA（fd00::/8，不路由） | 路由器 RA/SLAAC 下发的公网 GUA |
| compose 的 `enable_ipv6` | `true` | **不允许出现**（测试强制） |
| 路由器前缀变化 | 容器地址不变（假稳定） | 跟随前缀自动更新 |
| up.sh 对 L2 网络的 v6 要求 | 要求网络 EnableIPv6=true（否则 FATAL），建网带 `--ipv6` | 要求**不带** v6（遇到老 true 网络只 WARN），建网去 `--ipv6` |

---

## 升级方法

```bash
docker compose pull
docker compose -f docker-compose.macvlan.yml up -d   # 或 ipvlan
```

**如果 L2 网络是 v2.97 建的（EnableIPv6=true）**：compose 复用时会 WARN
提示。建议把网络删掉重建一次，让它变成真正的「无 v6 段」形态：

```bash
docker compose -f docker-compose.macvlan.yml down
docker network ls | grep macvlan        # 若旧网还在（被别的项目占用等）
docker network rm macvlan               # 手动删掉
docker compose -f docker-compose.macvlan.yml up -d
```

首次启动 SLAAC 收敛约 1~3s（RA 到达 + DAD），entrypoint 会等容器拿到 GUA
再起面板。

---

## 回归测试

`tests/network_mode_test.sh` 全量 **197 passed / 0 failed**：

| 覆盖点 | 说明 |
|--------|------|
| NM-28 翻转 | 两份 compose **禁止**出现 `enable_ipv6` |
| NM-37 扩充 | sysctl 单一真源四条（新增 `net/ipv6/conf/IFNAME/disable_ipv6|0`） |
| NM-38 新增 | endpoint sysctls 必须写在 service 段 networks 块（端点级），含 `IFNAME` 占位符与 `disable_ipv6=0`、`accept_ra=2` |

---

## 实机验证

192.168.50.63（飞牛 fnOS NAS，Docker 28.5.2，宿主 `ens18`，macvlan）。

| 检查 | 结果 |
|------|------|
| 宿主 ens18 SLAAC GUA + `proto ra` 默认路由 | ✅ |
| 容器 eth0 拿到 GUA（`dynamic mngtmpaddr`，约 3s） | ✅ `2408:832e:8ac:d220:b82f:dff:fe80:a0b4/64` |
| 容器默认路由 `via fe80::… dev eth0 proto ra` | ✅ |
| endpoint sysctl：`disable_ipv6=0` / `accept_ra=2` | ✅ |
| 启动日志：`[slaac] eth0: … accept_ra=2 — RA/SLAAC 可用` | ✅ |
| 容器内 `curl -6 http://[2400:3200::1]/` | ✅ HTTP 404（0.09s，可达即证明通） |
| 同网段 shim 经容器 GUA 访问 `:8443/healthz` | ✅ HTTP 200（7ms） |

> 宿主直接访问自己 parent 上的 macvlan 容器是**父子隔离**特性，不算故障；
> 验证用同网段 shim（`ip link add shim link ens18 type macvlan mode bridge`）。

---

## 文档

- [architecture.md](architecture.md) §11.1 / §12.8 —— 两级 sysctl、endpoint sysctls 必须端点级、SLAAC 回读复核
- [TROUBLESHOOTING.md](TROUBLESHOOTING.md) §1.3.4 —— 容器 v6 起不来 / 拿到 ULA 的处置
- [CHANGELOG.md](CHANGELOG.md) —— v2.98 完整变更清单
