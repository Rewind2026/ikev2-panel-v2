# CHANGELOG

本文件记录 v2.79.2 起每个版本的变更。

**版本号含义**：`v2.<minor>` 是功能版本（批量合并）；`v2.86-prNN` 是该版本内的单个 PR /
hotfix。升级前请重点看目标版本的「⚠️ 行为变更」。

版本之间不兼容的变更一律在对应版本的「⚠️ 行为变更」小节里显式标注，不藏在正文里。

**怎么追溯**：

| 想查 | 去哪 |
|------|------|
| 某行为什么时候改的 | 本文件（每个 PR 一个条目） |
| 某个功能现在怎么实现 | [architecture.md](architecture.md) |
| 某次改动的完整分析 | `git log` + `git show <sha>` |
| 代码与文档不符 | **以代码为准**，并顺手改文档（见 [docs/README.md](README.md) 维护约定） |

---

## v2.91（2026-10-21）— L2 模式支持固定容器 IPv4 + `up.sh` 认得 anchor 写法

**用户反馈：「L2 模式应该有 ipv4 固定地址参数」。** 属实 —— v2.90 之前
`grep -E 'ipv4_address|STATIC_IP|L2_IP|L2_ADDR'` 在整个仓库零命中，
ipam 段只有 `subnet` / `gateway`，容器 IP 完全由 Docker IPAM 随机分配。
需要固定地址（防火墙规则写死、多个实例区分）时无处可配。

### 固定 IPv4 放在哪里（容易放错）

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

### 短语法 → 长语法

`networks: [ikev2-l2]`（短语法）根本表达不了 `ipv4_address`，必须改成
`networks: {ikev2-l2: {ipv4_address: ...}}`（长语法）。

实测（192.168.50.63）：两种写法的**网络别名完全一致**（同为
`[<容器名>, <服务名>]`），所以改长语法零副作用。

**空值行为正好免掉条件语法**：`ipv4_address: ""` 时 compose 自动丢弃该键，
展开成 `ikev2-l2: {}`，Docker 退回自动分配 —— 「留空 = 自动分配」的语义
不需要任何 `if` 就能实现。

### 改动文件

| 文件 | 改动 |
|------|------|
| `docker-compose.macvlan.yml` / `.ipvlan.yml` | 顶部加 `x-l2-ipv4: &l2_ipv4 "${IKEV2_L2_IPV4:-}"`；`service` 段 `networks` 改长语法并加 `ipv4_address: *l2_ipv4`；`environment` 段加 `IKEV2_L2_IPV4: *l2_ipv4` |
| `docker-compose.yml`（host） | `environment` 段加 `IKEV2_L2_IPV4: "${IKEV2_L2_IPV4:-}"`，保持三份文件键集合一致 |
| `scripts/network-mode.sh` | 新增固定 IP 校验：合法 IPv4 / 落在 subnet 内 / 不等于网关；host 模式的 WARN 列表加入该变量 |
| `scripts/entrypoint.sh` | L2 分支启动日志回显 `static ipv4=`（留空时显示 `auto-assigned by Docker IPAM`） |
| `scripts/up.sh` | `compose_get` 重写为认得三种写法；新增宿主地址冲突检查；`--help` 更新 |
| `internal/web/handlers_settings.go` | 「部署级参数」清单加 `IKEV2_L2_IPV4`（只读展示，与其余 L2 三项一致） |
| `tests/network_mode_test.sh` | NM-57 扩展；新增 **NM-58 / NM-59 / NM-60** |
| `README.md` | anchor 示例改为 `${VAR:-默认}` 形式；新增「可选：固定容器 IPv4」小节 |

### ⚠️ 顺带修掉：README / TROUBLESHOOTING 的 anchor 示例写成了裸字面量

v2.90 的文档里写的是：

```yaml
x-l2-parent:  &l2_parent  "eth0"        # ← 错
```

用户照抄后，`${VAR:-}` 外壳被剥掉 → **`.env` 永久失效**，与文档自称的
优先级链（`shell env > .env > compose 顶部默认值`）自相矛盾。

正确写法是**把值填在 `${...:-}` 的括号里**：

```yaml
x-l2-parent:  &l2_parent  "${IKEV2_L2_PARENT:-eth0}"   # ← 对
```

外壳在，`.env` 用户才能覆盖；不写 `.env` 的话外壳留着也不影响。
README / TROUBLESHOOTING / compose 文件顶部注释三处现已统一。

### ⚠️ 顺带修掉：`up.sh` 的 `compose_get` 认不出 `${VAR:-默认}`

v2.90 新增的 `compose_get` 只匹配 `&anchor "字面量"`，而 compose 文件里实际是
`&anchor "${IKEV2_L2_PARENT:-ens18}"` —— 取到的是插值表达式本身，
`up.sh` 于是报「缺少必填变量 IKEV2_L2_PARENT」，**而用户明明在 compose 里填好了**。

重写为三形态匹配：

| 形态 | 示例 | 取值 |
|------|------|------|
| 1 | `&l2_parent "${IKEV2_L2_PARENT:-ens18}"` | `:-` 后的默认值 |
| 1' | `&l2_parent "${IKEV2_L2_PARENT:-}"` | 空（回落为空） |
| 2 | `&l2_parent "eth0"` | 引号内字面量 |
| 3 | `&l2_parent eth0` | 裸值 |

`compose_get_unquoted` 随之删除（合并进形态 3）。

### ⚠️ 顺带修掉：固定 IP 撞宿主地址会把宿主网络打挂

**这是本次最严重的发现。** daemon 只拦「不在 subnet 内」和「网络自己已占用」，
**不知道宿主某个接口上已经有这个 IP**：

| 固定 IP 情形 | daemon 行为 | 容器起得来吗 |
|-------------|-----------|------------|
| 不在 subnet 内（如 `10.99.99.5`） | 拒：`no configured subnet or ip-range contain the IP address` | ❌ |
| = 网关（`.1`） | 拒：`Address already in use` | ❌ |
| = 网络地址（`.0`）/ 广播（`.255`） | 拒：`Address already in use` | ❌ |
| **= 宿主正在用的地址（`.63`）** | **不拦** | ✅ **起得来，但抢走宿主 ARP** |

最后一种在 192.168.50.63 上复现两次：容器正常创建、正常启动，
随后宿主所有入站连接断掉 —— SSH 直接 `Connection reset by peer`，
网关 ping 得通但入站全断。**填错固定 IP 的代价可能不止是容器起不来，
而是宿主失联。**

daemon 拦不住，就只能由 `up.sh` 在 `up` 之前拦：新增
`_l2_ipv4_conflict_check()`，列出宿主所有接口的 IPv4 逐个比对，
冲突则 `die` 拒绝启动（容器不创建、宿主网络完好）。

### 验收

`tests/network_mode_test.sh` **155 passed / 0 failed**。
`go build` / `go vet` / `go test -count=1 ./...` 全绿。4 个 shell 脚本 `bash -n` 通过。

**实机（192.168.50.63，Docker 28.5.2 / Compose v2.40.3，群晖 NAS）**：

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
| 改 `.99` | ✅ 容器 IP = `.99`（改 anchor 重建即生效） |
| 不在 subnet 内 | ✅ daemon 拒：`no configured subnet or ip-range contain the IP address` |
| = 网关 | ✅ daemon 拒：`Address already in use` |
| `.env` 覆盖 anchor | ✅ 取 `.env` 的 `.88`，`environment` 与 `networks` 两段同步 |
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

UI 面板联动：

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

> 已知差异：63 上跑的是 `ghcr.io/rewind2026/ikev2-panel-v2:latest`（v2.89 镜像），
> Go 侧的 `deployOnlyParamList()` 新增行尚未进镜像，所以 UI「部署级参数」区
> **暂时看不到** `IKEV2_L2_IPV4`。容器 env 里该变量已正确存在，
> v2.91 镜像发布后 UI 即会显示。

### 安全约束（本次未触碰）

| 约束 | 状态 |
|------|------|
| `WriteDDNSConfig` 只能改注释 | ✅ `internal/panelstate/` 未改动 |
| `scripts/entrypoint.sh` 不改现有 `grep` 判定逻辑 | ✅ 仅在 L2 分支**新增** 3 行日志回显，判定逻辑一字未动 |
| `schema_version` 兼容老文件 | ✅ `internal/panelstate/` 未改动 |

---

## v2.90（2026-10-20）— L2 变量改用 YAML anchor：只改 compose，零 .env

**用户不需要额外准备 `.env` 了。** v2.89 的修法要求用户既编辑 compose、又写一个
`.env`，这对「拉到项目 → 编辑 compose → up」这条主路径是不合理的额外负担。

### 业界做法调研

