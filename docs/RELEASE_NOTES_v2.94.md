# v2.94 — 证书 / IP 段保存不再跳裸文本页，两张卡补上成功失败提示

发布日期：2026-10-21
镜像：`ghcr.io/rewind2026/ikev2-panel-v2:v2.94`（linux/amd64）

> 完整变更清单见 [CHANGELOG.md](CHANGELOG.md) 的 v2.94 小节。

---

## 一句话

**用户反馈「配置证书时，保存失败的提示直接跳转到新页面 这不对吧，
然后客户端 IP 段相关配置保存 清除等 也没有成功失败提示」——
两处都是同一个病根：这两张卡是全项目最后两个还在用 `http.Error` 的写操作，
反馈方式跟其它卡各写各的。**

---

## 症状

### 一、证书保存失败 → 浏览器停在 `/api/cert/save` 显示裸文本

切到 Let's Encrypt、域名留空，点「保存证书配置」，浏览器直接跳到
`https://192.168.50.66:8443/api/cert/save`，整页一行黑底白字：

```
保存失败: letsencrypt 模式必须填域名 (IKEV2_DOMAIN)
```

表单里已填的 Server CN、ACME 邮箱全丢，没有返回路径。

### 二、客户端 IP 段卡完全没有反馈

`handleSubnetSave` / `handleSubnetClear` 成功是裸 `302`（页面上什么提示都没有，
用户不知道自己点的那下生效没有、客户端要不要重新拨号），失败是裸文本错误页。

dev 模式清除那条分支更坑：不重启容器根本不会回到上一段，但界面一声不吭。

---

## 根因：每张卡各写各的响应方式

阿里云卡（v2.86-pr23l）和面板设置卡（批次 D）早就改成了
**flash + 302 + 卡片内联 banner**，cert / subnet 两张卡当时没跟上。

于是 `handleCertSave` / `handleCertClear` / `handleSubnetSave` /
`handleSubnetClear` 是全项目**最后四个还在用 `http.Error` 的写操作** ——
「每张卡各写各的响应方式，漏一处就多一个裸文本页」。

---

## 修复

### 一、抽出 `respondFlashSlot` 作为所有写操作的唯一响应出口

`respondAliyunFlash` / `respondSettingsFlash` 两个近乎重复的实现收敛成：

```go
respondFlashSlot(w, r, slot, kind, msg, extra)
```

两条路径：

| 调用方 | 响应 |
|---|---|
| AJAX / API（`wantsJSON(r)` 为真） | JSON：`ok` / `kind` / `error` / `msg`（`extra` 补端点自己的字段，如 cert 的 `restart_required`） |
| 普通浏览器表单 | flash 写到 session 的指定 slot + `302 → /`，模板在对应卡片内部就近渲染 banner |

`error` 和 `msg` 两个键给同一个值 —— `error` 是 aliyun/settings 一直在用的键，
`msg` 是 cert/subnet 原有契约的键，两个都留着避免打断任何一边调用方。

cert / subnet 各占一个独立 flash slot（`cert` / `subnet`），**互不覆盖**。

### 二、反馈落在自己的卡片里

模板在两张卡的按钮组正下方内联渲染 banner，配合 `layout.html` 的
`initDetailsPersistence()` 恢复卡片展开态和滚动位置 ——
用户停在原地看到反馈，**不跳顶、不白屏、表单内容不丢**。

### 三、顺手修掉的隐患

cert 端点原来的 `Accept` 判定是**精确字符串相等**（`== "application/json"`），
`fetch()` 默认发的 `Accept: */*` 会漏判 → 拿到 302 HTML 而不是 JSON。
现在统一走 `wantsJSON(r)`（`Accept` **包含** `application/json` 即算）。

---

## ⚠️ 行为变更

| 端点 | 之前（浏览器表单） | 现在 |
|---|---|---|
| `POST /api/cert/save` | 失败 400 `text/plain`，成功裸 302 | 一律 `302 → /` + flash |
| `POST /api/cert/clear` | 同上 | 同上 |
| `POST /api/subnet/save` | 失败 500 `text/plain`，成功裸 302 | 同上 |
| `POST /api/subnet/clear` | 同上 | 同上 |

**API 调用方不受影响**：`Accept: application/json` 或
`X-Requested-With: XMLHttpRequest` 仍拿到 JSON。

- 成功：`{"ok":true,"kind":"success","error":"...","msg":"..."}` + cert 额外带 `"restart_required":true`
- 失败：HTTP 400 + `{"ok":false,"kind":"error","error":"...","msg":"..."}`

> 顺带在 [API.md](API.md) 的「加新端点的清单」里立了硬性规矩：
> **面板上有表单的写操作，成功和失败都调 `respondFlashSlot` 收尾，不要用 `http.Error`。**

---

## 回归测试

新增 **12 条**（`internal/web/config_cards_flash_test.go`，全部通过）：

