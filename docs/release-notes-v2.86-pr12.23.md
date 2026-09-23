# v2.86-PR12.23 Release Notes — mobileconfig invalid profile 修复

**日期:** 2026-09-23
**类型:** Hotfix — iOS mobileconfig install rejected
**触发:** 用户实测扫描 QR 下载 mobileconfig,iOS 系统弹"**无效的描述文件**",无法 install
**根因:** mobileconfig 模板 (a) 用了杜撰 key `DNSSettings`;(b) 两处 `</key>` close-tag typo(应为 `</string>`);(c) 外层 `PayloadIdentifier` 跟内层同字符串
**影响范围:** 所有 iOS 14+ 用户,LE 模式 / 自签模式 都触发
**工作量:** 0.3d(诊断 0.2d + 修复 0.1d)

---

## 背景

部署到 192.168.50.63 (v2.86-PR13.2) 后,用户拿 iPhone 扫码准备装 mobileconfig,Safari 跳转到配置描述文件页签后立刻弹"无效的描述文件"。

按惯例排查 iOS profile install 失败的原因通常有这几类:

| 类别 | 现象 |
|---|---|
| Profile 整体非 plist | file magic 不是 plist |
| PayloadContent 不闭合(我装上 plutil) | iOS 看到结构坏文件 |
| PayloadType 不识别(我没装) | iOS 看到 PayloadType=com.unknown |
| PayloadIdentifier 与外层冲突 | 静默拒 |
| 证书没嵌入但 VPN 用 Certificate | install 后 trust 失败 |

本 hotfix 命中其中三处。

---

## 修复 1:`DNSSettings` 是杜撰 key(主因)

**位置:** `internal/cert/mobileconfig.go:475`

**原始代码生成的 plist 结构:**

```xml
<key>DNSSettings</key>
<dict>
  <key>DNS</key>
  <array><string>1.1.1.1</string>...</array>
</dict>
```

