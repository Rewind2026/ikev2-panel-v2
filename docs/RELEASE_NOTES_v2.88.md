# v2.88 — UI 质感层：四条轴 + 全站面板语言统一

发布日期：2026-10-20

把面板从「配色接近 EZZON」推进到「元素语言是 EZZON」。**纯前端改动**，
后端逻辑零变更，老配置零影响。

镜像：`ghcr.io/rewind2026/ikev2-panel-v2:v2.88`（linux/amd64）

---

## 这次补上了什么

v2.86 重设计时定的 7 条 EZZON 设计意图，落地后逐条对账发现 3 条根本没兑现：

| 设计意图 | 实际情况 |
|----------|----------|
| 像素级描边 / 光泽感 | 全站 0 处 sheen，27 个 transition 全是 hover 变色 |
| 暗调蓝灰 panel grid | `--ikev2-lane-1..8` 定义了但零规则消费（死变量） |
| 顶栏 HUD 大数字区 | `.topbar-hud` / `.hud-stat` 写完了但模板零引用（死代码） |

加上 2 个 `@keyframes` 全是一次性的（入场淡入、横幅消失），零循环动画 ——
所以界面「很不动」。v2.88 把这四条轴补齐。

### 轴 1 — 切角 / 锐利质感

卡片、按钮、输入框、chip 统一 `clip-path` 斜切（左上 + 右下双向），模拟 EZZON
的斜切键帽。受光边用两层实现：`::before` 画受光边 A，`::after` 配
`mask-composite: exclude` 挖空画受光边 B。按钮加扫光，登录卡加 conic 流光。

`clip-path` 只影响渲染、不改变盒模型，所以切角不引起任何重排。

### 轴 2 — 常驻微动效

网格漂移 26s / 舞台光呼吸 8s / 噪点 12s `steps(1,end)` / 状态脉冲 /
折叠箭头 / 隧道跑光 / HUD 入场脉冲。

### 轴 3 — HUD 数字升级

关键指标改 `tabular-nums`（数字等宽，滚动时不抖）+ 侧栏竖条。

### 轴 4 — 首页「服务就绪链路」8 格轨

首页顶部新增一条 8 格轨，绑 **8 个固定子系统**，格数恒为 8，不随在线用户数
变化。排序即依赖顺序：左半是管理面与配置依赖，右半是数据面 —— 任一格变红，
它右边的格子语义上就不该再是绿的，用户能直接读出「链路在哪断的」。

| # | 格位 | 状态含义 | 跳转 |
|---|------|----------|------|
| 1 | 管理口令 | 默认口令 = 红 | `/account` |
| 2 | IP 池 | 配了但运行态读不到 = 黄 | `#subnet-card` |
| 3 | 服务端证书 | 自签 = 黄（能用但要手动信任） | `#cert-card` |
| 4 | LE 续签 | 续签失败 = 红 / 14 天内到期 = 黄 | `#cert-card` |
| 5 | 阿里云凭证 | 未配 = 灰（只影响 LE + DDNS） | `#aliyun-card` |
| 6 | DNS 同步 | 上次失败 = 红 | `#aliyun-card` |
| 7 | IKE_SA | swanctl 超时 = 红 | 不可点击 |
| 8 | 面板设置 | 全绿才绿，否则黄（`N/8`） | `#settings-card` |

**第 8 格为什么是「全绿才绿，否则黄」**：8 项全部被面板显式设过才给绿。
「部分设过」是**正常运维状态**（用户只改了关心的那一两项），给绿色会让人
误以为「全部都由面板接管」；黄灯读作「还有 N 项在跟 env」，配 `title` 里的
计数是准确信息，给绿灯则是在撒谎。

取值全部来自 `homeData` 已有字段，**Go 侧零改动**。

### 全站面板语言统一

不再只做首页几个格子 —— banner / metric / settings-card / login-card / card /
empty-state / 顶栏 / 拓扑图全部接入同一套「面板零件」语言（`.ez-panel` /
`.ez-face` + 统一语义色 `--ez-accent`）。

### 排版与降级

- `.section-title` 收编所有散落的内联 `letter-spacing`
- **中文标签字距归零** —— `letter-spacing` 会把 CJK 字符逐字拆散，
  「面板设置」被渲染成「面 板 设 置」
- 对比度提升
- `prefers-reduced-motion: reduce` 时全部常驻动效降级为静态

---

## ⚠️ 修掉的 4 个真 bug

### 1. 噪点层洗白深色主题

`feTurbulence` 输出的是**不透明中灰**（各通道均值 ≈0.5，alpha 也是 0.5），
直接当背景图铺满全屏，等于在 `#050816` 的深色底上盖了一层 50% 不透明的灰纱
—— 深色主题当场失效。

headless Chrome 实测（首页截图像素平均亮度）：

| 状态 | luma | 结论 |
|------|------|------|
| 不加背景图 | 14.0 | ✅ 正确的深色底 |
| 裸 `feTurbulence` | 84.5 | ❌ 被洗白 |
| 加 `feColorMatrix` | 24.5 | ✅ 有颗粒感，底色仍是深色 |

