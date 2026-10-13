# v2.86-pr24 — 前端文本选择策略 + eye 按钮按压错位 + 文档体系重组

`v2.86-pr23p` 之后攒下的 6 个提交一次性定版。范围跨三块：一次全站 UI 审计的收尾、两处真实 UI 缺陷修复、文档体系从 52 份重组为 5 份。

镜像：`ghcr.io/rewind2026/ikev2-panel-v2:v2.86-pr24`（多架构 linux/amd64 + linux/arm64）

---

## ⚠️ 升级前必看

| 变更 | 影响 |
|------|------|
| **页面文字默认不可选中** | **用户可见，交互有变化。** 界面装饰（导航、按钮、标题、卡片头）双击不再误选一片。**数据仍可复制**：初始密码、服务器地址、swanctl 命令、证书参数、报错详情、审计日志、配置项值、以及所有输入框。如果你之前习惯拖选按钮上的文字，现在需要从相邻数据区复制 |
| 初始密码单击即全选整个密码 | 之前双击只选中一段，容易漏掉前后字符 |
| `/data` 卷名固定为 `ikev2-panel-data` | 目录改名不再静默产生空卷。**如果你之前的数据在旧命名卷里**，见下方「升级步骤」第 3 条 |

其余变更（二次确认改走 `data-confirm`、mobileconfig 失败改 PRG）对外行为不变，但修掉了实际不生效的逻辑 —— 详见下文。

---

## 修复

### 二次确认从来没生效过（P0）

模板里 5 处破坏性操作用的是 `onclick="return confirm(...)"`。而 CSP 是 `script-src 'self' 'nonce-…'`，**不含** `'unsafe-inline'` / `'unsafe-hashes'` —— 浏览器直接拒绝执行内联事件处理器。

结果：点「删除用户」不需要确认就真删，点「停用」直接踢线。这不是"体验不好"，是**安全控制形同虚设**。

已全部改为 `data-confirm="确认文案"`，在既有 nonce 脚本块里加事件委托统一处理。**没有为了兼容内联而放松 CSP** —— 放宽 CSP 会连带放开 XSS 面。

### banner 反射型 XSS（P0）

新增 `MsgHTML` 字段承载**字面量**应用标记，用户可控值仍走 `.Msg` 自动转义。渲染时按字段分流，外部 API 报错继续留在 `.Msg`。

### mobileconfig 校验失败返回裸文本页（P0）

表单校验失败时直接写 400 + 纯文本，用户看到的是浏览器原始报错，已填内容全丢。改为 PRG：303 重定向 → flash 携带错误与草稿 → 回到表单页回填并自动展开对应 `<details>`。草稿经 JSON 编码存在服务端 session，不进 URL。

### eye 按钮按压错位

点「显示/隐藏密码」时图标整体下坠约 18px 再弹回。不是 JS 问题，是 CSS 层叠冲突：

```css
.password-toggle{ transform:translateY(-50%); }   /* 垂直居中 */
button:active    { transform:translateY(1px);  }   /* 全局按压动效 */
```

`button:active` 特异性 `(0,1,1)` 高于 `.password-toggle` 的 `(0,1,0)`，`:active` 一命中就把 `translateY(-50%)` **整条覆盖**，居中失效。该按钮的 `transition` 不含 `transform`，所以位移是瞬时的，iOS 上 `:active` 粘住让现象更明显。

修复分两层，缺一不可：

1. 居中改用 `top/bottom` + `margin-top/bottom:auto`，定位不再经过 `transform`
2. 显式 `.password-toggle:active{ transform:none }` 抵消全局按压位移 —— 只做第 1 层时全局的 1px 仍然生效，而这个图标浮在输入框**内部**，1px 同样被看成错位

### 页面刷新后不在顶部

两个独立原因：`sessionStorage` 里的滚动位置提交表单后没被消费；浏览器原生 `history.scrollRestoration` 没关。两个都修。

---

## 新增

### 文本选择策略：装饰禁选，数据可复制

管理后台双击/拖拽会误选一片界面文字，选中块高亮后要再点一次才能取消，与 GitHub / GitLab / Vercel 后台的观感不符。

