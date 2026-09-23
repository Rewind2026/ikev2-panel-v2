# IKEv2 VPN 面板 v2 设计文档（全新）

**日期：** 2026-09-16
**状态：** 设计阶段（待用户审阅）
**里程碑代号：** v2
**目标交付物：** 一份独立的设计文档 + 架构理解，供全新项目开发使用

---

## 0. 写在最前面

**本文档与上一版（v1）彻底脱钩。**

v1 的设计/代码全部作废，本文不引用 v1 的任何模块、命名、决策。开发时**新建仓库**，从零开始。

v1 失败的根本原因：
- 把"商业级/企业级"特性当作"应该做的"，导致过度开发
- 几个根本性架构冲突未在设计阶段发现（Argon2id 与 EAP-MSCHAPv2 协议冲突、自定义 PBES2 与标准工具链冲突）
- 端到端真机验证缺失，等到测试才发现问题

**v2 的核心原则：先做能跑的，再做对的，最后做漂亮的。**

---

## 1. 背景与定位

### 1.1 你现有的 VPN 现状

| 工具 | 协议 | 用途 |
|---|---|---|
| SoftEther | L2TP/IPsec | iOS 设备 |
| 梅林路由器 | PPTP | 公司路由器（仅支持 PPTP） |
| easy-wg | WireGuard | Linux/macOS 桌面 |

**唯一缺口：Android 11+/iOS/macOS/Windows 上的 IKEv2/IPsec。** Android 11+ 默认内置 IKEv2 客户端，是为数不多原生支持 EAP-MSCHAPv2（用户名+密码）的现代平台。

### 1.1.1 关于"免证书"的常见误解（必读）

**EAP-MSCHAPv2 ≠ 免证书**。

IKEv2 协议的"客户端认证"和"服务器认证"是两件**独立**的事：

| 认证方向 | EAP-MSCHAPv2 模式 | 证书模式 |
|---|---|---|
| **服务器 → 客户端** | **必须证书**（防 MITM） | 必须证书 |
| **客户端 → 服务器** | 用户名+密码 | 客户端证书 |

**服务器证书的作用**：让客户端确认"我连的是真正的服务器，不是中间人"。

如果不验证服务器证书：
- 攻击者在咖啡馆架个假 Wi-Fi + 假 VPN 服务器
- 客户端输入用户名密码 → 攻击者拿到密码 → 攻击者用真服务器建立连接 → 全部流量被监听

**这就是为什么"网上说的免证书 IKEv2"是误导**。

证据来源：
- strongSwan 官方 iOS 文档：*"the CA certificate is required on the clients to verify the server certificate"*
- strongSwan 官方 Android 文档：*"The server always has to be authenticated with RSA/ECDSA (even when using EAP-TLS)"*
- strongSwan EAP 教程：所有 EAP 模式都必须配 `local.certs`

### 1.1.2 关于"用户装 CA 证书 = 手机被监控"的误解

**用户级 CA 证书 ≠ 设备级监控能力**。

| CA 等级 | 安装方式 | 能做什么 |
|---|---|---|
| **用户级 CA**（iOS 描述文件 / Android 用户证书） | 普通安装 | 仅验证这一个 VPN 服务器；**无法解密 HTTPS** |
| **设备级 CA**（需要 root 或 MDM） | 需要管理员权限 | **才能解密 HTTPS 流量** |

iOS 10+ 和 Android 9+ 已经强制要求"用户级 CA 不可信 HTTPS"——这意味着普通方式装的 CA **只对 VPN 连接生效，不能用于 HTTPS 中间人**。

**所以 v2 的 CA 证书不会"监控用户手机"**。但用户的心理不安需要文档说明。

### 1.1.3 客户端真实安装流程（iOS 为例）

1. 用户收到 `.mobileconfig` 链接
2. Safari 打开 → iOS 提示"安装 VPN 配置描述文件"
3. iOS 自动导入内嵌的 CA 证书到**用户级**信任区
4. 设置 → 通用 → VPN 与设备管理 → 安装
5. 进入 VPN 开关 → 输入用户名密码 → 连接

**整个流程中 CA 证书只在 VPN 连接时使用，不会用于其他用途**。

### 1.2 v2 的产品定位

**面向 1–50 人的内部 VPN 面板**。特征：

- **不追求商业级**：不假设有付费用户、合规审计、SLA、7×24 运维团队
- **不追求功能丰富**：用户能登录、能下线、能改密码即可
- **不追求漂亮 UI**：能用即可
- **追求：能跑、能修、能扩展**

### 1.3 网络栈决策：IPv6-only 默认（**必读**）

**真实约束**：宿主持 SoftEther 已经监听 IPv4 UDP 500/4500（L2TP/IPsec）。strongSwan **不能再监听 IPv4 UDP 500/4500**，否则 charon 启动失败。

**v2 网络栈设计（修订版）**：

**默认 IPv6-only**——不再"双栈并行"。

- `IKEV2_IPV6_ONLY=true`（默认）：strongSwan `local_addrs` 只配 IPv6 地址
- `IKEV2_IPV6_ONLY=false`（可选）：strongSwan 同时监听 IPv4 + IPv6，**仅当宿主 IPv4 UDP 500/4500 未被占用时启用**

**DNS 客户端选路**：

- DNS 必须配 **AAAA 记录**指向 strongSwan 服务器 IPv6 地址
- 客户端（iOS/macOS/Windows/Android）通过 DNS AAAA 解析到服务器
- **不配 A 记录**——避免客户端优先尝试 IPv4（IPv4 端口被占会失败，浪费 5 秒超时）

**重要前提**：客户端必须支持 IPv6 IKEv2 传输：

| 客户端 | IPv6 IKEv2 |
|---|---|
| iOS 9+ | ✅ |
| macOS 10.11+ | ✅ |
| Windows 10/11 | ✅ |
| Android 11+ 内置 | ✅ |
| Android strongSwan app | ✅（2.3.1+，`ipv6-transport` 选项） |
| Linux strongSwan | ✅（kernel 5.8+ UDP-encap for ESPv6） |

**启动时 IPv6 硬校验**（entrypoint.sh）：

```bash
# 检测 IPv6 公网地址是否存在
IPV6_ADDR=$(ip -6 addr show scope global | grep -v "fe80" | awk '/inet6/ {print $2}' | cut -d/ -f1)
if [ -z "$IPV6_ADDR" ]; then
  echo "FATAL: no global IPv6 address found"
  echo "This project REQUIRES a public IPv6 address"
  echo "Configure IPv6 on the host network first, then retry"
  exit 1
fi
echo "Using IPv6 address: $IPV6_ADDR"
```

**实施优先级**：

1. **必须**：宿主有公网 IPv6 地址
2. **必须**：DNS 配 AAAA 记录指向 strongSwan 服务器 IPv6
3. **必须**：swanctl.conf 配置 IPv6-only 监听（`local_addrs` 只填 IPv6 地址）
4. **必须**：mobileconfig 使用 IPv6 地址（`RemoteAddress` 填 IPv6 字符串）
5. **可选**：如果 IPv4 端口可用，关掉 SoftEther 或改 SoftEther 监听端口后可启用 IPv4 fallback

**为什么不"双栈"**：

- 宿主 IPv4 UDP 500/4500 被 SoftEther 占，strongSwan IPv4 监听必失败
- 即使能绑 IPv4 也会"先抢 IPv4 再切 IPv6"——多 5 秒客户端超时
- IPv6-only 是更简单、更可预测的方案

### 1.4 服务器证书：自签 / Let's Encrypt 二选一

| 选项 | 适用场景 | 优点 | 缺点 |
|---|---|---|---|
| **自签证书**（默认） | 内网 / 不在乎弹警告 | 简单，无外部依赖，零续期成本 | iOS/macOS 首次弹"未验证"警告；mobileconfig 必须内联 CA |
| **Let's Encrypt** | 有公网域名 | 零警告，iOS/macOS 直接信任，无需内联 CA | 需要 80 端口或 DNS API 凭证；每 90 天续期 |

**v2 同时支持两种**。在 `.env` 里通过 `IKEV2_CERT_MODE` 选择：

- `IKEV2_CERT_MODE=self-signed`（默认）：启动时检测/生成自签 CA + 服务器证书
- `IKEV2_CERT_MODE=letsencrypt`：启动时检测 LE 证书，到期前 30 天自动续期

#### 1.4.1 Let's Encrypt 模式详细设计（**新增**）

**工具选型：acme.sh**（**不**自己实现 ACME 客户端）

| 候选 | 决策 | 理由 |
|---|---|---|
| **acme.sh** | ✅ 选 | GitHub 25k+ stars、纯 shell + curl、零依赖、自动注册 cron、HTTP-01 内置、强Swan reload hook 范例多 |
| Lego (Go ACME 库) | ❌ 不选 | 我们不需要 HTTPS 服务端（swanctl 直接读 PEM）；Lego 是库，acme.sh 是 CLI + cron，更省事 |
| CertMagic (Caddy) | ❌ 不选 | 太重，是 HTTPS 服务器，会接管 443 端口 |
| 自研 ACME 客户端 | ❌ 绝不 | 重写轮子、bug 多、用户反馈里的"过度开发"根源 |

**核心流程**：

```
1. 容器首次启动（IKEV2_CERT_MODE=letsencrypt）：
   ├─ entrypoint.sh 检测 IKEV2_DOMAIN 环境变量
   ├─ 调用 acme.sh --issue -d <domain> --keylength 2048 --standalone
   │   （RSA 必须，**不要 ECDSA**，否则 iOS IKEv2 客户端会拒连）
   ├─ 调用 acme.sh --install-cert -d <domain> \
   │        --fullchain-file /etc/swanctl/x509/<domain>.pem \
   │        --key-file /etc/swanctl/private/<domain>.key \
   │        --reloadcmd "swanctl --load-creds"
   └─ acme.sh 自动注册 cron（容器内独立 crond 守护）

2. 续签触发（容器内 crond 每天 02:00 执行）：
   /etc/ikev2-panel/scripts/renew-cert.sh <domain>

3. 续签脚本逻辑（伪代码）：
   a. 检查当前证书有效期 < 30 天？否则跳过
   b. 备份当前证书到 /etc/ikev2-panel/certs-backup/<timestamp>/
   c. 调用 acme.sh --renew -d <domain> --keylength 2048
   d. 验证新证书有效期 ≥ 30 天？
      ├─ 是 → swanctl --load-creds + 清理 LAST_RENEW_FAILED 标志
      └─ 否 → 回退到上次的备份证书 + 设置 LAST_RENEW_FAILED 标志
              + 写日志（管理员面板可见红字告警）

4. 管理员面板健康检查（每 60s goroutine）：
   ├─ 检测 LAST_RENEW_FAILED 文件 → 首页顶部红字横幅告警
   ├─ 检测证书有效期 < 14 天 → 日志告警
   └─ 不发邮件 / 通知（v2 不做通知系统）
```

**容器内 cron 守护**：使用 `cron` deb 包（`apt-get install -y cron` + `cron`），entrypoint.sh 启动 `cron` 在后台。

**已知陷阱（参考 randyapps 实战）**：

| 陷阱 | 现象 | 修复 |
|---|---|---|
| **iOS 拒绝 ECDSA 证书** | iPhone/iPad 连接 IKEv2 静默失败，无报错 | 强制 `--keylength 2048`（RSA） |
| **`rereadall` 不检测覆盖** | 续签后 `ipsec rereadall` 不重读证书，VPN 失联 | 改用 `swanctl --load-creds`（swanctl 体系原生 reload） |
| **acme.sh standalone 占用 80 端口** | 续签时与已启动的 HTTP 冲突 | 续签脚本临时停掉 nginx/caddy（v2 不用 nginx，无此问题）；standalone 仅在证书首次申请时短暂使用 |
| **强Swan reload 不重读证书** | `swanctl --load-all` 不重读 `local.certs` | 续签脚本显式调用 `swanctl --load-creds`（仅加载证书），再 `--load-all`（加载全部） |

**Go 端职责**：

| 模块 | 职责 |
|---|---|
| `internal/cert/manager.go` | 检测模式（自签 vs LE）、启动期调用 `acme.sh --issue`、续签期触发 `renew-cert.sh` |
| `internal/health/certcheck.go` | 每 60s 检查 `LAST_RENEW_FAILED` 标志 + 证书有效期，渲染告警横幅 |
| `/admin` 首页 | 红色横幅："证书续签失败，已回退到上次证书，请检查日志" |

**为什么不直接用 certbot**：

- certbot 需要 Python + 系统包，镜像会变大
- acme.sh 纯 shell，体积小、依赖少
- 容器内 init 系统简单，acme.sh 的 cron 注册比 certbot 的 systemd timer 更可控

**完整续签脚本示例**（`/etc/ikev2-panel/scripts/renew-cert.sh`）：

```bash
#!/bin/bash
# 续签失败自动回退（v2 关键设计）
set -e

DOMAIN="$1"
BACKUP_DIR="/etc/ikev2-panel/certs-backup"
CERT_PATH="/etc/swanctl/x509/${DOMAIN}.pem"
KEY_PATH="/etc/swanctl/private/${DOMAIN}.key"
FAIL_FLAG="/etc/ikev2-panel/certs-backup/LAST_RENEW_FAILED"
TIMESTAMP=$(date +%Y%m%d-%H%M%S)

mkdir -p "$BACKUP_DIR"

# 检查当前证书是否需要续签（< 30 天）
check_days_left() {
    openssl x509 -in "$1" -noout -checkend 2592000 2>/dev/null && echo "ok" || echo "renew"
}

if [ "$(check_days_left "$CERT_PATH")" = "ok" ]; then
    echo "[renew] 证书还有 >30 天，跳过"
    exit 0
fi

# 备份当前证书
mkdir -p "$BACKUP_DIR/$TIMESTAMP"
cp "$CERT_PATH" "$BACKUP_DIR/$TIMESTAMP/fullchain.pem"
cp "$KEY_PATH" "$BACKUP_DIR/$TIMESTAMP/privkey.key"
echo "[renew] 已备份证书到 $BACKUP_DIR/$TIMESTAMP"

# 续签
if acme.sh --renew -d "$DOMAIN" --keylength 2048 --force; then
    # 验证新证书
    if [ "$(check_days_left "$CERT_PATH")" = "ok" ]; then
        swanctl --load-creds
        swanctl --load-all
        rm -f "$FAIL_FLAG"
        echo "[renew] 续签成功，新证书已加载"
        exit 0
    fi
fi

# 续签失败 → 回退到备份
echo "[renew] FATAL: 续签失败，回退到备份证书" >&2
cp "$BACKUP_DIR/$TIMESTAMP/fullchain.pem" "$CERT_PATH"
cp "$BACKUP_DIR/$TIMESTAMP/privkey.key" "$KEY_PATH"
swanctl --load-creds
swanctl --load-all
touch "$FAIL_FLAG"
exit 1
```

