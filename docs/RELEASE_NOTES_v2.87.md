# v2.87 — 配置统一：面板设置页 + 前端校验 + entrypoint 顺序修复

发布日期：2026-10-20

把原先只能手改 `.env` / compose 的配置项搬进面板，并在实机验收中修掉 3 个
会导致「保存了但不生效」的 bug。

镜像：`ghcr.io/rewind2026/ikev2-panel-v2:v2.87`（linux/amd64）

---

## ⚠️ 升级前必读

### 1. 老配置不会失效

`settings.json` 带 `schema_version`，本次改动**向后兼容**。已有 `/data/panel-state/`
的实例升级后行为不变，无需手工迁移。

### 2. 三态语义：什么时候会「跟随 env」

面板上**留空** = 跟随 env / 代码默认值，**不是**「清零」。这是刻意设计的：

| 盘上状态 | 含义 |
|----------|------|
| `key` 缺失 / 指针 `nil` | 跟随 env |
| `key` 存在且为 `false` / `0` | 面板显式覆盖 |

不使用 `null` —— JSON 的 `null` 无法区分「未设置」与「显式清空」。

### 3. 哪些参数改了要重启

只有一项是运行期立即生效的：

| 字段 | 生效方式 |
|------|----------|
| `log_level` | **运行期立即生效**（`slog.LevelVar`） |
| `log_format` | 需重启（slog handler 类型启动时定死） |
| `audit_retention_days` | 需重启（entrypoint 写 cron） |
| `connlimit_per_ip` / `hashlimit_per_min` | 需重启（entrypoint 写 iptables/nft） |
| `disable_mss_clamp` / `disable_ddos_protection` | 需重启 |
| `nft_family` | 需重启 |

其余 17 项部署级参数（网卡 / 数据目录 / 监听端口等）**只读**，改法仍是编辑 `.env`
后 `docker compose up -d`。运行时改它们要么不生效，要么直接把面板改成连不上自己，
所以面板上只展示当前值。

---

## 修掉的 3 个 bug

### 1. entrypoint 启动清理排在加规则之后（最严重）

**症状**：面板上把「每 IP 并发连接上限」从默认值改成 200 并保存，entrypoint 日志
照常打印

```
[entrypoint] iptables connlimit (UDP/500, DOCKER-USER) added
```

但重启后 `iptables -nL DOCKER-USER` 里**根本没有这条规则** —— 参数保存了，
完全不生效。

**根因**：§4.6 `apply_ddos_protection` 先按 comment 锚点 `v2.86-PR9 ...` 加上
`connlimit` / `hashlimit` 规则，紧接着 §4.7.1 的启动清理又**按同样的锚点**把
它们删掉（那段清理的本意是清上一轮容器留下的残留）。两段用同一个锚点，所以顺序
错了也不会有任何报错。

**修复**：把启动清理前移到 §4.5c（§4.6 加规则之前），函数定义与调用顺序同步
调整，退出时 trap 留在 §4.7 保持对称。

**回归保护**：`internal/panelstate/entrypoint_order_test.go` 钉住调用顺序。
测试有效性已验证 —— 删掉修复块后测试立即失败。

### 2. `optInt` 静默吞掉非法值

服务端解析表单整数时，解析失败原先返回 `nil`（= 跟随 env）而不报错，非法输入被
当作「没填」静默丢弃。改为返回 error → 400，**整张表单不落盘**。

### 3. 部署级参数表头压住第一行

「部署级参数（只读）」折叠区里，表头 `环境变量 / 当前值 / 说明` 压在第一行
`IKEV2_NETWORK_MODE` 上，两行文字重叠。

**根因**：全局 `thead th` 有 `position:sticky; top:var(--ikev2-topbar-h)`（48px），
本意是让主列表表头吸顶。但 `.deploy-only-table-wrap` 带 `overflow-x:auto` ——
一个 `auto` 的 X 轴会把 Y 轴也计算成 `auto`（CSS 规范行为），wrapper 成了滚动
容器，表头改为相对它定位、被往下推 48px。

**修复**：`.deploy-only-table th` 显式 `position:static`。

