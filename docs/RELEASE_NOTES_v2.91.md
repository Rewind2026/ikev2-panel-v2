# v2.91 — L2 模式支持固定容器 IPv4

发布日期：2026-10-21

给 L2（macvlan / ipvlan）模式补上**固定容器 IPv4** 参数，并顺带修掉两个
「用户照文档做仍然失败」的真实故障。

镜像：`ghcr.io/rewind2026/ikev2-panel-v2:v2.91`（linux/amd64）

> **本版同时包含 v2.90 的内容。** v2.90（L2 变量改用 YAML anchor，用户只改
> compose 顶部三行、零 `.env`）此前只 commit 未发布，本次一并出。
> 详见 [CHANGELOG.md](CHANGELOG.md) 的 v2.90 与 v2.91 小节。

---

## ⚠️ 先看这条：compose 文件不进镜像

这次的主体改动在 **compose 文件**里，而 `docker-compose*.yml` **不进镜像**
（[Dockerfile](../Dockerfile) 成品层只 COPY `scripts/`、`web/`、`configs/`）。

- **已经跑起来的容器**：什么都不用做。
- **需要固定 IP / 需要修 anchor 写法的人**：更新你那份 compose 文件（`git pull` 即可），
  **镜像拉不拉都一样**。

镜像里唯一的变化是 `PanelVersion` 字符串从 `v2.90` 变成 `v2.91`。

---

## 一、固定容器 IPv4（新增）

此前仓库里**完全没有任何** `ipv4_address` / `STATIC_IP` 之类的配置，
容器 IP 一律由 Docker IPAM 随机分配。需要固定地址（防火墙规则写死、多个实例区分）
时无处可配。

### 怎么配

在 `docker-compose.macvlan.yml` / `docker-compose.ipvlan.yml` **顶部**加第四行：

```yaml
x-l2-ipv4:    &l2_ipv4    "${IKEV2_L2_IPV4:-192.168.50.77}"
```

**留空 = 由 Docker 自动分配**（compose 会把空值键丢掉，展开成 `ikev2-l2: {}`，
Docker 退回自动分配）—— 不需要任何条件语法。

优先级与 L2 那三项一致：`shell env > .env 文件 > compose 顶部默认值`。

### 落在哪里（容易放错）

Docker 的静态地址属于 **service 段的 endpoint 配置**，不属于网络本身的 ipam：

```yaml
services:
  ikev2-panel:
    networks:
      ikev2-l2:
        ipv4_address: 192.168.50.77   # ← 这里
networks:
  ikev2-l2:
    ipam:
      config:
        - subnet: 192.168.50.0/24    # ← 这里配 subnet/gateway
          gateway: 192.168.50.1
```

塞进 ipam 会被 Docker **静默忽略** —— 无报错、地址不生效，用户填了却发现没生效
且无从排查。

因此两份 L2 compose 的 `service` 段 `networks:` 由短语法 `- ikev2-l2` 改成
长语法 `ikev2-l2: {ipv4_address: ...}`。实测两种写法的**网络别名完全一致**
（同为 `[<容器名>, <服务名>]`），所以改长语法零副作用。

### 约束

| 约束 | 谁来拦 |
|------|--------|
| 必须是合法 IPv4 | `scripts/network-mode.sh` |
| 必须落在 `IKEV2_L2_SUBNET` 内 | `network-mode.sh`（daemon 也会拒） |
| 不能等于网关 | `network-mode.sh`（daemon 也会拒） |
| **不能是宿主正在用的地址** | `scripts/up.sh` 部署前拦（**daemon 不拦**） |

### ⚠️⚠️ 最严重的一条：填了宿主自己的地址 → 宿主入站连接全断

docker daemon 只拒「不在子网内」和「网络自己已占用」的地址，
**不知道宿主某个接口上已经有这个 IP**。容器起来后抢走宿主该 IP 的 ARP，
宿主所有入站流量被容器吸走。

实测（192.168.50.63，Docker 28.5.2）：固定 IP 填 `192.168.50.63`（宿主自己的地址）
→ 容器正常创建、正常启动、**日志无任何报错** → 宿主 SSH 立刻
`Connection reset by peer`，面板也连不上；网关 ping 得通，但入站全断。两次复现。

**填错固定 IP 的代价可能不止是容器起不来，而是宿主失联。**

处置与预防见
[TROUBLESHOOTING.md §1.7.7](TROUBLESHOOTING.md)。`scripts/up.sh` 从本版起会在
`up` **之前**比对宿主所有接口的 IPv4，冲突则拒绝启动（容器不会创建，宿主网络完好）。

---

## 二、顺带修掉：`up.sh` 认不出 anchor 的 `${VAR:-默认}` 写法

