# v2.92 — 修复 L2 模式下 web 打不开（TLS 握手被网关 RST）

发布日期：2026-10-21
镜像：`ghcr.io/rewind2026/ikev2-panel-v2:v2.92`（linux/amd64）

> ⚠️ **本次修复的代码在 `scripts/entrypoint.sh` 里，不在 Go 二进制里。**
> 镜像的 entrypoint 脚本是构建时从仓库拷进去的，所以 v2.92 镜像自带修复。
> 完整变更清单见 [CHANGELOG.md](CHANGELOG.md) 的 v2.92 小节。

---

## 一句话

**L2 模式（macvlan / ipvlan）下从局域网访问面板，TCP 能建连但 TLS 握手必被重置，浏览器完全打不开。**
根因是容器的策略路由把同网段回包绕到了网关，网关按不对称路径处理并注入 RST。
v2.92 起 `table 220` 自带直连路由，回包不再绕网关。

---

## 症状

| 观察点 | 结果 |
|--------|------|
| `ping 192.168.50.66` | **通** |
| 容器 `docker ps` | `Up`，重启次数 **0**，看起来完全正常 |
| `docker logs` | `https listening addr=0.0.0.0:8443` 正常打印 |
| 容器内 `curl https://127.0.0.1:8443/login` | **200** |
| 容器内 `curl https://192.168.50.66:8443/login` | **200** |
| 容器日志 | 刷满 `http: TLS handshake error ...: write: connection reset by peer` |
| 走 **IPv6** 访问容器全局地址 | **正常**，能拿到面板证书 |
| 走 **IPv4** 访问 `https://<容器IP>:8443` | **失败**，TLS 握手被重置 |

**这个组合（容器内全 200、IPv6 正常、只有 IPv4 失败）是本问题的指纹。**

---

## 根因

strongSwan charon 的 kernel-netlink 插件会装一条策略路由规则：

```
220:    from all lookup 220
```

它把**所有**出向流量的查表强制到 `table 220`。而 v2.91 之前 `table 220` 里
只有一条 `default via <网关>`（`scripts/entrypoint.sh` §3.5 加的，XFRM 出向要用），
**同网段的直连路由是缺的**。

于是容器发往同网段对端（比如你的电脑）的**回包也被 default 带走，绕网关出去**：

1. 回包源 MAC 被换成**网关的 MAC**；
2. 路径变成「容器 → 网关 → 对端」，与入站方向不对称；
3. 网关按不对称路径处理，**注入 RST**。

浏览器侧就表现为「TCP 握上了、TLS 握手立刻被重置」。

### 抓包证据

192.168.50.66:8443，客户端 192.168.50.176：

```
# 中招时 —— SYN-ACK 与 RST 的源 MAC 都是网关的
11:43:52.622506 04:42:1a:5f:60:70 > bc:24:11:fb:01:bb: 192.168.50.66.8443 > 192.168.50.176.53154: Flags [S.]
11:43:52.628972 04:42:1a:5f:60:70 > bc:24:11:fb:01:bb: 192.168.50.66.8443 > 192.168.50.176.53154: Flags [R]
```

### 为什么 host 模式没事

host 模式下容器共享宿主 netns，宿主有
`ip rule 10: from <宿主IP> lookup route_ens18` 这条**优先级更高**的规则，
源地址是宿主 IP 的流量走 `route_ens18` 表，压根不进 `table 220`。

L2 模式容器是独立 netns，没有这条规则，只能靠 `table 220` 自己带直连路由。

---

## 修复

`scripts/entrypoint.sh` §3.5 在加 `default` 的同时，把 `main` 表里
`dev $OUT_IF` 的直连（`scope link`）路由**复制**进 `table 220`：

```
[entrypoint] table 220 default route added via 192.168.50.1 dev eth0
[entrypoint] table 220 direct route added: 192.168.50.0/24 dev eth0 scope link
```

效果：

- **同网段回包** → 命中 `table 220` 的直连路由 → 源 MAC 是容器自己的 → 不绕网关；
- **出公网** → 仍走 `table 220` 的 `default via 网关` → **XFRM 出向行为完全不变**。

⚠️ **这是 v2.92 唯一的实质改动**，不影响任何配置参数、UI 行为或数据格式。
升级无需改 compose，无需改 `.env`，无需清数据。

---

## 实机验证

192.168.50.63（群晖 NAS，Docker 28.5.2 / Compose v2.40.3），
**ipvlan 模式 + 固定容器 IP `192.168.50.66`**：

