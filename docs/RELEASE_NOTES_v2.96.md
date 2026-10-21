# v2.96 — macvlan / ipvlan 共享网络：固定名 + 自动创建 + 多项目可加入同一条

发布日期：2026-10-21
镜像：`ghcr.io/rewind2026/ikev2-panel-v2:v2.96`（linux/amd64）

> 完整变更清单见 [CHANGELOG.md](CHANGELOG.md) 的 v2.95 / v2.96 小节。
> 本次发布同时包含 v2.95（ipvlan 共享网络）与 v2.96（macvlan 共享网络）两批改动。

---

## 一句话

**用户诉求：「我先创建了一个 ipvlan 的网络，名字也叫 ipvlan，我希望当前的项目和
其他的项目都可以加入到这个 ipvlan 的网络中。如果我之前没创建 ipvlan，就需要自动
创建。」随后追加：「现在我希望 macvlan 也要改成自适应的，并且我希望无论是
macvlan 还是 ipvlan，自动创建的时候的名字也是 macvlan 或 ipvlan，不需要有其他的
字符标识。」**

现在两种 L2 驱动都做到了：**固定名 + 不存在自动建 + 存在自动复用 + 多项目可加入
同一条**，网络名就是驱动名本身。

---

## 为什么必须这么做

Docker 的 IPAM 是全局的，「各 project 各建各的」这条路根本走不通：

| 限制 | 报错 |
|------|------|
| Docker IPAM 全局 | `invalid pool request: Pool overlaps with other one on this address space` |
| ipvlan parent 独占 | `network di-xxx is already using parent interface ens18`（换子网也不行） |

也就是说，**同一张网卡上本来就只能有一条 ipvlan 网络**。与其撞车，不如显式共享。

---

## 处置逻辑（macvlan / ipvlan 完全一致）

`./scripts/up.sh` 的 `_l2_shared_network_ensure`：

| 情况 | 行为 |
|------|------|
| ① 同名网络已存在 | **复用**，并逐项比对参数；不符 → 报错并**列出差异**，绝不自动删改 |
| ② 同名不存在，但宿主机上有参数兼容的**同驱动**网络 | **自动接管**那条（仅限用户没显式指定名字时） |
| ③ parent 上已有别的同驱动网络 | **仅 ipvlan** 报冲突（parent 独占）；**macvlan** 允许共存 |
| ④ 什么都没有 | **创建**（`docker network create --driver <驱动> --subnet=… --gateway=… --ipv6 <名字>`） |

**② 只在用户没显式指定名字时才自动接管**。显式指定（`IKEV2_L2_NETWORK_NAME`，
或 compose 顶部的 `x-l2-netname` 写成非默认值）= 用户明确要求用那一条，
此时若那条不存在而另有兼容的，报错让用户选，不悄悄换。

**参数不符一律报错并逐项列出差异**（驱动 / 父接口 / 子网 / 网关 / mode / flag /
IPv6 开关）—— 共享网络的 IPAM 归**第一个建它的人**所有，另一条网络很可能正被
别的 project 用着，自动改 subnet 或删网络都会打断它。

---

## 默认网络名 = 驱动名

| 模式 | 默认网络名 | 覆盖变量 |
|------|-----------|---------|
| `macvlan` | `macvlan` | `IKEV2_L2_NETWORK_NAME` |
| `ipvlan` | `ipvlan` | `IKEV2_L2_NETWORK_NAME` |

**不带任何 project 标识**（不再是 `<project>_ikev2-l2`）。三种写法等价：

```bash
# 1. 环境变量
IKEV2_L2_NETWORK_NAME=ipvlan ./scripts/up.sh

# 2. .env 文件
#    IKEV2_L2_NETWORK_NAME=ipvlan

# 3. compose 顶部（第四个 anchor）
x-l2-netname: &l2_netname "${IKEV2_L2_NETWORK_NAME:-ipvlan}"
```

---

## 两种驱动的能力对照

| 驱动 | 跨 project 共享同一条网络 | 同一 parent 上多条网络 | parent 独占 | 容器 MAC |
|------|--------------------------|---------------------|-----------|---------|
| `ipvlan` | ✅ | ❌ | **是** | 与宿主共享 |
| `macvlan` | ✅ | ✅（子网不重叠即可） | 否 | 各自独立 |

---

## 一个被纠正的误判

v2.95 的文档曾写「macvlan 不可共享」，**那是错的**。