v2.90 的文档教用户这样填：

```yaml
x-l2-parent:  &l2_parent  "${IKEV2_L2_PARENT:-ens18}"
```

但 v2.90 新增的 `compose_get` 只认裸字面量 `&anchor "eth0"`，取到的是
插值表达式本身 → `up.sh` 报「缺少必填变量 IKEV2_L2_PARENT」
—— **而用户明明在 compose 里填好了**。

重写为认得三种写法：

| 形态 | 示例 | 取值 |
|------|------|------|
| 1 | `&l2_parent "${IKEV2_L2_PARENT:-ens18}"` | `:-` 后的默认值 |
| 1' | `&l2_parent "${IKEV2_L2_PARENT:-}"` | 空 |
| 2 | `&l2_parent "eth0"` | 引号内字面量 |
| 3 | `&l2_parent eth0` | 裸值 |

---

## 三、顺带修掉：文档里的 anchor 示例会让 `.env` 永久失效

README / TROUBLESHOOTING 里的示例原写成裸字面量：

```yaml
x-l2-parent:  &l2_parent  "eth0"        # ← 错
```

用户照抄后 `${VAR:-}` 外壳被剥掉 → `.env` 永久失效，与文档自称的优先级链
（`shell env > .env > compose 顶部默认值`）自相矛盾。

正确写法是**把值填在 `${...:-}` 的括号里**：

```yaml
x-l2-parent:  &l2_parent  "${IKEV2_L2_PARENT:-eth0}"   # ← 对
```

外壳在，`.env` 用户才能覆盖；不写 `.env` 的话外壳留着也不影响。
README / TROUBLESHOOTING / compose 文件顶部注释三处现已统一。

---

## 改动文件

| 文件 | 改动 |
|------|------|
| `docker-compose.macvlan.yml` / `.ipvlan.yml` | 顶部加 `x-l2-ipv4` anchor；`service` 段 `networks` 改长语法并加 `ipv4_address: *l2_ipv4`；`environment` 段加 `IKEV2_L2_IPV4: *l2_ipv4` |
| `docker-compose.yml`（host） | `environment` 段加 `IKEV2_L2_IPV4: "${IKEV2_L2_IPV4:-}"`，保持三份文件键集合一致 |
| `scripts/network-mode.sh` | 新增固定 IP 校验：合法 IPv4 / 落在 subnet 内 / 不等于网关；host 模式 WARN 列表加入该变量 |
| `scripts/entrypoint.sh` | L2 分支启动日志回显 `static ipv4=`（留空时显示 `auto-assigned by Docker IPAM`） |
| `scripts/up.sh` | `compose_get` 重写为三形态；新增宿主地址冲突检查；`--help` 更新 |
| `internal/web/handlers_settings.go` | 「部署级参数」清单加 `IKEV2_L2_IPV4`（只读展示，与其余 L2 三项一致） |
| `tests/network_mode_test.sh` | NM-57 扩展；新增 **NM-58 / NM-59 / NM-60** |
| `README.md` / `TROUBLESHOOTING.md` | anchor 示例改 `${VAR:-默认}`；新增固定 IP 小节与 §1.7.7 排查章节 |

---

## 验收

### 静态

| 项 | 结果 |
|----|------|
| `tests/network_mode_test.sh` | ✅ **155 passed / 0 failed** |
| `go build ./...` / `go vet ./...` / `go test -count=1 ./...` | ✅ 全绿 |
| 4 个 shell 脚本 `bash -n` | ✅ 通过 |

### 实机（192.168.50.63，Docker 28.5.2 / Compose v2.40.3，群晖 NAS）

三模式 compose 参数：

| 模式 | `config` 解析 | 容器起 | 容器 IP | 网关 | restarts | 面板 |
|------|-------------|--------|---------|------|----------|------|
| host | ✅ exit 0 | ✅ Up | —（共享宿主 netns） | — | 0 | 302 → 登录页 |
| ipvlan | ✅ exit 0 | ✅ Up | 192.168.50.2 | 192.168.50.1 | 0 | 容器内可访问 |
| macvlan | ✅ exit 0 | ✅ Up | 192.168.50.2 | 192.168.50.1 | 0 | 容器内可访问 |

固定 IPv4：

