# v2.97 — 纯 compose 路径可用：建网责任交还给 compose，`up.sh` 降级为可选增强

发布日期：2026-10-22
镜像：`ghcr.io/rewind2026/ikev2-panel-v2:v2.97`（linux/amd64）

> 完整变更清单见 [CHANGELOG.md](CHANGELOG.md) 的 v2.97 小节。
> v2.97 **只改了 compose / 脚本 / 文档，镜像里的面板程序逻辑零改动** ——
> 用 `:latest` 也能得到完全相同的行为。

---

## 一句话

**用户诉求：「目前的应用场景只有用 compose pull 容器，不存在在外部调用 up.sh
或其他脚本去 pull docker images 的情况。所以，如果只用 compose，我们能实现哪些？」**

现在**只跑 compose**（飞牛 fnOS Docker 管理器 GUI / 第三方面板编排页）就能拿到
完整的共享 L2 网络能力：网络不存在自动建、已存在自动复用、多项目可加入同一条、
名字固定为驱动名。

---

## 之前为什么起不来

v2.95 / v2.96 把「共享 L2 网络」的建网责任交给了 `scripts/up.sh`，compose 侧
写死 `external: true`。这个前提是「用户跑 `up.sh`」。

但真实部署场景是**只跑 compose**，**没有任何脚本介入**。写死 `external: true` 时
纯 compose 路径直接失败（63 实测复现）：

```
network ipvlan declared as external, but could not be found
```

这就是「用 63 的 Docker 管理器测试 docker compose 还是无法启动」的根因。

---

## 纯 compose 路径现在能做到什么（63 全部实测通过）

| 能力 | 结果 |
|------|------|
| 网络不存在 → 自动创建 | ✅ compose 自建，名字固定 `ipvlan`（有 `name:`，无 project 前缀） |
| 网络已存在 → 复用 | ✅ 复用（带正确 label 时），只出 warning |
| 多项目加入同一条网络 | ✅ P1 先建、P2 复用；两容器 IP `192.168.50.2` / `.3`，同挂 `ipvlan` |
| `down` 不误伤共享网络 | ✅ P1 down 时 Docker 报 `Resource is still in use` → 网络保留，P2 照常 |
| 必填变量留空 → 中文报错 | ✅ compose 在**拉镜像前**停下（见下） |
| 与 up.sh 路径互通 | ✅ up.sh 建的网带 label，纯 compose 能直接复用 |

### 留空时的报错长这样

```yaml
x-l2-subnet: &l2_subnet "${IKEV2_L2_SUBNET:-${IKEV2_L2_SUBNET:?请在 compose 顶部 x-l2-subnet 填宿主所在 IPv4 子网（如 192.168.50.0/24）}}"
```

```
error while interpolating x-l2-subnet: required variable IKEV2_L2_SUBNET is missing a value:
请在 compose 顶部 x-l2-subnet 填宿主所在 IPv4 子网（如 192.168.50.0/24）
```

**在 config 阶段就报错** —— 不拉镜像、不建容器、不建网络。63 实测
（Docker 28.5.2 / Compose v2.40.3）。

---

## 纯 compose 路径**做不到**的（都还是 up.sh 专属）

| 能力 | 说明 |
|------|------|
| 参数不匹配报差异表 | compose 只校验 label，不比对 driver / parent / 子网 / 模式 |
| ipvlan parent 独占预检 | compose 不做，撞了才由 daemon 报英文 |
| 容器 IPv4 与宿主地址冲突检查 | 撞上会让**宿主入站连接全断**（实测 SSH 直接 reset） |
| `./logs` bind mount 目录准备 | compose 路径要自己 `mkdir -p logs && chmod 1777 logs` |
| 老网络自动接管 | compose 不认参数兼容的旧网络 |

> 结论：**纯 compose 路径覆盖了 90% 的场景**（能跑、能共享、能多项目）。
> 剩下 10% 是「出问题时的诊断友好度」，不值得为它把建网责任从 compose 抢走。

---

## 改了什么

### compose 侧（`docker-compose.macvlan.yml` / `docker-compose.ipvlan.yml`）

```yaml
# networks 段：之前写死 external: true
external: "${IKEV2_L2_EXTERNAL:-false}"
```

