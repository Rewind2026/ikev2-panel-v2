# v2.100 — 修好 LE 证书链：Windows 11 收不到中间 CA，连接必然失败

发布日期：2026-10-29
镜像：`ghcr.io/rewind2026/ikev2-panel-v2:v2.100`（linux/amd64）

> 完整变更清单见 [CHANGELOG.md](CHANGELOG.md) 的 v2.100 小节。
> ⚠️ **本次必须换镜像**：证书拆分逻辑在 `entrypoint.sh` / `ikev2-reload.sh` 里，
> 只改配置文件不生效。

---

## 一句话

**用户诉求：「把证书部分修复一下。」**

之前服务器把 Let's Encrypt 的整份 fullchain 塞进**一个** `.pem` 文件，
而 strongSwan **每个证书文件只加载第一张** —— 于是服务器在 IKE_AUTH 阶段
**只发叶子证书，不发中间 CA**。Windows 11 的 IKEEXT 用系统根库构建信任链、
**不做 AIA 补链**，拿不到中间 CA 就直接丢弃响应（客户端侧连错误包都收不到）。

现在改成 strongSwan 的标准姿势：**叶子 → `x509/`，链上每张 CA → `x509ca/`**
（每张单独一个文件），并支持 AIA 向上补根兜底。

---

## 之前为什么连不上

### 证书链只发叶子

strongSwan 加载 `x509/` 与 `x509ca/` 下的证书时，**每个文件只读第一张**，
同一文件里后续的证书被静默忽略。旧逻辑：

```
/data/le/fullchain.pem  ──整份拷贝──▶  /etc/swanctl/x509/<domain>.pem
                                        （内含 leaf + intermediate + root）
                                        ↓ charon 只认第一张
                                        ↓ IKE_AUTH 只发 leaf
```

LE 从 2025 起默认签发的 fullchain 是 **3 张**（leaf → Let's Encrypt YR2 → ISRG Root YR），
所以丢的不止一张。

### 为什么 iPhone 没事、Windows 有事

| 客户端 | 链构建方式 | 结果 |
|--------|-----------|------|
| iOS / strongSwan App | **自己**按 AIA 补链（缺中间 CA 就去 AIA URL 拉） | 正常 |
| Windows IKEEXT | 用系统根库构建链，**不做 AIA 补链** | 缺中间 CA → 静默失败 |

Windows 的表现是「正在连接 → 立即失败」，抓包能看到 IKE_SA_INIT / IKE_AUTH
发出但收不到任何响应 —— 这不是「证书过期」类错误，客户端不会弹任何提示。

### 为什么 v2.86-PR19(A11) 的「剥根」没解决问题

A11 的做法是把 fullchain 末尾的根证书剥掉、只留 leaf + intermediate。
方向没错（RFC 5246 §7.4.1 确实不建议把信任锚塞进链里），但**只解决了「多不多」，
没解决「怎么装」** —— 剩下的 2 张证书仍然装在同一个文件里，charon 照样只认第一张。

---

## 改了什么

| 文件 | 改动 |
|------|------|
| `scripts/install-cert-chain.sh` | **新增**。fullchain 拆分 + AIA 向上补链 + 指纹去重 + 幂等清理 + 原子安装（POSIX sh，busybox 兼容） |
| `scripts/entrypoint.sh` | §7.5 删掉 A11 剥根块，改为调用 `install-cert-chain.sh`；helper 失败时回退旧的整份拷贝 |
| `scripts/ikev2-reload.sh` | 续签路径（`acme.sh --reloadcmd` hook）同款修复 —— 旧逻辑续签后整份 fullchain 又会原样进 `x509/`，问题复发 |
| `Dockerfile` | `COPY scripts/install-cert-chain.sh` + `chmod +x` |

Go 面板程序零改动。

### 安装后的目录长这样

```
/etc/swanctl/x509/vpn.example.com.pem          ← 只有叶子
/etc/swanctl/x509ca/vpn.example.com-chain-1.pem ← Let's Encrypt YR2
/etc/swanctl/x509ca/vpn.example.com-chain-2.pem ← ISRG Root YR（交叉签发，作为链一环下发）
```

配合 swanctl.conf 里已有的 `send_cert = always`，服务器会在 IKE_AUTH
带出完整链，Windows 11 才能构建出信任路径。

### AIA 向上补链（兜底）

链内最深一张若不是自签根，就读它的 `CA Issuers` AIA URI 把 issuer 拉下来装进
`x509ca/`，用 sha256 指纹去重防循环，最多向上走 4 层。fullchain 里本来就带全时
这一步自然空转；只对「只有叶子」的非标 fullchain 有意义。