| # | 测试 | 覆盖 |
|---|------|------|
| 1 | `Cert_ValidationFailure_NotBareTextPage` | 用户截图场景：LE 模式缺域名 → 302、CT 非 text/plain、首页有错误文案 |
| 2 | `Cert_Success_ShowsFeedback` | 成功也有提示，且文案含「需 docker compose restart」 |
| 3 | `Cert_Clear_FailureAndSuccess` | 清除的失败与成功两条路径 |
| 4 | `Cert_FlashRenderedInsideCertCard` | banner 渲染在**证书卡内部**，不是页顶 |
| 5 | `Subnet_ValidationFailure_NotBareTextPage` | 非法 CIDR → 302 + 非裸文本 |
| 6 | `Subnet_Success_ShowsFeedback` | 成功提示含「客户端需重新拨号」 |
| 7 | `Subnet_Clear_ShowsFeedback` | 清除有提示 |
| 8 | `Subnet_FlashRenderedInsideSubnetCard` | banner 在 IP 段卡内部 |
| 9 | `FlashSlotsAreIsolated` | cert / subnet 两个 slot 互不覆盖 |
| 10 | `Cert_JSONContract` | JSON 成功分支字段齐全 |
| 11 | `Subnet_JSONErrorContract` | JSON 失败分支 `ok=false` / `kind=error` / 含 `msg` |
| 12 | `Cert_ConfigNotPersistedOnFailure` | 校验失败不落脏配置 |

静态检查：`go build` / `go vet` / `go test ./...` 全部通过。

---

## 实机验证

192.168.50.63（飞牛 fnOS NAS，Docker 28.5.2 / Compose v2.40.3），
**ipvlan 模式 + 固定容器 IP `192.168.50.66`**，
正式镜像 `ghcr.io/rewind2026/ikev2-panel-v2:v2.94`。

### 一、真实 HTTP 提交流程（39 条断言全绿）

用 wget 打真实端到端流程（登录 → 带 CSRF 提交 → 跟回首页抓渲染后的 HTML）：

| 场景 | 响应 | 首页渲染 | 结果 |
|------|------|---------|------|
| **证书保存失败**（LE 模式 + 域名留空） | `302 → /`，CT 非 `text/plain` | 证书卡内 `cert-action-flash` + `banner-danger`，文案「保存失败：letsencrypt 模式必须填域名(IKEV2_DOMAIN)」 | ✅ **不再跳裸文本页** |
| 证书保存成功（自签模式） | `302 → /` | `cert-action-flash` + `banner-success`，文案「证书配置已保存…需 docker compose restart」 | ✅ |
| 证书清除 | `302 → /` | `banner-success`，文案「证书配置已清除」 | ✅ |
| **IP 段保存失败**（非法 CIDR） | `302 → /`，CT 非 `text/plain` | IP 段卡内 `subnet-action-flash` + `banner-danger`，文案「保存失败：ipv4_subnet 不合法…」 | ✅ |
| IP 段保存成功 | `302 → /` | `subnet-action-flash` + `banner-success`，文案「已保存并立即生效…客户端需重新拨号」 | ✅ |
| IP 段清除 | `302 → /` | `banner-success`，文案「已清除…已立即回到 10.10.0.0/24」 | ✅ |
| JSON 失败分支（`Accept: application/json`） | `400` + `{"ok":false,"kind":"error","error":…,"msg":…}` | — | ✅ 契约未丢字段 |
| JSON 成功分支 | `200` + `{"ok":true,…,"restart_required":true}` | — | ✅ |

### 二、反馈确实落在卡片内（不是页顶横幅）

抓首页真实 HTML，按行号定位：

```
395:  <details class="settings-card" id="cert-card">
426:    <form method="POST" action="/api/cert/save">
485:    <div class="banner banner-success dismissable cert-action-flash" …>   ← 在 cert-card 内
505:  <details class="settings-card" id="subnet-card">
540:    <div class="banner banner-success dismissable subnet-action-flash" …> ← 在 subnet-card 内
```

`cert-action-flash`（485 行）落在 `cert-card`（395–504 行）区间内，
`subnet-action-flash`（540 行）落在 `subnet-card`（472–558 行）区间内。

### 三、两个 slot 互不覆盖

`cert` / `subnet` 各占一个 flash slot，连续操作两张卡，
先做的 cert 反馈不会被后做的 subnet 反馈顶掉。

### 四、8 张 lane 卡片仍与实际一致（v2.93 成果未被本轮破坏）

| 卡片 | 显示 | state |
|------|------|-------|
| 管理口令 | 默认 | 🔴 danger |
| IP 池 | `10.10.0.0/24` | 🟢 ok |
| 服务端证书 | 自签 | 🟡 warn |
| LE 续签 | 不适用 | ⚪ off |
| 阿里云凭证 | 未配置 | ⚪ off |
| DNS 同步 | — | 🟡 warn |
| IKE_SA | — | ⚪ off |
| 面板设置 | — | 🟡 warn |

`swanctl --list-conns` → `ikev2-rw: IKEv2, no reauthentication, rekeying every 86400s, dpd delay 30s`（conn 已注册）
`swanctl --list-pools` → `ikev2-pool 10.10.0.1 0 / 0 / 254`

### 五、测试后 63 状态

`panel-state/` 只剩 `INITIAL_ADMIN_PASSWORD.txt`（初始口令横幅还在，符合预期）
+ `ratelimit.json`；测试产生的 `cert.conf` 已清除，`audit_log` 已清空（0 行）。

---

## 升级方法

### 用 docker compose 部署的

```bash
docker compose -f <你的compose文件> pull
docker compose -f <你的compose文件> up -d
```

### 用 1Panel 部署的

1Panel 里点「重建」/「更新」镜像到 `v2.94` 即可。

---

## 升级后自查

```bash
# 1. 切到 LE 模式、域名留空，点「保存证书配置」
#    → 应停留在首页，证书卡下方出现红色横幅，不再跳裸文本页

# 2. 在 IP 段卡点「保存并立即生效」
#    → 应停留在首页，卡片下方出现绿色横幅，提示客户端需重新拨号
```