| 测试 | 修复前 | 修复后 |
|------|--------|--------|
| 容器内 `curl https://127.0.0.1:8443/login` | 200 | 200 |
| 容器内 `curl https://192.168.50.66:8443/login` | 200 | 200 |
| `ip route get 192.168.50.176` | `via 192.168.50.1 dev eth0 table 220` | `dev eth0 table 220` |
| `ip route get 8.8.8.8` | `via 192.168.50.1 dev eth0 table 220` | `via 192.168.50.1 dev eth0 table 220`（**不变**） |
| 外部 `openssl s_client` TLS 握手 | `write:errno=104`（被 RST） | **成功**，拿到 `CN=vpn.example.com` 面板证书 |
| 抓包 RST 计数 | 每连接 1 个 | **0** |
| 抓包回包源 MAC | 网关 `04:42:1a:5f:60:70` | 容器 `bc:24:11:8c:32:36` |
| **重启容器后** | — | 修复**自动恢复**（entrypoint 每次启动都会加） |

### v2.92 正式镜像复验

以上是用手工覆盖脚本的临时容器验证的。CI 出镜像后，又用**正式发布的
`ghcr.io/rewind2026/ikev2-panel-v2:v2.92`** 镜像重建部署复验一遍：

| 检查项 | 结果 |
|--------|------|
| 镜像内 entrypoint 是否含修复 | **是**（`grep -c "table 220 direct route added"` → 1） |
| 启动日志 | `table 220 default route added via 192.168.50.1 dev eth0` + `table 220 direct route added: 192.168.50.0/24 dev eth0 scope link` |
| 网络模式 / 固定 IP | `ipvlan (ipvlan_mode=l2 ipvlan_flag=bridge)`，`static ipv4=192.168.50.66`，`inet 192.168.50.66/24` |
| 同网段路由 | `192.168.50.176 dev eth0 table 220`（**直连**） |
| 公网路由 | `8.8.8.8 via 192.168.50.1 dev eth0 table 220`（**不变**） |
| 容器内 curl（loopback / ownip） | **200 / 200** |
| 外部 TLS 握手 | **成功**，`O=IKEv2 Panel, CN=vpn.example.com` |
| 抓包 RST 计数 | **0** |
| 抓包回包源 MAC | `bc:24:11:8c:32:36`（**容器自己的**） |
| **重启容器后** | 修复**自动恢复**，`ownip=200`，`Restarts=0` |

---

## 回归测试

`tests/network_mode_test.sh` 新增 **NM-61**（3 项），锁死三条不变量：

1. `entrypoint.sh` 必须有「把 scope link 直连路由写入 table 220」的逻辑；
2. 必须**保留** `default via 网关` 那条（出公网不能断）；
3. 抄直连路由时**不得**用 `ip route show ... dev X` 过滤。

第 3 条来自一个真机踩坑：`ip route show` **带了 dev 过滤后，输出里就不再有
dev 字段**，照着解析会拿到空结果，修复静默失效：

```
$ ip -4 route show table main scope link dev eth0
192.168.50.0/24 proto kernel src 192.168.50.66     ← 没有 dev！
```

正确写法是**先取全部 `scope link` 路由，再用 awk 按 dev 过滤**。

测试总数：**155 → 158，全部通过**。

静态检查：`go build` / `go vet` / `go test ./...` / 全部 shell `bash -n` 均通过。

---

## 升级方法

### 用 docker compose 部署的

```bash
docker compose -f <你的compose文件> pull
docker compose -f <你的compose文件> up -d
```

### 用 1Panel 部署的

1Panel 里点「重建」/「更新」镜像到 `v2.92` 即可。

### 临时处置（不升级镜像也能解决）

容器内加一条路由，重启会丢，但能立刻恢复访问：

```bash
docker exec ikev2-panel ip route replace 192.168.50.0/24 dev eth0 scope link table 220
```

⚠️ 网段要换成你自己 `IKEV2_L2_SUBNET` 的值。

---

## 排查这个问题时用到的命令

```bash
# 1. 同网段回包是否绕网关？出现 "via <网关>" = 中招
docker exec ikev2-panel ip route get 192.168.50.176

# 2. table 220 里有没有直连(scope link)路由？
docker exec ikev2-panel ip route show table 220
#   中招：只有 default via <网关>
#   正常：多一行 192.168.50.0/24 dev eth0 scope link

# 3. 抓包看 RST 的源 MAC 是谁
#    源 MAC = 网关的 → 绕网关了
#    源 MAC = 容器的 → 正常
tcpdump -i <你的网卡> -nn -e 'host <容器IP> and port 8443'
```

---

## 完整变更清单

见 [docs/CHANGELOG.md](CHANGELOG.md) 的 v2.92 小节。