---

## 为什么这样改是安全的

* **不再改写源文件**：旧逻辑会 `cp` 回写 `/data/le/fullchain.pem`（把 3 张剥成 2 张，
  续签前源文件已被污染）。新逻辑只读源文件，全部在临时目录处理。
* **失败可回退**：helper 非 0（fullchain 损坏 / 磁盘满）时自动退回旧的整份拷贝，
  行为等同 A11 之前（leaf-first，iOS 仍可用），不会让容器起不来。
* **iOS / macOS 协商结果不变**：只是把证书换了个装法，发出的链更完整，
  不会改变已协商上的算法。
* **续签路径同步修复**：这是旧实现最容易漏的地方 —— 首启修好了，续签一次就退化回去。

---

## ⚠️ 行为变更

| 项 | 之前（v2.99） | 现在（v2.100） |
|----|--------------|--------------|
| `/etc/swanctl/x509/<domain>.pem` | 整份 fullchain（charon 只认第一张） | **只有叶子** |
| `/etc/swanctl/x509ca/` | 空 | **每张链 CA 一个独立文件** |
| 续签后是否退化 | **会**（整份又进 x509/） | **不会**（走同一个拆分 helper） |
| `/data/le/fullchain.pem` | 被剥根回写（污染） | **只读，不改** |
| Windows 11 证书链 | 只有 leaf → 静默失败 | **完整链 → 可构建信任路径** |

---

## 升级方法

```bash
docker compose pull
docker compose up -d
```

升级后自检（**每张证书一个文件**就是修好的标志）：

```bash
# 叶子：1 张
docker exec ikev2-panel sh -c 'grep -c "BEGIN CERTIFICATE" /etc/swanctl/x509/vpn.example.com.pem'
# 链 CA：LE 2025+ 是 2 张
docker exec ikev2-panel sh -c 'ls /etc/swanctl/x509ca/ | grep chain-'
docker exec ikev2-panel sh -c 'cat /etc/swanctl/x509ca/vpn.example.com-chain-*.pem | grep -c "BEGIN CERTIFICATE"'
```

预期：x509 计数 = 1，x509ca 里能看到 `chain-1` / `chain-2`，
且**每个文件里只有 1 张证书**（`grep -c` 分别 = 1）。

容器日志里会看到：

```
[cert-chain] chain CA #1 -> /etc/swanctl/x509ca/<domain>-chain-1.pem (CN=Let's Encrypt YR2)
[cert-chain] chain CA #2 -> /etc/swanctl/x509ca/<domain>-chain-2.pem (CN=ISRG Root YR)
[cert-chain] leaf -> /etc/swanctl/x509/<domain>.pem (total: leaf + 2 chain CA + 0 AIA issuer)
```

> ⚠️ 若你的 compose 里把 `pull_policy` 改成了 `never`（防 `up` 时被旧镜像覆盖），
> **记得改回 `always` 再 pull**。

---

## 客户端影响

| 客户端 | 变化 |
|--------|------|
| Windows 11 | **证书链不再缺失**（能否真正跑通数据面另见下方「已知限制」） |
| iOS / macOS | 无变化（本来就自己补链） |
| Android 12+ | 无变化（Android 侧证书校验逻辑未动） |

---

## 已知限制（与本次修复无关）

本次只修证书链。**Windows 11 在「端到端公网 IPv6 且无 NAT」的链路上，
会按 RFC 3948 发原生 ESP（IP 协议 50）**；容器用的 `kernel-libipsec` 是用户态 ESP，
内核收不到协议 50 便回 `ICMPv6 parameter problem`，Windows 收到后反复重建 CHILD_SA。

- 这是 **Windows 侧行为 + 容器无 XFRM** 的结构性组合，服务器侧已无解
  （伪造 NAT-D、`encap = yes` 均实测无效）
- 表现：**IKE 能连上、ping 不通**
- 可行方向：走 IPv4/NAT 链路，或改用带 XFRM 的宿主内核 strongSwan

排查详见 [TROUBLESHOOTING.md](TROUBLESHOOTING.md)。

---

## 文档

* [TROUBLESHOOTING.md](TROUBLESHOOTING.md) —— 证书链与 Windows 原生 ESP 相关排查
* [CHANGELOG.md](CHANGELOG.md) —— v2.100 完整变更清单
* `scripts/install-cert-chain.sh` —— 拆分 + AIA 补链实现
