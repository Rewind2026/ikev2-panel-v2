# v2.102 — Android IPv6 直连：kernel-libipsec 启用裸 ESP（raw_esp）

发布日期：2026-11-04
镜像：`ghcr.io/rewind2026/ikev2-panel-v2:v2.102`（linux/amd64）

> 完整变更清单见 [CHANGELOG.md](CHANGELOG.md) 的 v2.102 小节。
> ⚠️ **本次必须换镜像**：`raw_esp` 与 IPv6 分片上限都固化在**镜像内**的
> `/etc/strongswan.d/` 下，只改面板配置不生效。

---

## 一句话

**Android（AOSP 系统 VPN）走 IPv6 直连时连不上了 / 建不起隧道。**

容器里的 strongSwan 用的是 `kernel-libipsec`（用户态 ESP，无 XFRM），
它默认**只收发封装 ESP**（`raw_esp = no`）。而 AOSP 的设计是
**IPv6 链路永不浮动到 UDP-4500**（`IkeSessionStateMachine.needEnableEncapulation()`
仅 IPv4 为真，v6 不做 NAT-D 检测），只发裸 ESP（IP 协议 50）——
于是裸 ESP SA 装不上，CHILD_SA 建不起来。

本次把 `kernel-libipsec` 的 `raw_esp` 打开，**一行镜像内配置**解决：

```c
// strongSwan 6.0.1 源码 kernel_libipsec_plugin.c
if (lib->settings->get_bool(lib->settings,
                "%s.plugins.kernel-libipsec.raw_esp", FALSE, lib->ns))
{
        this->esp_handler = kernel_libipsec_esp_handler_create();  // RAW socket 收发裸 ESP
        ...
}
```

`kernel_libipsec_ipsec.c` 里 `require_encap = !lib->get(lib, "kernel-libipsec-esp-handler")`
—— `raw_esp = yes` 直接让裸 ESP SA 可安装。

**没有改动任何 strongSwan 源码**（已用官方源码包逐字节校验，见下）。

---

## 改了什么

**2 个文件：`Dockerfile` + 一条过时测试断言。**

| 文件 | 改动 |
|------|------|
| `Dockerfile` | ① 构建期写 `/etc/strongswan.d/charon/kernel-libipsec.conf`：`load = yes` + `raw_esp = yes`<br>② 构建期写 `/etc/strongswan.d/zz-fragment-size.conf`：`charon.fragment_size_v6 = 1150` |
| `internal/cert/mobileconfig_opts_test.go` | 修一条 **v2.101 遗留的过时断言**：`TestRenderMobileconfig_NilOpts_UsesDefault` 仍期望默认输出 `OnDemandRules`，但 v2.101 已把出厂默认 `OnDemandEnabled` 翻为 `false`（完全手动），默认**不应**输出 `OnDemandRules`。**只改测试，不改任何运行时行为** |

Go 面板运行时逻辑零改动。描述文件里的版本号（`PanelVersion`）**不变**
（按 v2.93–v2.100 惯例，纯服务端 / 构建期变更不 bump），
**不需要重新安装 iOS 描述文件**。

### 顺带固化了 IPv6 分片上限

`fragment_size_v6 = 1150` 此前只是容器内手工 drop-in，**重建容器就会丢**。
本次一并固化进镜像。

背景：2026-10-30 诊断发现服务器 → 手机方向的 4×1232 字节 IKE_AUTH 分片**全丢**
（疑似运营商按包大小丢弃），显式把 IKEv2 IPv6 分片总大小限到 1150 字节
（线上每片约 1102，低于 1200 门槛）。

客户端按 **RFC 7383** 自动适配，对 iOS / Android / Windows **完全无感**，
不需要任何客户端侧改动。

---

## ⚠️ 行为变更

