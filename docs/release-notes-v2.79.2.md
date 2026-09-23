# v2.79.2 (2026-09-17)

## 重大改动

### Android 11+ 系统原生客户端 EAP-MSCHAPv2 支持

v2.79.2 起，**Android 11+ 设备可以用系统原生 VPN 客户端**（无需第三方 APK）连接 IKEv2/IPsec 服务端，使用 **EAP-MSCHAPv2** 用户名/密码认证。

之前 Android 原生客户端只支持 PSK（共享密钥），v2.79.1 起我们统一删除了 PSK 段，改为 EAP-MSCHAPv2 统一认证机制。

#### 配置 Android 客户端

1. 系统设置 → 网络和互联网 → VPN → 添加 VPN
2. 类型选 **IKEv2/IPSec MSCHAPv2**
3. 服务器地址：`your.domain.com`
4. IPSec 标识符：留空或填服务器域名
5. 用户名 / 密码：在管理面板新建用户后填入

### iOS 18 ESP DH 协商修复

iOS 18 系统更新后默认禁用了几组弱 DH group，导致 ESP 协商失败。v2.79.2 调整 swanctl.conf，**禁用 server 主动提议弱 DH group、保留 client 接受所有合规 group**，保证与老客户端兼容。

### 部署改进：forwarding 自愈 + 持久化

新增 entrypoint §3 自愈链路 + docker-compose bind mount 持久化，**新机器首次部署完全无人值守**：

- 检测 `/proc/sys/net/ipv4/ip_forward` 和 `/proc/sys/net/ipv6/conf/all/forwarding`
- 未启用 → 容器内 `echo 1` 写到宿主（host 网络模式共享 net namespace）
- 同时写入宿主 `/etc/sysctl.d/99-ikev2.conf`，systemd-sysctl.service 启动时自动加载
- bind mount `/etc/sysctl.d:/host-sysctl.d:rw` 在容器内直接持久化

## 升级

```bash
cd /home/TEST/ikev2-panel-v2
docker compose pull       # 拉 v2.79.2 镜像
docker compose up -d      # 重启容器
```

## 镜像

- ghcr.io：`ghcr.io/rewind2026/ikev2-panel-v2:2.79.2`
- 多架构：`linux/amd64`、`linux/arm64`

## 已知问题（v2.80 计划修复）

- DB 事务粒度（部分写路径仍裸 SQL）
- swanctl reload 偶发竞态
- fd 上限硬编码
- Linux 客户端 conf 生成缺失
- Windows .pfx 导出未实现
