# v2.80 (2026-09-18)

## 改进

### iOS mobileconfig: 锁屏不断 VPN + 省电 (`DisconnectOnSleep/DisconnectOnIdle/NATKeepalive/OnDemand`)

调研发现 iOS 用户最常吐槽"VPN 锁屏后掉线,早上微信收不到",根因是 mobileconfig 缺 4 项关键字段。
v2.80 起 mobileconfig 模板默认开启这 4 项(对应 Apple 部署指南 + DTS Engineer 推荐配置):

| 字段 | 值 | 作用 | 来源 |
|---|---|---|---|
| `DisconnectOnSleep` | `<false/>` | iOS 锁屏**不主动断 VPN**(默认是 true,锁屏秒断) | [Apple DTS Quinn, 2018](https://developer.apple.com/forums/thread/95988) |
| `DisconnectOnIdle` | `0` (Never) | 闲置不主动断(默认会按 timeout 断) | [Apple Deployment Guide](https://support.apple.com/zh-cn/guide/deployment/dep4ce9487d/web) |
| `NATKeepaliveEnabled` | `<true/>` | 锁屏后由硬件网卡代发 NAT keepalive | [Apple Deployment Guide](https://support.apple.com/zh-cn/guide/deployment/dep4ce9487d/web) |
| `NATKeepaliveInterval` | `60` 秒 | 间隔(Apple 最小 20) | 同上 |
| `OnDemandEnabled` | `1` | 启用 OnDemand 规则 | strongSwan 官方 OnDemand 模板 |
| `OnDemandRules` | `[{Action: Connect}]` | Wi-Fi ↔ Cellular 切换自动重连 | [strongSwan appleIkev2Profile](https://docs.strongswan.org/docs/latest/interop/appleIkev2Profile.html) |

**注意**:`Always On VPN`(supervised 设备 MDM 才能用)普通描述文件无法开启。
上表 4 项是 mobileconfig 能做到的极限,核心目的是**锁屏不断 VPN**,耗电增加在硬件 NAT keepalive
加持下基本可忽略。

### IPv6 ULA pool 自动探测 (`§0.5b + §0.6b`)

之前 swanctl.conf pools 段 IPv6 写死 `fd00:1::/64`,极少数用户家里 LAN 段正好撞上会路由不到。
现在和 IPv4 `§0.5` + `§0.6` 完全对称 —— `§0.5b` 扫宿主机所有 `fd00::/8` ULA 段,
`§0.6b` 从候选 `fd00:1::/64 → fd00:2::/64 → fd00:3::/64 → fd00:10::/64 → fd00:20::/64`
选第一个不冲突的,写入 `%IKEV2_VPN_SUBNET_V6%` 占位符。

用户也可指定 `IKEV2_VPN_SUBNET_V6=fd00:99::/64` 覆盖。

## 升级

```bash
cd /home/TEST/ikev2-panel-v2
docker compose pull       # 拉 v2.80 镜像
docker compose up -d      # 重启容器
```

## 镜像

- ghcr.io:`ghcr.io/rewind2026/ikev2-panel-v2:2.80`
- 多架构:`linux/amd64`、`linux/arm64`

## 升级提示

- **iOS 用户**:重新下载 mobileconfig 重装 (建议先删旧 profile)
- **旧配置兼容**:`%IKEV2_VPN_SUBNET_V6%` 占位符 v2.79 模板里没有,升级到 v2.80 后 entrypoint 跑一次会写入实际值