没有采用全局禁选 —— 这个面板里大量内容必须能复制。实现方式是「**默认禁 + 白名单放开**」而非逐个禁用：后者漏一处就失效，且新加组件默认仍可被误选。

可复制的：初始密码、服务器地址、swanctl 命令、证书参数、报错详情、审计日志操作详情、配置项值、`<input>` / `<textarea>` / `<select>`。

顺带修掉两个真实的 Safari 兼容 bug：`.dismissable .dismiss` 与 `.settings-card > summary` 的 `user-select:none` 漏了 `-webkit-` 前缀，iOS Safari 会**静默忽略整条策略**。

---

## 文档

- 52 份 → 5 份，按「一个问题一个权威出处」重组
- **数据卷说法纠偏**：原文档写 `/var/lib/docker/volumes/...`，实机 DockerRootDir 是 `/vol1/docker`。卷名原写成 `{项目目录名}_ikev2-data`，实际早已固定为 `ikev2-panel-data`。现在一律用 `docker volume inspect --format '{{.Mountpoint}}'` 动态取
- **备份范围写清**：`backup-restore.sh` 只备份 `panel.db` / `panel-state/` / `le/` 三项，其余明确标注；默认 `DATA_DIR=/data` 是容器内路径，在宿主上跑必须覆盖，否则静默跳过
- 新增 TROUBLESHOOTING §8「数据/证书看不到」、§9.9（eye 按钮错位）、§9.10（版本号显示 `dev` 的原因）

---

## 升级步骤

```bash
# 1. 拉取镜像
docker compose pull

# 2. 更新仓库内的 docker-compose.yml 指向新 tag
#    image: ghcr.io/rewind2026/ikev2-panel-v2:${PANEL_TAG:-latest}

# 3. 如果你之前用的是旧命名卷，先确认当前卷名
docker volume ls | grep ikev2
#   期望: local ikev2-panel-data
#   若显示 ikev2-panel-v2_ikev2-data，说明数据在旧卷里，需先迁移
#   步骤见 https://github.com/Rewind2026/ikev2-panel-v2/blob/main/docs/TROUBLESHOOTING.md §8.2

# 4. 重建（注意用你原来的 project 名，否则容器名会冲突）
docker compose -p <你的project名> up -d --force-recreate

# 5. 验证
docker exec ikev2-panel cat /etc/ikev2-panel-version   # 应显示 v2.86-pr24，不再是 dev
docker exec ikev2-panel ls /data                        # panel.db / ca/ / server/ / le/ 齐全
docker exec ikev2-panel swanctl --list-conns            # 连接已加载
```

> **重建容器会重启 strongSwan，正在连接的 VPN 客户端会断开**，需重新连接。
>
> **不要手动清浏览器缓存** —— 页面 CSS 走 `assetVersion()` cache-bust（基于 mtime），改 CSS 会自动带新 `?v=`。

---

## 提交清单

| 提交 | 内容 |
|------|------|
| `4c13c0a` | docs: 文档体系 52 份 → 5 份，按「一个问题一个权威出处」重组 |
| `81a6b50` | fix: 60 条风险复核后的 A/B 类修复（pr24 落地部分） |
| `9b4d059` | fix(web): P0 二次确认失效 + banner 转义错位 + 表单裸文本页 |
| `ff342ec` | feat(web): 文本选择策略 —— 界面装饰禁选，数据可复制 |
| `ca92df5` | fix(web): eye 切换按钮按压错位 —— transform 定位被全局动效覆盖 |
| `a09da97` | docs: 数据卷说法纠偏 + 记录前端两条易踩约定 |
| `1fee26c` | docs(changelog): v2.86-pr24 定版，补行为变更小节，修子条目撞名 |

---

## 已知待办

- `POST /api/ddns/family` 后端已实现，但**前端无入口**。本版未动。
- 密码策略仍是项目自定义规则，未改 NIST SP 800-63B（有明确裁定记录，不是遗漏）

## 完整变更记录

[docs/CHANGELOG.md](https://github.com/Rewind2026/ikev2-panel-v2/blob/main/docs/CHANGELOG.md)
