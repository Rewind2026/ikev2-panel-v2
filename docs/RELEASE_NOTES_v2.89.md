# v2.89 — L2 模式必填变量：compose 插值语义对称化

发布日期：2026-10-20

修一个**部署期静默故障**：容器起得来、日志反复刷「缺少必填变量」、无限重启，
但从日志上完全看不出真实原因。

镜像：`ghcr.io/rewind2026/ikev2-panel-v2:v2.89`（linux/amd64）

---

## ⚠️ 先看这条：你可能不需要换镜像

这次修复的主体在 **compose 文件**里，而 `docker-compose*.yml` **不进镜像**
（[Dockerfile](../Dockerfile) 成品层只 COPY `scripts/`、`web/`、`configs/`）。

也就是说：

- **已经跑起来的容器**：什么都不用做。
- **正在部署 / 部署失败的人**：只需要修好你那份 compose 文件（或补齐环境变量），
  **镜像拉不拉都一样**。

镜像里唯一的变化是 `PanelVersion` 字符串从 `v2.88` 变成 `v2.89`。发这个版本的
理由是让版本号和 CHANGELOG 诚实，不是部署的必要条件。

---

## 故障现场

第三方面板（1Panel / 宝塔 / dpanel）部署 ipvlan 模式：

```
[entrypoint] ERR: IKEV2_NETWORK_MODE=ipvlan 缺少必填变量: IKEV2_L2_PARENT IKEV2_L2_SUBNET IKEV2_L2_GATEWAY
[entrypoint] FATAL: IKEV2_NETWORK_MODE=ipvlan 配置错误（详见上方 ERR）
```

`exit 12` + `restart: unless-stopped` → 无限重启，日志反复刷同一行。

## 根因：同一个变量，两条互不相干的插值路径

`IKEV2_L2_PARENT` / `IKEV2_L2_SUBNET` / `IKEV2_L2_GATEWAY` 在 compose 文件里
出现**两次**，走两条**互相独立**的插值：

| 位置 | 插值结果给谁 | 是否进容器 |
|------|--------------|-----------|
| `networks:` 段的 `parent` / `subnet` / `gateway` | Docker daemon 建网 | ❌ **不进** |
| `environment:` 段的同名三项 | 容器环境变量 | ✅ **进** |

entrypoint 读的是**容器里**那一份（`os.Getenv`），不是 `networks:` 段的。

而 `environment:` 段原本写的是 `${IKEV2_L2_PARENT:-}` —— **缺失即空串，不报错**。
于是当 `networks:` 段被改成 `:-<默认值>`（面板模板常为了先让 compose 起得来
这么改）时：

1. compose 插值通过 → 容器创建成功 → 网络也建起来了
2. 容器里三个变量却是**空串**
3. entrypoint `net_mode_validate` 判「缺少必填变量」→ `exit 12`
4. `restart: unless-stopped` → 无限重启

**为什么难查**：日志里只有「缺少必填变量」，看不出真实原因是
「变量压根没传进容器」。这个不对称只在 compose 被改过时才暴露 —— 用原始
compose + 完整 `.env` 部署的人永远不会碰到。

第三方面板还有第二层原因：它有自己的目录和自己的 `.env` 管理，
**不读项目目录下的 `.env`**。

## 修复

**两份 L2 compose 的 `environment:` 段改用 `:?`（缺失即报错）**：

```yaml
IKEV2_L2_PARENT: "${IKEV2_L2_PARENT:?必填：宿主父接口名，如 eth0}"
IKEV2_L2_SUBNET: "${IKEV2_L2_SUBNET:?必填：宿主所在 IPv4 子网，如 203.0.113.0/24}"
IKEV2_L2_GATEWAY: "${IKEV2_L2_GATEWAY:?必填：该子网的宿主网关，必须落在上面的子网内}"
```

改完之后，缺变量在 `docker compose up` 阶段**直接失败**并指名变量：

```
error while interpolating services.ikev2-panel.environment.IKEV2_L2_PARENT:
required variable IKEV2_L2_PARENT is missing a value: 必填：宿主父接口名，如 eth0
```

不用等容器起来再从刷屏日志里反推。

**host 那份（`docker-compose.yml`）保持 `:-` 不变** —— host 模式压根不用这三个
变量（entrypoint 对 host 分支只打 WARN 不拦截），写成 `:?` 会让所有 host
部署在 up 阶段就失败。

## 改动文件