| 项 | 之前 | 现在 |
|----|------|------|
| kernel-libipsec ESP 路径 | 仅封装 ESP（`raw_esp = no`） | **裸 ESP + 封装 ESP 都支持** |
| 裸 ESP SA（IP 协议 50） | 装不上 | **可安装** |
| Android IPv6 直连 | CHILD_SA 建不起来 | **正常** |
| IPv6 分片总大小 | 1280（默认） | **1150**（低于 1200，避免被运营商丢弃） |
| iOS / Windows（NAT 后 UDP-4500） | 正常 | **正常**（不受影响） |

> 裸 ESP 与封装 ESP 是**并存**关系，不是二选一：iOS / Windows 走 NAT 后仍是标准
> UDP-4500 封装路径，Android 直连走裸 ESP。

---

## 升级方法

```bash
docker compose pull
docker compose up -d
```

自检（看到 `raw_esp = yes` 且 SA 是非封装状态就说明生效了）：

```bash
# 1. 配置在位
docker compose exec ikev2-panel cat /etc/strongswan.d/charon/kernel-libipsec.conf
docker compose exec ikev2-panel cat /etc/strongswan.d/zz-fragment-size.conf

# 2. Android 连上后看 SA：local 端口应保持 [500]（未浮动到 4500）
docker compose exec ikev2-panel swanctl --list-sas
```

预期（Android IPv6 直连）：

```
ikev2-rw: #4, reqid 1, INSTALLED, TUNNEL, ESP:AES_GCM_16-256
   local  ...[500]      ← 注意是 500，不是 4500
   remote ...[10.10.0.1]
```

看到 `[500]` + `TUNNEL`（**非封装**）即裸 ESP 路径成立。

---

## 为什么没改 strongSwan 源码

排查过程中曾对 `ike_natd.c` 打过「fake NAT」三刀补丁（骗 charon 认为链路上有 NAT，
从而走封装路径）。`raw_esp = yes` 生效后这些补丁**全部失效**
（补丁针对的正是封装 ESP 路径），已于 2026-11-04 **全部回滚为上游原版**。

当前生产部署的 `ike_natd.c` 与官方 `strongswan-6.0.1.tar.bz2`
（MD5 `c3ddc81d1d11ce3d5431e15da7718748`）中的同名文件
**MD5 完全一致**：`50dc1df92ab60fc7722957287603036c` —— **纯原版、零代码补丁**。

镜像里只有两处运行时配置偏离上游默认值（均已固化进 Dockerfile）：
`kernel-libipsec.raw_esp = yes` 与 `charon.fragment_size_v6 = 1150`。

---

## 实测（192.168.50.130 生产环境）

| 客户端 | 结果 |
|--------|------|
| **Android**（AOSP 系统 VPN，IPv6 直连） | SA `INSTALLED, TUNNEL`（**非封装**），local 端口保持 `[500]` 未浮动，`udp4500` 流量为 0；in/out 计数与 `nft esp` 计数、`ipsec0` TX 相互吻合 |
| **iOS** | 走 NAT 后的标准 UDP-4500 封装路径，**不受影响** |
| **Windows** | 走标准 UDP-4500 路径，**正常** |

---

## 回滚

```bash
# 删掉 raw_esp 与分片上限，重启容器即回到封装 ESP 路径
docker compose exec ikev2-panel sh -c '
  sed -i "/raw_esp = yes/d" /etc/strongswan.d/charon/kernel-libipsec.conf
  rm -f /etc/strongswan.d/zz-fragment-size.conf
'
docker compose restart ikev2-panel
```

（若通过本仓库新版本部署，回滚 = 换回 `:v2.101` 镜像。）

---

## 客户端影响

| 客户端 | 变化 |
|--------|------|
| Android 12+（系统原生 IKEv2） | **修复**：IPv6 直连下隧道可建立、可上网 |
| iOS / macOS | 无变化 |
| Windows 10/11 | 无变化 |

---

## 文档

* [CHANGELOG.md](CHANGELOG.md) —— v2.102 完整变更清单
* [TROUBLESHOOTING.md](TROUBLESHOOTING.md) —— Android 连不上 / 有隧道无流量排查