| 做法 | 谁在用 | 特点 |
|------|--------|------|
| **YAML anchor + `x-` 字段** | Docker 官方文档《[Fragments](https://docs.docker.com/compose/compose-file/10-fragments/)》《[Extensions](https://docs.docker.com/compose/compose-file/11-extension/)》 | 定义一次，两处引用 |
| 字面量直接写死 | 多数个人项目 | 简单但不可覆盖 |
| `.env` + `${VAR:?}` | 商业/团队项目 | 需配套 `.env.example` |
| 自动探测 | Portainer / 1Panel 类平台 | 平台帮用户填 |

官方文档明确支持二者组合：*"Extensions can also be used with anchors and aliases"*。

### 方案

值在文件顶部定义一次，`environment:` 段与 `networks:` 段引用**同一个 anchor**：

```yaml
x-l2-parent:  &l2_parent  "${IKEV2_L2_PARENT:-}"
x-l2-subnet:  &l2_subnet  "${IKEV2_L2_SUBNET:-}"
x-l2-gateway: &l2_gateway "${IKEV2_L2_GATEWAY:-}"
```

用户改这三行即可，**不需要 `.env`**。anchor 里套的仍是 `${VAR:-默认}`，
所以 `.env` 路线完全不受影响（优先级 shell env > `.env` > compose 字面量）。

### 为什么比 v2.89 更好

三条，缺一不可：

1. **零额外动作** —— 用户只改 compose 顶部三行
2. **结构性消除不对称** —— 同一个 anchor 被两处引用，**不可能只改一边**。
   v2.89 的 `:?` 只是让不对称「早点报错」，并没有消除它；
   有人把 `environment:` 段改回 `:-` 去绕过报错，容器照样起得来但变量是空串。
3. **`.env` 能力不丢** —— 优先级链完整保留

### `SUBNET` / `GATEWAY` 留空是故意的

这两项每台机器都不同，没有通用默认值。写死一个「看起来能跑」的值比留空更危险 ——
容器会正常起来但网段是错的，排查成本远高于多看一眼注释。留空时
`net_mode_validate` 会明确指出缺哪一项。

### 改动文件

| 文件 | 改动 |
|------|------|
| `docker-compose.macvlan.yml` / `.ipvlan.yml` | 顶部加 `x-l2-*` anchor 块；两段引用改为 `*anchor` |
| `docker-compose.yml` | 更新注释（host 仍保持 `${...:-}`） |
| `scripts/up.sh` | 新增 `compose_get`：从 compose 顶部读 anchor 字面量作为回落，<br>否则用户在 compose 里填好了却被误报「缺少必填变量」 |
| `tests/network_mode_test.sh` | NM-57 重写为锁 anchor 三件事；**修掉 NM-49/50/51 的 pipefail 竞态** |
| `README.md` / `TROUBLESHOOTING.md` | 改为「改 compose 顶部三行」 |

### 验收

`tests/network_mode_test.sh` **132 passed / 0 failed**（连跑 10 次稳定）。
`go build` / `go vet` / `go test -count=1 ./...` 全绿。

**实机（192.168.50.63，Docker 28.5.2 / Compose v2.40.3）**：

| 场景 | 结果 |
|------|------|
| 三份 compose `config`（零 `.env`，anchor 留空） | ✅ 全部通过 |
| anchor 留空时两段是否同步为空 | ✅ `environment` 与 `networks` **同步为空** |
| 只改顶部三行 → `config` | ✅ 四条路径（env×3 + networks×3）值完全一致 |
| 只改顶部三行 → 真实 `up`（macvlan） | ✅ `running` `restarts=0`，`ip=192.168.50.2`，出网 OK |
| 只改顶部三行 → 真实 `up`（ipvlan） | ✅ 同上 |
| `.env` 覆盖 compose 顶部 | ✅ 取 `.env` 值，两段同步 |
| shell env 覆盖 `.env` | ✅ 取 shell 值 |
| 删掉 `.env` | ✅ 回落 compose 顶部字面量 |

`up.sh` 五场景：anchor 未填→正确报错 / 顶部字面量→通过 / `.env` 优先 /
裸值（无引号）→支持 / host 模式不受影响。

### 附带修复：测试里的 pipefail 竞态

NM-50 用 `code_only "$UP_SH" | grep -q` 做判据，而测试开了 `set -o pipefail`：
`grep -q` 命中即退出会让上游 `sed` 收到 SIGPIPE（141），整条管道被判失败。
`up.sh` 从 219 行涨到 284 行后，竞态从「稳定通过」变成「时好时坏」
（实测连跑 5 次得到 `FFPPF`）。改为预先算好 `$_up_code` 变量再匹配。
NM-49 / NM-51 同类判据一并修掉。

### 安全约束（本次未触碰）

| 约束 | 状态 |
|------|------|
| `WriteDDNSConfig` 只能改注释 | ✅ `internal/panelstate/` 未改动 |
| `scripts/entrypoint.sh` 不改现有 `grep` 判定逻辑 | ✅ 该文件未改动 |
| `schema_version` 兼容老文件 | ✅ `internal/panelstate/` 未改动 |

改动全在 compose / 脚本 / 文档层，**不涉及 Go 运行时逻辑**（仅版本号字符串）。

---

## v2.89（2026-10-20）— L2 模式必填变量：compose 插值语义对称化

> ⚠️ **本节已被 v2.90 取代。** v2.89 用 `${...:?}` 要求用户额外准备 `.env`，
> 对「只编辑 compose」的用户不合理。v2.90 改用 YAML anchor：用户只改 compose
> 顶部三行即可，且两条插值路径结构性不可能不一致。本节保留作为演进记录。

修一个**部署期静默故障**：容器起得来、日志反复刷「缺少必填变量」、无限重启，
但日志上完全看不出真实原因。

### 故障现象

第三方面板（1Panel / 宝塔 / dpanel）部署 ipvlan 模式时：

```
[entrypoint] ERR: IKEV2_NETWORK_MODE=ipvlan 缺少必填变量: IKEV2_L2_PARENT IKEV2_L2_SUBNET IKEV2_L2_GATEWAY
[entrypoint] FATAL: IKEV2_NETWORK_MODE=ipvlan 配置错误（详见上方 ERR）
```

`exit 12` + `restart: unless-stopped` → 无限重启，日志反复刷同一行。

### 根因：同一个变量，两条互不相干的插值路径

`IKEV2_L2_PARENT` / `IKEV2_L2_SUBNET` / `IKEV2_L2_GATEWAY` 在 compose 文件里
出现**两次**：

| 位置 | 插值结果给谁 | 是否进容器 |
|------|--------------|-----------|
| `networks:` 段的 `parent` / `subnet` / `gateway` | Docker daemon 建网 | ❌ 不进 |
| `environment:` 段的同名三项 | 容器环境变量 | ✅ 进 |

entrypoint 读的是**容器里**那一份（`os.Getenv`），不是 `networks:` 段的。

而 `environment:` 段原本写的是 `${IKEV2_L2_PARENT:-}` —— **缺失即空串**。
于是当 `networks:` 段被改成 `:-<默认值>`（面板模板常为了先让 compose 起得来
这么改）时：

1. compose 插值通过 → 容器创建成功 → 网络也建起来了
2. 容器里三个变量却是**空串**
3. entrypoint `net_mode_validate` 判为「缺少必填变量」→ `exit 12`
4. `restart: unless-stopped` → 无限重启

**为什么难查**：日志里只有「缺少必填变量」，看不出真实原因是
「变量压根没传进容器」。这个不对称只在 compose 文件被改过时才暴露 ——
用原始 compose + 完整 `.env` 部署的人永远不会碰到。

第三方面板的编排还有第二层原因：它有自己的目录和自己的 `.env` 管理，
**不读项目目录下的 `.env`**。

### 修复

**两份 L2 compose 的 `environment:` 段改用 `:?`（必填）**：

```yaml
IKEV2_L2_PARENT: "${IKEV2_L2_PARENT:?必填：宿主父接口名，如 eth0}"
IKEV2_L2_SUBNET: "${IKEV2_L2_SUBNET:?必填：宿主所在 IPv4 子网，如 203.0.113.0/24}"
IKEV2_L2_GATEWAY: "${IKEV2_L2_GATEWAY:?必填：该子网的宿主网关，必须落在上面的子网内}"
```

改完之后缺变量在 `docker compose up` 阶段**直接失败**并指名变量：

```
error while interpolating services.ikev2-panel.environment.IKEV2_L2_PARENT:
required variable IKEV2_L2_PARENT is missing a value: 必填：宿主父接口名，如 eth0
```

不用等容器起来再从刷屏日志里反推。

**host 那份（`docker-compose.yml`）保持 `:-` 不变** —— host 模式压根不用这三个
变量（entrypoint 对 host 分支只打 WARN 不拦截），写成 `:?` 会让所有 host
部署在 up 阶段就失败。

### 改动文件

| 文件 | 改动 |
|------|------|
| `docker-compose.macvlan.yml` | `environment:` 段三个 L2 变量 → `:?` + 根因注释块 |
| `docker-compose.ipvlan.yml` | 同上 |
| `docker-compose.yml` | 补注释：说明为什么本文件必须保持 `:-` |
| `tests/network_mode_test.sh` | 新增 **NM-57**：锁住「L2 两份 `:?` / host 一份 `:-`」两端 |
| `docs/TROUBLESHOOTING.md` | 新增 §1.3.2.1：第三方面板部署场景的完整排查步骤 |

### 验收

- `tests/network_mode_test.sh` → 120 passed / 0 failed（含新增 NM-57 共 9 项断言）
- 三份 compose 的 YAML 结构解析通过，`environment` 键数一致（46）
- `go build ./...` / `go vet ./...` / `go test -count=1 ./...` 全绿

### 实机验证（192.168.50.63，Docker 28.5.2 / Compose v2.40.3）

三份 compose 逐个真实 `up`：

| 模式 | 缺变量时 | 补齐变量后 |
|------|----------|------------|
| host | `config` 通过（`:-` 保持可选 ✅） | 容器起来，`/healthz` 200，面板 8443 监听正常 |
| macvlan | `up` 阶段报错并指名变量 | 容器起来，entrypoint 校验通过，拿到 `192.168.50.2/24` |
| ipvlan | `up` 阶段报错并指名变量 | 容器起来，entrypoint 校验通过，**IPv6 GUA `2408:832e:...` 经 SLAAC 拿到** |

**对照实验**（复现原故障 + 验证修复）：

| compose | `up` 阶段 | 容器状态 | entrypoint |
|---------|-----------|----------|------------|
| 旧版（两段都 `:-`） | 成功 | `Restarting (12)`，重启 8 次 | `缺少必填变量` + `FATAL` ← **用户截图的故障现场** |
| 修复后（environment 段 `:?`） | **直接报错** | 容器根本没建出来 | 走不到这一步 |

**面板设置联动**（走真实 HTTP 链路：登录 → `/api/settings/save` → `/api/settings/status`）：

- 保存后 `settings.json` 落盘（`schema_version: 1` 保持兼容），`status` API 全部项来源变 `panelstate`
- entrypoint 重启后接住：`[settings.json] applied: ... connlimit=37 hashlimit=120 family=v4`
- 非法值被服务端拦截并给出具体文案（如 `connlimit_per_ip 必须在 1 ~ 10000 之间,当前是 99999`），落盘值不变
- `session_ttl=60` 被拒（要求 `24h` 这类时长格式）、`nft_family=inet` 被拒（只接受 `v4`/`v6`）—— 服务端校验均生效
- **批次 E 优先级链路正确**：`env(88)` → 面板覆盖(`37`, `panelstate`) → 重启后 entrypoint 仍取 `37` → 清除面板设置后回退 `env(88)`

**附带发现**：63 的 `ens18` 虽为 `parentbus virtio`，但 macvlan 与 ipvlan 驱动的网络**均能正常创建并跑起容器** —— README 里记的「virtio 父接口 → device or resource busy」在这台 NAS 上不成立。该结论已实测，不是推断。

### 安全约束（本次未触碰）

- `WriteDDNSConfig` 只改注释不删除 —— `internal/panelstate/` 未改动
- `scripts/entrypoint.sh` 不改现有 `grep` 判定逻辑 —— 该文件未改动
- `schema_version` 兼容老文件 —— `internal/panelstate/` 未改动

本次改动全部在 compose 文件与测试/文档层，不涉及 Go 运行时逻辑。

---

## v2.88（2026-10-20）— UI 质感层：四条轴 + 全站面板语言统一

把面板从「配色接近 EZZON」推进到「元素语言是 EZZON」。改动全部集中在
`web/static/style.css` 的 §30 新增节与模板层，**不涉及任何后端逻辑**。

### 背景：v2.87 之后还差什么

v2.86 重设计定的 7 条 EZZON 设计意图里，落地时发现 3 条根本没兑现：

| 设计意图 | 实际情况 |
|----------|----------|
| 像素级描边 / 光泽感 | 全站 0 处 sheen，27 个 transition 全是 hover 变色 |
| 暗调蓝灰 panel grid | `--ikev2-lane-1..8` 定义了但零规则消费（死变量） |
| 顶栏 HUD 大数字区 | `.topbar-hud` / `.hud-stat` 写完了但模板零引用（死代码） |

另外 2 个 `@keyframes` 全是一次性的（入场淡入、横幅消失），零循环动画 ——
所以界面「很不动」。

### 四条轴

| 轴 | 落地位置 | 内容 |
|----|----------|------|
| 轴 1 切角 / 锐利质感 | §30.1 | 卡片、按钮、输入框、chip 统一 `clip-path` 斜切（左上 + 右下双向）；受光边 A（`::before`）+ 受光边 B（`::after` + `mask-composite: exclude`）；按钮扫光；登录卡 conic 流光 |
| 轴 2 常驻微动效 | §30.2 | 网格漂移 26s / 舞台光呼吸 8s / 噪点 12s `steps(1,end)` / 状态脉冲 / 折叠箭头 / 隧道跑光 / HUD 入场脉冲 |
| 轴 3 HUD 数字升级 | §30.3 | 关键指标改 `tabular-nums` + 侧栏竖条 |
| 轴 4 lane 轨 | §30.6 | 首页「服务就绪链路」8 格轨 |

### 轴 4：服务就绪链路

8 格绑 **8 个固定子系统**，格数恒为 8，不随在线用户数变化。排序即依赖顺序：
左半是管理面与配置依赖，右半是数据面 —— 任一格变红，它右边的格子语义上
就不该再是绿的，用户能直接读出「链路在哪断的」。

| # | 格位 | 判据 | 跳转 |
|---|------|------|------|
| 1 | 管理口令 | `IsDefaultPassword` | `/account` |
| 2 | IP 池 | `SubnetConfigured` / `SubnetCurrentIPv4` | `#subnet-card` |
| 3 | 服务端证书 | `CertMode` / `CertConfigured` | `#cert-card` |
| 4 | LE 续签 | `CertDaysLeft` / `CertLastRenewFailed` | `#cert-card` |
| 5 | 阿里云凭证 | `AliyunConfigured` | `#aliyun-card` |
| 6 | DNS 同步 | `DDNSReady` / `DDNSFailed` / `DDNSEnabled` | `#aliyun-card` |
| 7 | IKE_SA | `TotalActive` / `SAsUnavailable` | 不可点击 |
| 8 | 面板设置 | `Settings.Set*` × 8 | `#settings-card` |

四态：`ok` 就绪 / `warn` 降级可用 / `danger` 故障 / `off` 未启用。

**第 8 格的判据是「全绿才绿，否则黄」**：8 项全部被面板显式设过才给绿。
理由是「部分设过」是**正常运维状态**（用户只改了关心的那一两项），给绿色
会让人误以为「全部都由面板接管」；黄灯读作「还有 N 项在跟 env」，配 `title`
里的计数是准确信息，给绿灯则是在撒谎。计数用 `printf` 拼串 + `len` 取长
（每个 flag 只追加 1 个 ASCII 字符，串长 == 已设项数），不动 Go 代码。

取值全部来自 `homeData` 已有字段，**Go 侧零改动**。

### 全站面板语言统一（§30.7 ~ §30.9）

不再只做首页几个格子 —— banner / metric / settings-card / login-card / card /
empty-state / 顶栏 / 拓扑图全部接入同一套「面板零件」语言：

- §30.7 `.ez-panel` / `.ez-face` 零件 + 语义色统一走 `--ez-accent`
- §30.8 banner / metric / settings-card / login-card / card / empty-state
- §30.9 顶栏与拓扑图

### 排版与降级（§30.4 / §30.5）

- `.section-title` 收编所有散落的内联 `letter-spacing: 0.10em`
- **中文标签字距归零** —— 全站 computed-style 扫描后补齐 override。
  `letter-spacing` 会把 CJK 字符逐字拆散，「面板设置」被渲染成「面 板 设 置」
- 对比度提升（`--ikev2-fg-subtle` / `--ikev2-fg-strong` / `--ikev2-fg-disabled`）
- `prefers-reduced-motion: reduce` 时全部常驻动效降级为静态

### ⚠️ 修掉 / 规避的 4 个真 bug

1. **噪点层洗白深色主题** —— `feTurbulence` 输出是不透明中灰（各通道均值
   ≈0.5、alpha 也是 0.5），裸铺等于在 `#050816` 上盖 50% 灰纱。headless
   Chrome 实测平均亮度：不加噪点 `luma=14.0`（正确深色）→ 裸 feTurbulence
   `luma=84.5`（被洗白）→ 加 `feColorMatrix` `luma=24.5`（有颗粒感且仍是深色）
2. **中文标签被字距拆散** —— 同上，§30.4
3. **顶栏账号区点了没反应** —— 弹出菜单被放在 `.topbar-user-pill` 内，而 pill
   带 `clip-path`：**`clip-path` 会创建裁剪上下文，把溢出到 border-box 之外的
   后代一起裁掉** → 菜单被裁成一条线。修复：菜单移出 pill，成为 `.topbar-user-wrap`
   的兄弟节点；同时给 wrap 补 `position: relative` 作为定位上下文
4. **点 lane 快捷入口后卡片展开无法折叠** —— 早期版本在 `hashchange` 之外多听了
   `click` + `setTimeout(openTarget, 0)`，想覆盖「点同一个锚点两次」，结果
   `location.hash` 还是上一次的旧值，卡片被强行重新展开。修复：**只保留
   `hashchange`**

### banner 详情行改由宏内渲染

`Detail` / `DetailTime` 移进 banner 宏的 `.banner-body` 内部。原来它们是 banner
外面独立的 `.ddns-failed-detail`，靠 `margin: -0.5rem` 负边距硬贴在框边上 ——
实测文字裸露在切角外、一条条 banner 糊成一片。

安全边界不变：`Detail` 走 `{{.Detail}}` 默认转义（**不**经过 `safeHTML`），
可安全承载外部输入（阿里云 API 报错原文）。

### DDNS 兜底文案纠偏

`handlers_home.go` 兜底分支原文写「详见 `/var/log/charon.log`」—— **指错日志了**。
`charon.log` 是 strongSwan IKE 进程的日志，DDNS 同步是面板自己的 goroutine 跑的，
原因只可能在面板日志里。同时原文说「最近一次同步失败」，但该分支的前提正是
「本次进程压根没同步过」，真实语义是「上次运行失败，且本次尚未重试」。
新文案：`上次运行同步失败，本次进程尚未开始重试；具体原因见面板日志（docker compose logs ikev2-panel）`

### 安全约束（本次未触碰）

- `WriteDDNSConfig` **只改注释不删除** —— 本次未改动 `internal/panelstate/`
- `scripts/entrypoint.sh` **不改现有 `grep` 判定逻辑** —— 本次未改动该文件
- `schema_version` 兼容老文件 —— 本次未改动 `internal/panelstate/`

### 验收

- `go build ./...` / `go vet ./...` / `go test -count=1 ./...` 全绿
- 新增 `internal/web/v288_topbar_menu_test.go`：5 个防回归测试
  （菜单不在被裁剪的 pill 内 / wrap 是定位上下文 / 账号区整块可点 /
  真实模板渲染结构 / 真实 HTTP 链路走通 `/account` + `/logout`）
- 真实浏览器实测：登录页 / 首页 lane 轨 8 格数据 / lane 点击跳转 / summary
  折叠不回归 / 首屏带 hash 直接落地 / 面板设置前端校验拦截非法值 /
  合法值保存并持久化 / 控制台零报错
- 15 个新增 CSS 变量存在性校验

---

## v2.87（2026-10-20）— 配置统一：面板设置页 + 前端校验 + entrypoint 顺序修复

把原先只能手改 `.env` / compose 的配置项搬进面板。三批改动：批次 D（17 项已有项
可视化）、批次 E（8 项部署级参数）、以及本轮实机验收中发现并修掉的 3 个 bug。

### 核心：配置优先级链

```
panelstate(/data/panel-state/settings.json) > env(IKEV2_*) > 代码 factory 默认值
```

**三态语义**：`key` 缺失 / 指针 `nil` = 跟随 env；`key` 存在且为 `false`/`0` =
面板显式覆盖。不使用 `null`（无法区分「未设置」与「显式清空」），全部字段带
`omitempty`。`schema_version` 兼容老文件 —— 旧配置不会因本次升级突然失效。

### 批次 D — 17 项已有 env 参数进面板

`internal/panelstate/settings.go` 是唯一真相源：字段 + 常量 + `Validate()`。
`internal/runtime.Merge()` 负责 env → 覆盖值的合并。

### 批次 E — 8 项新参数

`log_level` / `log_format` / `audit_retention_days` / `connlimit_per_ip` /
`hashlimit_per_min` / `disable_mss_clamp` / `disable_ddos_protection` / `nft_family`

**生效方式分两类**（`log_level` 用 `slog.LevelVar` 运行期立即生效，其余需重启）：

| 字段 | 生效 | 原因 |
|------|------|------|
| `log_level` | 运行期立即 | `slog.LevelVar` + `config.SetLogLevel` |
| `log_format` | 需重启 | slog handler 类型启动时定死 |
| `audit_retention_days` | 需重启 | entrypoint 写 cron |
| `connlimit_per_ip` / `hashlimit_per_min` | 需重启 | entrypoint 写 iptables/nft |
| `disable_mss_clamp` | 需重启 | entrypoint §4 |
| `disable_ddos_protection` | 需重启 | entrypoint §5 |
| `nft_family` | 需重启 | entrypoint §4.5 |

其余 17 项部署级参数（网卡 / 数据目录 / 监听端口等）只读展示在
「部署级参数（只读）」折叠区 —— 运行时改它们要么不生效，要么直接把面板改成
连不上自己，所以只展示当前值并指向 `.env`。

### 前端校验

三层拦截，任何一层都不依赖「浏览器恰好支持某特性」：

1. **模板** `data-val-int` / `min` / `max` / `data-val-enum` / `data-val-bool`，
   上下界从 `.Settings.ConnLimitMin` 等常量渲染，与后端同源，不在 JS 里写死
2. **`initSettingsValidation()`**（`layout.html`，唯一 nonce 脚本块）——
   `submit` capture 阶段逐项校验，有错 `preventDefault()` + 聚焦首个错误字段
3. **服务端** `optInt` 对非整数直接返回 error → 400，整张表单不落盘

表单刻意加 `novalidate`：「留空 = 跟随 env」是合法状态，且原生气泡文案随浏览器
语言走。另有回归测试钉住「空串枚举必须放行」和「hidden 兜底必须排在 checkbox
前面」两个易踩的坑。

### ⚠️ 修掉的 3 个 bug

**1. entrypoint 启动清理排在加规则之后（最严重）**

原顺序是 §4.6 `apply_ddos_protection` 先加 `connlimit` / `hashlimit` 规则，
§4.7.1 启动清理再**按同样的 comment 锚点**把规则删掉。表现是 entrypoint 日志
打印 `connlimit (UDP/500, DOCKER-USER) added`，重启后 `DOCKER-USER` 链却是空的
—— 面板的 `connlimit_per_ip` / `hashlimit_per_min` **保存后完全不生效**。

两段用同一个锚点，所以顺序错了自己不会报错。修复：把清理前移到 §4.5c，
函数定义与调用顺序同步调整，退出时 trap 留在 §4.7 保持对称。
回归保护见 `internal/panelstate/entrypoint_order_test.go`。

**2. `optInt` 静默吞掉非法值**

原实现解析失败时返回 `nil`（= 跟随 env）而不报错，非法输入被当「没填」静默丢弃。
改为返回 error → 400。

**3. 部署级参数表头压住第一行**

全局 `thead th` 有 `position:sticky; top:var(--ikev2-topbar-h)`（48px），本意是
让主列表表头吸顶。但 `.deploy-only-table-wrap` 带 `overflow-x:auto` —— 一个
`auto` 的 X 轴会把 Y 轴也计算成 `auto`（CSS 规范行为），wrapper 成了滚动容器，
表头改为相对它定位、被往下推 48px，正好压在第一行 `IKEV2_NETWORK_MODE` 上。

修复：`.deploy-only-table th` 显式 `position:static`。

### 验收

`192.168.50.63` 实机，**host / macvlan / ipvlan 三种网络模式 × 8 项参数**全量
真实浏览器测试：修改 → 保存 → 生效。macvlan/ipvlan 的限速规则落容器自己的
`INPUT` 链（host 模式落宿主 `DOCKER-USER`），三种模式均实测到阈值生效。

三种 compose（`docker-compose.yml` / `.macvlan.yml` / `.ipvlan.yml`）的
`image` / `capabilities` / `volumes` / `tmpfs` / `environment` 一致性由
`tests/network_mode_test.sh`（NM-30~NM-36）强制校验。

---

## v2.86-PR20（2026-10-16）— 网络模式抽象：host / macvlan / ipvlan

在只有 host 的基础上增加 **macvlan** 与 **ipvlan** 两种 L2 独立 IP 模式，
解决 host 覆盖不到的场景：宿主上 500/4500/8443 已被别的服务占用、需要多实例、
需要容器在二层上有独立身份。

**host 模式零改动兼容** —— 不改 `.env`、不改 `docker-compose.yml` 的
`network_mode: host`，`docker compose up -d` 与改造前逐字节一致。

### 设计：三个独立完整的 compose 文件 + 一张模式规格表

| 文件 | 角色 |
|------|------|
| `docker-compose.yml` | **host 模式**（用默认文件名，老用户 `docker compose up -d` 零改动） |
| `docker-compose.macvlan.yml` | **macvlan 模式**，完整独立 |
| `docker-compose.ipvlan.yml` | **ipvlan 模式**，完整独立 |
| `scripts/network-mode.sh` | **网络模式规格表**（唯一数据源），entrypoint source |
| `scripts/up.sh` | 分发封装：读 `IKEV2_NETWORK_MODE` 选文件 + 建 `./logs` |

**三个文件都是完整的，不做叠加**。这意味着：

- ✅ 不需要 `!reset` / `!override` 标签 → **任何 Docker Compose 版本都能用，
  不设版本下限**（早期设计用 override 时要求 ≥ 2.24.4，已取消）
- ✅ 用户可以直接用 docker 官方命令，不需要经过 `up.sh`
- ⚠️ 代价：三份服务定义（image / capabilities / volumes / tmpfs / environment）
  必须保持一致，**改一处漏两处**是唯一的结构性风险

针对最后这条，`tests/network_mode_test.sh` 的 **NM-29~NM-36** 强制校验三份一致：

| 用例 | 校验内容 | 漏掉的后果 |
|------|---------|-----------|
| NM-29 | image / container_name / restart / pull_policy | 版本漂移 |
| NM-30 | cap_add / cap_drop / devices / security_opt | 缺 FOWNER → chmod 失败 → 无限重启 |
| NM-31 | 顶层卷名锁定 `ikev2-panel-data`、无 `external:` | **静默新建空卷 → panel.db / LE 证书全丢** |
| NM-32 | 三条挂载（`/data`、`./logs`、`/host-sysctl.d`） | 数据或日志不落盘 |
| NM-33 | tmpfs 三份一致 | 容器内进程写不进去 → 500 |
| NM-34 | environment 键集合三份一致 | 某个模式下变量没透传 |
| NM-35 | 14 个关键变量三份都在 | 同上 |
| NM-36 | `IKEV2_NETWORK_MODE` **硬编码**且等于文件自身模式 | `.env` 覆盖 → 声明与实际 netns 不一致 |

NM-31 尤其重要：`v2-72` 和 `PR19` 已各真实踩过一次「静默建空卷、panel.db 与
LE 证书消失但面板照常显示」的坑，注释里还留着事故记录。这三份文件必须由测试守住。

### 三模式的差异（全部收在 network-mode.sh）

entrypoint 只 source `network-mode.sh`，里面定义能力开关（用返回码而非全局
变量，规避 `set -euo pipefail`）、`net_mode_validate()` 全量校验、以及
Docker 侧的差异（各 compose 文件的 networks 段）。

| 关注点 | host | macvlan / ipvlan |
|--------|------|------------------|
| `IKEV2_OUT_IF` 语义 | 宿主网卡名 | **容器网卡名**（通常 `eth0`） |
| DDoS 规则链 | `DOCKER-USER` | 容器私有 netns 内的 `INPUT` |
| forwarding 持久化 | 写宿主 `/host-sysctl.d/99-ikev2.conf` | **不写**（per-netns，写宿主无效） |
| IPv6 地址收敛 | 单次探测 | 3 次 × 2s 重试（等 DAD） |

### ⚠️ 行为变更

1. **L2 模式下 `IKEV2_OUT_IF` 的语义变成「容器网卡名」**。沿用宿主网卡名
   （如 `ens18`）会 **FATAL `exit 14`**：该值在容器 netns 内不存在，
   nft 模板会生成一条永不匹配的 `oifname` 规则 → MASQUERADE 静默失效 →
   客户端能连 VPN 但出不了网。这是本次改造唯一「启动成功但功能不可用」的坑，
   所以选择 FATAL 而非 WARN。**L2 模式下建议删掉这一行让脚本自动探测。**
2. **L2 模式不再写宿主的 `/etc/sysctl.d/99-ikev2.conf`**。forwarding 是
   per-netns 的，写宿主对容器那个 netns 毫无作用，只会留下误导性配置。
3. **L2 模式的 DDoS 防护链从 `DOCKER-USER` 改为容器自己的 `INPUT`** ——
   L2 模式容器是私有 netns，里面没有 `DOCKER-USER` 链，沿用会静默跳过防护。
4. **换网络模式 = 换 compose 文件**，且必须重建容器。
5. **`IKEV2_NETWORK_MODE` 在每份 compose 文件里硬编码为自身模式，不接受 `.env` 覆盖。**
   网络由文件决定，变量只用于 entrypoint 内部的行为分支。早期用
   `${IKEV2_NETWORK_MODE:-macvlan}` 有洞：`.env` 里残留的 `IKEV2_NETWORK_MODE=host`
   会覆盖默认值，造成「容器在 macvlan netns 里、entrypoint 按 host 分支跑」——
   DDoS 去找不存在的 `DOCKER-USER` 链（静默跳过防护）、`persist_sysctl` 误写宿主
   sysctl.d、L2 的 SLAAC 运行时复核整个被跳过。NM-36 专门守这一条。
6. **L2 模式下公网 IPv6 探测改为 3 次 × 2s 有限重试**（等 DAD 完成），
   host 模式仍是单次，行为不变。
7. `.env` 里已有 `IKEV2_NETWORK_MODE=bridge` 的用户不受影响 —— `bridge`
   仍会被 FATAL 拒绝（v2.86-PR14 起已移除），只是报错文案更新。

### 安全设计：把静默故障提前变成启动失败

L2 模式引入了两处「能启动成功但功能不可用」的路径，都加了硬拦截：

| 检查 | 失败行为 | 拦截的理由 |
|------|---------|-----------|
| `IKEV2_OUT_IF` 在容器 netns 内是否存在 | FATAL `exit 14` | 不校验会退化成 MASQUERADE 静默失效 |
| L2 三条 sysctl 运行时回读（v4/v6 forwarding + `accept_ra`） | FATAL `exit 10` | 不复核则 sysctl 被静默忽略时 v4 正常、v6 永远拿不到 |

`accept_ra=2` 是关键：内核的功能性默认是「forwarding 关闭时接受 RA、开启时不接受」，
而本项目必开 forwarding，不显式覆盖就会丢掉所有 RA。
所需配置在 L2 compose 文件里静态注入 —— 容器内 `/proc/sys` 是 ro，entrypoint 改不了。

原设计中的 `IKEV2_L2_IPV6_SYSCTL` 开关被**删除**：compose 的配置是静态的，
容器内也无法事后改这些 sysctl，这个开关只会制造「以为关掉了」的错觉。
改为「始终强制注入 + 运行时回读复核」。

### 实机验证修正（Docker 28.5.2 / Compose v2.40.3，192.168.50.63）

在真实二层环境（parent=`ens18`，`192.168.50.0/24`，上游发 RA）验证后，
修正了三处**静态审查发现不了**的问题：

1. **sysctl 注入通道从 networks 段改为 service 段 `sysctls:`**
   原方案的 `networks.ikev2-l2.options.com.docker.network.endpoint.sysctls`
   有两个独立问题：
   - `networks.<name>.options` **不是合法 Compose 字段**，
     `docker compose config` 直接报 `additional properties 'options' not allowed`，
     两个 L2 文件都起不来；
   - 即便绕过 Compose 直接用 `docker network create -o`，该 driver option
     在 macvlan/ipvlan 上实测**完全不生效**（容器内 `v6fwd=0 ra=1 gua=0`）。
   改为 service 段 `sysctls:` 后实测 `v4fwd=1 v6fwd=1 ra=2 gua=1`。
   逐条失败现象见 architecture.md §12.8.2。
   之前注释里「service 段 sysctls 会被 runc 拒绝」的说法**只对 host 模式成立**
   （host 模式共享 netns），已更正。

2. **新增 `networks.ikev2-l2.enable_ipv6: true`**
   不开的话 Docker 建端点时把 `disable_ipv6` 置 1，接口上收不到任何 RA
   （实测 `dis6=1 gua=0`）。且这个值**无法**用 service 段 sysctls 改回来。

3. **SLAAC 等待时长从 3 次 × 2s 改为 6 次 × 3s（最多 15s）**
   原值最多等 4 秒，实测 GUA 收敛需要 2~5 秒（上游 RA 周期 + DAD），
   4 秒不够，会间歇性 `exit 10`。

另修复两个 entrypoint / 脚本缺陷：

- **SLAAC 复核的子 shell 陷阱**：用 `net_mode_sysctl_checks | while read`
  会把循环体放进子 shell，循环内的赋值传不回来 ——
  「有不符合项」会被**静默当成通过**。改为 here-doc 重定向。
- **`up.sh --no-detect` 会导致启动失败**：脚本声称该参数是 no-op，
  但只 warn 没剔除，参数一路透传给 `docker compose up`，
  Docker Compose v2.40.3 直接报 `unknown flag: --no-detect`。
  现在真正消费掉该参数；其余参数用数组透传，避免引号污染。

### 实机验证结果

| 验证项 | 结果 |
|--------|------|
| `docker compose config` × 3 份文件 | 全部 rc=0 |
| 镜像构建（Dockerfile 含 network-mode.sh 检查） | 成功 |
| host 模式启动 | 正常，500/4500/8443 占用，`RestartCount=0`，FOWNER 已含 |
| macvlan 全链路 | 正常，GUA 3s 收敛，v4/v6 forwarding=1，`RestartCount=0` |
| ipvlan | 配置正确，但测试宿主 `ens18` 是 virtio 网卡，macvlan/ipvlan 均报 `device or resource busy`（同接口 macvlan 亦失败；改用 `docker0` 作 parent 时 ipvlan 正常）→ 属宿主限制，非配置问题 |
| 退出码 12（7 个非法配置用例） | 全部正确拦截 |
| 退出码 14（`IKEV2_OUT_IF` 不存在） | 正确拦截 |
| `up.sh --no-detect` / 参数透传（6 用例） | 全部正确 |

### 其他

- 删除死变量 `IKEV2_ACTUAL_NETWORK_MODE`，改为真正写
  `/run/ikev2-panel/netmode.env`（MODE / OUT_IF / DRIVER / HAS_OWN_IP /
  PROCSYS_RW / DDOS_CHAIN 六个键），供 `docker exec` 排查。
- `up.sh` 新增 `--help`，并把实际执行的 `docker compose` 命令原样打出来。
- entrypoint 缺失 `network-mode.sh` 时 FATAL `exit 12`（镜像构建不完整）。
- 退出码语义扩展：`12` 覆盖全量模式校验，`14` 覆盖 L2 模式的网卡不存在，
  `10` 覆盖 L2 模式的 SLAAC 复核。原有语义不变。
- 文档：architecture.md §11.1 改写为三模式、新增 §11.1.1 规格表说明与
  §12.8.1 SLAAC 链路、§11.2 补 `FOWNER`、§11.4 补退出码；
  TROUBLESHOOTING.md §1.3 拆为四个子节、新增 §1.7 L2 专项；
  README.md 补三模式部署章节与 L2 前置条件。

### 文档补漏（virtio 限制 + 限速作用域）

补齐两处此前只在 CHANGELOG 里提过、但用户查不到的限制：

| 缺口 | 补在哪 |
|------|-------|
| **父接口是 virtio 时 macvlan / ipvlan 都起不来** | `README.md` L2 前置条件表新增一行 + `ip -d link show` / `docker network create` 两条自查命令；`TROUBLESHOOTING.md` 新增 §1.7.6 专节（含 docker0 对照实验表、解法优先级） |
| **L2 模式下 tc 限速作用域在容器 netns 内** | `README.md` 限速章节补充：宿主上 `tc qdisc show dev eth0` 查不到是正常的，排查一律用 `docker compose exec` |

同时区分了两个容易混淆的报错：`device or resource busy`（父接口 virtio）
与 `could not find an available, non-overlapping IPv4 address pool`
（`IKEV2_L2_SUBNET` 与已有 Docker 网段重叠）。

**仍未验证**：客户端连上 VPN 后经 IPv6 出口的**完整数据面出网**（已验证 GUA 获取、
forwarding、SLAAC 复核，但未跑端到端 iperf）；macvlan 与 ipvlan 的
**多用户并发丢包率对比**（§9.4 P-06）。后者需要一台非 virtio 父接口的宿主，
当前测试机的 `ens18` 是 virtio（见上表）。

**已验证**：见上方「实机验证结果」表 —— host 与 macvlan 两条路径已端到端跑通，
单元测试 80/80，4 个文件语法检查通过。后续审查发现的缺陷与修复见下方
[PR20 审查修复](#v286-pr20-审查修复2026-10-19)。

---

## v2.86-PR20 审查修复（2026-10-19）

对 PR20 的三模式实现做了一次完整代码审查（合理性 / 可复用性 / 可优化空间）。
结论是**设计成立、实现有洞**：三模式共用一张规格表、compose 显式内联、
sysctl 注入通道这三项判断都站得住；但探测链和失败处理上有 4 个 P0、
3 个 P1，外加 `up.sh` 维护了第二份校验实现。

这 4 个 P0 有个共同点：**症状都是"启动成功、面板全绿、日志里没有任何异常"**，
只能靠逐个功能试出来 —— 这也是它们此前一直没被发现的原因。

### ⚠️ 行为变更

| 变更 | 之前 | 之后 |
|------|------|------|
| **公网 IPv6 探测** | 从 `/proc/net/if_inet6` 取**每张网卡的第一条** `2000::/3` 地址，不看 DAD 状态 | 从 `ip -6 addr show` 取**任一** DAD 已收敛的 `2000::/3` 地址 |
| **运行时 IPv6 监控**（`ipv6watch` / DDNS） | 同样把 scope 当 flags，`tentative`/`dadfailed` 地址会被选中 | 按位掩码过滤，只接受 DAD 已收敛的地址 |
| **`IKEV2_OUT_IF` 下发** | 只存在于 entrypoint 局部变量，**从不 export** | `export`，容器内三个下游消费者都能读到 |
| **L2 网段校验** | 不查每段是否 ≤255，也不拒前导 0 | `_nm_valid_octet` 严格 0-255 且无前导 0 |
| **nft 规则校验** | 无条件执行，最多等 260 秒 | 仅在 §3.5 真的装了表时才执行 |
| **纯 IPv6 主机启动** | 卡 260 秒后写 `iptables-warn.flag`（假告警） | 打印一行 `not applicable ... skipping` 后继续 |

前三条会让**原本"看起来正常"的实例变成真的能用**：限速套错网卡、DDNS 指错接口、
把未收敛地址写进 `local_addrs` 这类问题此前日志里没有任何异常，只能靠逐个功能试出来。

### P0-1 — `detect_public_ipv6` 有两个独立缺陷

**缺陷一：把 scope 当成了 flags。** 原实现判 `$4=="01" || $4=="00"`，
而 `/proc/net/if_inet6` 的 `$4` 是 scope（`01`=loopback / `00`=global），
flags 在 **`$5`**。所以 `tentative`（DAD 未完成）和 `dadfailed`（DAD 冲突）
的地址从未被排除 —— 容器刚启动时会把一个**还没确认可用**的地址交给 charon。

**缺陷二（本轮新发现，影响更大）：每张网卡只取第一条地址。** 内层
`awk ... {print $1; exit}` 的 `exit` 只拿网卡上的第一条。而 macvlan/ipvlan 的
容器 `eth0` 上，Docker 分配的 ULA（`fd48:...`）**必然先于** SLAAC 获得的 GUA 出现：

```
内层取到 fd48:...  →  外层 case 2xxx: 匹配不上 → 跳过
                  →  外层下一轮仍喂同一网卡名 → 又取到同一条 ULA
                  →  永远走不到 GUA，函数返回空
```

也就是说 **L2 模式下公网 IPv6 探测恒返回空** —— 而 L2 恰恰是最需要它的模式。
实测对照（左 = 原实现，右 = 修复后）：

| 场景 | 原实现 | 修复后 |
|------|--------|--------|
| L2：ULA 排在 GUA 之前 | `∅`（返回空） | `2408:832e:8a5:be0:...` ✅ |
| L2：GUA 处于 `tentative` | 返回未收敛的地址 ❌ | `∅`（继续等） ✅ |
| L2：GUA 处于 `dadfailed` | 返回冲突地址 ❌ | `∅` ✅ |
| host：稳定 GUA | 正常 | 正常（不变） ✅ |
| host：GUA 已 `deprecated` | 返回过期地址 ❌ | `∅` ✅ |

**修复**是改用 `ip -6 addr show`，一次解决两个问题：它按**地址**逐行输出，
不存在"每网卡取第一条"；并且输出里带 `tentative` / `dadfailed` / `deprecated`
**文字标签**，直接匹配即可。

> 刻意用文字标签而不是硬编码 `$5` 的位掩码（`0x20`=deprecated、
> `0x40`=tentative、`0x80`=home/valid 等）—— 内核位含义可能随版本演进，
> 文字标签才是稳定契约。`nodad`（跳过 DAD）是稳定态，**应当接受**。

顺带修正 `2000::/3` 的上界：此前只匹配 `2xxx:`，漏掉 `3000::`–`3fff::`。

### P0-2 — 探测到的 `OUT_IF` 从未 export，下游全部退回默认值

§0.1 探测出网卡名后只写进了自己的局部变量，**没有 `export IKEV2_OUT_IF`**。
三个下游消费者因此一直读各自的硬编码默认值：

| 消费者 | 位置 | 此前实际用的值 |
|--------|------|----------------|
| `ikev2-updown` 的 `tc` 限速 | `scripts/ikev2-updown` | `IKEV2_OUT_IF:-eth0` → 恒为 `eth0` |
| DDNS `Iface` | `cmd/ikev2-panel/main.go` | `os.Getenv(...)` → 空 |
| `ipv6watch` `Iface` | `cmd/ikev2-panel/main.go` | 同上 |
| `config.OutIf` | `internal/config/config.go` | `getenv(..., "eth0")` → `eth0` |

host 模式下 `eth0` 恰好常常是对的（容器网卡就叫 eth0），所以**没人发现**；
macvlan/ipvlan 模式下容器私有没有别的接口名可猜，症状是"限速好像没生效"、
"DDNS 不更新"，且**日志里没有任何异常**。

补上 `export IKEV2_OUT_IF="$OUT_IF"` 后，这条链路上容器内外读到同一个值。

### P0-3 — `ipv6watch` 把 scope 当成 flags（与 P0-1 同型）

`internal/swanctl/ipv6watch.go` 的 `detectGlobalV6` 里有和 P0-1 **完全同型**的
字段索引错误，但发生在另一处 —— 而且这次影响的是**运行时**：

```go
addr32 := fields[0]
dev    := fields[5]
flags  := fields[3]   // ← 这是 scope，flags 在 fields[4]
if flags == "10" { continue }
```

`/proc/net/if_inet6` 的第 4 字段是 **scope**（`00`=global / `20`=link / `10`=compat），
第 5 字段才是 **flags**。本机实测：

| addr | scope | flags | dev |
|------|-------|-------|-----|
| `::1` | `10` | `80` | lo |
| `2408:...` | `00` | `00` | ens18 |
| `fe80::...` | `20` | `80` | ens18 |
| `2001:db8:...` | `00` | `c0` | veth-probe1 |

所以原代码有两重失效：

1. **`tentative` / `dadfailed` 从未被排除** —— 注释说"忽略 deprecated (0x10)"
   实际是在拿 scope 跟字符串 `"10"` 比，flags 为 `0x10` 的 `deprecated` 地址
   （scope 是 `00`）照样被选中。
2. **scope 为 `10` 的地址被误杀** —— 只有 loopback 的 `::1` 命中，纯属巧合正确。

后果落在 `ipv6watch` 与 DDNS 两处：ISP 重拨前缀后，本可能拿到一个**尚未 DAD 收敛**
的地址写进 `local_addrs`，客户端连得上却拿不到地址，且**日志里没有任何异常**。
DDNS 侧同理，会把一个不可用地址同步到 DNS。

**修复**：把解析抽成纯函数 `parseIfInet6Line`，按位掩码过滤
（`DADFAILED 0x08` / `DEPRECATED 0x10` / `TENTATIVE 0x40`），并保留
`TEMPORARY 0x20`（隐私地址是稳定态，应当接受）。抽成纯函数是为了能脱离真实网卡
做单元测试 —— 状态过滤是这个函数的全部价值，而它恰好是最容易写错索引的地方。

顺带修正 `isGlobalV6`：只判首字符 `2`，漏掉 `3xxx::`–`3fff::` 整段
（与 P0-1 同一个上界问题）。

### P0-4 — L2 网段校验放过超范围 IPv4

`network-mode.sh` 的 `_nm_valid_ipv4_cidr` 只查字符集与段数，**不查每段是否 ≤255**，
于是 `IKEV2_L2_SUBNET=999.0.113.0/24` 能一路通过校验，直到 Docker 建网段才失败 ——
而那时的报错完全指不到 `.env` 的哪一行。

更隐蔽的是**前导 0**：POSIX sh 的 `$((010))` 按**八进制**求值得 8，
Docker 侧按**十进制**得 10。同一份配置在两处算出不同的网络，且**不报错**，
只表现为"网关莫名其妙不在子网内"。现在 `_nm_valid_octet` 显式拒绝前导 0
（Go 1.17+ 的 `net.ParseIP` 同样拒绝，两边判定一致），早拒好过让用户拿到
一条语焉不详的 docker 报错。

### P1 三项一致性

| 编号 | 问题 | 处理 |
|------|------|------|
| P1-1 | §7.9 规则完整性校验**无条件**执行，但表只在 `IPV6_ONLY=true` 且有 `V4_GW` 时才装 | §3.5 装表时置 `NFT_EXPECTED=1`，§7.9 据此守卫。真正适用的路径重试行为与上限**完全不变** |
| P1-2 | `net_mode_out_if_is_host()` **零生产调用**，且语义与 `net_mode_has_docker_user()` 完全相同 | 直接删除（连带 NM-20），模式差异改由 §0.1 的存在性校验 + `export` 统一处理 |
| P1-3 | `sed -i "s\|%IKEV2_OUT_IF%\|...\|g"` —— 模板里**从来没有**这个占位符（grep 命中 0 次） | 删除空转 sed，补注释说明 `OUT_IF` 走环境变量而非模板占位符 |

P1-1 修掉的是纯 IPv6 主机上"启动卡 4 分钟 + 面板 banner 报『客户端无法访问
公网』"的假告警 —— 不是规则不稳定，是那条路径压根不适用。

### `up.sh` 复用 `network-mode.sh`：消除第二份校验实现

`up.sh` 原先自带一份 `require_l2_env()`，与 `net_mode_validate()` 是同一件事的
两份实现。规则一改就漂移 —— `net_mode_validate` 后加的 `PARENT != lo`、
`GATEWAY` 必须落在 `SUBNET` 内、`macvlan_mode=passthru` 拦截，`up.sh` 全都不知道，
只能等容器起来才 `exit 12`。现在 `up.sh` 在 `docker compose` **之前**就能报出与
容器内完全一致的错误。

沙箱行为测试 20/20 通过，其中三项是旧版会静默放行、新版能提前拦下的：

| 场景 | 旧 `up.sh` | 新 `up.sh` |
|------|-----------|-----------|
| gateway 不在 subnet 内 | 放行 → 容器 `exit 12` | 启动前 ERR |
| `parent=lo` | 放行 → 容器 `exit 12` | 启动前 ERR |
| `macvlan_mode=passthru` | 放行 → 容器 `exit 12` | 启动前 ERR |

细节：`network-mode.sh` 是纯 POSIX sh、零副作用，直接 `.` source；
模式专属开关（`IKEV2_MACVLAN_MODE` / `IKEV2_IPVLAN_MODE` / `IKEV2_IPVLAN_FLAG`）
只读取不 `export`，避免 `env_get` 与 Compose 自身 `.env` 解析的差异污染插值。

### 测试：80 → 111

新增 **NM-40~NM-56**。两条关键设计：

**entrypoint 无法整体 source**（1200+ 行 + 大量顶层副作用），所以测试用 `awk`
**原样抠出真实函数**，再用覆盖 `PATH` 的假 `ip` 喂 fixture —— 测的是**生产代码本身**：

```sh
DP6_SRC="$(awk '/^detect_public_ipv6\(\) \{/{f=1} f{print} f&&/^\}$/{exit}' "$ENTRY_SH")"
```

**`ipv6watch` 同理**：把解析从 `detectGlobalV6`（依赖真实网卡）里抽成纯函数
`parseIfInet6Line`，测试直接喂 `/proc/net/if_inet6` 的原始行文本。
样本的 dev/scope/flags 三列取自本机真实文件，地址的 hex→冒号展开经 `cut`
逐 4 字符核对（第一版测试期望是我手算的，结果算错了两处，被机械核对抓出来）。

| 用例 | 覆盖 |
|------|------|
| NM-41~47 | `tentative` / `dadfailed` / `deprecated` 被跳过；`nodad` / `dynamic` 被接受；`3fff::` 上界；link-local / ULA / loopback 排除 |
| NM-48 | entrypoint 存在 `export IKEV2_OUT_IF="$OUT_IF"` |
| NM-49 | 模板无 `%IKEV2_OUT_IF%`，entrypoint 无对应死 `sed` |
| NM-50~51 | `up.sh` source `network-mode.sh`、调用 `net_mode_validate`、已删除 `require_l2_env` |
| NM-52 | **ULA 排在 GUA 前面时不遮蔽 GUA**（缺陷二的回归防护） |
| NM-53~54 | `_nm_valid_ipv4` / `_nm_valid_ipv4_cidr` 的段数、≤255、前导 0 |
| NM-55~56 | 超范围/前导 0 的子网必须由 **SUBNET 校验**拒下 |
| Go: `TestParseIfInet6Line_DADFlags` | 12 条：稳定/temporary 接受，tentative/dadfailed/deprecated/link-local/loopback/ULA/网卡不匹配/字段不足/非 hex 全拒 |
| Go: `TestParseIfInet6Line_SkipsTentativeAndFindsStable` | 逐行扫描时跳过不可用状态继续找 |

**守护必须匹配具体报错文案，不能只 grep 变量名**。NM-55/56 一开始写成
`grep -q 'IKEV2_L2_SUBNET'`，结果网关那条报错
（`GATEWAY 不在 IKEV2_L2_SUBNET 内`）里也含这个变量名 —— 变异测试里
两道关全被拆掉，用例照样通过。改成 `grep -q 'SUBNET=.*不是合法 IPv4 CIDR'`
后，变异才会如实失败。同理 NM-49/50/51 只扫**可执行代码**
（`code_only()` 先剥掉注释行），否则本文件里解释"为什么删掉那行 sed"的
注释会让自己失败。

**变异测试**验证过这些守护不是空跑：

| 变异 | 失败数 |
|------|--------|
| `_nm_valid_ipv4` 退化成 `return 0` | 11 |
| `_nm_valid_octet` 放开前导 0 | 3 |
| `detect_public_ipv6` 还原为真实原始实现 | 7 |

### 文档

- architecture.md §11.1 —— "唯一静默故障"的表述已修正。原写法只提存在性校验，
  会让人以为 `OUT_IF` 语义对了就没事；实际 P0-2 证明**不 export 更严重**。
- architecture.md 新增 §12.8.3 —— nft 规则校验的 `NFT_EXPECTED` 守卫。
- architecture.md §10 新增"§0.2 为什么要按 DAD 状态过滤" —— 记录
  `/proc/net/if_inet6` 的真实字段布局与两个缺陷，避免后人重写回错版本。
- architecture.md §11.1.1 —— 函数表删除 `net_mode_out_if_is_host`，
  补 `up.sh` 复用说明；§1 / §11.1 的重试参数同步为 `6 次 × 3s`。

### 本轮明确不改的

无。上一版这里记的两处（`ipv6watch` 的 `fields[3]` 索引错误、
`_nm_valid_ipv4_cidr` 不校验 ≤255）已在本轮作为 P0-3 / P0-4 修复。

唯一保留的已知取舍：`ipv6watch` 的行解析仍走 `/proc/net/if_inet6` 而不是 netlink。
这是刻意的 —— netlink 要引入 `golang.org/x/sys/unix` 依赖，而
`/proc/net/if_inet6` 是内核长期稳定的 ABI，且与 entrypoint §0.2 的修复前
实现同源，改一处只需记一次。

---

## v2.86-pr24（2026-10-13）— 前端文本选择策略 + eye 按钮按压错位 + 文档体系重组

`v2.86-pr23p` 之后攒了 7 个提交，本次一并定版。范围跨三块：一次全站 UI 审计的收尾
（P0/P1）、两处真实 UI 缺陷修复、文档体系从 52 份重组为 5 份。

提交构成（6 个 commit）：

| 提交 | 内容 |
|------|------|
| `4c13c0a` | docs: 文档体系 52 份 → 5 份，按「一个问题一个权威出处」重组 |
| `81a6b50` | fix: 60 条风险复核后的 A/B 类修复（pr24 落地部分） |
| `9b4d059` | fix(web): P0 二次确认失效 + banner 转义错位 + 表单裸文本页 |
| `ff342ec` | feat(web): 文本选择策略 —— 界面装饰禁选，数据可复制 |
| `ca92df5` | fix(web): eye 切换按钮按压错位 —— transform 定位被全局动效覆盖 |
| `a09da97` | docs: 数据卷说法纠偏 + 记录前端两条易踩约定 |

### pr24-a — 文本选择策略：装饰禁选，数据可复制

起因是用户反馈「所有文字都可以复制，不符合常规 UI 设计理念」。核实后确认属实：
全站除 3 处零散规则外没有任何 `user-select` 约束，双击 / 拖拽会误选一片界面文字。

**没有全局禁选。** 这个面板里大量内容**必须**能复制：初始密码、服务器地址、
swanctl 命令、证书参数、报错详情（用户要复制去搜解决方案）。一刀切禁选会把
这些功能一起废掉。采用的是行业惯例（GitHub / GitLab / Vercel 后台）：
**界面装饰禁选，数据可复制。**

实现选「默认禁 + 白名单放开」而非逐个禁用：后者漏一处就失效，且新加组件默认
仍可被误选。

| 区域 | 选择 | 理由 |
|------|------|------|
| 导航、按钮、chip、卡片标题、metric 数字、空状态提示、form-help | 禁选 | 纯装饰，误选无意义 |
| `code` / `pre` | 可复制 | 服务器地址、证书参数、命令、路径都在这里 |
| `input` / `textarea` / `select` / `option` | 可复制 | 用户要选中编辑，禁掉直接没法用 |
| `.banner-body` | 可复制 | 报错详情要复制去搜方案 / 提 issue |
| `.one-time-secret` | 可复制 + 单击全选 | 页面提示「仅显示这一次」，必须能记录 |
| `.timeline-*` / `.kv-inline` / `dd` | 可复制 | 审计详情与配置值 |

#### 顺带修掉两个真实的 Safari 兼容 bug

写回归测试时发现既有代码里两处 `user-select:none` **漏了 `-webkit-` 前缀**：
`.dismissable .dismiss` 与 `.settings-card > summary`。Safari / iOS Safari 只认
`-webkit-user-select`，漏前缀意味着折叠面板标题仍可被误选。加了断言
`TestSel_NoWebkitPrefixGap` 全量扫描防复发。

#### 回归测试

新增 `internal/web/p2_text_selection_test.go`（6 项）。重点是
`TestSel_DataWhitelistSelectable`：白名单少任何一条都是**功能受损**而非观感问题，
测试注释里逐条写明后果。

#### 实机验证（192.168.50.63）

源码传到服务器本地构建 `ikev2-panel-v2:sel-p1a`，用原 project 名 `ddnsv6`
重建容器（该容器由 `docker compose -p ddnsv6` 编排，直接 `up` 会撞容器名冲突）。
镜像内 `user-select` 出现次数 3 → 9；部署后数据卷、`panel.db`(56K)、证书、
面板 HTTP 200、`swanctl --list-conns`、UDP 500/4500 监听全部正常。

### pr24-b — eye 切换按钮按压错位（transform 定位被全局动效覆盖）

用户反馈：登录页点「显示/隐藏密码」的 eye 按钮，按下去图标整体跳一下、位置错开。

**不是 JS 问题，是 CSS 层叠冲突：**

```css
.password-toggle{ transform:translateY(-50%); }   /* 垂直居中 */
button:active    { transform:translateY(1px);  }   /* 全局按压动效 */
```

`button:active` 特异性 `(0,1,1)` > `.password-toggle` 的 `(0,1,0)`，`:active`
命中即把 `translateY(-50%)` 整条覆盖 → 居中失效，图标下坠约 18px（自身高度一半
+1px）。且 `.password-toggle` 的 `transition` 只声明了
`color`/`background-color`/`border-color`，不含 `transform`，所以位移是**瞬时**的，
iOS 上 `:active` 粘住会让现象更明显。

**修法（两层）**：

1. 居中改用 `top:0; bottom:0; margin-top:auto; margin-bottom:auto`，
   定位不再经过 `transform`。
2. 再加 `.password-toggle:active{ transform:none; }` 显式抵消全局按压位移。
   **只做第 1 层是不够的** —— 居中改掉后全局 `button:active` 的 `translateY(1px)`
   仍然生效，这个图标浮在输入框**内部**，1px 位移同样被看成"错位"。
   它的按压反馈本来就由 `:hover` / `[aria-pressed]` 的底色 + 描边 + 图标变色承担，
   不需要位移。

由此定下一条规矩并写进测试注释：**transform 不能同时承担定位和按压动效。**
绝对定位元素的居中一律走 `top`/`bottom` + `margin:auto`；
浮在表单控件**内部**的图标按钮还要额外断掉按压位移。

> 过程记录：第一次只改了居中就收工，用户实机反馈"点下去还是跳"。
> 复核发现是漏了第 2 层。**没部署就宣布修好，是这次的真实教训**——
> 本地 CSS 改动对线上零影响，必须实机验证过才算修完。

#### 回归测试

新增 `internal/web/p3_press_offset_test.go`（3 项）：

- `TestP3_EyeToggleCentersWithoutTransform` —— 锁住本次修复的**两层**：
  基础规则不得出现 `transform` 且必须用 `top`/`bottom` + `margin:auto` 居中；
  `.password-toggle:active` 必须显式 `transform:none`。第二条是补上次漏掉的。
- `TestP3_NoAbsoluteCenteringViaTransform` —— **文件级**拦截「绝对定位 +
  `top:50%` + `translateY(-50%)`」组合，防止同样的写法被复制到别的浮层按钮。
- `TestP3_PressOffsetStaysSubtle` —— 全局按压位移上限 ±2px，对应用户说的
  「动画太夸张」。

测试有效性已验证：临时把 CSS 改回旧写法，三个断言如期失败并打印出错规则，
恢复后通过 —— 不是永远绿的空测试。

#### 实机部署（192.168.50.63）

镜像 `ikev2-panel-v2:sel-p1b`，用原 project 名 `ddnsv6` 重建容器。
构建需加 `--network=host`（容器内 DNS 走不通 bridge，这是既有环境问题）。

| 检查项 | 结果 |
|--------|------|
| 容器 | `ikev2-panel-v2:sel-p1b` Up |
| `/etc/ikev2-panel-version` | `v2.86-pr23p-4-g9b4d059` —— 不再是 `dev` |
| `docker inspect` version label | 同上 |
| 容器内 CSS | `top:0;bottom:0;margin-*-auto` + `.password-toggle:active{transform:none}` |
| 线上 HTTP 拉到的 CSS | 含 `.password-toggle:active`（grep 计数 1） |
| `/login` | HTTP 200 |
| `/data` | `panel.db{,-shm,-wal}` `ca/ le/ server/ acme-sh/ panel-state/ panel-tls/` 齐全 |
| 数据卷 | `/vol1/docker/volumes/ikev2-panel-data/_data`（未变） |
| `swanctl --list-conns` | 连接已加载 |
| UDP 500 / 4500 | IPv4 + IPv6 均监听 |

> 页面 CSS 走 `assetVersion()` cache-bust（基于 mtime），改 CSS 会自动带新
> `?v=`，不需要手动清浏览器缓存。

#### 顺带记录（非代码改动）

用户看到登录页版本号是 `dev`。核查确认**不是 bug**：版本号构建期注入
（`Dockerfile` 的 `ARG PANEL_VERSION` 写进 `/etc/ikev2-panel-version`，
`main.go` 读不到才 fallback `dev`）。CI 会传这个 build-arg，所以 ghcr.io 的正式
镜像不会显示 `dev`；只有自测镜像是默认值。已写入
[TROUBLESHOOTING §9.10](TROUBLESHOOTING.md#910-登录页版本号显示-dev)，附确认命令和
正确的 `docker build --build-arg` 用法（命令已在本仓库实测）。

### pr24-c — 文档对齐代码：数据卷说法纠偏（无代码改动）

起因是用户问「docker compose 部署后证书 / 配置 / 数据库为什么没映射到宿主机」。
核查 compose 实际写法后确认：**这不是部署故障，是文档过期**。本节只改文档。

#### 192.168.50.63 实测证据

| 命令 | 实际输出 |
|------|---------|
| `docker volume ls \| grep ikev2` | `local ikev2-panel-data` —— 卷名恒定，与目录名无关 |
| `docker inspect --format '{{range .Mounts}}…'` | 3 条：`/etc/sysctl.d`(bind)、`ikev2-panel-data`(volume)、`<项目>/logs`(bind) |
| `docker info \| grep 'docker root'` | `Docker Root Dir: /vol1/docker` —— **不是 `/var/lib/docker`** |
| `docker volume inspect --format '{{.Mountpoint}}'` | `/vol1/docker/volumes/ikev2-panel-data/_data` |
| `docker exec ikev2-panel ls -la /data` | `acme-sh/ ca/ le/ panel.db{,-shm,-wal} panel-state/ panel-tls/ server/` |
| `docker logs \| grep 'limit dir'` | `limit dir: /var/lib/ikev2-panel/limits (container)` —— **带宽**限速文件不在 `/data` |

#### 静态核实 → 实测后自我纠正的三处

第一轮只读代码得出的结论，实测后发现自己也写错了两处，一并记录：

| 项 | 只读代码时的说法 | 实测后更正 |
|---|---|---|
| 卷路径 | `/var/lib/docker/volumes/ikev2-panel-data/_data` | **`/vol1/docker/...`**，数据根目录可配，一律改成 `docker volume inspect --format '{{.Mountpoint}}'` 动态取 |
| `/data` 内容 | 列了 6 项，含 `limits/` | 实测 8 项，多了 `ca/`（自签 CA 私钥）、`server/`（服务端私钥）；**带宽限速的 `limits/` 根本不在 `/data`** —— `main.go` 只在 `/var/lib/ikev2-panel/limits` 不存在时才回退，那是 dev 分支。注意别和 `/data/panel-state/ratelimit.json`（**登录**限流计数，确实在 `/data`）混淆 |
| 备份范围 | 漏了 `ca/`、`server/` | 两者都是私钥，脚本确实不备份，已在文档里显式标注 |

#### 修正的文档错误

| 文档原说法 | 代码/实测实际 | 裁定 |
|-----------|--------------|------|
| 卷名是 `{项目目录名}_ikev2-data` | 顶层 `name: ikev2-panel-data`，恒定 | 文档错 |
| 卷名是 `ikev2-panel-v2_ikev2-data`（本文件第 1 条判❌） | 同上，早已落地 | **CHANGELOG 判定与代码相反** |
| 备份示例硬编码 `/var/lib/docker/volumes/ikev2-panel-v2_ikev2-data/_data` | 卷名错 + 路径根错，照抄双重失败 | 文档错 |
| 排查示例宿主直接跑 `sqlite3 /var/lib/docker/.../panel.db` | 同上，且宿主未必装了 sqlite3 | 改为 `docker exec` |
| `/data` 下持久化四项 | 实测 8 项 | 漏了 `ca/`、`server/`、`panel.db-wal/-shm` |

#### 改了什么

- **README §部署要点 / 数据卷**：命名卷 vs bind mount 说清；补「宿主可见 / 只在卷里」
  对照表；`/data` 内容换成实测表格；加 DockerRootDir 不可硬编码的警告；
  补限速状态实际在容器可写层的说明。
- **README §备份与恢复**：`DATA_DIR` 改用 `{{.Mountpoint}}` 动态取；写明只备份三项、
  哪些私钥不在内，给整卷 tar 命令。
- **architecture §11.3**：同上重写 + 限速目录归属说明。
- **architecture §14.2**：「看数据库」改 `docker exec ikev2-panel sqlite3 /data/panel.db`
  （已核实 `sqlite3` 在 runtime 镜像层）。
- **TROUBLESHOOTING**：新增 §8「数据 / 证书看不到」（8.1 找不到 `./data` 不是故障、
  8.2 卷被换掉怎么恢复、8.3 备份脚本报"无数据"怎么解、8.4 限速计数清零不是 bug），
  原 §8 面板 UI 异常顺延为 §9，小节号与目录锚点、两处交叉引用同步更新。
- **CHANGELOG**：第 1 条判定 ❌ → ✅，落地数 6 → 7。

**教训**：compose 的 `name:` 早在 v2.86-PR19(A10) 就写死了，但三份文档仍停留在
PR17 之前的"随目录名漂移"叙事，且 CHANGELOG 里有一条与代码**正好相反**的判定。
文档漂移到和代码矛盾，会让人照着错误路径排查 —— 比没有文档更糟。

第二条教训更实际：**只读代码得出的结论也可能错**。第一轮我把
`/var/lib/docker/volumes/...` 当成事实写进文档，而实机的 DockerRootDir 是
`/vol1/docker`；又把 `limits/` 列为 `/data` 持久化项，实际它在容器可写层。
凡是"路径在哪""哪个目录被持久化"这类问题，一律上实机 `ls` 一次再落笔。

判定"某条行为变更是否落地"时，只认代码，不认上一版文档的结论。

### pr24-d — 前端可用性修复（P0 两条 + P1/F1/D3 四条）

一次全站前端审计的产出。审计原则是**先确认不是误判再改** —— 每条都用线上输出、
源码、HTTP 行为、CSS 特异性或权威文档验证过，其中 4 条上报项被判定为误判并排除
（见本节末尾「被推翻的上报项」）。

#### P0：两处安全 / 数据丢失级缺陷

| # | 缺陷 | 后果 | 修法 |
|---|------|------|------|
| P0-1 | banner 的富文本走 `.Msg`（受 Go 模板自动转义） | 页面上直接显示 `&lt;strong&gt;…&lt;/strong&gt;`，全是乱码样子的标签文本 | 新增 `MsgHTML` 字段，**只接受代码里的字面量 HTML**；`.Msg` 保持转义不变，动态值一律拆到独立元素 |
| P0-2 | 5 处破坏性操作用 `onclick="return confirm(...)"` | CSP 是 `script-src 'self' 'nonce-…'`，**无** `'unsafe-inline'` / `'unsafe-hashes'`，浏览器直接拒绝内联事件处理器 → **二次确认从来没生效过**，点一下就真删 / 真踢线 | 全部换成 `data-confirm="…"`，在既有 nonce 脚本块里加事件委托统一处理；**不放松 CSP** |

P0-1 的边界（不要越界）：`MsgHTML` 里禁止插模板变量。密码、用户名、外部 API 错误串
一律不进 `MsgHTML`，而是渲染成同级独立元素走默认转义。回归断言在
`internal/web/p0_banner_confirm_test.go`。

P0-2 覆盖的 5 个动作：重置密码 / 删除用户 / 停用用户 / 清空 mobileconfig 覆盖项 /
恢复出厂默认值。

#### P1 / F1 / D3

| # | 缺陷 | 修法 |
|---|------|------|
| P1-1 | mc 表单校验失败返回 `http.Error(400)` | 用户落到一个**脱离站点的纯文本页**：没导航、没返回链接、已填内容全丢、英文错误串混进中文界面。改为 303 + flash 报错 + 把**已填内容原样带回**回填表单，非法 `<details>` 自动展开并标红 |
| P1-2 | Android 参数页「复制全部参数」少了 `cert_type` | 页面上特意警告"不要选 RSA 证书"，复制的内容里却没这一项，用户照抄必然踩坑。补齐 8 个字段，与显示内容逐字对齐 |
| P1-3 | `.terminal-card` 与内部 `<pre>` 都是 `overflow-x: auto` | 嵌套双层横向滚动条，手机上几乎无法操作。改为只有卡片滚，`<pre>` 交给父级 |
| F1 | `.copy-btn` 从未定义；`.btn-secondary:not([class])` 是死选择器 | 补齐 `.copy-btn` / `.copy-btn-block` 样式（含 hover / focus-visible / disabled），删掉永不匹配的死选择器 |
| D3 | 剪贴板降级路径吞异常、无条件报「已复制 ✓」 | `execCommand('copy')` 的**布尔返回值必须检查**；失败如实提示"复制失败，请手动选中复制"。Android 页尤其致命 —— 警告文案让用户"用完清空剪贴板"，失败却报成功等于让用户放松警惕反复点 |

#### 另两处：滚动位置 + 活跃隧道地址

**刷新后不在最顶部** —— 两个**互相独立**的原因，缺一个都修不好：

1. `sessionStorage` 的 `ikev2-scroll-y` 提交后**从来没被删过**。用户只要提交过一次表单，
   之后每次加载（含点导航、刷新）都命中"savedScrollY 优先级最高"分支被甩回旧位置。
2. 原生 `history.scrollRestoration` 没关，浏览器刷新时自己恢复一遍，和应用逻辑打架。

修法：key 一次性消费（读完即清）+ `<head>` 里 `scrollRestoration = 'manual'`（必须早于
浏览器恢复时机）+ `html { scroll-padding-top: calc(var(--ikev2-topbar-h) + 0.75rem) }`
让锚点不被 sticky 顶栏盖住。

**活跃隧道的本地 / 远端为空** —— parser 读的是**不存在的 VICI key**
`local-addr` / `remote-addr`。真实 `list-sas` 只给拆开的
`local-host` + `local-port` + `remote-host` + `remote-port`，
所以两个字段恒为空串。已改为 `getHostPort()` 拼成 `host[port]`（与 `swanctl` CLI 显示一致，
IPv6 不需额外转义）。

#### 测试

新增 `internal/web/p0_banner_confirm_test.go` + `internal/web/p1_interaction_test.go`
（共 20 个用例），另外补了 parser 的真实 VICI key fixture。这类缺陷单测本来测不出来
—— 断言写在模板文本和 DOM 结构上，靠正则锁住"不许退回 inline handler""MsgHTML 里不许
出现模板插值"。

#### 被推翻的上报项（记录在此，避免以后重复排查）

| 上报项 | 裁定 |
|--------|------|
| `data-copy-text` 里的 `&#10;` 会显示成字面文本 | **误判**。`&#10;` 是合法 HTML 字符引用，浏览器解码成换行 |
| mc 表单的 flash 会被上方标签页高度挡住 | **误判**，与实际渲染顺序矛盾 |
| 新建用户表单校验失败后输入丢失 | **误判**，后端本就正确回填 |
| 锚点按钮完全没样式 | **夸大**。确有死选择器，但其它规则和具体 class 已提供部分样式 |

#### ⚠️ 行为变更

| 变更 | 影响 |
|------|------|
| **页面文字默认不可选中**（`body { user-select: none }`） | **用户可见**。界面装饰（导航、按钮、标题、卡片头）双击不再误选。**数据仍可复制**：初始密码、服务器地址、swanctl 命令、证书参数、报错详情、审计日志、`<dd>` 配置值、`<input>`/`<textarea>`/`<select>`。若你依赖选中按钮文字来复制，需改用对应数据区 |
| 初始密码单击即全选整个密码（`user-select: all`） | 之前双击只选中一段，容易漏掉前后字符 |
| 二次确认从内联 `onclick` 改为 `data-confirm` + 委托处理 | 行为不变，但**不再依赖 `unsafe-inline`** —— CSP 未放宽，这是修复方向而非妥协 |
| mobileconfig 校验失败改为 PRG（303 重定向 + flash 回填） | 失败后不再返回裸文本页；已填内容保留并自动展开对应 `<details>` |
| `/data` 卷名固定为 `ikev2-panel-data` | 目录改名不再静默产生空卷。**若你之前在旧命名卷上有数据**，需按 [TROUBLESHOOTING §8.2](TROUBLESHOOTING.md) 迁移 |
| `POST /api/ddns/family` 仍无前端入口 | 本版未变，属已知待补项 |

> ⚠️ **升级前必看第 1 条**：文字选择策略改变了全站交互。若你之前习惯拖选界面文字（比如复制按钮上的字），
> 现在需要从相邻的数据区复制。完整白名单见 [architecture.md §12.11](architecture.md)。

### v2.86-PR19（A12/A13/A14/A15）— iOS + Android 真机排障修复

在 `192.168.50.63` 上用 iOS 实际调试，暴露并修复了**一组此前从未被发现的缺陷**。
此前所有验证都在单测里"通过"，但真机链路一走就断 —— 说明单测覆盖不到跨组件的接缝。

#### ⚠️ 行为变更

| 变更 | 影响 |
|------|------|
| Docker 镜像新增 `eap-identity` 插件 | **必须**，否则服务端无法索要 EAP Identity，EAP 认证根本起不来（`EAP-Identity request configured, but not supported`） |
| entrypoint 强制往 `DOCKER-USER` 插 `ipsec0` ACCEPT | **必须**，否则 CHILD_SA 能建立但客户端完全无法访问网络（见 architecture §13.1.1） |
| IPv6 forwarding 改为**无条件**检查（A16） | **必须**，原先被 `IPV6_ONLY=true` 跳过导致恒为 0（见下方 A16） |
| LE 模式强制剥掉 fullchain 尾部的根证书 | **必须**，iOS 拒绝带根的 fullchain |
| LE 模式下 `local.id` 强制取 `IKEV2_DOMAIN` | **必须**，原先取 `IKEV2_SERVER_CN`（compose 默认 `vpn.example.com`）与证书 CN 不符 |
| mobileconfig 的 `ServerCertificateCommonName` 在 LE 模式无条件取 `Domain` | **必须**，原先 `if cfg.ServerCN == ""` 的兜底被 compose 默认值绕过，下发错 CN 导致 iOS 报 "an unexpected error occurred" |
| Android 指引页在 LE 模式不再渲染 `/ca.cert.pem` 链接 | 该路由在 LE 模式下返回 404，渲染出来就是死链 |
| `.sswan` 的 `secret` 从 `"由用户输入"` 改为 `""` | 前者会被 strongSwan 当成字面密码 |
| `nft -f` 失败不再 `exit 15` | 改为降级 + 告警，由 §7.9 retry 循环补规则；原来会触发无限重启循环 |

#### A16：IPv6 forwarding 为 0（自愈从未生效）

**第一层根因**：`enable_forwarding ipv6` 被包在 `if [ "$IKEV2_IPV6_ONLY" != "true" ]` 里。
默认配置正是 `=true`，所以这个函数**一次都没被调用过**。

**第二层根因（更关键，A16 排查中才暴露）**：**entrypoint 里的"自愈"从来就没成功过**，
IPv4 / IPv6 两个协议族都没有。容器内 `/proc/sys` 是 **ro** 挂载：

```
$ grep ' /proc/sys ' /proc/mounts
proc /proc/sys proc ro,nosuid,nodev,noexec,relatime 0 0
```

`echo 1 > /proc/sys/net/...` 必然失败（`Read-only file system`），`NET_ADMIN` 救不了 ——
ro 是挂载属性，不是 capability。IPv4 之所以一路绿灯，只是宿主本来就开着
`ip_forward=1`，走的是函数开头"已开直接返回"分支，从没触发过写入。

原日志文案提示"检查 NET_ADMIN capability / docker daemon 需要 CAP_SYS_ADMIN"，
把人引向了完全错误的方向。

**修复**：

1. 移除 `IKEV2_IPV6_ONLY` 条件，与 IPv4 对齐无条件检查。
   依据：`IKEV2_IPV6_ONLY` 只控制监听/拨号用哪个网络栈，不影响 ESP 内层协议族，
   条件分支与该前提自相矛盾。
2. 失败行为按影响面区分：IPv4 为 0 → `FATAL exit 11`（所有客户端都出不了网）；
   IPv6 为 0 → `WARN` + 宿主侧修复命令（客户端会 IPv4 回退，属降级不属故障）。
3. 文案改为明确指向宿主，不再暗示 capability 能解决。

**转发链路的其余环节本来全部就位**，缺的只是这一个开关：

| 环节 | 状态 |
|---|---|
| `local_ts = 0.0.0.0/0, ::/0` | ✅ 恒定双栈（v2.86-PR12.10 起） |
| nft `ip6 ikev26` masquerade + MSS | ✅ |
| `DOCKER-USER` 的 `ip6tables -i/-o ipsec0 ACCEPT` | ✅ |
| **`net.ipv6.conf.all.forwarding`** | ❌ **恒为 0 → 本次修复** |

**63 上实测**：改后 `net.ipv6.conf.all.forwarding` 保持为 1；ens18 的 SLAAC 地址未丢失
（`valid_lft` 持续续期 595→598→601），`ping -6 240c::6666` 3/3 通。**无副作用。**

**注意**：`nft table ip6 ikev26` 的 counter 仍为 0，这是**正确行为** ——
客户端只拿到 IPv4，remote TS 是 `10.10.0.1/32`，本就不会产生 IPv6 内层流量。
见 A17。

**必须在宿主上设置**（compose 的 `sysctls:` 块在 host 网络模式下会被 runc 以
`not allowed in host network namespace` 拒绝，v2-79.2 已实测）：

```bash
sudo sysctl -w net.ipv4.ip_forward=1
sudo sysctl -w net.ipv6.conf.all.forwarding=1
printf 'net.ipv4.ip_forward = 1\nnet.ipv6.conf.all.forwarding = 1\n' \
  | sudo tee /etc/sysctl.d/99-ikev2.conf
```

#### A17：单个 pool 的 `addrs` 不支持混合地址族（客户端只拿到 IPv4）

**本条不是缺陷，是被记录的事实。**排查 A16 时顺带发现，`forwarding` 修好后
counter 仍为 0，进一步定位到这里。

**现象**：`configs/swanctl-ipv6-only.conf` 写的是 `addrs = 10.10.0.0/24, fd00:1::/64`，
`swanctl` 报 `successfully loaded 1 pools`，**无任何警告**，但 pool 里只有 IPv4。

**对照实测**（strongSwan 6.0.1，192.168.50.63）：

| `addrs` | `swanctl --list-pools` |
|---|---|
| `fd00:1::/64` | `fd00:1::1` 0 / 0 / **2147483646** ✅ |
| `10.10.0.0/24, fd00:1::/64` | `10.10.0.1` 0 / 0 / **254** ❌ |

**根因**：单个 pool 的 `addrs` 不支持混合地址族，混写时 IPv6 段被静默丢弃。
官方 [Virtual IP Addresses](https://docs.strongswan.org/docs/latest/features/vip.html#_responder_configuration)
要求按地址族拆成多个 pool。

**影响评估：当前无影响，不建议贸然修改**。客户端（iOS / Android）都只拿到 `10.10.0.1`，
走 IPv4，功能完整。改成双栈属于行为变更（客户端地址形态变化），且需注意：

1. 客户端要在 `vips` 里显式请求 `0.0.0.0, ::`，否则仍只拿一个。
2. 本项目宿主实测 `curl -6 ipv6.google.com` 超时（路由器对 LAN→WAN IPv6 出向有限制），
   客户端一旦真发 IPv6 可能反而变慢。

所以本次只记录，不改行为。

**排查陷阱**：有在线租约时 `swanctl --load-pools` 报
`vici pool ikev2-pool has online leases, unable to replace`；需先
`swanctl --terminate --child ikev2-rw`（不带 `--child` 会报 `missing terminate selector`）。

#### A16 被推翻的诊断

A16 最初写成"客户端**同时**拿到 IPv4 和 IPv6，Happy Eyeballs 优先试 ULA IPv6 被静默丢弃，
用户感受只是慢一拍"。**这个论断是错的**，且是建立在未验证的假设上的：

当时只看到 `nft ip6 ikev26` counter 为 0，就推断客户端在发 IPv6 包。实际查
`swanctl --list-pools` 才发现 pool 里**根本没有 IPv6 条目**，客户端从未拿到过 IPv6 地址。

教训：counter 为 0 有两种可能（客户端没发 / 发了被丢），必须先看
`--list-pools` 和 `--list-sas` 的实际地址才能定性，不能只凭 counter 推断。

同理，"Android 会因为 IPv6 forwarding 为 0 而踩坑"这个推论**不成立** ——
Android 和 iOS 一样只拿 IPv4，行为一致。

**上线前实测（63）**：手动写 `1` 后确认无副作用 —— ens18 的 SLAAC 地址未丢失
（valid_lft 595→598→601 持续续期）、`ping -6 240c::6666` 3/3 通。
`bash -n` + 三分支覆盖测试（已开 / 可写自愈 / 内核无 IPv6）全部通过。

#### 修复清单

| # | 缺陷 | 位置 |
|---|------|------|
| 1 | `eap-identity` 插件未编入镜像（认证链路无法启动） | `Dockerfile` |
| 2 | nft base chain 的 `accept` 不是终态，挡不住 docker 的 `policy drop` | `scripts/entrypoint.sh` |
| 3 | `local.id` 在 LE 模式取错来源，与证书 CN 不符 | `scripts/entrypoint.sh` |
| 4 | mobileconfig `ServerCN` 兜底条件式失效 | `cmd/ikev2-panel/main.go` |
| 5 | LE fullchain 含根证书，iOS 拒装 | `scripts/entrypoint.sh` |
| 6 | `eap_id = %any` 被误改成 `%identity`（约束语义，对 iOS/Android 都失败） | `configs/swanctl-ipv6-only.conf` |
| 7 | `OnDemandProfile=manual` + `OnDemandEnabled=true` 产出非法 plist，iOS 拒装 | `internal/cert/mobileconfig.go` |
| 8 | 用户级 mobileconfig 表单无回填，保存任一项静默清空该用户全部配置 | `internal/web/` + 模板 |
| 9 | `ExcludeLocalNetworks` 注释与 UI 文案与 Apple 文档相反（局域网实际不可达） | `internal/cert/mobileconfig.go` + 模板 |
| 10 | admin 层选 `home_wifi_disconnect` 永远保存不上（无 SSID 字段但校验强制要求） | `internal/panelstate/mobileconfig_default.go` |
| 11 | `isValidIP` 不校验 0–255，`300.300.300.300` 能进 plist | `internal/cert/mobileconfig.go` |
| 12 | `captive_probe` 的 `Domains` 是空字符串 | `internal/cert/mobileconfig.go` |
| 13 | Android 指引页在 LE 模式渲染 404 死链 | `web/templates/android_content.html` |
| 14 | Android 原生指引缺`证书类型` / `服务器标识` 字段（易误选「RSA 证书」导致失败） | `web/templates/android_content.html` |
| 15 | `.sswan` 只发 modp2048，与服务端强算法优先设计背道而驰 | `internal/web/handlers_config.go` |
| 16 | `.sswan` 注释声称「扫码导入」，但面板从未生成该二维码 | `internal/web/handlers_config.go` |
| 17 | 用户列表空态文案说 Android 可扫码（`.mobileconfig` 对 Android 无效） | `web/templates/users_list_content.html` |
| 18 | **IPv6 forwarding 被 `IPV6_ONLY=true` 跳过，恒为 0** | `scripts/entrypoint.sh` |

#### 两次被推翻的诊断（教训）

1. **「nft 要求 `iifname` 指向的接口已存在」** —— 实测 5 种组合全部通过，接口不存在不是问题。
   我曾据此在 entrypoint 加了"轮询等 ipsec0 出现"，后又撤掉（charon 在 §6 才启动，等不到）。
2. **「把自定义链 priority 调到 -10 抢在 docker 前面」** —— base chain 的 `accept`
   根本不终止跨 chain 遍历，改 priority 无效。

两次都源于**没有实测就下结论**。真正的根因是排查时手工往宿主 `nft -f` 热加载了带重复
chain 的表，污染了环境。教训已写进 architecture §13.1.1。

#### 新增文档

- architecture §13.1.1 nft `accept` 非终态（含实测证据与被排除的假设）
- architecture §13.1.2 `eap_id` 的 `%identity` vs `%any`
- architecture §12.5 扩充 mobileconfig 易错点（+5 条结构性陷阱）
- TROUBLESHOOTING §2.3.1 Android strongSwan App 连不上（此前只覆盖原生客户端）

#### 测试

新增 14 个回归测试（`mobileconfig_a13_test.go` / `mobileconfig_default_a13_test.go` /
`sswan_config_test.go`）。其中抽出 `renderSSwanTemplate` 的原因是：Go **编译期不检查**
`fmt.Sprintf` 格式符与实参数量，错了只在运行时产出 `%!s(MISSING)` 垃圾配置。

#### Android 支持现状（重要）

`.mobileconfig` 对 Android **完全无效** —— `PayloadType = com.apple.vpn.managed` 是
Apple MDM 专有标识，strongSwan app 只认 `.sswan`。面板二维码指向 `/install/<token>`，
返回的是 `.mobileconfig`，Android 浏览器只会下载一个装不上的文件，且 token 一次性作废。

Android 走项目已有的两条独立通道：

- **系统原生**：`/users/<id>/android` 手工填参（指引已补全CA / 证书类型 / 服务器标识）
- **strongSwan App**：`/users/<id>/sswan` 导入 `.sswan`（需手填密码 + 确认本地 ID）

服务端侧对 Android 兼容：`eap_id = %any` 对 ID 类型不敏感（比 `%identity` 更兼容）、
`mobike = yes` 匹配、proposals 含 modp2048 兜底给老设备。

### 已完成

| 改动 | 影响 |
|------|------|
| 文档体系重构：52 个文件 → 5 个（见 [docs/README.md](README.md)） | 无代码影响 |
| 新增 [API.md](API.md)：49 条 HTTP 端点 + 鉴权模型（此前零文档） | 无代码影响 |
| 代码注释里指向已删文档的 38 处引用，全部改指现存章节 | 无代码影响 |
| `gofmt`：60 个文件统一（含不可见 BOM 与注释对齐） | 无逻辑改动 |
| `tmp-dnsq/` → `cmd/dnsq/`，修复其编译失败并参数化 | 新增排查工具 |
| Dockerfile 增加 `dnsq` 构建，使文档里的 `docker compose exec ... dnsq` 可用 | 镜像多一个二进制 |
| DDNS 状态文件 `/etc/ikev2/ddns.conf` → `/data/panel-state/ddns-runtime.conf`，旧文件启动时自动迁移 | 修数据丢失：`/etc/ikev2` 没挂载，容器重建后 DDNS 开关/RR/节流时间戳全回默认 |

### ✅ 未落地的部分已从仓库移除（v2.86-pr24 的 A/B 类修复）

**这是接手本仓库时必须先知道的事。**

一批针对 A/B 类问题的单测曾写进工作区（34 个新 `_test.go`）。绝大部分实现已补齐：

| 已补齐 | 内容 |
|--------|------|
| 证书 | 半缺失 / 0 字节拒绝启动（`errInconsistentCert`） |
| 上下文 | `ctx` 全链路透传（`dns.call` / `upsertRecord` / `tick`，含可中断退避） |
| 可观测性 | `/metrics` 标签基数上限（`maxLabelSeries`）+ 安装 token 路径归一（`isInstallToken`） |
| 输入校验 | `expires_at` 按日历日期比（不掺时区偏移）、`speed_limit_mbps` 不再静默吞 `Atoi` 错误 |
| 过期扫描 | `GetExpiredUsers` 带 `enabled = 1`，避免每 60s 重复调 `Terminate` |
| DDNS | 记录缺失时自动 `AddDomainRecord`（与 ddns-updater 一致）、同步周期每轮重读（无需重启）、状态文件迁进 `/data` 卷 + 旧文件迁移 |
| swanctl | reload 失败原子回滚、`unwrapSA` 剥层 |
| fsutil | `AtomicWriteFile` 改用唯一 tmp 名 —— 固定名并发写会写出「A 的头 + B 的尾」 |
| config | `listenIsLoopback` + 非 loopback 暴露提示（只提示不 refuse） |

**剩下 5 个测试文件引用的符号经复核后判定不落地，已从仓库删除。**

它们断言的是**明确裁定的"不做"**，留在仓库里会让 `go test ./...` 和 `go vet ./...`
直接编译失败（CI 的 `runner-test` stage 跑的就是这个）：

| 已删除的文件 | 断言的符号 | 不落地的裁定 |
|-------------|-----------|-------------|
| `internal/auth/password_policy_parity_test.go`<br>`internal/auth/password_strength_test.go` | `MinPasswordLen` / `MaxPasswordLen` / `ValidatePasswordStrength` / `commonWeakPasswords`（NIST SP 800-63B 密码策略） | 会推翻 `handlers_account.go` 里的**用户决策 Q1**（最低 8 位、不强制复杂度）。改这个要用户明确授权 |
| `internal/auth/ratelimit_sweep_test.go` | `RateLimiter.Sweep` / `SweepLoop` / `FlushInterval` / `maxBuckets` | 限流桶清扫与容量上限。现有实现无功能性缺陷，属容量优化，不是正确性问题 |
| `internal/store/db_perms_test.go` | `hardenDBPerms` / `hardenDirPerms` | 防御性·低 —— `DataDir` 已是 0700 且容器 `cap_drop: ALL`、面板以 root 运行无降权 uid，没有实际可利用方 |
| `internal/store/schema_version_test.go` | `schemaVersion` / `maxOpenConns`（`PRAGMA user_version` 版本化） | 单机单版本部署，没有多版本迁移场景 |

`internal/cert` 的 4 个证书卫生用例（EKU / SAN）**保留且通过** —— 实现已按断言修好
（叶证书 EKU 只含 ServerAuth、SAN 按 CN 类型二选一、不再无条件塞 `127.0.1`）。

原先记录的 15 条「行为变更」逐条核对结果：

| # | 变更 | 状态 |
|---|------|------|
| 1 | 卷名固定为 `ikev2-panel-data` | ✅ compose 顶层 `name: ikev2-panel-data`，与目录名解耦（**此前本文档误记为 ❌，实际早已落地**） |
| 2 | 面板降权 uid 10001 + `SETUID`/`SETGID` | ❌ 无 `IKEV2_PANEL_UID`，面板仍以 root 运行 |
| 3 | VICI socket 属组处理 | ❌ entrypoint 无 `chgrp` |
| 4 | `pull_policy: always` → `missing` | ❌ 仍是 `always` |
| 5 | forwarding 只挂 `/proc/sys/net`，删 `persist_sysctl` | ❌ `persist_sysctl` 仍在，走 `/host-sysctl.d` |
| 6 | 证书半缺失拒绝启动 | ✅ `cert.errInconsistentCert` |
| 7 | DDNS 记录不存在时自动创建 | ✅ `upsertRecord` 走 `AddDomainRecord`，与 ddns-updater 一致 |
| 8 | mobileconfig 域名改用运行时来源 | ✅ `ServerID` 由调用方传入，不读 `ALIYUN_DOMAIN` |
| 9 | LE 模式缺凭证 `exit 18` | ✅ `scripts/entrypoint.sh` |
| 10 | `expires_at` 不得早于今天 | ✅ 按日历日期比，不掺时区偏移 |
| 11 | swanctl 加载改 `--load-conn`（不中断活跃 SA） | ❌ **已撤销**：CLI 无此子命令（只有复数 `--load-conns`），且本项目只有一条连接、细粒度重载零收益，改的又是 pool 而非 conn。详见 architecture §12.1 |
| 12 | `/var/log` 1777 → 0755 | ❌ 仍是 1777 |
| 13 | 默认监听保持 `0.0.0.0:8443` | ✅ 未改动，符合预期；另补了一条措辞准确的暴露提示 |
| 14 | 密码策略改 NIST SP 800-63B | ❌ 已裁定保留「用户决策 Q1」，不改 |
| 15 | 数据库 schema 版本化（`PRAGMA user_version`） | ❌ |

**结论**：pr24 的 15 条行为变更落地 7 条，其余 8 条均为**明确裁定不做**（不是遗漏）。
`go build ./...` / `go vet ./...` / `go test ./...` 现已全绿。

⚠️ **一次被撤销的实现**：第 11 条（`--load-conn`）中途被实现并标 ✅，随后核实官方文档
发现该子命令在 `swanctl` CLI 中**不存在**（只有复数 `--load-conns`，且不接受连接名
参数），且本项目只有 `ikev2-rw` 一条连接，细粒度重载零收益。已撤销实现 + 撤回文档声明。
教训记在 `load_conn_test.go` 头部：测试替身会把被测对象的失败模式一起替身掉，
"我们调用了什么参数"通过 ≠ "这个命令能工作" —— 现在改用官方子命令白名单校验。

**教训（值得单列）**：测试文件不是"断言写完就能进仓库"。断言一个**决定不做的行为**，
等于给 CI 埋一颗编译炸弹。这批文件里 5 个都是这么来的 —— 断言本身写得挺严谨，
错的是它们断言的东西被裁定不落地。**裁定不做时，要连测试一起删。**

工作底稿（含每条风险的复核裁定与真机验证过程）在仓库外
`/opt/ikev2-analysis/`：`00-review-verdict.md`（裁定书）、`12-fix-report.md`（修复报告）、
`archive/`（已归档的 17 份审计 / 13 份提案 / 16 份旧 release notes / design.md）。

---

## v2.86-pr20（2026-09-23）— Dev 模式工作流 + 静态资源 cache-bust

Dev 模式可直接本机启服务调试，不必打包 docker。修复 IDE 浏览器 CSS 错误
（进程级 cache 问题）与真实浏览器不一致。

---

## v2.86-pr19（2026-09-23）— 整站 UI 重新设计

纯前端（templates + CSS + 静态资源缓存策略），后端逻辑 0 改动。full reset + cache control。

---

## v2.86-pr18（2026-09-23）— 登录页整页重新设计

visual-first brand split，与整站 Indigo/Inter 设计语言对齐。

---

## v2.86-pr14（2026-09-23）— 网络模式收敛到 host 唯一

移除 bridge/其他网络模式支持，只保留 `network_mode: host`。这是本项目能"导入 compose
直接用"的前提。

---

## v2.86-pr13.4（2026-09-23）— 容器日志持久化 + 轮转

`./logs` 用 bind mount（便于宿主直接看日志），`./data` 保留 named volume。

---

## v2.86-pr13.3 — PanelState subnet 对齐 cert.conf

修复重启后面板用户的 IP 段配置丢失（此前两处配置不一致，cert.conf 的值胜出）。

---

## v2.86-pr12.23（2026-09-23）— mobileconfig invalid profile 修复

Hotfix，iOS 拒绝安装。三个原因：`DNSSettings` 是杜撰 key（主因）、`</key>` 闭合标签
typo、外层 `PayloadIdentifier` 与内层冲突。

---

## v2.86-pr12.21（2026-09-21）— Visual-First UI 重构 + Dev 工作流

---

## v2.86（2026-09-19）— 综合规范化

累计 4.8d / 7 PR。设计原则：家用 1-50 人规模优先，不为企业/国家级场景加码。

⚠️ **行为变更（13 条）**：

- PR-11 Docker 减权 + `read_only` + 清华源 GPG 校验
- PR-9 登录端点 rate limit + HTTP 安全 header 全 7 项 + DDoS 防护（connlimit + hashlimit）
- PR-10 IKEv2 算法去弱（RFC 8247 + NIST 800-131A）+ RFC 7383 fragmentation
  + RFC 6023 childless；**保留 modp2048 fallback** 给 iOS 14/15/16、Win7
- PR-12 续签 atomic rename + acme.sh pinned commit + renew cron 随机化
- PR-13 filelog 脱敏 + logrotate
- PR-14 DDNS retry jitter
- PR-15 SA lifecycle 审计

同版本追加：PR12.10~PR12.17 紧急 hotfix、iptables → nft 迁移（PR12.16）、
listener/parser VICI nested Message 解析修复（PR12.18）。

明确砍掉（家用无关）：PQC（2027+ 标准）、ESP TFC（抗流量分析）、IFB（上传限速）。

---

## v2.85（2026-09-19）— 8 个 PR 全量合并 + Phase 5 A+B

累计 6.0 人日。部署一致性、默认密码持久化、时区统一、main 退出链路、
alidns 错误码与签名、DDNS 持久化与节流、IPv6 tc 限速、PR-8 健康检查 +
审计日志 + 双确认删除、audit 90 天 retention cron、TCP MSS clamp。

---

## v2.84（2026-09-18）— DDNS 支持 IPv4 / IPv6 / 双栈可选

⚠️ 新增 `v4` / `v6` / `dual` 枚举，默认值影响已有部署。

---

## v2.83（2026-09-18）— 阿里云凭证统一 + 面板 HTTPS 证书热重载

阿里云凭证来源统一（SIGHUP 热重载），顺带修 v2-82 告警 bug。

---

## v2.82（2026-09-18）— 阿里云 DDNS 自动同步

---

## v2.81（2026-09-18）---

---

## v2.80（2026-09-18）---

---

## v2.79.2（2026-09-17）—

---

## 更早的版本

v2.79.2 之前的变更没有留下文档。代码历史从 v2.79.2 起是完整的。