```yaml
# 顶部三项必填变量：出厂「留空 + 中文报错」形态
x-l2-parent:  &l2_parent  "${IKEV2_L2_PARENT:-${IKEV2_L2_PARENT:?请在 compose 顶部 x-l2-parent 填父接口名（如 ens18）}}"
x-l2-subnet:  &l2_subnet  "${IKEV2_L2_SUBNET:-${IKEV2_L2_SUBNET:?请在 compose 顶部 x-l2-subnet 填宿主所在 IPv4 子网（如 192.168.50.0/24）}}"
x-l2-gateway: &l2_gateway "${IKEV2_L2_GATEWAY:-${IKEV2_L2_GATEWAY:?请在 compose 顶部 x-l2-gateway 填网关（如 192.168.50.1）}}"
```

`external` 的两种取值（63 实测）：

| 取值 | 场景 | 行为 |
|------|------|------|
| `false`（默认） | 纯 compose 路径 | compose 自己建：不存在就建，已存在就复用 |
| `true` | up.sh 路径 | compose 只加入，不创建也不删除 |

### 关键：external=true 会跳过 label 校验，external=false 不会

compose 复用一条已存在的同名网络时会校验它上面的 label
`com.docker.compose.network` 是否等于 networks 段的 **key**（`ikev2-l2`）：

| 场景 | `external: false` | `external: true` |
|------|------------------|------------------|
| 网络不存在 | compose 建成 | 报 `declared as external, but could not be found` |
| 网络已存在、**带** label | 复用（只出 warning） | 复用（无 warning） |
| 网络已存在、**无** label | **报错** `incorrect label ... (expected: "ikev2-l2")` | 复用（跳过校验） |

这张表决定了整个设计：**up.sh 建的网必须带 label**，否则纯 compose 路径复用不了它。

### up.sh 侧

- 建网时带 `--label com.docker.compose.network=ikev2-l2` → up.sh 建的网
  能被纯 compose 路径复用（两条路径从此互通）；
- 建完网后**强制** `IKEV2_L2_EXTERNAL=true` 并 export → compose 只加入，
  不会跟 up.sh 抢网络的管理权，也不会在「自动接管来的老网络」上撞 label 校验；
- `compose_get()` 新增「嵌套 `:?`」形态识别（出厂形态下用户还没填值 → 返回空，
  让 `net_mode_validate` 报「缺少必填变量」，而不是把内层表达式当默认值）。

### network-mode.sh 侧

- 新增常量 `NET_SHARED_NETWORK_COMPOSE_KEY="ikev2-l2"`（compose networks 段的 key）；
- `net_mode_validate` 校验 `IKEV2_L2_EXTERNAL` 只能是 `true` / `false`；
- host 模式忽略 `IKEV2_L2_EXTERNAL`（只 WARN）。

---

## ⚠️ 行为变更

| 项 | 之前（v2.96） | 现在（v2.97） |
|---|---|---|
| compose networks 段的 external | 写死 `true` | `"${IKEV2_L2_EXTERNAL:-false}"`（默认 false = compose 自己建） |
| 顶部三项必填变量的出厂值 | 有默认值（`ens18` / `192.168.1.0/24` / `192.168.1.1`） | **留空 + 中文报错**（`:?` 嵌套形态） |
| 谁建 L2 网络 | 只有 `up.sh` | **compose（默认）或 up.sh**（两条路径互通） |
| up.sh 建的网络 | 无 compose label（纯 compose 复用不了） | 带 `com.docker.compose.network=ikev2-l2` |
| 纯 compose 部署 | **起不来**（`declared as external`） | **可用** |

### 升级必读

v2.96 的 compose 文件顶部三项带**写死的默认值**（`ens18` / `192.168.1.0/24` /
`192.168.1.1`）。v2.97 改成留空 —— 升级后**必须填上真实值**，否则 compose 会在
拉镜像前用中文报错提醒你。

`.env` 用户不受影响（`.env` 优先级高于文件里的字面量）。

---

## 怎么填那三个值

在**宿主**上查：

```bash
ip -o -4 route show default     # 第一列 dev 后面那个 = 父接口名；第三列 = 网关
ip -o -4 addr show <父接口>     # inet 那一列的 CIDR = 子网
```