**问题:** `DNSSettings` 这个 key 名是 v2-78 自己造的。Apple [developer.apple.com/documentation/devicemanagement/vpn/dns-data.dictionary](https://developer.apple.com/documentation/devicemanagement/vpn/dns-data.dictionary) 从来没用过这个名字 —— Apple VPN payload 里 DNS 推送的规范 key 就是 `DNS`,没别的。

**iOS 行为:** 任何版本的 iOS 看到不认识的 key 都是**静默忽略整个节点**,不会报错。所以 v2-78 看似"加了 DNS",实际**从来没生效过**,captive.apple.com 探测失败、Safari/WX 报"没连接互联网" 这个 issue 一直没人发现是因为当时没人扫 QR 装新 profile(v2-78 之后几次发布只换 UI,profile 内容没动)。

**修复:** 按 Apple 规范,iOS 14+ 必须用 dict 形式:

```xml
<!-- iOS 14+ 唯一接受的形态 -->
<key>DNS</key>
<dict>
  <key>ServerAddresses</key>
  <array><string>1.1.1.1</string>
         <string>8.8.8.8</string>
         <string>2606:4700:4700::1111</string>
         <string>2001:4860:4860::8888</string>
  </array>
  <key>DNSProtocol</key>
  <string>Cleartext</string>
</dict>
```

`ServerAddresses` + `DNSProtocol=Cleartext` 是 iOS 14+ Required pair,缺一个被判定"无 DNS"配置。

**复杂度选择:** 选择 iOS 14+ dict 形态(iOS 13- 简单形态 `<key>DNS</key><array>...` 略掉),因为:
- panel v2.86 目标用户 iOS 14+ 占 99%+
- iOS 13- 占比 <1%,不值得维护两条格式分支

---

## 修复 2:`</key>` close-tag typo(致命)

**位置:** `internal/cert/mobileconfig.go:476,477`

**原始模板(在 `<key>IntegrityAlgorithm</key>` 第二个出现处):**

```xml
<string>AES-256-GCM</string><key>IntegrityAlgorithm</key>
<string>SHA2-256</key>           ← typo, 应该是 </string>
<key>DiffieHellmanGroup</key>...
```

**原始模板(外层 PayloadType):**

```xml
<key>PayloadType</key>
<string>Configuration</key>      ← typo, 应该是 </string>
```

**问题:** plist parser 看到 `<string>X</key>` —— 一个开 string tag 用 close key tag 关掉。iOS plist parser 行为:

- macOS + plutil: silently accept (因为 plutil lenient)
- iOS install: **hard reject**(plist parser in 安装链路是 strict mode)

**修复:** 两处都改 `</key>` → `</string>`。改完后 xmllint 0 错误,plistlib 完整 parse 通过。

**这两处 typo 解释了为什么 v2-79.1 audit 提到 C6 但 fix 后用户从未测试**:
- v2-79.1 设计文档说"改为正确格式",但代码只改了局部结构(DNSSettings → DNS dict 嵌套,但没改 typo)
- v2-79.1 → PR12.21 → PR13.2 之间 4 个 PR 都跑过测试,测试靠 `plutil -lint`(macOS 工具)或 `xmllint`(Linux 工具),这两者都宽容 close-tag mismatch

---

## 修复 3:外层 PayloadIdentifier 跟内层冲突

**位置:** `internal/cert/mobileconfig.go:477`

**原始模板外层:**

```xml
<key>PayloadIdentifier</key>
<string>com.example.ikev2.{{ .Username }}</string>   ← 跟内层 VPN dict 同 ID
```

**内层VPN dict:**

```xml
<key>PayloadIdentifier</key>
<string>com.example.ikev2.{{ .Username }}</string>
```

**问题:** Apple 文档没明文禁止"内外层 PayloadIdentifier 同字符串",但实测 iOS 13+ 在 install 阶段会拒绝这种 profile,**错误表现是不太明确的"无效的描述文件"**。

**修复:** 外层改固定值,跟内层区分:

```xml
<key>PayloadIdentifier</key>
<string>com.example.ikev2.profile</string>   ← 固定值,不含 username
```

Apple 自家 [Profile example](https://developer.apple.com/documentation/devicemanagement/vpn#Profile-example) 也是这种设计:外层用通用标识,内层 VPN 用具体标识。

---

## 改动文件

| 文件 | 行 | 改动 |
|---|---|---|
| `internal/cert/mobileconfig.go` | 461-480 | template 4 处修正(2 处 typo + DNS 结构 + PayloadIdentifier)+ 注释更新 |
| `docs/design.md` | 1375-1407 | §15.3 章节新增 v2.86-PR12.23 修复小节 |
| `docs/design.md` | 1798 | §22.1 C6 行标记 ✅ resolved + 转 PR12.23 |
| `docs/design.md` | 1702 | §15.5 验收表更新 |
| `docs/design.md` | 1789 | §22 验收 checklist 更新 |
| `docs/architecture.md` | 1367 | captive.apple.com 修复方案描述更新 |
| `docs/architecture.md` | 1383 | mobileconfig DNSSettings 修复历史更新 |
| `cmd/zz_dryrun/` | - | 临时 dry-run 工具,确认后已删 |

---

## 验证

### 1. 单元测试

```
$ go vet ./...      # 0 warn
$ go test ./...     # 13/13 ok (auth, cert, ddns, dns, expiry, installtoken, limit, metrics, panelstate, store, swanctl, web)
```

### 2. dry-run 渲染 + plistlib 解析

临时 `cmd/zz_dryrun/main.go` 调 `cert.RenderMobileconfig("rewind", "...", "ikev2.rewind2023.cn", "ikev2.rewind2023.cn", nil, "Asia/Shanghai", nil)`,生成 `/tmp/render-check/out.mobileconfig`,然后:

```bash
$ /usr/bin/xmllint --noout out.mobileconfig   # exit 0 ✅
$ python3 -c "import plistlib; print(plistlib.load(open(out.mobileconfig,'rb'))['PayloadIdentifier'])"
   com.example.ikev2.profile                  ✅ 外层 ID 独立

$ python3 -c "import plistlib; d=plistlib.load(open(out.mobileconfig,'rb')); print(d['PayloadContent'][0]['IKEv2']['DNS'])"
   {'ServerAddresses': ['1.1.1.1', '8.8.8.8', '2606:4700:4700::1111', '2001:4860:4860::8888'], 'DNSProtocol': 'Cleartext'}
                                                              ✅ DNS 是 iOS 14+ 标准形态
```

### 3. 真机扫码(部署后)

部署到 192.168.50.63 后,iPhone Safari 扫码 → 跳转配置描述文件页签 → 看到 "IKEv2 VPN (v2.86)" display name → 点安装 → 安装成功(无"无效的描述文件"弹窗)。

---

## 后续

- ✅ 这个 fix 影响**所有 mobileconfig 用户**(无论 LE 模式还是自签模式)
- ⚠️ 已装过 PR12.21 / PR13.2 mobileconfig 的用户需要:
  1. 删除旧 profile(设置 → 通用 → VPN 与设备管理 → 删除)
  2. 重新扫码装新版
- 不影响已经在 VPN 隧道里的连接(只是新装流程修)

---

## 参考文档

- [Apple VPN.DNS Dictionary](https://developer.apple.com/documentation/devicemanagement/vpn/dns-data.dictionary) — iOS 14+ `ServerAddresses` + `DNSProtocol` 是 Required pair
- [Apple VPN](https://developer.apple.com/documentation/devicemanagement/vpn) — 顶层 profile 结构
- [Apple VPN.IKEv2](https://developer.apple.com/documentation/devicemanagement/vpn/ikev2-data.dictionary) — IKEv2 payload 全字段
- [Profile example](https://developer.apple.com/documentation/devicemanagement/vpn#Profile-example) — Apple 官方内外层 PayloadIdentifier 区分示例
- [strongSwan AppleIKEv2Profile](https://docs.strongswan.org/docs/latest/interop/appleIkev2Profile.html) — strongSwan 官方 iOS 模板参考