当时的观察是在 63 上看到
`failed to create the macvlan port: device or resource busy`，就下了结论。
这次逐层排查发现，**真正的原因是宿主 `ens18` 本身不支持 macvlan 设备**：

```bash
ip link add link ens18 name mvtest0 type macvlan mode bridge
# RTNETLINK answers: Device or resource busy
```

连手工建设备都失败 —— 这跟 Docker、跟「共享」都没关系，是**宿主限制**。
换个能建 macvlan 设备的 parent 再测：同一条 macvlan 网络挂 3 个容器
（不同 project）全部正常，IP `.2` / `.3` / `.4`，MAC 各不相同。

> **判据要看清楚是「驱动限制」还是「宿主限制」。** 把宿主的限制当成驱动的限制，
> 会直接导致一个本该支持的能力被写死为不支持 —— 这正是 v2.95 的教训。

---

## ⚠️ 行为变更

| 项 | 之前 | 现在 |
|---|---|---|
| macvlan 网络名 | `<project>_ikev2-l2`（各 project 一条） | `macvlan`（默认，**所有 project 一条**） |
| ipvlan 网络名 | `<project>_ikev2-l2`（各 project 一条） | `ipvlan`（默认，**所有 project 一条**） |
| 谁建网络 | compose | `scripts/up.sh`（compose 侧改 `external: true`） |
| macvlan 的 parent 独占预检 | 有（**是错的**，macvlan parent 不独占） | 已删除 |
| 老用户的既有 L2 网络 | 照旧使用 | **自动接管**（parent + 子网 + 模式全等时），升级不失败 |
| 同名网络参数不一致 | 不可能发生 | **报错 + 列出差异**，不自动删改 |
| 显式指定名字的语义 | 不存在此概念 | 显式指定 = 明确要求用那一条，不自动接管 |

### 升级必读

`external: true` 意味着 **compose 不再兜底建网** —— 网络不存在时
`docker compose up` 会报 `declared as external, but could not be found`。

**L2 模式请走 `./scripts/up.sh`**（它做「不存在则建、存在则验、兼容则接管、
冲突则报错」）。`host` 模式不受影响，`docker compose up -d` 照旧可用。

老用户升级后，`up.sh` 会自动接管你现有的 `<project>_ikev2-l2` 网络
（parent + 子网 + 模式全等时），**不需要任何手工操作**。

---

## 回归测试

`tests/network_mode_test.sh` 的 **NM-62 / NM-63** 泛化到两种驱动：

| 覆盖点 | 条数 |
|--------|------|
| NM-62 网络名合法性（ipvlan / macvlan 两种模式各测） | 7 |
| NM-63 compose 与 up.sh 的接线（两份 compose × name / anchor / external） | 11 |
| NM-63 默认名两处一致 + 就是驱动名 + 按模式取默认名 | 5 |
| NM-63 up.sh 对 macvlan 也调用共享逻辑 + parent 预检只对 ipvlan 生效 | 2 |

全量 **180 passed / 0 failed**（`bash tests/network_mode_test.sh`）。

静态检查：`go build ./...` / `go vet ./...` / `go test ./...` 全部通过。

---

## 实机验证

192.168.50.63（飞牛 fnOS NAS，Docker 28.5.2 / Compose v2.40.3）。

### 一、插值实测

| 场景 | `docker compose config` 输出 |
|------|---------------------------|
| macvlan 默认 | `name: macvlan` + `external: true` |
| ipvlan 默认 | `name: ipvlan` + `external: true` |
| 指定 `IKEV2_L2_NETWORK_NAME=mynet` | `name: mynet` |

### 二、端到端 8 场景 22 条断言全绿

| 场景 | 结果 |
|------|------|
| 网络不存在 → 自动创建 | ✅ 名字 / 驱动 / parent / 子网 / 模式 / `enable_ipv6` 全对 |
| 网络已存在 → 复用 | ✅ Network Id 不变，日志「复用，不修改」 |
| 参数不匹配 → 报错 | ✅ 差异清单里列出了实际子网 |
| 显式指定不存在的网络 | ✅ 报错，不静默接管 |
| 名字不同但参数兼容 | ✅ 自动接管，且**不会**另建重复网络 |
| ipvlan 模式 | ✅ 同样自动创建出名为 `ipvlan` 的网络 |
| **两个 project 加入同一条 macvlan 网络** | ✅ 都 running，IP `192.168.71.2` / `.3`，MAC 各不相同 |
| `compose down` | ✅ 共享网络仍在，另一个 project 的容器不受影响 |