`feColorMatrix` 的作用：前 3 行把 RGB 全部钉成纯白（不再继承噪点的灰），
第 4 行 `0.035 0.035 0.035` 把 alpha 压到 ~1.8% 并让它随噪点起伏 ——
这才是「颗粒」而不是「纱窗」。

### 2. 中文标签被字距拆散

见上。全站 computed-style 扫描后补齐 override。

### 3. 顶栏右上角点账号区没反应

弹出菜单原本放在 `.topbar-user-pill` 内，而 pill 带 `clip-path`：

> **`clip-path` 会创建裁剪上下文，把溢出到元素 border-box 之外的后代一起裁掉。**

所以菜单被裁成了一条线，看起来就是「点了没反应」。

修复：菜单移出 pill，成为 `.topbar-user-wrap` 的**兄弟节点**；同时给 wrap
补 `position: relative` 作为定位上下文。账号区（头像 / 用户名 / 齿轮）整块
可点，都能切换菜单。

### 4. 点 lane 快捷入口后，卡片展开无法折叠

早期版本除 `hashchange` 外多听了 `click` + `setTimeout(openTarget, 0)`，
本意是覆盖「点同一个锚点两次」的情况。结果用户点 summary 想把卡片折回去时，
`click` 同样冒泡，`location.hash` 还是上一次留下的旧值，卡片被强行重新展开
—— 表现为「展开后无法折叠」。

修复：**只保留 `hashchange`**，删掉 `click` 监听。

---

## 其他修正

### banner 详情行改由宏内渲染

`Detail` / `DetailTime` 移进 banner 宏的 `.banner-body` 内部。原来它们是
banner 外面独立的 `.ddns-failed-detail`，靠 `margin: -0.5rem` 负边距硬贴在
框边上 —— 实测文字裸露在切角外、一条条 banner 糊成一片。

安全边界不变：`Detail` 走 `{{.Detail}}` 默认转义（**不**经过 `safeHTML`），
可安全承载外部输入（阿里云 API 报错原文）。

### DDNS 兜底文案纠偏

首页 DDNS 兜底分支原文写「详见 `/var/log/charon.log`」—— **指错日志了**。
`charon.log` 是 strongSwan IKE 进程的日志，DDNS 同步是面板自己的 goroutine
跑的，原因只可能在面板日志里。

同时原文说「最近一次同步失败」，但该分支的前提正是「本次进程压根没同步过」，
真实语义是「上次运行失败，且本次尚未重试」。

新文案：

> 上次运行同步失败，本次进程尚未开始重试；具体原因见面板日志（docker compose logs ikev2-panel）

---

## 安全约束（本次未触碰）

| 约束 | 状态 |
|------|------|
| `WriteDDNSConfig` 只能改注释，不能删除 | ✅ `internal/panelstate/` 未改动 |
| `scripts/entrypoint.sh` 不改现有 `grep` 判定逻辑 | ✅ 该文件未改动 |
| `schema_version` 兼容老文件 | ✅ `internal/panelstate/` 未改动 |

本次改动文件全部在 `web/`（模板 + CSS）与 `internal/web/`（1 个文案）范围内，
**不涉及 strongSwan / swanctl / iptables / nftables / 证书 / 数据库**。

---

## 验收

| 项 | 结果 |
|----|------|
| `go build ./...` | ✅ |
| `go vet ./...` | ✅ |
| `go test -count=1 ./...` | ✅ 全绿 |
| `go test -run TestV288 -v` | ✅ 5/5 PASS |
| 15 个新增 CSS 变量存在性 | ✅ |
| 真实浏览器：登录页 / lane 轨 8 格数据 | ✅ 状态与数据一致 |
| 真实浏览器：lane 点击跳转 3 个锚点 | ✅ 全部命中并展开 |
| 真实浏览器：summary 折叠不回归 | ✅ |
| 真实浏览器：首屏带 hash 直接落地 | ✅ |
| 真实浏览器：面板设置前端校验拦截非法值 | ✅ 页面未跳转，错误文案正确 |
| 真实浏览器：合法值保存并持久化 | ✅ 跳转 `/` + flash + 值持久化 |
| 浏览器 console | ✅ 零报错 |

新增 `internal/web/v288_topbar_menu_test.go`，5 个防回归测试：

1. 菜单不在被 `clip-path` 裁剪的 pill 内
2. `.topbar-user-wrap` 是定位上下文
3. 账号区整块可点
4. 真实模板渲染结构正确
5. 真实 HTTP 链路走通 `/account` + `/logout`

---

## 升级

```bash
docker compose pull
docker compose up -d
```

或直接改 `.env` 里的 tag 后 `docker compose up -d`。

宿主网络模式（`network_mode: host`）下 500 / 4500 / 8443 需空闲。

**纯前端改动，无需迁移、无需清缓存**（`assetVersion` cache-bust 会自动生效，
但浏览器若缓存激进可强刷一次）。

完整变更清单见 [docs/CHANGELOG.md](CHANGELOG.md) 的 v2.88 小节。