然后把 compose 顶部三行里 `:-` 后面的整段内容替换成你的值（**外层 `${...}` 外壳保留**，
留着它 `.env` 才能覆盖）：

```yaml
x-l2-parent:  &l2_parent  "${IKEV2_L2_PARENT:-ens18}"
x-l2-subnet:  &l2_subnet  "${IKEV2_L2_SUBNET:-192.168.50.0/24}"
x-l2-gateway: &l2_gateway "${IKEV2_L2_GATEWAY:-192.168.50.1}"
```

**别忘了 `./logs` 目录**（compose 路径没有 `up.sh` 帮你建）：

```bash
mkdir -p logs && chmod 1777 logs
```

---

## 回归测试

`tests/network_mode_test.sh`：

| 覆盖点 | 条数 |
|--------|------|
| NM-63 第 3 条改写：networks 段 external 必须是插值开关 | 2 |
| NM-64 嵌套 `:?` 形态（parent / subnet / gateway × 两种驱动） | 6 |
| NM-64 compose networks 段 key 与 `NET_SHARED_NETWORK_COMPOSE_KEY` 一致 | 2 |
| NM-64 up.sh 建网带 label / 强制 external=true | 2 |
| NM-64 `IKEV2_L2_EXTERNAL` 合法性（非法报错 / true 通过 / host 忽略） | 6 |

全量 **195 passed / 0 failed**（`bash tests/network_mode_test.sh`）。

---

## 实机验证

192.168.50.63（飞牛 fnOS NAS，Docker 28.5.2 / Compose v2.40.3）。

| 场景 | 结果 |
|------|------|
| 纯 compose：网络不存在 → 自动创建 | ✅ `Network ipvlan Created`，名字固定 `ipvlan`，label 自动带 `com.docker.compose.network=ikev2-l2` |
| 纯 compose：跨 project 复用同一条 | ✅ P1 先建、P2 复用；两容器 IP `192.168.50.2` / `.3`，同挂 `ipvlan` |
| 纯 compose：P1 down | ✅ 网络仍在（`Resource is still in use`），P2 照常 |
| 纯 compose：必填变量留空 | ✅ config 阶段中文报错，**不拉镜像** |
| up.sh：建网 | ✅ label = `{"com.docker.compose.network":"ikev2-l2"}`，容器 Up，IP `192.168.50.2` |
| up.sh：down 不删网 | ✅ 网络还在 |
| 两条路径互通 | ✅ up.sh 建的网 → 纯 compose 复用成功（仅 warning） |

> ⚠️ 63 的 `ens18` **不支持 macvlan 设备**（宿主限制，详见 v2.96 发布说明），
> 所以 macvlan 侧验证在 `docker0` 上做。

### 真实部署目录已切到新版（63 `/vol1/1000/TEST/`）

| 项 | 值 |
|---|---|
| compose 文件 | `docker-compose.yml`（旧版备份在 `docker-compose.yml.bak-v296`） |
| 父接口 / 子网 / 网关 | `ens18` / `192.168.50.0/24` / `192.168.50.1` |
| 固定容器 IPv4 | `192.168.50.66` |
| 网络 | `ipvlan`（compose 自建，label `com.docker.compose.network=ikev2-l2`） |
| 容器 | `ikev2-panel` Up，IP `192.168.50.66`，v6 GUA `fd82:f5a0:6f67::2` |
| 面板 | `GET /healthz` → 200，`GET /login` → 200（走容器自身 IP） |

启动输出（**纯 compose，无任何脚本介入**）：

```
 ikev2-panel Pulling
 ikev2-panel Pulled
 Network ipvlan  Creating
 Network ipvlan  Created
 Container ikev2-panel  Created
 Container ikev2-panel  Started
```

> ⚠️ 部署目录里额外补了 `logs/`（`mkdir -p logs && chmod 1777 logs`）——
> 纯 compose 路径没有 `up.sh` 帮你建这个 bind mount 目标。

---

## 文档

- [architecture.md](architecture.md) §11.1.2 —— 共享 L2 网络的完整设计 + `external` 语义
- [TROUBLESHOOTING.md](TROUBLESHOOTING.md) §1.7.9 —— 共享网络相关报错的处置
- [README.md](../README.md) —— 网络模式章节的共享网络说明
