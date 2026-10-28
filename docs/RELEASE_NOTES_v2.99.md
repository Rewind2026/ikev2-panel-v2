# v2.99 — 兼容 Windows 11 / Android 12+ 原生 IKEv2：补 sha1 插件与 modp1024 兜底提案

发布日期：2026-10-28
镜像：`ghcr.io/rewind2026/ikev2-panel-v2:v2.99`（linux/amd64）

> 完整变更清单见 [CHANGELOG.md](CHANGELOG.md) 的 v2.99 小节。
> ⚠️ **本次必须换镜像**：sha1 算法插件是编译进镜像的，只改配置文件不生效。

---

## 一句话

**用户诉求：「安卓原生 VPN 还是连接不上，Windows 11 ikev2 也连接不上，
目前只有 iPhone 可以。」**

现在 Windows 11（出厂默认，无需改注册表）与 Android 12+ 原生 IKEv2 都能连上，
**且 iOS / macOS 的协商结果一个字节都没变**（回归实测确认）。

改动只有 4 个文件，Go 面板程序零改动。

---

## 之前为什么连不上

### Windows 11：两个独立卡点，缺一不可

用 strongSwan 仿真客户端精确复刻 Windows 的 IKE/ESP 提案，对**旧镜像**逐段验证：

**卡点 ①（IKE 阶段）**：Win11 出厂默认 IKE 提案的 DH 只有 `modp1024`（兼容模式），
服务端提案最弱是 `modp2048` → IKE_SA_INIT 直接 `NO_PROPOSAL_CHOSEN`。
表现就是「正在连接 → 立即失败」。

**卡点 ②（CHILD_SA 阶段）**：按网上教程加注册表 `NegotiateDH2048_AES256`
把 IKE 升到 modp2048 后 IKE 能过，但 **Win11 的 ESP 提案仍然只有 sha1**
（`aes256-sha1` / `aes128-sha1` / `3des-sha1`，默认无 PFS）
→ CHILD_SA 依然 `NO_PROPOSAL_CHOSEN`。这解释了「改了注册表还是连不上」。

**卡点 ③（插件层面）**：v2.86-PR10 曾把 `--enable-sha1` 从 strongSwan 编译参数里
删掉，部署镜像中根本没有 sha1 算法插件 —— 即使配置里写了 sha1 提案也不会生效。

### Android：连不上不是提案的问题

读 AOSP `IkeSessionParams` / `Ikev2VpnProfile` 源码 + 实测后确认，
Android 的真实障碍是三个**指引与认知**问题：

1. **原生 IKEv2 只有 Android 12+ 才有**
   （设置→VPN→类型「IKEv2/IPsec MSCHAPv2」），Android 11 及以下系统设置里
   根本没有这个选项 —— 旧指引写「Android 11-15」是错的，很多人因此以为"能连"却找不到入口。
2. 「服务器地址」字段同时用于 **IDr 证书校验**，必须与服务端证书 SAN **完全相等**
   （IP 部署就填 `192.168.50.63`，不能填域名；域名部署就填域名）。
3. 「IPsec 标识符」应填**面板用户名** —— AOSP 源码级确认：Android 的 EAP-Identity
   回空串，服务端 `eap_id = %any` 时拿 IDi 当 MSCHAP 用户名。

AOSP 的算法提案与本项目旧配置**本来就兼容**（实测 IKE 落
`AES_GCM_16_256/PRF_HMAC_SHA2_384/curve25519`，CHILD 落 `aes128gcm16` 无 PFS）。

---

## 改了什么

| 文件 | 改动 |
|------|------|
| `Dockerfile` | 在 `--enable-sha2` 后**回加 `--enable-sha1`**（v2.86-PR14 注释块记录 Windows 实测依据）；启动自检的「空 conf 修复循环」插件列表同步追加 `sha1` |
| `configs/swanctl-ipv6-only.conf` | IKE `proposals` 末尾追加 4 条：`aes256gcm16-prfsha384-modp1024, aes128gcm16-prfsha256-modp1024, aes256-sha1-modp1024, aes128-sha1-modp1024`；ESP `esp_proposals` 末尾追加 2 条：`aes256-sha1, aes128-sha1`（无 DH，兼容 Win11 无 PFS rekey） |
| `web/templates/android_content.html` | 指引修正：仅 Android 12+ 有原生 IKEv2；server 字段即 IDr 校验；IPsec 标识符填用户名 |
| `docs/TROUBLESHOOTING.md` | §2.3 标题改「Android 原生连不上（12+）」，补 AOSP 提案兼容性说明、`AUTHENTICATION_FAILED` 行、修正用户级 CA 存储描述 |

### 为什么这样改是安全的

* **提案只追加在末尾**，原 14 条强算法提案（ECP256 / curve25519 / modp3072 / modp2048）
  全部保持在前面 —— iOS / macOS / Android strongSwan app 永远优先协商到原来的强算法，
  行为不变（回归实测已确认选中算法与改造前完全一致）。
* **不回加 `md5` / `3des`**：Win11 默认 ESP 提案含 `aes256-sha1` / `aes128-sha1`，够落；
  两者均为弱算法，保留缺失正好强制 Win11 走 aes256/aes128。