顺带一提：项目里此前已在两处踩过同一类坑（`home_content.html` 有注释说侧栏和
users 列表「用 `<table>` 会触发 Chrome sticky 表头覆盖 tbody 行的渲染 bug」），
当时是靠**改用 CSS Grid 绕开**的。本次是修根因。

---

## 新增：面板设置页

### 配置优先级链

```
panelstate(/data/panel-state/settings.json) > env(IKEV2_*) > 代码 factory 默认值
```

`internal/panelstate/settings.go` 是唯一真相源（字段 + 常量 + `Validate()`），
`internal/runtime.Merge()` 负责 env → 覆盖值的合并。

### 前端校验三层

任何一层都不依赖「浏览器恰好支持某特性」：

1. **模板** —— `data-val-int` / `data-val-min` / `data-val-max` / `data-val-enum` /
   `data-val-bool`。上下界从 `.Settings.ConnLimitMin` 等常量渲染，与后端**同源**，
   不在 JS 里写死
2. **`initSettingsValidation()`**（`layout.html`，唯一 nonce 脚本块）——
   `submit` capture 阶段逐项校验，有错则 `preventDefault()` + 聚焦首个错误字段；
   `input` / `change` 只对已处于 `is-invalid` 态的字段重算，改对即消错
3. **服务端** —— `optInt` 遇非整数直接 400

表单刻意加 `novalidate`：「留空 = 跟随 env」是合法状态，且原生气泡文案随浏览器
语言走。

另有回归测试钉住两个易踩的坑：

- **空串枚举必须放行** —— 它就是 `<select>` 首项「跟随 env 默认」
- **hidden 兜底必须排在 checkbox 前面** —— 否则取消勾选会被误读成「跟随 env 默认」

### CSP 约束

CSP 为 `script-src 'self' 'nonce-{per-request}'`，无 `unsafe-inline`。所有交互 JS
都在 `layout.html` 的唯一 nonce 脚本块内，事件一律用 `document` 委托，不写内联
`onclick` / `onsubmit`。

---

## 验收

`192.168.50.63` 实机，**host / macvlan / ipvlan 三种网络模式 × 8 项参数**全量真实
浏览器测试（修改 → 保存 → 生效）：

| 网络模式 | 限速规则落点 | 实测 |
|----------|--------------|------|
| host | 宿主 `DOCKER-USER` 链 | `conn src/32 > 200` + `limit: above 77/min burst 10 mode srcip` |
| macvlan | 容器自己的 `INPUT` 链 | v4 + v6 双链阈值 200 / 77-per-min |
| ipvlan | 容器自己的 `INPUT` 链 | v4 + v6 双链阈值 200 / 77-per-min |

- 9 个受控字段的 `data-val-*` 全部正确渲染（4 int / 3 enum / 2 bool）
- 11 组绕过前端的非法 POST 全部被服务端拒（HTTP 400），`settings.json` **完全未变**
- 「全部恢复默认」正确**绕过**校验并清空文件；重启后回落到代码默认值
- `log_level` 保存后立即出现 DEBUG 日志且容器未重启
- 浏览器 console 无 CSP 违规、无 JS 错误
- 三份 compose 的 `image` / `capabilities` / `volumes` / `tmpfs` / `environment`
  一致性由 `tests/network_mode_test.sh`（NM-30~NM-36）强制校验

---

## 已知环境限制（非代码问题）

`192.168.50.63` 是 virtio 虚拟网卡，其虚拟交换层对 ipvlan「同 MAC 多 IP」的
CAM/ARP 学习不可靠 —— ipvlan 容器对宿主和同网段机器都不可达（ARP `FAILED`、
ping 100% 丢包）。这正是 compose 注释里预告的「出现该现象就改用
`docker-compose.macvlan.yml`」。ipvlan 的功能层已用容器内 curl 完整验证。

---

## 升级

```bash
docker compose pull
docker compose up -d
```

或直接改 `.env` 里的 tag 后 `docker compose up -d`。

宿主网络模式（`network_mode: host`）下 500 / 4500 / 8443 需空闲。

完整变更清单见 [docs/CHANGELOG.md](CHANGELOG.md) 的 v2.87 小节。