### 三、真实面板容器（24 条断言全绿）

正式镜像 `ghcr.io/rewind2026/ikev2-panel-v2:v2.96` 部署到 63，
**ipvlan 模式**（生产配置：parent `ens18` / 子网 `192.168.50.0/24` /
固定容器 IP `192.168.50.66`）。

**升级路径零操作** —— 这是本次最关键的验证点：

| 项 | 升级前 | 升级后 |
|----|--------|--------|
| 镜像 | `:latest` | `:v2.96`（程序自报 `version=2.96`） |
| 网络名 | `vpn_ikev2-l2` | `vpn_ikev2-l2`（**未改名**） |
| 是否新建了 `ipvlan` 网络 | —— | **没有**（自动接管了旧网络） |
| 容器 IP | `192.168.50.66` | `192.168.50.66` |
| v6 GUA | `fd56:e94c:f97f::2` | `fd56:e94c:f97f::2` + 公网 `2408:832e:8a4:5be0:…` |
| 数据卷 | `ikev2-panel-data` | `ikev2-panel-data`（老库复用） |

`up.sh` 的输出：

```
[up.sh] ! 网络模式: ipvlan（parent=ens18 subnet=192.168.50.0/24 gateway=192.168.50.1）
[up.sh] ! 容器固定 IPv4: 192.168.50.66
[up.sh] ✓ 宿主机上已有兼容的 ipvlan 网络 'vpn_ikev2-l2'（parent/子网/模式一致）→ 直接复用
[up.sh] !   提示：可用 IKEV2_L2_NETWORK_NAME=vpn_ikev2-l2 显式指定，避免以后误判
[up.sh] running: docker compose -f docker-compose.ipvlan.yml up -d
```

**面板功能实测**（容器内 `openssl s_client` 发裸 HTTP，24 条断言全绿）：

| 检查 | 结果 |
|------|------|
| `GET /login` | ✅ 200，带 CSP 头，返回 HTML |
| `GET /` | ✅ 未登录跳登录页 |
| `GET /healthz` / `GET /readyz` | ✅ 200 |
| `GET /api/cert/status` | ✅ 正常鉴权响应（不是 500） |
| `GET /login` 经容器**自身 IP** | ✅ 200（证明监听在 `0.0.0.0`） |
| `swanctl --load-all` | ✅ 成功，连接已注册 |
| `swanctl --list-conns` | ✅ 能列出 `ikev2` 连接 |
| 老数据卷复用 | ✅ `panel.db` 在位 |
| 旧 IP 段配置读回 | ✅ `[subnet.conf] applied: ipv4=10.10.0.0/24`（`panel state wins`） |
| 容器日志 | ✅ 无 `level=ERROR`（仅 1 条自签证书生成前的瞬态 WARN） |

> 另：63 的 `ens18` 不支持 macvlan 设备（宿主限制，见上文「被纠正的误判」），
> 所以 macvlan 的实机验证在 `docker0` 上做 —— macvlan 在 `docker0` 上能正常
> 建设备，共享能力与 parent 无关。

### 四、一个既有限制（不是本次回归）：宿主访问不到容器 IP

宿主访问 `192.168.50.66:8443` 报 `No route to host`（ARP `FAILED`）。
**对照实验证明与 v2.96 无关** —— 同一网络上的 v2.94 容器**同样**不可达：

| 容器 | 镜像 | 宿主 → 容器 IP |
|------|------|---------------|
| `cmp94` | `:v2.94` | `No route to host` |
| `cmp96` | `:v2.96` | `No route to host` |
| `ikev2-panel` | `:v2.96` | `No route to host` |

这是 **ipvlan 模式本身**的特性：宿主自己的 `192.168.50.63` 与容器 IP 之间
ARP 不通（`ip neigh show 192.168.50.66` → `FAILED`）。**IPv6 同样**
（邻居发现 `FAILED`），但容器确实拿到了公网 GUA，外部客户端走 IPv6 可达。

> 排查这类问题时**先做对照实验**（同网络、同 parent，换个镜像版本起临时容器），
> 否则很容易把宿主的既有限制误判成新版本的回归。

---

## 文档

- [architecture.md](architecture.md) §11.1.2 —— 共享 L2 网络的完整设计
- [TROUBLESHOOTING.md](TROUBLESHOOTING.md) §1.7.9 —— 共享网络相关报错的处置
- [README.md](../README.md) —— 网络模式章节的共享网络说明