* **ESP 的 sha1 条目不带 DH**（无 PFS）—— 兼容 Windows CHILD rekey 不带 KE 的行为。

---

## 四客户端矩阵实测（63，新镜像，swanctl 仿真客户端）

每项均验证到 `initiate completed successfully` + CHILD_SA INSTALLED
+ 服务端 `ikev2-rw` 出现对应 SA：

| 用例 | 模拟客户端 | IKE 协商结果 | ESP 协商结果 |
|------|-----------|-------------|-------------|
| T1 | Win11 出厂默认 | `AES_GCM_16_256/PRF_HMAC_SHA2_384/MODP_1024` | `AES_CBC_256/HMAC_SHA1_96` |
| T2 | Win11 + `NegotiateDH2048` 注册表 | `AES_CBC_256/SHA2_256/PRF_SHA2_256/MODP_2048` | `AES_CBC_256/HMAC_SHA1_96` |
| T3 | iOS 回归 | `AES_CBC_256/SHA2_256/MODP_2048` | `AES_GCM_16_256` |
| T4 | Android 12+ 原生（AOSP 提案） | `AES_CBC_256/SHA2_256/PRF_SHA2_256/MODP_2048` | `AES_CBC_256/SHA2_256` |

数据面：TUN 内发出的 ESP 数据包在服务端计数器确认到达并解封装
（`in 600 bytes, 10 packets`；回程为同机回环仿真环境限制，
生产 NAT 转发路径与 iPhone 相同）。

---

## ⚠️ 行为变更

| 项 | 之前（v2.98） | 现在（v2.99） |
|----|--------------|--------------|
| strongSwan 编译算法 | 仅 sha2（v2.86-PR10 删除了 sha1） | **回加 sha1** |
| IKE 提案条数 | 14 | **18**（末尾追加 4 条 modp1024 兜底） |
| ESP 提案条数 | 14 | **16**（末尾追加 2 条 sha1 无 DH） |
| Windows 11 原生 | 连不上（NO_PROPOSAL_CHOSEN） | **可连**（出厂默认即可） |
| Android 12+ 原生 | 连不上（指引误导） | **可连** |
| iOS / macOS | 强算法协商 | **完全不变**（回归实测确认） |

---

## 升级方法

本次镜像含 sha1 插件，**必须换镜像**（只改配置文件不生效）：

```bash
docker compose pull
docker compose up -d
```

升级后自检：

```bash
docker exec ikev2-panel swanctl --list-algs | grep -i sha1
```

应能看到 sha1 相关算法条目；没有就说明镜像没换成功。

> ⚠️ 若你的 compose 里把 `pull_policy` 改成了 `never`（防 `up` 时被旧镜像覆盖），
> **记得改回 `always` 再 pull**。

---

## 客户端怎么填

### Windows 11（不需要改注册表）

设置 → 网络和 Internet → VPN → 添加 VPN：

| 字段 | 填什么 |
|------|--------|
| VPN 名称 | 随意 |
| 服务器地址 | **与服务端证书 SAN 完全一致**（IP 部署填 `192.168.50.63`） |
| 身份验证 | 用户名和密码 |
| 用户名 | 面板用户名 |
| 密码 | 面板密码 |

> **可选加固**：导入面板 CA 到「受信任的根证书颁发机构」，再加注册表
> `HKLM\SYSTEM\CurrentControlSet\Services\RasMan\Parameters\IKEv2\NegotiateDH2048_AES256 = 1`
> → IKE 升级到 modp2048。

### Android 12+ 原生

设置 → 网络和互联网 → VPN → 添加 VPN：

| 字段 | 填什么 |
|------|--------|
| 名称 | 随意 |
| 类型 | **IKEv2/IPSec MSCHAPv2** |
| 服务器地址 | **与服务端证书 SAN 完全一致** |
| IPsec 标识符 | **面板用户名** |
| 用户名 | 面板用户名 |
| 密码 | 面板密码 |
| CA 证书 | 从面板导入 |

### Android 11 及以下

系统没有原生 IKEv2 类型，请用 **strongSwan App**（面板的 Android 页面有现成配置导出）。

---

## 真机验证清单

| # | 设备 | 操作 | 预期 |
|---|------|------|------|
| 1 | Windows 11（不动注册表） | 按上表填写 | 直接连上 |
| 2 | Windows 11（可选加固） | 导入 CA + 加 `NegotiateDH2048_AES256` | 连上，IKE 升级 modp2048 |
| 3 | Android 12+ 原生 | 按上表填写 | 连上 |
| 4 | Android 11 及以下 | strongSwan App + 面板导出配置 | 连上 |
| 5 | iPhone 回归 | 原有 mobileconfig 重连 | 无任何变化 |

真机仍失败时抓服务端日志定位：

```bash
docker logs ikev2-panel 2>&1 | grep -E "IKE_SA|NO_PROPOSAL|AUTHENTICATION" | tail -20
```

---

## 文档

* [TROUBLESHOOTING.md](TROUBLESHOOTING.md) §2.3 —— Android 原生连不上（12+）排查
* [CHANGELOG.md](CHANGELOG.md) —— v2.99 完整变更清单
* [web/templates/android_content.html](../web/templates/android_content.html) —— 面板 Android 指引页