| 用例 | 结果 |
|------|------|
| 留空 | ✅ 自动分配 `.2`；env `IKEV2_L2_IPV4=`（空串）；别名 `[ikev2-panel ikev2-panel]` |
| 固定 `.77`（ipvlan） | ✅ 容器内 `inet 192.168.50.77/24` + 路由正确 + 出网 `http=200` |
| 固定 `.78`（macvlan） | ✅ IP / 路由 / env / 出网全对，别名不变 |
| 改 `.99` | ✅ 容器 IP = `.99` |
| 不在 subnet 内 | ✅ daemon 拒：`no configured subnet or ip-range contain the IP address` |
| = 网关 | ✅ daemon 拒：`Address already in use` |
| `.env` 覆盖 anchor | ✅ 取 `.env` 的 `.88`，两段同步 |
| 固定 = 宿主 `.63` | ✅ **up.sh 拦住，容器未创建，宿主网络完好** |
| 固定 `.77` 走 up.sh | ✅ 放行，容器 IP = `.77` |
| 留空走 up.sh | ✅ 放行，自动分配 `.2` |

env 优先级链（compose / env / 面板三层）：

| 场景 | `environment` 段 | `networks` 段 |
|------|---------------|-------------|
| 只有 compose 顶部默认值 | `ens18` | `parent: ens18` |
| `.env` 覆盖 | `from-env0` | `parent: from-env0` |
| shell env 覆盖 `.env` | `from-shell` | `parent: from-shell` |
| 删掉 `.env` | 回落 `ens18` | 回落 `ens18` |

→ **标准优先级链完整保留**：`shell env > .env 文件 > compose 顶部默认值`。

UI 面板联动（真实 HTTP 链路：登录 → `/api/settings/save` → `/api/settings/status`）：

| 用例 | 结果 |
|------|------|
| UI 保存时区 / session_ttl / log_level | ✅ 302，落盘 `settings.json` |
| 保存后 `/api/settings/status` | ✅ `source` 变 `panelstate` |
| 重启容器后 | ✅ `settings.json` 保留、仍生效 |
| 日志级别改 debug | ✅ 日志真的变 `level=DEBUG` |
| 全勾「跟随 env」 | ✅ `settings.json` 被删除，`source` 全回 `env` |
| 非法值 `connlimit_per_ip=abc` | ✅ 拒绝保存，不落盘（不静默改错配置） |
| 缺 CSRF token | ✅ 403 |
| L2 模式下 UI 改参数 | ✅ 保存 / 生效 / 重启保留全通过 |
| compose → 容器 env → UI 展示 | ✅ 三者完全一致（ipvlan / macvlan 各验一次） |

**v2.91 正式镜像复验**（63 上真实拉取 `ghcr.io/rewind2026/ikev2-panel-v2:v2.91`）：

| 用例 | 结果 |
|------|------|
| 镜像内 `/etc/ikev2-panel-version` | ✅ `2.91` |
| ipvlan + 固定 `.77` | ✅ 容器 IP = `.77`，env / 别名 / 日志 / 路由全对 |
| macvlan + 固定 `.78` | ✅ 容器 IP = `.78`，出网 `http=200` |
| host 模式 | ✅ `Up`，`restarts=0`，面板 302 → 登录页 |
| `.env` 覆盖固定 IP | ✅ `environment` 与 `networks` **两段同步**取 `.88` |
| up.sh 冲突拦截（固定 = 宿主 `.63`） | ✅ 拒绝启动，容器未被重建，宿主网络完好 |
| **UI「部署级参数」区显示 `IKEV2_L2_IPV4`** | ✅ `192.168.50.77` |

> 之前用 v2.89 镜像做 UI 验证时，部署级参数区看不到 `IKEV2_L2_IPV4` ——
> 那是因为 Go 侧的新增行未进镜像。v2.91 镜像已包含该项，现已正常显示。

---

## 安全约束（本次未触碰）

| 约束 | 状态 |
|------|------|
| `WriteDDNSConfig` 只能改注释，不能删除 | ✅ `internal/panelstate/` 未改动 |
| `scripts/entrypoint.sh` 不改现有 `grep` 判定逻辑 | ✅ 仅在 L2 分支**新增** 3 行日志回显，判定逻辑一字未动 |
| `schema_version` 兼容老文件 | ✅ `internal/panelstate/` 未改动 |

---

## 升级

### 你需要固定容器 IP

1. `git pull` 更新 `docker-compose.macvlan.yml` / `docker-compose.ipvlan.yml`
2. 顶部加一行 `x-l2-ipv4: &l2_ipv4 "${IKEV2_L2_IPV4:-<你要的地址>}"`
3. `docker compose -f docker-compose.<mode>.yml up -d`（**必须 recreate**）

### 你用的是第三方面板（1Panel / 宝塔 / dpanel）

同样替换 compose 文件。面板的「环境变量」优先级更高 ——
如果那里已经有 `IKEV2_L2_*` 项，面板里的值会覆盖 compose 顶部的默认值。

### 已经跑起来了 / host 模式 → 不用动

---

完整变更清单见 [docs/CHANGELOG.md](CHANGELOG.md) 的 v2.91 小节。