| 文件 | 改动 |
|------|------|
| `docker-compose.macvlan.yml` | `environment:` 段三个 L2 变量 → `:?` + 根因注释块 |
| `docker-compose.ipvlan.yml` | 同上 |
| `docker-compose.yml` | 补注释：说明为什么本文件必须保持 `:-` |
| `tests/network_mode_test.sh` | 新增 **NM-57**：锁住「L2 两份 `:?` / host 一份 `:-`」两端 |
| `docs/TROUBLESHOOTING.md` | 新增 §1.3.2.1：第三方面板部署场景的完整排查步骤；§1.7.6 改写 |
| `README.md` | L2 前置条件表述修正（见下） |

## 附带修正：README 对 virtio 的过度概括

README 原写「父接口不能是 virtio」。实测**两台机器的 `ens18` 都是
`parentbus virtio`，结论却相反**：

| 机器 | `parentbus` | macvlan / ipvlan |
|------|------------|-----------------|
| 云主机 | virtio | ❌ `device or resource busy` |
| 群晖 NAS | virtio | ✅ 均正常 |

`virtio` 只是**风险信号，不是判决**。已改为「以探针命令实际输出为准」。

---

## 验收

### 静态

| 项 | 结果 |
|----|------|
| `tests/network_mode_test.sh` | ✅ 120 passed / 0 failed（含 NM-57 共 9 项断言） |
| 三份 compose YAML 结构解析 | ✅ `environment` 键数一致（46） |
| `go build ./...` / `go vet ./...` / `go test -count=1 ./...` | ✅ 全绿 |

### 实机（192.168.50.63，Docker 28.5.2 / Compose v2.40.3）

三份 compose 逐个真实 `up`：

| 模式 | 缺变量时 | 补齐变量后 |
|------|----------|------------|
| host | `config` 通过（`:-` 保持可选 ✅） | 容器起来，`/healthz` 200，8443 监听正常 |
| macvlan | `up` 阶段报错并指名变量 | 容器起来，校验通过，拿到 `192.168.50.2/24` |
| ipvlan | `up` 阶段报错并指名变量 | 容器起来，**IPv6 GUA `2408:832e:...` 经 SLAAC 拿到**，`RestartCount=0` |

**对照实验**（复现原故障 + 验证修复）：

| compose | `up` 阶段 | 容器状态 | entrypoint |
|---------|-----------|----------|------------|
| 旧版（两段都 `:-`） | 成功 | `Restarting (12)`，重启 8 次 | `缺少必填变量` + `FATAL` ← **原故障现场** |
| 修复后（environment 段 `:?`） | **直接报错** | 容器根本没建出来 | 走不到这一步 |

**面板设置联动**（走真实 HTTP 链路：登录 → `/api/settings/save` → `/api/settings/status`）：

- 保存后 `settings.json` 落盘（`schema_version: 1` 保持兼容），`status` API 全部项来源变 `panelstate`
- entrypoint 重启后接住：`[settings.json] applied: ... connlimit=37 hashlimit=120 family=v4`
- 非法值被服务端拦截且**不落盘**（如 `connlimit_per_ip 必须在 1 ~ 10000 之间,当前是 99999`）
- **优先级链路正确**：`env(88)` → 面板覆盖(`37`, `panelstate`) → 重启后仍取 `37` → 清除面板设置后回退 `env(88)`

---

## 安全约束（本次未触碰）

| 约束 | 状态 |
|------|------|
| `WriteDDNSConfig` 只能改注释，不能删除 | ✅ `internal/panelstate/` 未改动 |
| `scripts/entrypoint.sh` 不改现有 `grep` 判定逻辑 | ✅ 该文件未改动 |
| `schema_version` 兼容老文件 | ✅ `internal/panelstate/` 未改动 |

本次改动全部在 compose 文件与测试/文档层，**不涉及 Go 运行时逻辑**。

---

## 升级

### 你正在用第三方面板部署 → 见这个

1. 拉本仓库的 `docker-compose.ipvlan.yml`（或 `docker-compose.macvlan.yml`）替换掉
   你手上那份
2. 在面板的环境变量里补齐三项（面板不读项目目录的 `.env`）：

   | 变量 | 怎么查真实值 |
   |------|--------------|
   | `IKEV2_L2_PARENT` | `ip -o -4 route show default` → 第一列的 `dev` 后面那个名字 |
   | `IKEV2_L2_SUBNET` | `ip -o -4 addr show <parent>` → `inet` 那一列的 CIDR |
   | `IKEV2_L2_GATEWAY` | `ip -o -4 route show default` → 第三列 |

3. `docker compose up -d`

完整排查步骤见
[TROUBLESHOOTING.md §1.3.2.1](TROUBLESHOOTING.md)。

### 已经跑起来了 → 不用动

### host 模式 → 不用动

---

完整变更清单见 [docs/CHANGELOG.md](CHANGELOG.md) 的 v2.89 小节。