### 1.5 部署兼容性：ESP 协议与内核 IPsec 后端选型（**新增**）

#### 1.5.1 真实约束：ESP（IP 协议号 50）在容器化 VPS 上的可用性

strongSwan 默认通过 Linux 内核的 **XFRM 子系统** 实现 IPsec：ESP 包以**原始 IP 协议号 50** 在网络层传输，不走 UDP。

**问题**：很多 VPS（OpenVZ 7+、部分 LXC、部分 KVM 容器化方案）宿主内核**不向容器 namespace 转发 ESP 包**。Docker 默认 bridge 网络下，charon 能收到 UDP 500/4500，能完成 IKE 协商，但 SA 建立后**所有 ESP 数据包丢失**，客户端表现为"连接成功但无法传输任何流量"。

Docker `network_mode: host` 理论上可绕过 ESP 转发问题，但会带来：
- 与 SoftEther（IPv4 UDP 500/4500）的端口冲突（§1.3 已分析）
- 失去 Docker 网络隔离与可观测性
- 在某些 VPS（共享宿主内核的容器化方案）上 host 网络本身也救不了 ESP

**官方背书**：[strongSwan Cloud Platforms 文档](https://docs.strongswan.org/docs/latest/howtos/cloudPlatforms.html) 对此的官方推荐：

> "Container-virtualized environments often do not offer a working IPsec stack to the software in the container. Therefore the `kernel-libipsec` interface might have to be used instead."

#### 1.5.2 三个候选方案对比

| 方案 | ESP 兼容性 | Docker | 性能 | 复杂度 |
|---|---|---|---|---|
| **A. 宿主机 systemd 服务**（无容器） | ✅ 全支持 | ❌ | 🟢 最优 | 中（需 install.sh + 2 个 unit） |
| **B. Docker + libipsec + bridge 网络**（**v2 默认**） | ✅ **全兼容** | ✅ | 🟡 用户态软加密（弱于内核 10–30%，50 人场景无感） | **低**（Dockerfile 加编译选项 + compose 改 1 个字段） |
| C. Docker `network_mode: host` + 内核 XFRM（v2 旧设计） | ⚠️ 部分 VPS 不行 | ✅ | 🟢 最优 | 0 改动（已知坑） |

#### 1.5.3 v2 选 B 方案：Docker + kernel-libipsec 插件 + UDP 4500 强制

**思路**：[strongSwan kernel-libipsec 插件](https://docs.strongswan.org/docs/latest/plugins/kernel-libipsec.html) 提供**用户态 ESP 实现**：

> "The plugin enforces UDP encapsulation (NAT-T). ESP packets are received are decrypted by libipsec and then injected via TUN device."

即把 ESP 包**封装到 UDP 4500 上传输**，完全绕开内核 XFRM。Docker 默认 bridge 网络即可工作，**不再需要 `network_mode: host`**，也不再依赖宿主内核 ESP 转发。

**具体改动**：

1. **Dockerfile**：strongSwan 编译时加 `--enable-kernel-libipsec`（与 `--enable-kernel-netlink` 共存，netlink 仅用于路由）
2. **docker-compose.yml**：`network_mode: host` → 默认 bridge 网络 + 端口映射
3. **capabilities**：去掉 `SYS_ADMIN`（XFRM 不再需要），保留 `NET_ADMIN`（TUN 设备 + 路由表）
4. **entrypoint.sh**：容器内自己做 IPv4 MASQUERADE（强Swan 测试套件标准做法）

**性能边界**（[官方文档](https://docs.strongswan.org/docs/latest/plugins/kernel-libipsec.html)明示）：

> "`libipsec` is not intended for scenarios with high amounts of traffic or high burst traffic. It is not optimized for performance and buffers each packet in memory."

**v2 推荐范围**：≤20 用户 + ≤5 Mbps/用户 = 总带宽 ≤100 Mbps。**超出此范围的用户应该选方案 A（宿主机 systemd）**。这点会在 README 明确标注。

#### 1.5.4 专家模式：仍然支持 host 网络

保留 `IKEV2_NETWORK_MODE=host` 环境变量作为可选开关，**仅在 KVM/Xen 硬件虚拟化 + 已知宿主 ESP 转发正常**的 VPS 上使用。Dockerfile 同时编译 `kernel-libipsec` 与 `kernel-netlink`，运行时由 strongSwan `charon.plugins.kernel-libipsec.use_netlink` 选择后端。

**默认行为**：`IKEV2_NETWORK_MODE=bridge` + libipsec 后端（兼容性优先）。

#### 1.5.5 与 §1.3 IPv6-only 默认的协同

libipsec 强制 UDP 4500 封装后，原 IPv6-only 设计**不受影响**——客户端仍只连公网 IPv6 的 UDP 500/4500。只是 ESP 数据包走用户态而非内核 XFRM。mobileconfig、Swanctl 配置、限速脚本都不需要改。

---

### 1.3 用户场景

**单一角色：管理员**（你自己或一两个信任的人）

管理员做的事：
1. 第一次：拉起服务（一次 `docker compose up -d`）
2. 日常：登录面板 → 新增/删除/停用用户 → 用户扫码装配置
3. 偶尔：看当前在线用户；改管理员密码；备份证书目录

**普通用户做的事**：
1. 管理员把 mobileconfig 链接发给我（或扫 QR 码）
2. 安装到手机 → 输入用户名密码 → 连上

**不会做的事**（v2 明确不做）：
- 自助注册
- 自助改密码
- 邀请码、邀请链接
- 多管理员账号/权限分级
- 计费、付费、订阅
- 流量统计图表、报表导出

---

## 2. 目标 & 非目标

### 2.1 目标

- **一次 `docker compose up -d` 就能跑起来**，包含强Swan + 面板 + 默认管理员账号
- 支持 **1–50 用户** 的 IKEv2 EAP-MSCHAPv2 接入
- 跨平台客户端配置一键下发：
  - iOS/macOS：**未签名 `.mobileconfig`** + QR 码
  - Android/Windows/Linux：**手写配置文件** + QR 码（strongSwan / Windows VPN 设置 / 网络管理器）
- Web 管理后台支持：登录、新增/删除/停用/启停用用户、改密码、查看当前连接、下载用户配置
- 服务器自签证书自动生成，**证书到期前 7 天告警**（写日志，不发邮件）
- **每用户限速**（Mbps 级，0 = 不限速，默认 10 Mbps）
- **账户过期时间**（可选，0 = 永不过期；过期后自动断连 + Go 程序定时清理）
- **流量统计**（每用户累计 bytes_in / bytes_out，管理员面板可见）
- 数据备份：一个 `backup.sh` 脚本（cron 跑），tar 整个数据目录
- 端到端真机测试覆盖：Linux strongSwan 客户端 + iOS 内建 + Android 内建

### 2.2 非目标（明确不做）

| 不做 | 原因 |
|---|---|
| **客户端证书认证（EAP-TLS）** | 用户抵触"装证书"，心理阻力远大于技术收益 |
| **mobileconfig CMS/PKCS#7 签名** | iOS/macOS 接受未签名，仅弹警告 |
| **自定义私钥加密（PBES2 自定义头）** | 必须用标准工具链，自造轮子必出问题 |
| **VICI HTTP API 封装** | `swanctl --load-all` 一条命令搞定 |
| **JWT + refresh token** | 单一管理员，cookie + bcrypt 完全够 |
| **多管理员 / RBAC / 审计日志入库** | 一个人管到底，journald 留日志就行 |
| ~~**配额 / 限速 / 流量统计**~~ | 改为：每用户限速（Mbps）+ 账户过期时间 + 流量累计（必要）；月度配额 / 实时带宽图（不做） |
| **多节点集群 / 中央管控** | 单服务器足以支撑目标规模 |
| ~~**Let's Encrypt 自动签发**~~ | **加回作为"高优先级服务器证书选项"**：用户既然有域名，LE 证书提供零警告体验。ACME 客户端实现 ~150 行 Go |
| ~~**IPv6 优先 / 双栈并行优化**~~ | **加回**：宿主持 SoftEther 已占 IPv4 UDP 500/4500；strongSwan 必须监听 IPv6 才能共存。strongSwan 同时监听 IPv4 + IPv6，由客户端 Happy Eyeballs 选可用 |
| **Docker 多阶段构建 + s6-overlay** | 直接用 strongSwan 官方镜像 + 单 stage |
| **swanctl.conf 热加载 VICI 接口** | 直接 `exec.Command("swanctl", "--load-all")` |
| **mobileconfig 自动通知 / 邀请链接** | 管理员自己复制 URL 或截图发微信 |

### 2.3 范围控制铁律

> **任何没出现在 §2.1 目标里的特性 = 不做。**
>
> 任何"未来可能需要"的特性 = 不做（除非有用户明确要求）。
>
> **代码 = 目标的直接实现，不多写一行。**

---

## 3. 核心架构决策（取舍表）

| # | 决策 | 选择 | 拒绝 | 理由 |
|---|---|---|---|---|
| 1 | 认证方案 | **EAP-MSCHAPv2**（用户名+密码） | EAP-TLS（证书） | 用户抵触"装证书"；MSCHAPv2 在所有目标平台原生支持 |
| 2 | 密码存储 | **明文**存数据库 | Argon2id / bcrypt / NTLM-Hash | 服务器在你手里；明文 = 与 swanctl secrets 完美一致；几十人妥协 |
| 3 | 服务器证书 | **自签证书**，强Swan `pki` 生成 | Let's Encrypt | 省去 ACME/DNS API；自签在 mobileconfig 内联根证书即可 |
| 4 | 私钥加密 | **不加密**（0600 权限保护） | 标准 PBES2 / 自定义 PBES2 | 私钥只在容器内，外部拿不到；多一层加密 = 多一处 bug |
| 5 | HTTP 框架 | **Go 1.22+ `net/http` ServeMux** | gin/echo/chi | 标准库够用；少一个依赖 |
| 6 | 模板引擎 | **Go 标准库 `html/template`** | React/Vue/Alpine.js | 服务端渲染，单 HTML 即可 |
| 7 | 数据库 | **SQLite**（`modernc.org/sqlite` 纯 Go） | Postgres/MySQL/JSON 文件 | 单文件、零运维、纯 Go 无 CGO |
| 8 | Session | **cookie + 服务端 session 表** | JWT / OAuth | 单服务器；JWT 无撤回机制 |
| 9 | 密码哈希（管理员） | **bcrypt** | argon2id / scrypt / md5 | Go 标准库 `crypto/bcrypt` 几行代码搞定 |
| 10 | TLS（HTTPS） | **Go 内嵌 TLS，自签证书** | Caddy 反代 / nginx | 省一个进程；自签足够（管理员手动信任） |
| 11 | 进程管理 | **单 entrypoint.sh** | s6-overlay / supervisord / systemd | 容器内只有 2 个进程（强Swan + Go），shell 循环足够 |
| 12 | 镜像基础 | **`strongswan/strongswan:6.1.0` 官方镜像** | 自编译 strongSwan | 官方镜像有 Debian 安全更新；少维护成本 |
| 13 | Docker 构建 | **单 stage** | 多阶段 | 官方镜像基础上 COPY 一个二进制 |
| 14 | 客户端配置 | **iOS/macOS 未签名 .mobileconfig + 其他平台手写配置** | 全平台统一格式 | iOS/macOS 接受未签名；其他平台用各原生格式 |
| 15 | 二维码 | **服务端生成 PNG**（`skip2/go-qrcode`） | 前端 JS 生成 | 服务端生成 = 一次 HTTPS 请求就拿到所有东西 |
| 16 | 日志 | **Go stdout + strongSwan journald** | 自建日志库 / 数据库 | 容器内默认行为；日志归 Docker 管 |
| 17 | 监控 | **不做** | Prometheus / 探活 | 几十人规模，挂了重启即可 |
| 18 | 国际化 | **中文为主**（UI 字符串硬编码中文） | i18n 框架 | 单一用户群，无需多语言 |

---

## 4. 关键安全决策（取舍表）

### 4.1 必要的安全措施（v2 必须做）

| 措施 | 实现 | 理由 |
|---|---|---|
| 面板 HTTPS（自签证书） | Go 内嵌 TLS，证书在数据目录 | 防面板密码嗅探；自签无额外成本 |
| 管理员密码 bcrypt | `crypto/bcrypt` | 即使数据库泄露，密码不可逆 |
| 防火墙只开必要端口 | UDP 500、UDP 4500、ESP、面板端口（8443 默认） | 最小暴露面 |
| 容器内 0600 权限保护 | 所有私钥 / 配置文件 / 数据库 | 防止容器内横向越权 |
| 随机密码生成 | `crypto/rand` + 12 位字符 | 新增用户默认密码足够强 |
| CSRF token | session 关联的随机 token | 防止跨站请求 |
| HTTPS-only cookie | cookie Secure + HttpOnly | 防 JS 读取 cookie |
| 输入校验 | 字符串长度/字符集校验 | 防注入（虽然 Go template 自带转义） |
| 限速（登录端点） | 内存计数器，5 次失败锁 5 分钟 | 防爆破（管理员端点） |
| 自动证书生成 | 启动时检测缺失自动生成 | 一次拉起就能用 |

### 4.2 明确不做（v2 不做）

- 2FA / TOTP — 管理员一人，bcrypt + HTTPS 已够
- 失败登录锁定持久化 — 内存里即可（重启后清零）
- 审计日志入库 — journald / docker logs 够用
- 入侵检测 — 单服务器自用，没必要
- 防 DDoS — 自用规模遇不到
- WAF / IP 白名单 — 内网访问即可

### 4.3 妥协项（接受风险）

| 妥协 | 风险 | 缓解 |
|---|---|---|
| 密码明文存数据库 | 数据库泄露 = 全部密码泄露 | 数据库放在容器内，外部进不来；定期备份加密 |
| 单一管理员账号 | 没人备份账号 = 失账号 | 第一次启动打印"请立刻改默认密码"警告 |
| 自签证书 | 用户首次连接弹"证书不可信" | mobileconfig 内联 CA 证书，iOS/macOS 信任；Android/Windows 在 mobileconfig 内附带 CA |
| 端口全暴露 | 公网可访问面板 | 默认监听 0.0.0.0:8443；建议用户用防火墙限制源 IP |
| 容器 root 运行 | 容器逃逸 = 主机 root | Docker 默认 root；用 `--cap-drop=ALL --cap-add=NET_ADMIN` 收紧 |

---

## 5. 用户体验流程

### 5.1 管理员首次部署

```
1. git clone <repo>
2. cd ikev2-panel-v2
3. docker compose up -d
4. 看启动日志，复制"默认管理员密码"
5. 浏览器打开 https://<server-ip>:8443
6. 浏览器警告"证书不安全" → 信任
7. 用默认账号登录，立刻改密码
8. 完成
```

### 5.2 管理员新增用户

```
1. 登录面板
2. 点"新增用户"
3. 填用户名 + 限速（默认 10 Mbps，0=不限）+ 过期时间（可选）+ 备注
4. 自动生成 12 位随机密码（一次性显示）
5. 点"创建"
6. 用户列表显示新用户
7. 点用户名 → 看到 mobileconfig 下载链接 + 二维码 + 配置文件二维码
8. 把链接/二维码 + 密码发给用户
```

### 5.3 普通用户接入（iOS）

```
1. 收到管理员发的 .mobileconfig 链接
2. Safari 打开链接
3. iOS 提示"安装 VPN 配置描述文件"
4. 输入锁屏密码确认
5. 设置 → VPN → 打开开关
6. 第一次连接输入用户名 + 密码
7. 连接成功
```

### 5.4 普通用户接入（Android）

```
1. 收到管理员发的二维码（strongSwan 配置）
2. 安装 strongSwan VPN Client（Google Play）
3. 扫码导入配置
4. 输入用户名 + 密码
5. 连接
```

### 5.5 管理员改用户密码

```
1. 登录面板
2. 用户列表点用户名
3. 点"重置密码"
4. 自动生成新密码，显示一次
5. 管理员截图/复制发给用户
```

### 5.6 管理员吊销用户

```
1. 登录面板
2. 用户列表点"删除"
3. 确认
4. swanctl 子配置被移除 + 触发 reload
5. 用户立刻无法连接
```

### 5.7 管理员查看当前连接

```
1. 登录面板
2. 主页显示当前 SAs：`swanctl --list-sas` 解析后的列表
3. 显示：用户名、客户端 IP、虚拟 IP、建立时间、本次连接流量
4. 不支持"强制下线"按钮（swanctl --terminate 需要 client identity，略复杂）
```

### 5.8 用户列表

```
1. 登录面板
2. 用户列表显示：用户名、状态、限速、过期时间、累计流量、最近活跃
3. 支持按用户名搜索、按状态过滤
4. 点击用户进入详情页（含本次连接流量、本月流量、最近10 次连接）
```

### 5.9 过期账户自动处理

```
Go 后台 goroutine（每 60 秒跑一次）：
1. SELECT * FROM users WHERE expires_at > 0 AND expires_at < NOW
2. 对每个过期用户：
   a. 更新 swanctl 子配置 → 标记 disabled
   b. swanctl --terminate --ike-id <username>（强制下线已连接用户）
   c. 写日志
3. 管理员面板上过期用户标红（但不删除）
```

---

## 6. 数据模型

### 6.1 用户表 `users`

```sql
CREATE TABLE users (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    username        TEXT    NOT NULL UNIQUE,
    password        TEXT    NOT NULL,         -- 明文密码（妥协项，§4.3）
    enabled         INTEGER NOT NULL DEFAULT 1,  -- 0=停用，1=启用
    note            TEXT    NOT NULL DEFAULT '',  -- 管理员备注
    speed_limit_mbps INTEGER NOT NULL DEFAULT 10, -- 单用户限速 (Mbps)，0 = 不限速
    expires_at      INTEGER NOT NULL DEFAULT 0,   -- 账户过期时间 (unix timestamp)，0 = 永不过期
    bytes_in_total  INTEGER NOT NULL DEFAULT 0,  -- 累计下行字节
    bytes_out_total INTEGER NOT NULL DEFAULT 0,  -- 累计上行字节
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    last_used_at    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_users_enabled ON users(enabled);
CREATE INDEX idx_users_expires ON users(expires_at);
```

### 6.2 会话表 `sessions`

```sql
CREATE TABLE sessions (
    id            TEXT    PRIMARY KEY,      -- 随机 32 字节 hex
    csrf_token    TEXT    NOT NULL,         -- 随机 32 字节 hex
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL,
    user_agent    TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX idx_sessions_expires ON sessions(expires_at);
```

### 6.3 管理员表 `admins`

```sql
CREATE TABLE admins (
    id            INTEGER PRIMARY KEY CHECK (id = 1),  -- 全局只能 1 个管理员
    username      TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,         -- bcrypt hash
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
```

### 6.4 不需要的数据表

明确**不做**的表：

- `audit_log` — 写日志到 stdout，不入库
- `quota_usage` — 不做月度配额
- `bandwidth_samples` — 不做实时带宽采样（按需 `swanctl --list-sas` 查询即可）
- `connections_log` — 看 swanctl 实时输出即可
- `notifications` — 不做通知
- `api_keys` — 不用 API，cookie only

---

## 7. API 设计

### 7.1 路由表（全部在 `/` 前缀下，监听 8443）

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/login` | 无 | 登录页面 |
| POST | `/login` | 无 | 提交用户名密码 |
| POST | `/logout` | session | 登出 |
| GET | `/` | session | 首页：当前 SAs + 用户数 |
| GET | `/users` | session | 用户列表 |
| GET | `/users/new` | session | 新增用户表单 |
| POST | `/users` | session | 创建用户 |
| GET | `/users/{id}` | session | 用户详情（含配置下载/二维码） |
| POST | `/users/{id}/reset-password` | session | 重置密码 |
| POST | `/users/{id}/disable` | session | 停用 |
| POST | `/users/{id}/enable` | session | 启用 |
| POST | `/users/{id}/delete` | session | 删除 |
| GET | `/users/{id}/mobileconfig` | session | 下载 .mobileconfig |
| GET | `/users/{id}/qr.png` | session | 二维码 PNG（strongSwan / mobileconfig） |
| GET | `/healthz` | 无 | 健康检查（返回 200 OK） |

**总计：15 个路由，全部用标准 `net/http` ServeMux。**

### 7.2 表单字段

`POST /users` 接收：
- `username` (string, required, 长度 3-32, 字符集 `[a-z0-9_-]`)
- `note` (string, optional, 长度 ≤ 200)
- `speed_limit_mbps` (int, optional, 范围 0-1000，默认 10，0 = 不限速)
- `expires_at` (string, optional, 格式 "2026-12-31"，空 = 永不过期)
- `enabled` (checkbox, default on)

**响应**：302 重定向到 `/users/{id}`

`POST /users/{id}/reset-password` 接收：无
**响应**：302 重定向到 `/users/{id}`，**一次性显示新密码**

### 7.3 错误处理

| 场景 | 行为 |
|---|---|
| 用户名重复 | 422 + 错误消息 |
| 用户名非法字符 | 422 + 错误消息 |
| 未登录访问受保护资源 | 302 → `/login?next=...` |
| Session 过期 | 302 → `/login` |
| CSRF token 缺失/不匹配 | 403 |
| 用户不存在（GET/POST id） | 404 |
| swanctl reload 失败 | 500 + 在 UI 显示错误，**数据库变更回滚** |

---

## 8. 部署架构

### 8.1 容器内进程拓扑

```
┌─────────────────────────────────────────────────────┐
│ Docker Container (strongswan/strongswan:6.1.0)      │
│                                                     │
│  ┌──────────────┐    ┌──────────────────┐           │
│  │   charon     │    │   ikev2-panel    │           │
│  │  (strongSwan)│    │  (Go binary)     │           │
│  │              │    │                  │           │
│  │  UDP 500/4500│    │  TCP 8443        │           │
│  │  ESP proto   │    │  (HTTPS)         │           │
│  │              ◄────┤                  │           │
│  │  swanctl     │    │  exec.Command    │           │
│  │  --load-all  │    │  swanctl --load-*│           │
│  └──────────────┘    └──────────────────┘           │
│         ▲                    │                       │
│         │                    │                       │
│  /etc/swanctl/        /data/  (sqlite + certs)      │
│  (config includes)    (持久化卷)                     │
│                                                     │
└─────────────────────────────────────────────────────┘
                ▲                       ▲
                │ UDP/ESP               │ HTTPS
                │                       │
        ┌───────┴───────┐      ┌────────┴────────┐
        │  VPN 客户端    │      │  管理员浏览器    │
        │  (各平台)      │      │  (Chrome/Safari) │
        └───────────────┘      └─────────────────┘
```

### 8.2 端口

| 端口 | 协议 | 用途 |
|---|---|---|
| UDP 500 | IKE | IKEv2 控制面（IPv4 + IPv6） |
| UDP 4500 | IKE | NAT-Traversal（IPv4 + IPv6） |
| ESP (IP proto 50) | ESP | 加密数据（IPv4 + IPv6） |
| TCP 8443 | HTTPS | 管理面板 |
| TCP 80 | HTTP | **仅 Let's Encrypt 模式需要**，用于 ACME HTTP-01 挑战 |

### 8.3 目录结构

```
/data/                          # 持久化卷（Docker volume）
├── panel.db                     # SQLite 数据库
├── ca/                          # CA 证书目录
│   ├── ca.cert.pem
│   └── ca.key.pem               # 0600
├── server/                      # 服务器证书
│   ├── server.cert.pem
│   └── server.key.pem           # 0600
├── panel-tls/                   # 面板 HTTPS 证书
│   ├── cert.pem
│   └── key.pem                  # 0600
├── swanctl/                     # strongSwan 配置
│   ├── swanctl.conf             # 主配置（静态，手写）
│   └── users/                   # 用户配置（动态，Go 程序写）
│       ├── alice.conf
│       └── bob.conf
└── secrets/                     # strongSwan secrets
    ├── _panelhelper_secret      # 内部 IPC（如果用 eap-radius）
    └── (不需要，eap-mschapv2 走 swanctl.conf 内的 secrets 块)
```

### 8.4 启动流程

```
entrypoint.sh:
1. 检查 /data 是否初始化：
        - 生成 CA 证书（如果缺失）
        - 生成服务器证书（如果缺失）
        - 生成面板 HTTPS 证书（如果缺失）
        - 初始化 SQLite 数据库 schema（如果缺失）
        - 创建默认管理员账号（如果缺失），打印随机密码到 stdout
2. 启动 charon：
        ipsec start
3. 首次加载 swanctl 配置：
        swanctl --load-all
4. 启动 Go 面板（前台运行，PID 1）：
        exec /usr/local/bin/ikev2-panel
```

### 8.5 关闭流程

```
SIGTERM/SIGINT:
1. Go 面板优雅关闭（关闭 listener，等待 5s）
2. Docker 默认发送 SIGKILL 给 PID 1
3. charon 由 entrypoint 接管（如果有 PID 1 重定向）
4. 实际部署：只跑强Swan + Go 两个进程，崩了 docker 重启即可
```

---

## 9. swanctl.conf 模板设计

### 9.1 主配置（`/etc/swanctl/swanctl.conf`）

**IPv6-only 模式**（默认，推荐）：

```swanctl
connections {
    ikev2-rw {
        # IPv6-only 监听（避开宿主 IPv4 UDP 500/4500 被占）
        local_addrs = 2001:db8::75
        local {
            auth = pubkey
            certs = server.cert.pem
            id = ikev2.example.com
        }
        remote {
            auth = eap-mschapv2
            eap_id = %any
        }
        children {
            ikev2-rw {
                local_ts = 0.0.0.0/0, ::/0
                mode = tunnel
                esp_proposals = aes256gcm16-sha256, aes128gcm16-sha256
                updown = /usr/local/bin/ikev2-updown
            }
        }
        version = 2
        mobike = yes
        proposals = aes256gcm16-sha256-modp2048, aes128gcm16-sha256-modp2048
        send_certreq = yes
        unique = replace
        rekey_time = 24h
    }
}

include /etc/swanctl/conf.d/*.conf
```

**IPv4 fallback 模式**（可选，仅当宿主 IPv4 端口可用时）：

```swanctl
connections {
    ikev2-rw {
        # 双栈监听（IPv4 端口必须可用）
        local_addrs = 192.168.50.75, 2001:db8::75
        local {
            auth = pubkey
            certs = server.cert.pem
            id = ikev2.example.com
        }
        remote {
            auth = eap-mschapv2
            eap_id = %any
        }
        children {
            ikev2-rw {
                local_ts = 0.0.0.0/0, ::/0
                mode = tunnel
                esp_proposals = aes256gcm16-sha256, aes128gcm16-sha256
                updown = /usr/local/bin/ikev2-updown
            }
        }
        version = 2
        mobike = yes
        proposals = aes256gcm16-sha256-modp2048, aes128gcm16-sha256-modp2048
        send_certreq = yes
        unique = replace
        rekey_time = 24h
    }
}

include /etc/swanctl/conf.d/*.conf
```

**Go 端根据 `IKEV2_IPV6_ONLY` 环境变量生成对应模板**：

```go
// internal/swanctl/writer.go
func renderMainConfig(ipv6Only bool, serverIPv6, serverIPv4 string) string {
    if ipv6Only {
        return ipv6OnlyTemplate(serverIPv6)
    }
    return dualStackTemplate(serverIPv4, serverIPv6)
}
```

### 9.2 用户子配置（`/etc/swanctl/conf.d/alice.conf`）

```swanctl
# 用户 alice 配置（Go 程序生成，启用时存在，停用/删除时被删除）
secrets {
    eap-alice {
        id = alice
        secret = "随机生成的12位密码"
    }
}
```

**关键设计：每个用户一份独立 .conf 文件**。Go 程序通过 create/delete 文件 + `swanctl --load-all` 实现用户管理。

### 9.2.1 限速实现（修订版）

通过 swanctl 的 `updown` 脚本实现。脚本位置：`/usr/local/bin/ikev2-updown`

**关键设计：每个用户的 class ID 持久化到文件，避免重复 + 避免 down 误删其他用户规则。**

```bash
#!/bin/bash
# 由 swanctl 在 SA up/down 时调用
# 环境变量：
#   PLUTO_MY_ID        = 服务器 ID
#   PLUTO_PEER_ID      = 客户端 eap_id (e.g. "alice")
#   PLUTO_PEER_SOURCEIP = 客户端虚拟 IP
#   PLUTO_CONNECTION   = 连接名
#   PLUTO_VERB         = up | down

set -e

LIMIT_MBPS_FILE="/var/lib/ikev2-panel/limits/${PLUTO_PEER_ID}"
CLASS_ID_FILE="/var/lib/ikev2-panel/classids/${PLUTO_PEER_ID}"
OUT_IF="${IKEV2_OUT_IF:-eth0}"

# 启动时建立根 qdisc（只一次，所有用户共用）
ensure_root_qdisc() {
  if ! tc qdisc show dev "$OUT_IF" | grep -q "qdisc htb"; then
    tc qdisc add dev "$OUT_IF" root handle 1: htb default 999 || true
    # default class 999 = 不限速
    tc class add dev "$OUT_IF" parent 1: classid 1:999 htb \
      rate 1000mbit ceil 1000mbit || true
  fi
}

case "$PLUTO_VERB" in
  up)
    ensure_root_qdisc
    if [ -f "$LIMIT_MBPS_FILE" ]; then
      LIMIT=$(cat "$LIMIT_MBPS_FILE")  # 单位 Mbps，0 = 不限速
      if [ "$LIMIT" -gt 0 ]; then
        # 分配并持久化该用户的 class ID
        mkdir -p /var/lib/ikev2-panel/classids
        if [ -f "$CLASS_ID_FILE" ]; then
          CLASS_ID=$(cat "$CLASS_ID_FILE")
        else
          CLASS_ID=$((RANDOM % 9000 + 100))  # 100-9099 范围
          echo "$CLASS_ID" > "$CLASS_ID_FILE"
        fi

        # 添加 leaf class（限速）
        tc class add dev "$OUT_IF" parent 1: classid 1:$CLASS_ID htb \
          rate "${LIMIT}mbit" ceil "${LIMIT}mbit"

        # 添加 filter（用 u32 匹配 VIP）
        tc filter add dev "$OUT_IF" parent 1: protocol ip prio 1 u32 \
          match ip src "$PLUTO_PEER_SOURCEIP" flowid 1:$CLASS_ID

        echo "[updown] Applied ${LIMIT}Mbps for user ${PLUTO_PEER_ID} (VIP ${PLUTO_PEER_SOURCEIP}, class ${CLASS_ID})"
      fi
    fi
    ;;
  down)
    # **关键修复**：只删该用户的 class，不重建根 qdisc
    if [ -f "$CLASS_ID_FILE" ]; then
      CLASS_ID=$(cat "$CLASS_ID_FILE")
      tc class del dev "$OUT_IF" classid 1:$CLASS_ID 2>/dev/null || true
      rm -f "$CLASS_ID_FILE"
      echo "[updown] Removed class ${CLASS_ID} for user ${PLUTO_PEER_ID}"
    fi
    # **不要** 删根 qdisc！否则其他用户的限速也失效
    ;;
esac
```

**关键修复**（与 v2 初版对比）：

| 项 | v2 初版（有 bug） | v2 修订版 |
|---|---|---|
| Class ID | `$RANDOM`（每次重算） | **持久化到文件**（避免冲突） |
| Root qdisc 创建 | 每次 up 都重建 | 只创建一次 |
| down 时清理 | `tc qdisc del ... root`（**会删所有用户的规则**） | **只删该用户的 class**（不影响其他用户） |
| 多用户并发 | Class ID 冲突 | Class ID 持久化，不会冲突 |

**Go 端**：写入限速文件 `/var/lib/ikev2-panel/limits/<username>`，内容是数字（Mbps）。

**已知限制**：

- 上传（client → server）限速通过 `PLUTO_PEER_SOURCEIP` 匹配，**下载**（server → client）需要 `tc filter match ip dst`，v2 默认只做单向上传限速
- 这个限制写在 README，让管理员知道

#### 9.2.1.1 IPv6 tc 限速（v2.85-PR7,flower classifier）

**问题**：v2 默认 `IKEV2_IPV6_ONLY=true`，IPv6 用户占绝大多数。但 `tc filter ... u32 match ip src ...` **只识别 IPv4**，IPv6 流量被静默忽略 → 用户在面板给某用户设 5 Mbps，IPv6 拨入时**实际无限速**。

**修复**（v2.85-PR7）：用 `tc flower` classifier 补 IPv6 路径。

| 项 | IPv4 路径（沿用） | IPv6 路径（PR7 新增） |
|---|---|---|
| classifier | `u32` | `flower` |
| protocol | `ip` | `ipv6` |
| match key | `match ip src <VIP>` | `src_ip <VIP>`（flower 用 `src_ip` 不是 `ip6_src`，tc 按 IP 字面量自动判断 v4/v6）|
| 内核要求 | 任意 Linux（u32 早于 2.6） | **Linux ≥ 4.1**（flower 引入）|
| dual-stack | 与 v6 共享 class ID | 与 v4 共享 class ID（HTB class 协议无关）|

**family 路由**：`/etc/ikev2-panel/family.env`（entrypoint 生成）一行 `FAMILY=<value>`：

| `IKEV2_IPV6_ONLY` | 容器地址 | family |
|---|---|---|
| true | — | `ipv6`（默认）|
| false | 只有 v4 | `ipv4` |
| false | v4 + v6 都有 | `dual`（v4 + v6 同时限速）|

显式传 `IKEV2_FAMILY` 覆盖（运维手动切）。

**dual-stack 设计要点**：
- v4 + v6 filter **共享同一个 class ID**（HTB class 是 L3 无关的；filter 各一条）
- 节省资源：1000 个双栈用户 = 1000 个 class + 2000 个 filter，不是 2000 个 class
- class add 重复执行是幂等的（同一 classid 重复 add 失败，但 `|| true` 已容错）

**内核兼容性**（updown 脚本里 `flower_supported()` 函数探测）：

| 内核 | flower | IPv6 限速行为 |
|---|---|---|
| ≥ 4.1 | ✅ | flower filter 生效 |
| 3.16 – 4.0 | ❌ | updown WARN，IPv6 限速 silently disabled，SA 不阻塞 |
| < 3.16 | ❌ | 同上 |

**Go API**（`internal/limit/limiter.go`，v2.85-PR7）：

```go
// 纯函数：只生成 tc 命令，不 exec
cmds := limit.BuildTcCommands(limit.FamilyIPv6, "up", "eth0", 1234, "fd00:1::5", "fd00:1::1")
// → [class add ... htb rate 1000mbit, filter add ... protocol ipv6 prio 1 flower ip6_src fd00:1::5 flowid 1:1234]

// 把 1000mbit 占位换成实际 Mbps
cmds = limit.SetRate(cmds, 50)
// → rate 50mbit ceil 50mbit
```

**不破坏兼容性**：
- IPv4-only 用户（`IKEV2_IPV6_ONLY=false`，老 v1 迁移过来）走 u32 路径，**零行为变更**
- 老 entrypoint 没生成 `family.env` → updown 默认 `FAMILY=ipv4` → 走 u32 路径（向后兼容老镜像）
- `BuildTcCommands` / `SetRate` 是新 API，旧 `WriteLimitFile` / `RemoveLimitFile` 函数签名不变

**swanctl 主配置引用**：

```swanctl
children {
    ikev2-rw {
        updown = "/usr/local/bin/ikev2-updown"
        ...
    }
}
```

### 9.2.2 过期账户处理

**swanctl 本身不检查过期**。实现策略：

1. **连接建立时**：Go 后台 goroutine 每 60 秒扫描 `users.expires_at`
2. 过期用户的 `.conf` 文件保留（不删除，因为用户可能改时间续期）
3. 但在 `.conf` 文件里加注释 + Go 进程加 iptables drop 规则阻止该用户连接：

```bash
# 在 NAT 转发链加：丢弃过期的虚拟 IP 流量
iptables -A FORWARD -s <expired_user_vip> -j DROP
```

或者更简单：**过期用户的 `.conf` 文件里把 `secret` 替换为随机无效密码**，连接会 EAP 失败。

### 9.3 为什么不放在主配置

- 删一个用户 = 删一个文件，比改大文件安全（不会破坏其他用户的 secret）
- 用户配置文件可以单独备份/查看
- `swanctl --load-all` 会自动重新加载整个 include 目录

---

## 10. mobileconfig 设计

### 10.1 iOS/macOS .mobileconfig

**未签名**版本，iOS 会弹"未签名"警告但仍可安装。结构：

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>PayloadContent</key>
    <array>
        {{ if .IncludeCA }}
        <dict>
            <key>PayloadType</key>
            <string>com.apple.security.root</string>
            <key>PayloadCertificateFileName</key>
            <string>ca.cert.pem</string>
            <key>PayloadContent</key>
            <data>{{ .CACertBase64 }}</data>
        </dict>
        {{ end }}
        <dict>
            <key>PayloadType</key>
            <string>com.apple.vpn.managed</string>
            <key>PayloadIdentifier</key>
            <string>com.example.ikev2.alice</string>
            <key>PayloadUUID</key>
            <string>{{ .PayloadUUID }}</string>
            <key>PayloadDescription</key>
            <string>IKEv2 VPN (username/password)</string>
            <key>UserDefinedName</key>
            <string>IKEv2 VPN</string>
            <key>VPNType</key>
            <string>IKEv2</string>
            <key>IKEv2Settings</key>
            <dict>
                <key>RemoteAddress</key>
                <string>{{ .ServerAddress }}</string>
                <key>LocalIdentifier</key>
                <string>{{ .Username }}</string>
                <key>RemoteIdentifier</key>
                <string>{{ .ServerID }}</string>
                <key>AuthenticationMethod</key>
                <string>Certificate</string>
                <key>ExtendedAuthenticationEnabled</key>
                <true/>
                <key>MOBIKE</key>
                <true/>
                <key>DeadPeerDetectionRate</key>
                <string>Medium</string>
            </dict>
            <key>ProxyType</key>
            <string>None</string>
        </dict>
    </array>
    <key>PayloadDisplayName</key>
    <string>IKEv2 VPN</string>
    <key>PayloadIdentifier</key>
    <string>com.example.ikev2.{{ .Username }}</string>
    <key>PayloadUUID</key>
    <string>{{ .ProfileUUID }}</string>
    <key>PayloadType</key>
    <string>Configuration</string>
    <key>PayloadVersion</key>
    <integer>1</integer>
</dict>
</plist>
```

**关键点**：

1. **可选 CA 证书 payload** — `{{ if .IncludeCA }}` 控制
   - **自签模式**：`IncludeCA=true`，mobileconfig 内联 CA 证书
   - **Let's Encrypt 模式**：`IncludeCA=false`，iOS/macOS 已信任 LE 根证书，无需内联
2. **`RemoteAddress`** 是单一地址（IPv4 或 IPv6）；**不混用 `ServerAddresses` 数组**——iOS 不同版本对双栈字段行为不一致
3. **`MOBIKE = true`** 显式声明（iOS 默认开启，但显式更稳）
4. **`ProxyType = None`** 防止 iOS 让 VPN 流量走系统代理
5. **`PayloadUUID` / `ProfileUUID` 必须用 `crypto/rand` 生成全局唯一值**（每个用户不同）
6. **`PayloadIdentifier` 含 username**（`com.example.ikev2.alice`）—— iOS 用此字段作为 profile 唯一键
7. `AuthenticationMethod = Certificate` — 服务器端用证书认证客户端信任它
8. `ExtendedAuthenticationEnabled = true` — 触发用户名密码弹窗
9. **未做 CMS 签名** — iOS 接受未签名 profile，会弹"未验证"提示

**UUID 生成要求**：

```go
// internal/cert/mobileconfig.go
import "crypto/rand"

func generateUUID() string {
    var b [16]byte
    rand.Read(b[:])
    // 设置版本 (4) 和变体 (10xx)
    b[6] = (b[6] & 0x0F) | 0x40
    b[8] = (b[8] & 0x3F) | 0x80
    return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
        b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
```

### 10.2 Android/strongSwan 配置

不用 .mobileconfig，给一段 strongSwan 客户端配置文本：

```
# 写入 /etc/swanctl/conf.d/client.conf
# 管理员打印或 QR 码下发

connections {
    ikev2-rw {
        local {
            auth = eap-mschapv2
            eap_id = alice
        }
        remote {
            auth = pubkey
            id = ikev2.example.com
        }
        remote_addrs = <server-ip>
        children {
            ikev2-rw {
                remote_ts = 0.0.0.0/0, ::/0
                mode = tunnel
                esp_proposals = aes256gcm16-sha256, aes128gcm16-sha256
                updown = /usr/lib/ipsec/_updown iptables
            }
        }
        version = 2
        mobike = yes
        proposals = aes256gcm16-sha256-modp2048
        send_certreq = yes
        unique = replace
        rekey_time = 24h
    }
}

secrets {
    eap-alice {
        id = alice
        secret = "<由用户输入>"
    }
}
```

---

## 11. 实施计划（参考，**非本文档重点**）

> 这一节是给开发时参考，不在设计阶段死扣。

### M1: 基础
- 目录结构
- Docker 单 stage 镜像
- entrypoint.sh（CA 证书生成 + charon 启动）
- 默认 swanctl.conf

### M2: 存储
- SQLite schema 初始化
- 用户/会话/管理员模型
- 默认管理员创建（首次启动生成随机密码）

### M3: 认证
- 登录/登出路由
- bcrypt 验证
- Cookie session + CSRF

### M4: 用户管理
- 列表 / 新增 / 删除 / 停用 / 重置密码
- swanctl 子配置生成/删除
- `swanctl --load-all` 调用

### M5: 客户端配置
- .mobileconfig 模板渲染
- strongSwan 配置文本渲染
- 二维码生成

### M6: 监控
- 首页：swanctl --list-sas 解析
- 简单的用户统计

### M7: 打包
- docker compose 文件
- 启动文档
- 端到端真机测试

---

## 12. 验收标准（v2 完成定义）

- [ ] `docker compose up -d` 一次启动成功
- [ ] 启动日志打印默认管理员密码
- [ ] 浏览器 HTTPS 访问面板，能登录
- [ ] 登录后能改默认密码
- [ ] 新增用户 → 数据库写入 → swanctl 子配置文件生成 → `swanctl --load-all` 调用成功
- [ ] 客户端连接（真机）→ 输入用户名密码 → IKE_SA 建立 → 拿到虚拟 IP → ping 通服务端
- [ ] 客户端连接断开后重连成功
- [ ] 客户端重连后再发起多个连接，无内存泄漏
- [ ] 删除用户 → swanctl 子配置文件删除 → `swanctl --load-all` 成功 → 用户无法连接
- [ ] 重置密码 → 用户能用新密码连接
- [ ] 防火墙关闭后服务无法访问
- [ ] `docker compose down` 后数据保留
- [ ] `docker compose up -d` 再次启动后数据可恢复

**新增验收（限速/过期/流量）：**

- [ ] **限速**：新建用户指定 5 Mbps → 连接后 `iperf3` 测试下行速度 ≤ 6 Mbps（容忍误差）
- [ ] **限速**：同用户设置 0（不限速）→ `iperf3` 测试可达物理上限
- [ ] **限速**：两个不同用户（5Mbps + 10Mbps）同时连接 → 各自速度符合限速
- [ ] **过期**：新建用户设过期时间 = 1 分钟后 → 等待 → 用户连接被拒绝
- [ ] **过期**：过期后改回未来时间 → 用户能继续连接
- [ ] **流量统计**：用户连接跑 1GB 下载 → 用户列表 `bytes_in_total` 增加约 1GB
- [ ] **流量统计**：重置面板 → 数据保留

---

## 13. 不在 v2 范围（明确写在最后）

以下功能**v2 明确不做**，未来如需要请开 v3：

- 多管理员账号 / RBAC
- 2FA / TOTP
- 审计日志入库 / 报表
- 用户自助改密码
- 月度流量配额（v2 仅做"每用户总流量"展示，不做月度限额）
- 实时带宽图 / 流量趋势图表
- 多节点集群
- API token / 第三方 API
- iOS mobileconfig CMS 签名
- 自定义 PBES2 私钥加密
- 客户端证书认证（EAP-TLS）
- Docker 多阶段构建 / s6-overlay
- Prometheus 监控 / 健康告警
- 国际化 i18n
- 通知系统（邮件/Telegram/Webhook）

**v2 已包含但仅作为可选项：**

- ~~IPv6 优先 / 双栈并行~~ → **已加回**，swanctl 配置 `local_addrs` 包含 IPv6
- ~~Let's Encrypt 自动签发~~ → **已加回**，`IKEV2_CERT_MODE=letsencrypt` 启用

**v2 仍不支持的（高级需求）：**

- **Let's Encrypt DNS-01 挑战**（需要 DNS API 凭证；HTTP-01 已实现）
- **ACME 通配符证书**（`*` 域名）
- **IPv6-only 客户端连接**（v2 默认双栈，单 IPv6 需要更多配置）
- **IPv6 限速**（updown 脚本用 tc，但 tc 对 IPv6 有限制）

---

## 14. 文档状态

- [ ] 用户审阅通过
- [ ] 架构理解文档（另一份）完成
- [ ] 进入实施阶段

---

# v2-79 增量设计（2026-09-17）

> v2-78 在生产环境验证成功后，用户提出 5 个真实痛点。v2-79 全部解决：
>
> | 用户痛点 | v2-78 状态 | v2-79 解法 |
> |---|---|---|
> | 1. 公网 IPv6 动态变化要改 `.env` + 重 build | 硬编码 `IKEV2_SERVER_ADDR_V6` | §0.2 自动探测 + §17 ipv6watch 守护（v2-72 已具备） |
> | 2. 网卡名硬编码（`ens18` 因部署环境而异） | 硬编码 `IKEV2_OUT_IF=ens18` | §0.1 自动探测（`ip -4 route show default`） |
> | 3. 换网络环境网关 / 网段变了 | 硬编码 `10.10.0.0/24` + 手动改 .env | §0.4 / §0.5 / §0.6 自动探测 + 自动选不冲突的 VPN 子网 |
> | 4. "改了宿主机配置"误解 | iptables 规则虽在容器内但没文档说明 | §19.4 文档澄清 + README 故障排查 |
> | 5. 不允许 host 网络怎么办 | 只支持 host | §0.7 计划（v2-79 文档化 + docker-compose 注释保留 bridge/ipvlan 模板） |
>
> **v2-79 核心目标**：所有"跟宿主环境相关"的参数都自动探测；用户唯一必填的只有 `IKEV2_SERVER_CN`（证书 CN 用）。镜像跨网络环境通用，不需要重 build。

---

## 15. v2-78 回顾（v2-79 起点）

v2-78 在用户的生产环境跑通的关键链路（v2-79 文档需保留这些决策的引用）：

### 15.1 IPv6-only + libipsec + 自编译 strongSwan 6.0.1（沿用 §1.5）

v2-79 不改这部分，只在自动探测链里复用。

### 15.2 出向路由表 220 + FORWARD ACCEPT + MASQUERADE（v2-78 新增，v2-79 自动化，v2.86-PR12.16 改用 nft 自定义表）

v2-78 调试 iPhone 能拨号但上不了网的根因（家用路由器对 LAN→WAN 的 IPv6 出向有防火墙限制，IPv4 出向正常）：
1. swanctl 把客户端流量按 `local_ts=0.0.0.0/0, ::/0` 劫持进 ipsec0
2. 出向走 XFRM policy routing table 220：strongSwan 默认只放客户端 VIP 的直连路由 (`10.10.0.1 dev ipsec0 proto static`)，**没有 default route** → 出向包被丢
3. docker 在 host 网络模式下让 iptables FORWARD 默认 DROP，没有 ACCEPT 规则 → 转发也丢
4. 内网 VPN 子网客户端源 IP 在公网不可路由，需要 MASQUERADE

**v2-78 §3.5 三件套**（v2-79 entrypoint.sh §3.5 完整保留 + 自动化）：
- table 220 default route via IPv4 GW
- iptables FORWARD ACCEPT ipsec0 ↔ OUT_IF
- iptables MASQUERADE VPN_SUBNET → OUT_IF

**v2-79 增强**：MASQUERADE 用的源网段从硬编码 `10.10.0.0/24` 改为 `%IKEV2_VPN_SUBNET%` 占位符（§16 + §16.3）。

**v2.86-PR12.16 重大改动**：iptables → nft 自定义表
- 之前 `iptables -I FORWARD 1 -i ipsec0 -j ACCEPT` 写在 docker daemon 自己的 `filter` 表里
  → daemon 异步 `iptables-restore` 会挤掉规则（实测 5-60s 后清规则）
- 现在改用 nft 自定义表 `table ip ikev2` / `table ip6 ikev26`
  → docker daemon 完全碰不到 → 0 残留风险
- `nft -f` 一次原子提交，无 `-I FORWARD N` 位置坑
- `fib daddr type != local` 比 `addrtype ! --dst-type LOCAL` 更严格（查 FIB 路由表）
- `tcp option maxseg size set rt mtu` 比 `--clamp-mss-to-pmtu` 更准（查 fib 取路径 MTU）
- §4.6 DDoS connlimit/hashlimit 仍走 `iptables -I DOCKER-USER`（DOCKER-USER 是 daemon 管的 chain，自定义 nft 表无法覆盖）

**v2.86-PR12.17 增强**：§7.9 retry `re_add_ikev2_rules` 改为先 `nft delete table` 清 stale 表再 `nft -f` 重建，处理跨 netns / nft 服务重启场景。

完整 ruleset 见 [scripts/ikev2.nft.template](../scripts/ikev2.nft.template)，调用方式见 `entrypoint.sh §3.5`。

### 15.3 mobileconfig DNS 推送（v2-78 新增，v2-79 沿用，v2.86-PR12.23 修正 key 名）

v2-78 在 mobileconfig 加 DNS 推送（4 个 DNS server），推 1.1.1.1 / 8.8.8.8 / 2606:4700:4700::1111 / 2001:4860:4860::8888。

**根因**：iOS 17+ 把所有 UDP 53 也按 `0.0.0.0/0` 强制路由进 ESP 隧道 → server 端 swanctl pools 没 dns 时客户端 DNS 解析走不通 → captive.apple.com 探测超时 → Safari/WX 直接报"没连接互联网"（即使 TCP 443 实际能 curl 通）。

**v2-79 同步加到 swanctl.conf 的 pools.dns**（两个渠道都推，兼容老客户端）：
- mobileconfig 的 `<key>DNS</key>`（iOS 优先用）
- swanctl.conf 的 pools.dns（strongSwan 通过 ModeConfig 推送给客户端）

**v2.86-PR12.23 audit 修复**（C6 §22.1）：

原代码 key 名写错：
```xml
<!-- 错的 -->
<key>DNSSettings</key>
<dict>
  <key>DNS</key>
  <array><string>1.1.1.1</string>...</array>
</dict>
```

`DNSSettings` 是**杜撰 key**，[Apple devicemanagement 文档](https://developer.apple.com/documentation/devicemanagement/vpn/dns-data.dictionary)从没用过这个名字，iOS 任何版本都静默忽略整个节点 → DNS 实际从未生效。

正确形态（iOS 14+）：
```xml
<!-- 对的 -->
<key>DNS</key>
<dict>
  <key>ServerAddresses</key>
  <array><string>1.1.1.1</string>...</array>
  <key>DNSProtocol</key>
  <string>Cleartext</string>
</dict>
```

`ServerAddresses` + `DNSProtocol=Cleartext` 是 iOS 14+ Required pair，缺一个就判定"无 DNS"配置。同样的 close-tag typo 修了两处：ChildSA `IntegrityAlgorithm` 的 `</key>` → `</string>`、外层 `PayloadType` 同样 → 之前 iOS plist parser 在 install 阶段就 reject。详见 release-notes-v2.86-pr12.23.md。

### 15.4 swanctl.conf 的 pools 段配置（v2-79 占位符化）

v2-78 硬编码 `addrs = 10.10.0.0/24, fd00:1::/24`：
- IPv4：10.10.0.0/24，分配给客户端作为 tunnel inside IP
- IPv6：fd00:1::/64，ULA 段（公网 IPv6 走 server 公网 IP，但 iOS 16+ 要求至少一个 IPv6 pool，给个 ULA 占位）

v2-79 改为 `%IKEV2_VPN_SUBNET%`，由 entrypoint §0.6 自动选 + §5 注入。

### 15.5 ipv6watch 守护（v2-72 已实现，v2-79 文档化）

`internal/swanctl/ipv6watch.go` 已存在（v2-72 实现），v2-79 在 main.go L358 显式启动（IPv6-only 模式才需要）：
- 每 60s 扫 `/proc/net/if_inet6`
- IPv6 变化 → sed 替换 swanctl.conf 的 local_addrs → swanctl --load-all
- 强Swan 用新地址监听 500/4500 → 客户端重拨能找到 server

**v2-79 在 README / 设计文档 §17 把这个能力明确告诉用户**，让用户知道 ISP 重拨不需要任何操作。

---

## 16. 自动探测链 §0（entrypoint.sh）

### 16.1 探测顺序

```
环境变量（IKEV2_OUT_IF / IKEV2_SERVER_ADDR_V6 / 等）
    ↓ 没传就
自动探测（/proc/net/if_inet6 / ip route / ip addr）
    ↓ 探测不到就
FATAL 退出（带明确错误提示 + 修复指引）
```

### 16.2 探测项对照表

| 参数 | 探测方式 | 失败时 |
|---|---|---|
| **出口网卡** | 1. 用户环境变量  2. `ip -4 route show default`  3. `ip -6 route show default`  4. 第一个非 lo 的 UP 接口 | FATAL |
| **公网 IPv6** | 1. 用户环境变量  2. `/proc/net/if_inet6` 取 `2000::/3` 段第一个 | FATAL（仅 IPv6_ONLY=true） |
| **公网 IPv4** | 1. 用户环境变量  2. 出口网卡上非 RFC1918 的第一个 | 警告（不致命） |
| **IPv4 网关** | `ip -4 route show default` 取 `via X.X.X.X` | 用于 MASQUERADE / table 220 default route |
| **LAN 网段** | 容器内 `ip -4 addr` 聚合 /24 | 用于检测 VPN 内部段冲突 |
| **VPN 内部虚拟 IP 段** | 候选 `10.10.0.0/24 → 10.13.0.0/24 → 10.17.0.0/24 → 10.42.0.0/24 → 10.66.0.0/24`，选第一个不与 LAN 重叠 | 默认 `10.10.0.0/24`（有 WARN） |

**v2.86-PR13.3:面板运行时 subnet 配置重启保持**

`/data/panel-state/subnet.conf` 启动期优先级最高（对称 cert.conf / aliyun.creds）:

1. `/data/panel-state/subnet.conf`（面板 UI 填的，运行期可改，**重启仍生效**）
2. `IKEV2_VPN_SUBNET` / `IKEV2_VPN_SUBNET_V6` env
3. entrypoint §0.6 自动探测

面板"保存并立即生效"按钮走的 handler（`Manager.UpdatePoolsAndReload`）运行期 sed `/etc/swanctl/swanctl.conf` 立即生效。
面板"清除"按钮恢复 startup 池段（`Server.StartupIPv4Subnet`/`StartupIPv6Subnet`，由 main.go 启动期读 swanctl.conf 注入），不需重启容器。

详细设计见 [docs/release-notes-v2.86-pr13.3.md](release-notes-v2.86-pr13.3.md)。

**v2.86-PR13.4:容器日志持久化 + 轮转**

之前 `/var/log` 是 tmpfs 128m,有两个问题:
- 容器重启即丢 → 排查"重启前发生了什么"无据可查
- 128m 写满 → 容器内 Go / strongSwan 进程写不进去 → 500 错误 / charon 异常退出

**改造**(对齐 `ddnsv6` 项目的日志范式):

| 路径 | 类型 | 轮转方式 | 持久化 |
|---|---|---|---|
| 容器内 `/var/log/*.log` | bind mount → 宿主 `./logs` | sticky bit 1777 + 手工 `logrotate`(可选) | ✅ 持久 |
| 容器 stdout/stderr | `logging.driver=json-file` | `max-size=20m` + `max-file=5` (daemon 层) | ✅ 持久 |

**为什么 `./logs` 用 bind mount 不用 named volume**:
- 跟 ddnsv6 项目对齐(`./ddns_logs:/var/log/ddns`)
- 用户能直接 `ls ./logs/` / `tail -f`,运维直觉好
- named volume 要 `docker volume inspect` 看路径,不直观

**为什么 `./data` 保留 named volume**:
- 里面是 SQLite DB + LE 私钥 + acme.sh account.json,bin mount 到源码目录会让数据跟 `.git` 距离太近,误删风险
- named volume 由 docker 管理,可备份(`docker run --rm -v ikev2-panel-v2_ikev2-data:/data -v $(pwd):/backup alpine tar czf /backup/...`)

**scripts/up.sh 启动前准备**:`mkdir -p ./logs && chmod 1777 ./logs`(sticky bit 防容器逃逸后误删其他日志)。

详细验证步骤见 [docs/release-notes-v2.86-pr13.4.md](release-notes-v2.86-pr13.4.md)。

### 16.3 关键改动文件

| 文件 | v2-78 | v2-79 |
|---|---|---|
| `scripts/entrypoint.sh` §0 | 没有自动探测；`ens18` 在 §2 探测；IPv6 在 §1 探测 | **§0.1 ~ §0.6 完整自动探测链**，所有变量可在 §5 占位符替换里用 |
| `configs/swanctl-ipv6-only.conf` | `local_addrs = %IKEV2_SERVER_ADDR_V6%`（必填） | `%IKEV2_LOCAL_ADDRS_DIRECTIVE%`（可填可省略） |
| `configs/swanctl-ipv6-only.conf` | `local_ts = 0.0.0.0/0, ::/0`（硬编码双栈） | `local_ts = %IKEV2_LOCAL_TS%`（按 IKEV2_IPV6_ONLY 自动填 `::/0` 或 `0.0.0.0/0, ::/0`） |
| `configs/swanctl-ipv6-only.conf` | `addrs = 10.10.0.0/24, fd00:1::/64`（硬编码） | `addrs = %IKEV2_VPN_SUBNET%, fd00:1::/64`（§0.6 自动选） |
| `scripts/entrypoint.sh` §3.5 | MASQUERADE 源 `10.10.0.0/24`（硬编码） | MASQUERADE 源 `%IKEV2_VPN_SUBNET%`（自动选） |
| `docker-compose.yml` | `IKEV2_SERVER_ADDR_V6=2408:...:1bb`（硬编码你的地址） | `IKEV2_SERVER_ADDR_V6=`（留空） |
| `docker-compose.yml` | `IKEV2_OUT_IF=ens18` | `IKEV2_OUT_IF=`（留空） |
| `docker-compose.yml` | 没有 `IKEV2_NETWORK_MODE` | 新增，默认 `auto`（v2-80+ 自动切换） |
| `docker-compose.yml` | 没有 `IKEV2_VPN_SUBNET` | 新增，默认 `auto` |
| `scripts/ikev2-updown` | `OUT_IF="${IKEV2_OUT_IF:-eth0}"`（写死 fallback） | v2-79 透传 entrypoint 注入的 `IKEV2_OUT_IF`（fallback 保留） |
| `internal/swanctl/ipv6watch.go` | 已在用（v2-72） | v2-79 文档化（README + §17） |

### 16.4 IP forwarding 自愈（v2-79 三层兜底）

Docker host network mode 下默认会把 IPv6 forwarding 重置为 0（docker 安全策略），但 v2-79 部署不应该要求用户"先 sudo sysctl -w"。所以设计成**三层兜底**：

| 层级 | 在哪做 | 做什么 | 失败时 |
|---|---|---|---|
| **L1 compose sysctls** | `docker-compose.yml` `sysctls:` | docker daemon 起容器前改宿主内核的 `net.ipv6.conf.all.forwarding` 和 `net.ipv4.ip_forward`，同时写进容器 `/proc/sys` | L2 接棒 |
| **L2 entrypoint 自愈** | `entrypoint.sh §3 enable_forwarding()` | host 模式下容器内 `/proc/sys` 是 rw（`privileged: true`），直接 `echo 1 > /proc/sys/...`，等同于改宿主（共享 net ns） | L3 接棒 |
| **L3 FATAL** | `entrypoint.sh §3` | 输出明确错误：①compose 加 sysctls ②手动 sudo sysctl -w ③检查 docker daemon 的 CAP_SYS_ADMIN | 退出码 11 |

**为什么这样设计**：
- L1 是"正常路径"，依赖 docker daemon 权限（多数发行版 systemd unit 默认就有 CAP_SYS_ADMIN）
- L2 是"保险"，单 host 网络下能自愈，盲赌 99% 场景
- L3 是"最末兜底"，说明宿主机 sysctl 是真 read-only（极少数 hardening 环境）
- bridge / ipvlan 模式不需要这些 key——容器内 `/proc/sys` RO 写不进去是正常的，不影响功能

### 16.5 §0 探测出来的变量怎么流通到 swanctl

```
[启动]
  entrypoint §0.1 detect_out_if  →  $OUT_IF
              §0.2 detect_public_ipv6  →  $IKEV2_SERVER_ADDR_V6
              §0.4 detect_v4_gateway  →  $V4_GW
              §0.6 detect_vpn_subnet  →  $IKEV2_VPN_SUBNET
  entrypoint §3  IP forwarding 校验（三层兜底：L1 compose sysctls → L2 自愈 → L3 FATAL）
  entrypoint §3.5  table 220 default + FORWARD ACCEPT + MASQUERADE 全部用 $OUT_IF / $V4_GW / $IKEV2_VPN_SUBNET
  entrypoint §5  占位符替换：
       %IKEV2_LOCAL_ADDRS_DIRECTIVE%  ←  $IKEV2_SERVER_ADDR_V6（v6 → v4 → 留空）
       %IKEV2_LOCAL_TS%               ←  ::/0（IPv6_ONLY）或 0.0.0.0/0, ::/0
       %IKEV2_VPN_SUBNET%             ←  $IKEV2_VPN_SUBNET
       %IKEV2_SERVER_CN%              ←  用户必填项
       %IKEV2_SERVER_CERT_FILE%       ←  自签模式：server.cert.pem / LE 模式：$DOMAIN.pem
       %IKEV2_OUT_IF%                 ←  $OUT_IF（strongSwan 内嵌使用，但目前没有内嵌字段）

[运行时]
  cmd/ikev2-panel/main.go L358 启动 ipv6watch goroutine（IPv6-only 模式）
  ipv6watch 每 60s 扫 /proc/net/if_inet6
       变化 → sed + swanctl --load-all
```

---

## 17. 公网 IPv6 动态变化：v2-79 已具备应对能力

### 17.1 已有机制（v2-72 + v2-79）

`internal/swanctl/ipv6watch.go`（v2-72 实现，v2-79 文档化）：
- 每 60s 扫 `/proc/net/if_inet6`
- IPv6 变化 → `sed` 替换 `swanctl.conf` 的 `local_addrs` → `swanctl --load-all`
- 强Swan 用新地址监听 500/4500 → 客户端重拨能找到 server

**注意**：ipv6watch 用 `swanctl --load-all`（不是 VICI load-conns），原因是 VICI 的 load-conn/load-cert/load-shared 协议层虽然能增量加载，但需要先 unload 现有连接；`--load-all` 在大改重启（host/network 等）场景更简单，**单 SA 中断 ~500ms 在 1-50 人规模完全可接受**。

### 17.2 用户体验

- ISP 重拨 → ISP 给宿主新 IPv6 → `/proc/net/if_inet6` 60s 内更新 → swanctl 自动 reload → 客户端无需改任何东西
- **完全无感**

### 17.3 移动客户端（iPhone/Android）的副作用

- iPhone 拨号时记下 server IP（v6）→ server IP 变了，客户端下次需要重拨
- 客户端的 mobileconfig 装的是域名（`IKEV2_SERVER_CN`）→ iOS 重拨时重新 DNS 解析 → 拿到新 IP
- **只要 DNS AAAA 跟着变**（用户做 DDNS 同步）→ 客户端零感知
- DDNS 不在 v2-79 自动做（v2-80+ + 用户明确要求再加）

### 17.4 v2-79 之前的版本

v2-72 之前没有 ipv6watch，ISP 重拨后用户必须手动改 .env + 重 build + 重启容器。v2-72+ 自动感知，v2-79 文档化。

---

## 18. 网络模式自动选择（v2-80 实现）

### 18.1 四种模式对比

| 模式 | 适用场景 | 公网 IP | 端口冲突风险 | v2-80 支持 |
|---|---|---|---|---|
| **`network_mode: host`** | 主流推荐，宿主有公网 IP | ✅ 直绑 | ⚠️ UDP 500/4500 与宿主机其他服务冲突 | ✅ 默认 + auto 探测首选 |
| **`bridge` + libipsec** | 端口冲突时 fallback；ESP 走 UDP/4500 绕过 XFRM | ⚠️ 需要端口映射 | ✅ 无冲突 | ✅ auto 探测 fallback（仅有公网 v4 时） |
| **`bridge` + ipvlan** | 想做网络隔离 + 有公网 IPv6 子网 | ✅ | ⚠️ 需手工建 ipvlan 网络 | ✅ auto 探测 fallback + auto-create ipvlan 网络 |
| **`bridge` + macvlan** | fallback for ipvlan 不支持的环境 | ⚠️ 父接口是宿主物理网卡 | — | ❌ 不提供模板 |

**v2-80 默认 host 网络**（docker-compose.yml `network_mode: host`），但推荐用 `./scripts/up.sh` 自动探测。

### 18.2 自动选择策略（v2-80 实现）

```bash
# 探测逻辑（v2-80 scripts/auto-network.sh 实现）
# 规则严格按 design.md §18.2 优先级:
if OUT_IF 上有 2000::/3 公网 IPv6:
    IKEV2_NETWORK_MODE=host          # 直绑宿主网卡
elif OUT_IF 上有公网 IPv4:
    IKEV2_NETWORK_MODE=bridge        # + libipsec + 端口映射 500:500/udp 4500:4500/udp
else:
    IKEV2_NETWORK_MODE=ipvlan        # 自动 docker network create -d ipvlan
```

**实现机制**：业界标准做法（`docker-compose.override.yml`）。

- 探测脚本 `scripts/auto-network.sh` 在 docker compose 启动**前**（宿主上）跑
- 探测结果写到 `docker-compose.override.yml`，docker compose 自动合并
- 用户命令从 `docker compose up -d` 改成 `./scripts/up.sh`（包了一层探测）
- 老用户继续 `docker compose up -d` 100% 兼容（docker-compose.yml 默认 host 网络）

### 18.3 v2-80 实现状态

- ✅ `scripts/auto-network.sh` 实现探测 + 生成 override
- ✅ `scripts/up.sh` 包装探测 + 启动
- ✅ `docker-compose.yml` 头部注释引用 `./scripts/up.sh`
- ✅ `entrypoint.sh` §0.7 校验实际模式 vs 声明模式（不匹配时 WARN 提示跑 up.sh）
- ✅ docker-compose.override.yml 加入 `.gitignore`（每次自动生成，不入库）
- ✅ ipvlan 网络 auto-create（幂等；失败时退回 bridge+libipsec）
- ✅ 用户显式 `IKEV2_NETWORK_MODE=host|bridge|ipvlan` 完全尊重

### 18.4 跟原 §18.3 的对比

| 项 | v2-79 | v2-80 |
|---|---|---|
| `IKEV2_NETWORK_MODE` | docker-compose.yml 加了字段但没读 | `./scripts/up.sh` 真正使用 |
| 探测逻辑位置 | 设计文档占位 | `scripts/auto-network.sh` 实际跑 |
| ipvlan 网络 | 用户手工建 | auto-create |
| 用户体验 | 改 docker-compose.yml 注释 | `./scripts/up.sh` 一行 |

---

## 19. 关键设计决策（v2-79）

### 19.1 为什么 `IKEV2_OUT_IF` 仍然可选保留

即使 90% 场景自动探测就够，留 `IKEV2_OUT_IF` 让用户能：
- 多网卡服务器指定特定 WAN 口（如：内网 192.168.1.x + WAN 192.168.50.x）
- 容器化但需要固定出口（如 K8s sidecar）
- 调试时手动锁定
- 强制走某个虚拟接口（VPN inside interface `ipsec0`）

**注意**：v2-79 探测出的 `$OUT_IF` 是宿主真实网卡（如 `ens18`），不是 `ipsec0`；swanctl.conf 的 `local_addrs` 仍然按公网 IP 监听（由 §0.2 探测）。

### 19.2 为什么 VPN 内部段候选只 5 个

简单胜于复杂。家用场景下 `10.10.0.0/24` / `10.13.0.0/24` / `10.17.0.0/24` / `10.42.0.0/24` / `10.66.0.0/24` 已覆盖 99% 冲突场景。
真发生冲突可以传 `IKEV2_VPN_SUBNET=10.99.0.0/24` 手动指定。

### 19.3 DDNS（v2-82 实现）

v2-82 把 §19.3 的扩展点实现了：**阿里云 alidns AAAA 记录自动同步**。

**适用场景**：
- VPS 公网 IPv6 动态分配（SLAAC / PPPo6E 重拨），DDNS 把域名解析跟着变
- 客户端拿域名（vpn.example.com）拨号，DNS 解析后取到当前 VPS 的真实 IPv6
- 用户不用手动改 .env + 重建 + 重启容器

**架构**：

```
┌────────────────────────────────────┐     ┌──────────────────────┐     ┌──────────────────┐
│ swanctl.DetectGlobalV6 (已有)       │ ──► │ ddns.Sync (新增)      │ ──► │ alidns HTTP API  │
│ (读 /proc/net/if_inet6,60s 周期)    │     │ (节流 + 重试 + 状态)   │     │ (UpdateRecord)   │
└────────────────────────────────────┘     └──────────────────────┘     └──────────────────┘
```

**关键设计决策**：
1. **不开新 goroutine 轮询 IP**：直接复用 `swanctl.RunIPv6Watch` 已有的 60s 探测周期
2. **节流 60 秒**：Throttle 窗口内即使 IP 变也不调 API（防阿里云限流）
3. **证书无关**：证书 SAN 只放域名（v2-79 重构已定），DNS-01 续签跟 IP 变化解耦——DDNS 不触发重新签证书
4. **失败重试 + 告警**：3 次重试（1s/4s/9s 退避）后仍失败 → 写 `/data/le/LAST_DDNS_FAILED` → 面板告警横幅
5. **开关**：env `IKEV2_DDNS_ENABLED` + 运行时 state file `/etc/ikev2/ddns.conf`（面板 UI 可调）
6. **凭证缺失时静默 skip**：避免用户没填凭证时报错刷屏

**为什么不自动签新证书**：
- 证书 SAN 只放域名（v2-79 设计），跟 IP 无关
- DNS-01 验证用 `_acme-challenge.example.com` TXT，跟 AAAA 无关
- IP 变了客户端解析新域名 → 拿到新 IP → 连上 → 证书仍有效

**最小权限的阿里云 RAM 策略**（用户在 RAM 控制台创建）：

```json
{
  "Version": "1",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "alidns:DescribeDomainRecords",
      "alidns:UpdateDomainRecord"
    ],
    "Resource": "acs:alidns:*:*:domain/example.com"
  }]
}
```

**用户配置**（`.env`）：

```bash
IKEV2_DDNS_ENABLED=true
ALIYUN_ACCESS_KEY_ID=your_ak
ALIYUN_ACCESS_KEY_SECRET=your_sk
ALIYUN_DOMAIN=example.com
ALIYUN_RR=vpn   # 完整域名 = vpn.example.com
```

**面板 UI**：
- 卡片显示：当前 IPv6 / 最近同步时间 / 成功失败 / 启用关闭按钮
- 告警横幅：连续 3 次失败时顶部显示"DDNS 同步失败"+ 失败原因
- API：`GET /api/ddns/status` `POST /api/ddns/toggle`

### 19.4 关于"改宿主机配置"的澄清

v2-78 之前用户反馈："你改了我宿主机的网络配置"。

**事实**：
- v2-78 §3.5 的 `iptables -t nat -A POSTROUTING -j MASQUERADE` 是**容器内**（network namespace 隔离）
- 宿主的 iptables 完全没碰（docker 用 host 网络时，容器 = 宿主的网络栈，但 iptables 规则在容器内**追加**到自己的链）
- 宿主的 sysctl forwarding 也没动（v2-79 entrypoint 检测到没开会 FATAL，提示用户在 docker-compose.yml 加 `sysctls:`）

**v2-79 文档明确写**：
- ✅ 容器内 iptables / ip route / ip rule — 由 entrypoint.sh §3.5 / §0 自管理
- ❌ 宿主机 sysctl / iptables / ip rule — **绝不主动修改**
- 需要的 sysctl 通过 docker-compose.yml 的 `sysctls:` 字段注入容器（由 docker daemon 处理）

**v2.86-PR12.16 进一步隔离**：FORWARD ACCEPT + MASQUERADE + MSS clamp 全部切到 nft **自定义表** `ikev2` / `ikev26`
- 之前在 docker daemon 自己的 `filter` / `nat` 表里加规则（跟 daemon 自管规则共用，daemon 重写会挤掉）
- 现在用自定义表名 → docker daemon 完全碰不到 → 不存在"挤掉"风险
- §4.6 DDoS connlimit/hashlimit 仍走 `iptables -I DOCKER-USER`（DOCKER-USER 是 daemon 管的 chain，Docker 官方推荐 hook 点）
- 宿主的 nft 规则同样不碰（network namespace 隔离）

### 19.5 v2-79 仍保留的 v1/v2 老设计（已实现的，不要误以为"没做"）

| 项 | 之前在哪里 | v2-79 状态 |
|---|---|---|
| **`send_cert = always`** | v2-74 修 iOS 不发 CERTREQ 的坑 | ✅ 保留（swanctl.conf L48） |
| **`proposals` 包含 GCM 和 CBC** | v2-74 兼容 iOS 16（CBC）和 iOS 17+（GCM） | ✅ 保留 |
| **`eap_identity = %identity`** | v2-76 修 iOS EAP identity | ✅ 保留 |
| **DNSSettings 4 个 DNS** | v2-78 修 captive.apple.com 探测 | ✅ 保留（mobileconfig + swanctl.conf 双推）→ v2.86-PR12.23 改用 Apple 规范 DNS 顶层 + ServerAddresses |
| **`rekey_time = 24h`** | v2 起步就是 | ✅ 保留 |
| **`mobike = yes`** | v2 起步就是 | ✅ 保留 |
| **`MOBIKE = 1` (mobileconfig)** | v2-74 | ✅ 保留 |
| **`unique = replace`** | v2 起步就是 | ✅ 保留 |

### 19.6 v2-83 阿里云凭证统一

**背景**:v2-82 实施后,DDNS 用 `ALIYUN_ACCESS_KEY_*`,acme.sh 用 `Ali_Key`/`Ali_Secret`,用户配置两套 env,实际是同一对凭证。

**改动**:面板 UI 新增"阿里云 API 凭证"卡,凭证写在 `/data/panel-state/aliyun.creds`(JSON,0600),entrypoint.sh 启动时读这个文件注入到 acme.sh `account.conf`。

**优先级链**(高 → 低):
1. `/data/panel-state/aliyun.creds`(面板 UI 填的)
2. `IKEV2_ALIYUN_KEY_ID` / `IKEV2_ALIYUN_KEY_SECRET`(v2-83 新统一命名,推荐)
3. `ALIYUN_ACCESS_KEY_ID` / `ALIYUN_ACCESS_KEY_SECRET`(v2-82 legacy DDNS,标 deprecated)
4. `Ali_Key` / `Ali_Secret`(acme.sh 原生命名,legacy)

**DDNS 同步直读凭证文件**:v2-83 改用 `ddns.Config.CredentialGetter` 注入函数,每次 tick 从 `/data/panel-state/aliyun.creds` 重读凭证 → 面板 UI 改凭证后,DDNS 下次 tick(默认 60s)自动用新凭证,无需重启容器。

**凭证独立目录 `/data/panel-state/`**:不放 `/data/le/` 是因为 `/data/le/` 已有 LE 私钥 + 续期 backup,backup 任务打包上云时凭证跟着外泄 = 拿到 RAM 全权限 key + LE 私钥。独立目录 + 0700 权限隔离风险资产。

**凭证保存 UX**(评审 C 建议):
- AccessKey Secret 字段 `type="password"`(不显示明文)
- 保存后只显示 KeyID 掩码(首 4 + ... + 尾 4)
- 保存需要 `confirm=yes` 二次确认(防止误操作)
- 顶部黄色横幅提示"未配置凭证 → LE 签发 + DDNS 不可用"

### 19.7 v2-83 面板 HTTPS 证书热重载(SIGHUP)

**背景**:Go `http.Server.ListenAndServeTLS(cert, key)` 启动时读一次 cert,后续不会重读。acme.sh `--reloadcmd` 触发的是 `ikev2-reload.sh`,**只 reload charon,不 reload Go http server** → 续期后 ~60 天内用户看到的都是"过期但仍有效"的旧证书。

**改动**:
- Go 端用 `atomic.Pointer[tls.Certificate]` 缓存当前 cert,`TLSConfig.GetCertificate` 回调 `atomic.Load` 拿最新值(无锁,O(1),**不在握手路径做 I/O**)
- 监听 SIGHUP → 重读 `/data/panel-tls/{cert,key}.pem` → `atomic.Store` 替换
- `ikev2-reload.sh` 续期 hook 加 3 步:复制 LE cert 到 `/etc/swanctl/` + 复制到 `/data/panel-tls/` + `kill -HUP $(pidof ikev2-panel)`

**`TLSConfig` 预构造 vs `ListenAndServeTLS(cert, key)`**:评审 B 发现的坑——Go `ListenAndServeTLS` 在 `srv.TLSConfig == nil` 时内部构造新 cfg 并赋值,导致 `GetCertificate` 丢失。修复:`srv.TLSConfig` 提前赋值,改用 `ListenAndServeTLS("", "")` 让 `GetCertificate` 生效。

**TLS resumption 不重新调 GetCertificate**:续期后到 ticket TTL(默认 1 小时,TLS 1.3 12 小时)之间,长连接 resumption 会继续用旧 cert。这是预期行为(旧 LE 证书仍有效),只是 `curl -vI` 看到的 fingerprint 不一致。

**顺手修 v2-82 告警脱钩**:`CheckLERenewStatus` 内部已经检查 `/data/le/LAST_RENEW_FAILED` 文件存在 → `LastRenewFailed=true`,但 v2-82 实现的 `loadLEWarning` 没看这个字段,只有 `DaysLeft<14` 才告警 → renew-cert.sh 回退到旧证书后面板要等 ~30 天才报"将过期"。v2-83 修复:home 模板显示 `LastRenewFailed=true` 的红色横幅,日志指向 `/var/log/ikev2-renew.log`。

### 19.8 v2-84 DDNS family 选择(v4 / v6 / dual)

**背景**:v2-82/v2-83 DDNS 只同步 AAAA 记录。IPv4-only VPS 用户根本看不到 DDNS 卡片(`main.go` L303 强约束 `cfg.IPv6Only == true`)。现实场景至少三种:纯 IPv4 / 纯 IPv6 / 双栈。

**目标**:DDNS family 可选(枚举 `v4` / `v6` / `dual`),默认 `dual`(向后兼容 + IPv4-only 兜底),运行时由面板 UI 或 env 切换。

**新增配置**:
- `IKEV2_DDNS_FAMILY` ∈ {`v4`, `v6`, `dual`},默认 `dual`
- `IKEV2_DDNS_PROBE_TARGET` IPv4 探测目标,默认 `8.8.8.8`(中国大陆改 `223.5.5.5`)

**关键设计决策**(对应评审 P0 修复):

1. **枚举 vs 双 bool**:选枚举,状态空间更紧(v3 个合法组合 vs 2^2 + 重复 `disabled`),UI 单选 radio 更清晰
2. **IPv4 探测**:`net.Dialer{Timeout: 3s}.Dial("udp", target+":80")` 拿 `LocalAddr()`,跟 v2-83 的 IPv6 探测范式一致(从 `/proc` 拿)
3. **IPv4 强制过滤**(`isGlobalV4`):loopback / link-local / RFC1918 / CGNAT / multicast,**绝不写进 DNS**;照搬 IPv6 的 `isGlobalV6` 范式
4. **并发 upsert**:errgroup-like 用 stdlib `sync.WaitGroup`,v4 retry 1s/4s/9s 不阻塞 v6(避免双栈场景下 A 慢拖累 AAAA)
5. **per-type 节流**:独立 `map["A"|"AAAA"]time.Time`,v4 / v6 节流窗口互不影响
6. **结构化 LastSync**:`V4IP/V4Error/V6IP/V6Error` 独立字段,顶层 `Success = 任一 family 成功过`;不再用单一 `Success+Error` 误导用户(v2-83 dual 下 v4 失败会让 v6 成功"被吞掉")
7. **状态文件 INI 格式**:v2-83 裸 `true|false` 升级 → 启动时自动迁移到 `enabled=true\nfamily=dual`
8. **Atomic write**:复用 `swanctl.AtomicWriteFile`(write tmp + rename),SIGKILL 写中途不会损坏文件

**面板 UI**:DDNS 卡片加 family radio(三个选项),dual 模式下 IPv4 / IPv6 状态独立显示(各自 IP + 各自 error)。`/api/ddns/family` POST handler 双重白名单校验(handler 端 + `Sync.SetFamily` 端)。

**架构约束(必须 host 网络)**:IPv4 DDNS 依赖 `net.Dial("udp", 8.8.8.8:80")` 拿"出口 IPv4"。bridge 网络下:
- `/proc/net/fib_trie` 是宿主视角,容器拨号可能命中容器默认路由 → 返回容器 IP
- `net.Dial` 的 LocalAddr 是 docker bridge NAT 后的容器 IP

当前 docker-compose 默认 host 网络,所以开箱即用。release-notes 强提示。

**复杂度统计**:~280 行净增(代码 + 测试 + 文档),评审 3-agent 0 阻塞(8 个 P0 已修进设计)。

---

## 20. v2-79 验证清单

- [ ] `IKEV2_OUT_IF=`（空）+ `IKEV2_SERVER_ADDR_V6=`（空）+ `IKEV2_SERVER_CN=my.domain` 启动成功
- [ ] 启动日志打印 `[auto-detect] Outbound interface: ens18`、`[auto-detect] Public IPv6: 2408:...`
- [ ] 启动日志打印 `[auto-detect] VPN subnet: 10.10.0.0/24 (LAN: 192.168.50.0/24)`（§0.6 生效）
- [ ] 客户端拨号 → IKE_SA 建立 → 拿到虚拟 IP（如 10.10.0.2）→ Safari 能开网页
- [ ] 改宿主网卡名（ens18 → eth0）+ 重启容器 → 容器自动探测到 eth0 → 正常服务
- [ ] 改宿主 LAN 段为 10.10.0.0/24 + 重启容器 → 容器自动跳过 10.10.0.0/24 → 用 10.13.0.0/24，swanctl.conf pools 也跟着换
- [ ] iptables MASQUERADE 规则的源网段跟 §0.6 选的一致（`iptables -t nat -L POSTROUTING` 看）
- [ ] IPv6 公网地址 ISP 重拨变化 → 60s 内 swanctl 自动 reload → 客户端无需改任何东西
- [ ] iPhone 拨号 → 设置 → VPN → DNS 看到 1.1.1.1 + 8.8.8.8（v2-78 DNS 推送 → v2.86-PR12.23 已改用 Apple 规范 ServerAddresses + DNSProtocol=Cleartext）
- [ ] 用户的 `.env` 里 `IKEV2_SERVER_ADDR_V6=2408:...` 不变（保持旧值），宿主 IP 真变了 → 容器仍能跑（探测会覆盖用户值）

## 21. v2-79 → v2-80 路线图（v2-84 状态更新）

| 项 | 优先级 | v2-84 状态 |
|---|---|---|
| §0.7 自动选网络模式（host/bridge/ipvlan） | 中 | ✅ **已实现**（`./scripts/up.sh` + `scripts/auto-network.sh`，详见 §18） |
| DDNS 同步（阿里云 alidns） | 中 | ✅ **已实现**（v2-82,详见 §19.3） |
| 阿里云凭证统一（面板 UI 卡 + env 优先级链） | 中 | ✅ **已实现**（v2-83,详见 §19.6） |
| 面板 HTTPS 证书热重载（SIGHUP + atomic.Pointer） | 中 | ✅ **已实现**（v2-83,详见 §19.7） |
| `LAST_RENEW_FAILED` 告警脱钩修复 | 低 | ✅ **已实现**（v2-83 顺手修） |
| **DDNS family 选择(v4 / v6 / dual)** | 中 | ✅ **已实现**(v2-84,详见 §19.8) |
| 登录端点限速(5 次失败锁 5 分钟) | 低 | ⏸️ 留作 P2 |
| IPv6 限速(`tc u32` → `tc flower` 或 nftables+cgroups) | 低 | ⏸️ 留作 P2 |
| DDNS 同步（Cloudflare / 其他 provider） | 低 | ⏸️ 留作扩展点(架构预留 Provider interface,但只实现 aliyun) |
| backup.sh / 多 stage / 登录限速文档收口 | 低 | ⏸️ design 标记"已确认不留 / 已修正 / 已妥协" |
| 移动配置自动通知 / 邀请链接 | 不做 | v3 再说 |
| mobileconfig CMS/PKCS#7 签名 | 不做 | v3 再说 |

---

## 22. v2-79 文档状态

- [x] 用户审阅通过 v2-78
- [x] v2-79 增量设计 §15~21 完成
- [x] v2-79 架构理解文档同步（architecture.md §15、§18.1、§11 等同步更新）
- [x] 镜像跨网络环境通用（不再因 IPv6 / 网卡名硬编码需要重 build）
- [x] v2-79.1 增量：三处 Critical 修复（详见 §22.1）
- [x] v2-79.2 增量：Android 原生客户端支持 + iOS 18 ESP DH（详见 §22.2）
- [ ] 用户实测 v2-79.2（生产环境验收）

### 22.1 v2-79.1 增量

三处 Critical 修复（基于三个 agent 并行审查后的实际影响筛选——用户的部署场景下其他问题暂不动）：

| # | 问题 | 文件 | 改动 |
|---|---|---|---|
| C5 | iOS 18 默认推 ecp256 → 降级 MODP2048（耗电/性能差）| `configs/swanctl-ipv6-only.conf:44` | `proposals` 追加 `aes256gcm16-sha256-ecp256` / `aes128gcm16-sha256-ecp256` / `aes256gcm16-sha256-curve25519` / `aes128gcm16-sha256-curve25519`（ECDH 系列在 MODP 之前，best-of 匹配）|
| C4 | server.key.pem 私钥被写成 0o644（任何同主机用户可读）| `cmd/ikev2-panel/main.go:453-477` | `pair` 结构加 `isKey` 字段，私钥 dst 用 0o600，cert 仍 0o644 |
| C6 | mobileconfig DNSSettings 用了 `<array><dict>` 错误格式，iOS 17/18 静默忽略整个节点 | `internal/cert/mobileconfig.go:35` | 改为 `<dict><key>DNS</key><array>...</array></dict>` | ✅ **v2.86-PR12.23 resolved**:改成 Apple [VPN.DNS](https://developer.apple.com/documentation/devicemanagement/vpn/dns-data.dictionary) iOS 14+ 规范形态(顶层 `<key>DNS</key><dict><key>ServerAddresses</key><array>...</array><key>DNSProtocol</key><string>Cleartext</string></dict>`)。DNSSettings 是杜撰 key,任何版本均被静默忽略;ServerAddresses + DNSProtocol 是 iOS 14+ Required pair,缺一个判定"无 DNS"配置。详见 release-notes-v2.86-pr12.23.md。|

**为什么只修这 3 个**：另外 6 个 "Critical" 经审视后属于"通用项目标准 / 你的部署场景用不上 / 是设计妥协项"——详见 §21 v2-80 路线图。

**没动的高 / 中 / 低问题（共 23 个）**全部进 v2-80+ backlog。

### 22.2 v2-79.2 增量

**触发**：v2-79.1 完成后用户对四平台兼容性的提问（"我们一直用 iOS 测，Android/Linux/Windows 怎么办？"）。agent 审查发现两个真问题 + 一个 Android 原生客户端兼容方案需求。

**两条真 Critical + 一个新功能（最终方案是只修前两条，第三条撤回）**：

| # | 类型 | 问题 | 文件 | 改动 | 状态 |
|---|---|---|---|---|---|
| F1 | 真 Critical | iOS 18 ECDH IKE SA 已协商成功，但 ESP SA 独立重协商 DH——v2-79.1 漏改了 `esp_proposals`，数据通道仍走 MODP2048，耗电和性能损失仍在 | `configs/swanctl-ipv6-only.conf:36` | `esp_proposals` 同步追加 4 条 ECDH 系列 | ✅ 已修 |
| F2 | 真 Critical | `internal/cert/generate.go:5` 注释错误"iOS 拒 ECDSA，必须 RSA 2048"（实际 iOS 17+ 完全支持 ECDSA），下次维护会被误导 | `internal/cert/generate.go:5` | 注释改为"默认 RSA 2048 兼容所有客户端；ECDSA (P-256) iOS 17+ 也支持但暂未启用——切 ECDSA 会破坏现有 mobileconfig" | ✅ 已修 |
| ~~A1~~ | ~~新功能~~ | ~~Android 11-15 设置 → VPN → 添加 → 只支持 PSK，不支持 EAP-MSCHAPv2~~ | ~~server 端新增 `ikev2-psk` 段 + PSK 文件管理 + web Android 配置页~~ | ~~详见 §22.2.1~~ | ❌ **已撤回（v2-79.2 末次重构时删除）** |

#### 22.2.1 ~~v2-79.2 Android 原生客户端设计~~ → **方案撤回记录**

**初版方案（v2-79.2 中段，已撤回）**：

agent 审查时给了一个错误的判断："Android 11-15 设置 → VPN → 添加 → 只支持 PSK，不支持 EAP-MSCHAPv2（AOSP issue b/130257419，Google 7 年没修）"。基于这个错误判断，v2-79.2 中段做了：

- `configs/swanctl-ipv6-only.conf` 新增 `ikev2-psk` 段
- `internal/swanctl/writer.go` 加 `EnsurePSK(serverCN)` / `RotatePSK(serverCN)` / `parsePSKConfFull`
- `internal/web/handlers_config.go` 加 `/users/{id}/android` 详情页 + `/users/{id}/rotate-psk` POST
- `cmd/ikev2-panel/main.go` 启动时 EnsurePSK + 注入 PSK 到 Server
- 新增 `web/templates/android.html` 展示 PSK 共享密钥

**撤回原因（用户提问 + 查证后）**：

1. **用户实测发现**：Android 11+ 系统设置里"添加 VPN" → 类型下拉菜单里第一个就是 `IKEv2/IPSec MSCHAPv2`，能填用户名密码（截图证明）—— 这跟 agent 说的"只支持 PSK"明显不符。

2. **查 AOSP 源码反驳**：
   - `android.net.Ikev2VpnProfile.Builder.setAuthUsernamePassword(user, pass, serverRootCa)` Javadoc **明文写**：
     > "Setting this will configure IKEv2 authentication using **EAP-MSCHAPv2**."
   - `com.android.settings.vpn2.ConfigDialog.changeType()` 里 `TYPE_IKEV2_IPSEC_USER_PASS` 就是设置里那个 "IKEv2/IPSec MSCHAPv2" 选项，接受 username + password + ipsecCaCert + ipsecServerCert。
   - `requiresUsernamePassword(TYPE_IKEV2_IPSEC_USER_PASS)` 返回 `true`。

3. **结论**：Android 11+ 原生客户端**直接支持 EAP-MSCHAPv2 + 证书**，跟 iOS mobileconfig 走同一个 server 端 `ikev2-rw` connection 段。不需要 PSK 段、不需要 strongSwan app、不需要任何额外代码。

**v2-79.2 末次重构（删除 PSK 段）**：

- 删除 `configs/swanctl-ipv6-only.conf` 的 `ikev2-psk` 段
- 删除 `internal/swanctl/writer.go` 的 `EnsurePSK` / `RotatePSK` / `parsePSKConfFull`
- 删除 `cmd/ikev2-panel/main.go` 的 EnsurePSK 启动逻辑 + `pskForAndroid` 变量 + Server.PSK 字段
- 删除 `internal/web/server.go` 的 `PSK` 字段 + `POST /users/{id}/rotate-psk` 路由
- 删除 `internal/web/handlers_config.go` 的 `handleRotatePSK`
- 改写 `web/templates/android.html`：从"展示 PSK 共享密钥"改成"展示 4 字段（server / CN / username / password）+ CA 证书下载链接"
- `internal/web/handlers_config.go` 的 `handleUserAndroidConfig` 数据结构从 `{PSK string}` 改成 `{CACertPEM []byte}`

**教训**：

1. agent 引用"AOSP issue b/130257419"是**道听途说、无源码支撑的虚构判断**——本应该先看 AOSP 源码再下结论
2. 用户通过"身边有 Android 设备 + 截图"实测快速揭穿了错误判断，比反复跑代码审查更高效
3. v2-79.2 这次重构净减少约 280 行代码（PSK 段 + PSK 文件 + web 配置 + 启动逻辑），最终方案**比初版更简单、更安全**

#### 22.2.2 v2-79.2 Android 11+ 原生客户端最终方案

**认证协议**：iOS 和 Android 走**同一个 server 端 connection 段（`ikev2-rw`）**：
- iOS 17/18 mobileconfig → `ikev2-rw`（EAP-MSCHAPv2 + 证书）
- Android 11+ 系统设置 → `ikev2-rw`（EAP-MSCHAPv2 + 证书）
- strongSwan app（任何平台） → `ikev2-rw`（EAP-MSCHAPv2 + 证书）

**Android 用户操作流程**（系统原生，不装任何 app）：
1. 设置 → 网络和互联网 → VPN → 右上角 `+`
2. 类型选 `IKEv2/IPSec MSCHAPv2`（**Android 11-15 默认第一个选项**）
3. 填：服务器地址 / IPSec 标识符（= server CN）/ 用户名 / 密码
4. 可选：IPSec CA 证书（自签模式建议装，LE 模式可跳过）
5. 保存 → 点连接

**面板 `/users/{id}/android` 详情页作用**：把 server / CN / 用户名 / 密码 / CA 证书下载链接集中展示，省得 Android 用户去翻 iOS 截图或问管理员。

**Android 没有 mobileconfig 等价物**：iOS 有一键 .mobileconfig 安装流程，Android 必须手动填 4 个字段——这是 Apple vs Google 的产品设计差异，不是 bug。

#### 22.2.3 v2-79.2 23 个 backlog 问题最终结论

23 个问题全部确认**不做**（用户场景不触发）：
- 6 个原 v2-79.1 提的 Critical：3 真已修（C4/C5/C6），3 伪按场景筛掉
- 用户质疑"过度开发"后又提的 5 个"真必要"：经用户逐条反问后，5 个全是伪必要
- 最终所有 23 个问题均进 backlog（**v2-79.2 不发布任何额外修复**）


