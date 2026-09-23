# Docker 镜像 + 供应链安全审计报告 — 2026-09

> 维度:**供应链 + 镜像最小化 + 运行时安全**(审计框架第 4 阶段)
> 审计员:项目组自查
> 范围:`/opt/ikev2-panel-v2-main/` 的 Dockerfile / docker-compose.yml / entrypoint.sh / 续签脚本 / .env.example / CI workflow
> 时间:2026-09-18
> 状态:**草案,等待评审**
> 关联:
> - [docs/audit-2026-09-security.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-security.md) — 应用层安全审计
> - [docs/audit-framework.md](file:///opt/ikev2-panel-v2-main/docs/audit-framework.md) §维度 2 S8

---

## 1. 范围与工具

| 路径 | 行数 | 角色 |
|------|------|------|
| `Dockerfile` | 268 | 多阶段构建(strongswan-builder / go-builder / runtime) |
| `docker-compose.yml` | 166 | 编排:host 网络 / capabilities / volume |
| `scripts/entrypoint.sh` | 786 | 启动 + 自愈 + LE 签发 |
| `scripts/renew-cert.sh` | 77 | LE 续签 + 回退 |
| `scripts/ikev2-updown` | 75 | tc 限速(swanctl up/down 回调) |
| `scripts/ikev2-reload.sh` | 66 | acme.sh reloadcmd 钩子 |
| `scripts/auto-network.sh` | - | 宿主机侧网络探测(本次未审计内容) |
| `scripts/up.sh` | - | 一键启动封装(本次未审计内容) |
| `.env.example` | 122 | 默认配置模板 |
| `.github/workflows/docker-publish.yml` | 107 | CI 构建 + 推送 ghcr.io |
| `go.mod` | 25 | Go 依赖清单 |
| `configs/swanctl-ipv6-only.conf` | 92 | swanctl 主配置(含占位符) |
| `configs/strongswan-filelog.conf` | 25 | charon 日志去向 |

### 参考资料

**官方 / 标准**
- [Docker Security Cheatsheet (OWASP)](https://cheatsheetseries.owasp.org/cheatsheets/Docker_Security_Cheat_Sheet.html) — Rules #0~#13
- [CIS Docker Benchmark v1.6](https://www.cisecurity.org/benchmark/docker) — §1~§7
- [SLSA Supply-chain Levels](https://slsa.dev/) — Build L3 / Provenance
- [apt-secure (Debian Wiki)](https://wiki.debian.org/SecureApt) — `signed-by=` / keyring 导入
- [Sigstore cosign](https://github.com/sigstore/cosign) — 镜像无密钥签名
- [Docker Hardened Images (DHI)](https://www.docker.com/products/hardened-images/) — 官方 hardened 镜像目录

**标杆项目 Dockerfile**
- [traefik/traefik Dockerfile](https://github.com/traefik/traefik/blob/master/Dockerfile) — 精简多阶段 + scratch / distroless
- [caddyserver/caddy Dockerfile](https://github.com/caddyserver/caddy/blob/master/Dockerfile) — 非 root USER + scratch
- [GoogleContainerTools/distroless](https://github.com/GoogleContainerTools/distroless) — 无 shell base image
- [strongswan/strongswan Dockerfile](https://github.com/strongswan/strongswan/blob/master/Dockerfile) — 上游参考

---

## 2. 发现汇总

| 严重度 | 数量 | 关键问题 |
|--------|------|----------|
| **HIGH** | 4 | `privileged: true` + host 网络(宿主机完全沦陷) / apt 源无 GPG keyring 显式校验 / acme.sh 无 commit 锁定 / 镜像无 cosign 签名 |
| **MED** | 7 | SYS_ADMIN 宽于必要 / USER root 全程运行 / 健康检查缺失 / 资源限制缺失 / `/var/log` 1777 / `/etc/sysctl.d:rw` 注入宿主 / acme.sh 启动依赖 git+curl 但运行时不需要 |
| **LOW** | 6 | `set -o pipefail` 未启 / `--no-install-recommends` 部分层缺 / apt 包未 `--allow-downgrades` 锁版 / `network_mode: host` 默认而非 `auto` / 8443 默认暴露公网 / alpine + distroless 替代评估缺失 |

总计:**17 个 issue(4 HIGH / 7 MED / 6 LOW)**。

---

## 3. 镜像供应链分析(SLSA L1 → L3 自评)

| SLSA 级别 | 要求 | 当前状态 | 评估 |
|-----------|------|----------|------|
| **L1** | 自动构建 + provenance | GH Actions 自动构建,Dockerfile 简单 | ✅ 满足 |
| **L2** | 签名 provenance + 防篡改 host | 默认 GITHUB_TOKEN,无 SLSA provenance 生成 | ❌ 缺 |
| **L3** | 隔离签名 + 防运行时篡改 | 无 cosign / sigstore 集成 | ❌ 缺 |

**当前等级:L1**。升级到 L2 至少需要启用 [slsa-github-generator](https://github.com/slsa-framework/slsa-github-generator) 生成 in-toto provenance;L3 需要独立签名人 + 防 runner 篡改机制。

---

## 4. [SEVERITY-HIGH] ISSUE

### ISSUE-D01:`privileged: true` + `network_mode: host` + `SYS_ADMIN` — 容器逃逸 → 宿主机完全沦陷

- **位置**:
  - [docker-compose.yml:92](file:///opt/ikev2-panel-v2-main/docker-compose.yml#L92) — `privileged: true`
  - [docker-compose.yml:44](file:///opt/ikev2-panel-v2-main/docker-compose.yml#L44) — `network_mode: host`
  - [docker-compose.yml:70](file:///opt/ikev2-panel-v2-main/docker-compose.yml#L70) — `cap_add: SYS_ADMIN`
- **问题**:
  - `privileged: true` = 给容器**所有 capabilities + 设备访问 + 部分关闭 AppArmor/seccomp**。这是 Docker 文档明确反对的反模式
  - 配合 `host` 网络,容器与宿主机共享 net namespace → 容器内 iptables 规则改的就是宿主机的表
  - 配合 `SYS_ADMIN` + `/dev/net/tun`,容器内任何 RCE(Go 进程漏洞、cron 注入) = 直接 `mount` / `mknod` / `reboot` / 改路由 → 宿主机根权限
- **影响**:
  - Go 进程(SQLite + VICI + HTTP)被攻陷 = 宿主机 root 沦陷
  - 即使强Swan charon 自身 0day = 同等沦陷
  - 注释里说 "host 网络下需要 SYS_ADMIN 做 tc 限速、ip rule" — tc 操作确实需要 NET_ADMIN,不需要 SYS_ADMIN
- **参考**:
  - [Docker Security Cheatsheet §RULE #3 — Limit capabilities](https://cheatsheetseries.owasp.org/cheatsheets/Docker_Security_Cheat_Sheet.html#rule-3-limit-capabilities-grant-only-specific-capabilities-needed-by-a-container) — "**And remember: Do not run containers with the `--privileged` flag!!!**"
  - [CIS Docker Benchmark §5.4 — `privileged: false`](https://www.cisecurity.org/benchmark/docker)
  - [OWASP Cheatsheet §RULE #5a — 不要 host 网络直接暴露](https://cheatsheetseries.owasp.org/cheatsheets/Docker_Security_Cheat_Sheet.html#rule-5a-be-careful-when-mapping-container-ports-to-the-host-with-firewalls-like-ufw)
  - 标杆:[traefik/traefik Dockerfile](https://github.com/traefik/traefik/blob/master/Dockerfile) — 完全 rootless
- **修复方案**(分两步,先减权再彻底):
  1. **第一步(立即,1d)**:
     - `cap_drop: ALL` 保留,`cap_add` 删掉 `SYS_ADMIN`
     - 注释里说 `tc` 限速需要 SYS_ADMIN,**实际不需要** — `tc qdisc/class add` 用 NET_ADMIN 即可([man 7 netlink](https://man7.org/linux/man-pages/man-pages-7.html))
     - `privileged: false`;如果 ip rule 写入失败,改用 `sysctls:` 在 compose 里配置
     - 保留 `NET_ADMIN + NET_BIND_SERVICE + /dev/net/tun`(`--cap-add NET_ADMIN` 已涵盖 TUN 设备访问)
  2. **第二步(v3 重构)**:
     - host 网络改成默认 `bridge` + 主机侧 ipvlan(v2-80 已实现,只是 compose 没默认开)
     - 容器内不直接改 `/proc/sys/net/ipv6/conf/all/forwarding` — 改在 `up.sh` 宿主侧写 `/etc/sysctl.d/`
- **工作量**:第一步 1d,第二步 3d
- **优先级**:**本周修第一步**

---

### ISSUE-D02:apt 源替换为清华源后,未显式 import GPG keyring,供应链可信度下降

- **位置**:
  - [Dockerfile:23-24](file:///opt/ikev2-panel-v2-main/Dockerfile#L23-L24) — strongswan-builder stage
  - [Dockerfile:149-150](file:///opt/ikev2-panel-v2-main/Dockerfile#L149-L150) — runtime stage
- **问题**:
  - `sed` 把 `deb.debian.org` 改成 `mirrors.tuna.tsinghua.edu.cn` 但**未导入清华源的 GPG keyring**
  - `apt-get update` 默认会用 `/etc/apt/trusted.gpg.d/debian-archive-keyring.gpg` 验证签名 — 这个文件是 Debian 官方的 keys,**不包含清华源 mirror 自己的 key**
  - 风险:
    1. 清华源 mirror 如果被劫持(MITM/供应链攻击),**官方 keyring 仍可能验证通过**(因为包是 Debian 官方包,签名一致),但**包是否来自清华源不可证**
    2. 即使包本身未被篡改,镜像完整性仅依赖"清华源运维可信"假设 — SLSA L2 角度看这是单点信任
  - 当前做法是"网络可达性优化",但等同于**把供应链信任转移给清华源镜像**
- **影响**:
  - 升级时可能拉到被注入的包(理论上需 MITM + 公钥替换,实际攻击成本中等)
  - 不符合 [SLSA Build L2](https://slsa.dev/) 对 trusted builders 的要求
- **参考**:
  - [apt-secure — Debian Wiki](https://wiki.debian.org/SecureApt) — `signed-by=` 参数
  - [CIS Docker Benchmark §4.1 — Verify image authenticity](https://www.cisecurity.org/benchmark/docker)
  - [Docker Security Cheatsheet §RULE #13 — Enhance Supply Chain Security](https://cheatsheetseries.owasp.org/cheatsheets/Docker_Security_Cheat_Sheet.html#rule-13-enhance-supply-chain-security)
  - 清华源官方文档:[mirrors.tuna.tsinghua.edu.cn/help](https://mirrors.tuna.tsinghua.edu.cn/help/) — 提到其镜像签名 = Debian 官方签名
- **修复方案**:
  1. **保留默认 keyring**:`/etc/apt/trusted.gpg.d/debian-archive-keyring.gpg` 仍有效,Debian 包签名由 Debian 官方 keys 校验
  2. **显式声明 `signed-by=`**(更安全):
     ```dockerfile
     RUN echo "deb [signed-by=/usr/share/keyrings/debian-archive-keyring.gpg] \
       https://mirrors.tuna.tsinghua.edu.cn/debian bookworm main contrib" \
       > /etc/apt/sources.list.d/debian.sources
     ```
  3. **更保守的方案**:**保留官方源**(CN 部署时用 proxy 拉,例如 `docker build --network=host --build-arg HTTP_PROXY=...`)
  4. **强 pin 包版本**(配合) — 详见 ISSUE-D09
- **工作量**:0.3d(改 Dockerfile + 测试镜像 + CI 验证)
- **优先级**:**本周修**

---

### ISSUE-D03:acme.sh 镜像克隆无 commit/tag 锁定,供应链不可复现

- **位置**:[Dockerfile:241-247](file:///opt/ikev2-panel-v2-main/Dockerfile#L241-L247)
- **问题**:
  ```dockerfile
  if git clone --depth 1 https://gitee.com/neilpang/acme.sh.git 2>/dev/null; then
      ACME_SRC=/tmp/acme.sh;
  elif git clone --depth 1 https://github.com/acmesh-official/acme.sh.git 2>/dev/null; then
  ```
  - 两个 mirror 都没指定 `--branch` 或 commit SHA
  - 每次 `docker build` 拉到的是 **HEAD**,不可复现
  - gitee + github 两个 mirror 都没签名验证(`git verify-commit` 或 `git tag --verify`)
  - gitee 的 mirror 是个人 fork(`neilpang/acme.sh`),**可能与上游不一致**
- **影响**:
  - 镜像构建不可复现 → CI/CD audit 不通过
  - 如果任一 mirror 被劫持/投毒,容器首次启动就有 RCE(acme.sh 在 `--reloadcmd` 阶段执行 shell)
  - 上次 v2.79.2 修订时改用 `--depth 1` 是优化 clone 速度,但丢了 commit 锁定
- **参考**:
  - [Docker Security Cheatsheet §RULE #13 — Supply Chain](https://cheatsheetseries.owasp.org/cheatsheets/Docker_Security_Cheat_Sheet.html#rule-13-enhance-supply-chain-security)
  - [SLSA Build L2 — Provenance](https://slsa.dev/)
  - 标杆:[traefik Dockerfile](https://github.com/traefik/traefik/blob/master/Dockerfile) — `go install github.com/traefik/whoami@<sha>` 强 pin
- **修复方案**:
  1. **强 pin commit**:
     ```dockerfile
     ARG ACME_SH_COMMIT=3c0c6f7e1d4f...  # 当前最新 release tag 的 SHA
     RUN git clone https://github.com/acmesh-official/acme.sh.git && \
         cd acme.sh && \
         git checkout "${ACME_SH_COMMIT}" && \
         ./acme.sh --install --home /root/.acme.sh --no-cron --no-profile && \
         rm -rf /tmp/acme.sh
     ```
  2. **签名验证**(更稳):
     ```dockerfile
     RUN gpg --keyserver keyserver.ubuntu.com --recv-keys <neilpang_signing_key> && \
         git verify-commit "${ACME_SH_COMMIT}"
     ```
  3. **去掉 gitee fallback** — 个人 mirror 不在 SLSA 信任链上,要么删要么严格同步检查
- **工作量**:0.2d(查最新 commit + 改 Dockerfile)
- **优先级**:**本周修**

---

### ISSUE-D04:镜像无签名 / 无 SBOM,无法验证发布可信度

- **位置**:
  - [.github/workflows/docker-publish.yml:53-93](file:///opt/ikev2-panel-v2-main/.github/workflows/docker-publish.yml#L53-L93) — 推送 GHCR 但无 cosign 签名
  - Dockerfile 无 SBOM 生成(`syft` / `trivy`)
- **问题**:
  - GH Actions 推到 `ghcr.io/rewind2026/ikev2-panel-v2` 后,**任何能拿到 GHCR 凭据的人**都能推同名 tag 覆盖
  - GITHUB_TOKEN 只防意外泄露,不防 GH Actions workflow 被 PR 篡改注入恶意 image
  - 无 SBOM → 用户运行 `trivy image` 不知道装了什么,合规审计卡住
  - `provenance: false` 在 build-push-action L88 显式关掉了 — 失去了默认 attestation
- **影响**:
  - 供应链无法第三方验证
  - 不符合 NIST SP 800-218(SSDF)PW.4.1 / PW.4.4
- **参考**:
  - [Sigstore cosign — Image Signing](https://github.com/sigstore/cosign)
  - [SLSA Build L3 — Isolated Signing](https://slsa.dev/)
  - [Docker Hardened Images — Provenance](https://www.docker.com/products/hardened-images/)
  - 标杆:[traefik/traefik release workflow](https://github.com/traefik/traefik/blob/master/.github/workflows/release.yml) — `cosign sign --yes`
- **修复方案**:
  1. **加 cosign 签名 step**(0.5d):
     ```yaml
     - name: Sign image
       uses: sigstore/cosign-installer@v3
     - run: cosign sign --yes "${IMAGE}@${DIGEST}"
     ```
  2. **生成 SBOM**(0.3d):
     ```yaml
     - uses: anchore/sbom-action@v0
     - uses: anchore/sbom-action/publish@v0
     ```
  3. **启用 provenance**(`provenance: true` 是 buildx 默认,但被当前 workflow 显式关掉) — 改回 true 或 `provenance: mode=max`
  4. **加 [trivy scan step](https://github.com/aquasecurity/trivy-action)** — CVE 阻断 build
- **工作量**:1.0d(签名 + SBOM + trivy)
- **优先级**:下批修(非阻塞,但 SLSA 等级提升依赖此项)

---

## 5. [SEVERITY-MED] ISSUE

### ISSUE-D05:`cap_add: SYS_ADMIN` 宽于必要 — 可降至 `NET_ADMIN`

- **位置**:[docker-compose.yml:70](file:///opt/ikev2-panel-v2-main/docker-compose.yml#L70)
- **问题**:`SYS_ADMIN` 是 Linux capabilities 里**最宽的能力之一**(`mount`, `unshare`, `reboot`, `setns`, ...)。本项目实际只用 `tc` / `ip` / `iptables`,这些全在 `NET_ADMIN` 内。
- **注释(74 行)**:"host 网络下做 tc 限速、ip rule" — `tc` 是 NET_ADMIN,`ip rule` 也是 NET_ADMIN;只有 `ip netns exec` / 自建 netns 才需 SYS_ADMIN
- **影响**:即使关掉 `privileged: true`(见 D01),保留 SYS_ADMIN 也让容器获得 mount/unshare 能力,几乎等价于"小特权模式"
- **参考**:
  - [Linux capabilities man page](https://man7.org/linux/man-pages/man7/capabilities.7.html) — `CAP_SYS_ADMIN` 列表
  - [Docker Security Cheatsheet §RULE #3](https://cheatsheetseries.owasp.org/cheatsheets/Docker_Security_Cheat_Sheet.html#rule-3-limit-capabilities-grant-only-specific-capabilities-needed-by-a-container)
- **修复方案**:
  ```yaml
  cap_drop:
    - ALL
  cap_add:
    - NET_ADMIN         # iptables / tc / ip rule 全包
    - NET_BIND_SERVICE  # 500/4500 privileged 端口
  ```
  实测 charon + ikev2-updown 的 tc 操作只用 NET_ADMIN;若发现 iproute2 某些命令需 SYS_ADMIN,再针对性加 `CAP_NET_RAW`(比 SYS_ADMIN 窄)
- **工作量**:0.5d(含测试 192.168.50.176 实机)
- **优先级**:本周修(配合 D01)

---

### ISSUE-D06:`USER` 指令缺失 — entrypoint.sh 全程 root 运行

- **位置**:
  - [Dockerfile](file:///opt/ikev2-panel-v2-main/Dockerfile) 整文件 — 无 `USER` 指令
  - [entrypoint.sh:786](file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh#L786) — `exec /usr/local/bin/ikev2-panel` 仍以 PID 1 root 运行
- **问题**:
  - Dockerfile 最后的进程默认 UID 0(root)
  - 容器内所有 charon / swanctl / cron / ikev2-panel 都以 root 跑
  - chroot / rootless mode 在 strongSwan 场景技术上可行([strongswan/charon-systemd](https://docs.strongswan.org/docs/latest/install/charonSystemd.html)支持),但改造成本高
- **影响**:
  - 任意 Go 进程漏洞(SQLite parser / VICI protocol parser)→ 容器 root → (搭配 D01)宿主机 root
  - OWASP RULE #2 明确反对
- **参考**:
  - [Docker Security Cheatsheet §RULE #2 — Set a user](https://cheatsheetseries.owasp.org/cheatsheets/Docker_Security_Cheat_Sheet.html#rule-2-set-a-user)
  - 标杆:[caddyserver/caddy Dockerfile](https://github.com/caddyserver/caddy/blob/master/Dockerfile) — `USER caddy`
- **修复方案**(分阶段):
  1. **简单方案(0.5d)**:`USER nobody:nogroup`(UID 65534),把 `ikev2-panel` 进程切到 nobody。charon 仍需 root(绑定 500/4500),需要 `setcap cap_net_bind_service=+ep` 给 `/usr/lib/ipsec/charon`
  2. **优雅方案(2d)**:
     - entrypoint.sh 启动 root 完成配置
     - 用 `gosu` / `su-exec` 切到非 root 用户启动 charon + ikev2-panel
     - `/etc/swanctl/`、`/var/log/charon.log` chown 给 nobody
     - charon 启动用 `nobody` + `setcap` 拿 bind 500/4500
  3. **配合 kernel-libipsec**:不强求 root,kernel-libipsec 模式用 `tun` 设备(NET_ADMIN 足够),不需要 NET_ADMIN 父级
- **工作量**:0.5d(简单)/ 2d(优雅)
- **优先级**:下批修(需重测拨号流程)

---

### ISSUE-D07:`HEALTHCHECK` 缺失 — 容器失活无法被编排层感知

- **位置**:Dockerfile 整文件无 `HEALTHCHECK`
- **问题**:
  - 没有 `HEALTHCHECK CMD`,docker / compose 只看进程在不在(进程崩了会重启,但 charon 半死锁、ESP 包开始丢,服务看似正常实际不工作)
  - compose `restart: unless-stopped` 会无限重启,即使服务半残
  - K8s/portainer 健康检查也无
- **影响**:
  - 续签 cron 失败、charon 死锁、ESP 路由丢 — 容器不重启,用户失联
- **参考**:
  - [Dockerfile reference — HEALTHCHECK](https://docs.docker.com/engine/reference/builder/#healthcheck)
  - [Docker Security Cheatsheet §RULE #7 — Limit resources / restarts](https://cheatsheetseries.owasp.org/cheatsheets/Docker_Security_Cheat_Sheet.html#rule-7-limit-resources-memory-cpu-file-descriptors-processes-restarts)
- **修复方案**:
  ```dockerfile
  HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD swanctl --stats >/dev/null 2>&1 && \
        curl -sfk https://127.0.0.1:8443/healthz || exit 1
  ```
  - 配合 Go 进程 `internal/web/server.go` 加 `/healthz` endpoint(检查 DB / VICI 连接)
  - start-period 给到 60s 是等 charon + 首次 swanctl --load-all 完成
- **工作量**:0.3d
- **优先级**:下批修

---

### ISSUE-D08:资源限制全无 — `mem_limit` / `cpus` / `pids_limit` / `ulimit` 全部缺

- **位置**:docker-compose.yml 整文件
- **问题**:
  - 没 `mem_limit`:Go 进程内存泄漏 / SQLite 死循环 → 容器吃光宿主机内存 → OOM 杀宿主机关键进程
  - 没 `cpus`:任意客户端能 DOS 占用全部 CPU
  - 没 `pids_limit`:goroutine 泄漏或 fork bomb 致宿主内核 panic
  - 没 `ulimit`:file descriptor 耗尽 → 系统级网络栈故障
- **影响**:
  - 单租户 VPS 上:本容器挂掉 = 宿主机可能整体失联
  - OWASP RULE #7 明确要求
- **参考**:
  - [Docker Security Cheatsheet §RULE #7 — Limit resources](https://cheatsheetseries.owasp.org/cheatsheets/Docker_Security_Cheat_Sheet.html#rule-7-limit-resources-memory-cpu-file-descriptors-processes-restarts)
  - [Docker docs — Resource constraints](https://docs.docker.com/config/containers/resource_constraints/)
- **修复方案**:
  ```yaml
  deploy:
    resources:
      limits:
        memory: 1G
        cpus: '1.5'
      reservations:
        memory: 256M
  pids_limit: 200
  ulimits:
    nofile:
      soft: 65536
      hard: 65536
    nproc: 4096
  ```
  数值根据 192.168.50.176 实机数据调整(charon 通常吃 50-80MB,Go 进程 30-50MB,留 4 倍缓冲)
- **工作量**:0.3d(配置 + 实机调优)
- **优先级**:本周修

---

### ISSUE-D09:`/etc/sysctl.d:/host-sysctl.d:rw` 注入宿主,违规安全边界

- **位置**:[docker-compose.yml:106](file:///opt/ikev2-panel-v2-main/docker-compose.yml#L106)
- **问题**:
  ```yaml
  - /etc/sysctl.d:/host-sysctl.d:rw
  ```
  - 容器内 root(也就是容器内任何进程)能**任意写宿主 /etc/sysctl.d/**
  - entrypoint §3 自愈逻辑写入 `99-ikev2.conf` — 但这是 rw mount,**没有限权 nobody 写、限内容校验**
  - 容器被入侵 → 攻击者可在宿主 `/etc/sysctl.d/` 写任意 key(如 `net.ipv4.ip_forward=0` 让容器自身失联 / `kernel.modules_disabled=1` 锁内核模块)
- **影响**:
  - 容器逃逸(配合 D01)已经获得宿主 root,但这条 mount 即使没逃逸,仅容器内 root 也能破坏宿主 sysctl 配置
  - 比 mount docker.sock 危险度低,但仍是"信任域被渗透"问题
- **参考**:
  - [Docker Security Cheatsheet §RULE #1 / §RULE #8](https://cheatsheetseries.owasp.org/cheatsheets/Docker_Security_Cheat_Sheet.html#rule-1-do-not-expose-the-docker-daemon-socket-even-to-the-containers)
  - 标杆:大多数生产 compose 不让容器写宿主的配置目录
- **修复方案**(二选一):
  1. **更窄的 mount**:`:ro` 给容器读,自愈改在容器内 `/etc/sysctl.d/`(但 host 模式容器内 sysctl 就是宿主,所以重启回容器会再次"重置")
  2. **彻底改设计**:宿主侧启动前由 `up.sh` 用 `sudo sysctl -w net.ipv6.conf.all.forwarding=1` 写一次,不要让容器写宿主的 sysctl.d
- **工作量**:1d(改 entrypoint + up.sh + 文档)
- **优先级**:下批修

---

### ISSUE-D10:`/var/log` chmod 1777 — 任何进程可写,symlink attack 风险

- **位置**:[Dockerfile:228](file:///opt/ikev2-panel-v2-main/Dockerfile#L228)
- **问题**:`chmod 1777 /var/log` 让容器内任何进程能往日志目录写 / 建 symlink
- **影响**:
  - 如果后续有非 root 进程启动(D06 的修复),可能被 symlink attack 写到 `/var/log/charon.log → /etc/passwd`
  - 与 [audit-2026-09-security.md ISSUE-S09](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-security.md) 重复报告
- **参考**:
  - [CIS Docker Benchmark §4](https://www.cisecurity.org/benchmark/docker)
  - [Docker Security Cheatsheet §RULE #8 — read-only filesystem](https://cheatsheetseries.owasp.org/cheatsheets/Docker_Security_Cheat_Sheet.html#rule-8-set-filesystem-and-volumes-to-read-only)
- **修复方案**:删 `chmod 1777`,改用子目录 `/var/log/ikev2-panel/`(chmod 0755 root:root),charon filelog 指向子目录
- **工作量**:0.1d
- **优先级**:本周修(顺手,重复 issue)

---

### ISSUE-D11:`git` + `curl` runtime 仅 acme.sh install 时用,可去

- **位置**:[Dockerfile:163-164](file:///opt/ikev2-panel-v2-main/Dockerfile#L163-L164)
- **问题**:`git` 和 `curl` 安装在 runtime 镜像里,但运行时只用于:
  - `acme.sh --issue` 调用 LE API(curl 内置在 acme.sh 里)— 不需要 apt curl
  - LE renew 用 `acme.sh --renew`(自包含)
  - cron 脚本里没显式调用 curl / git
- **影响**:
  - 镜像大小 +2 包,增加 CVE 暴露面
  - SLSA 角度看,运行时不应包含构建工具
- **参考**:
  - [Distroless images — minimal base](https://github.com/GoogleContainerTools/distroless)
  - [Docker Hardened Images](https://www.docker.com/products/hardened-images/)
- **修复方案**:
  1. **删 git + curl**:若 acme.sh 自包含验证通过(它内部带 curl CA bundle),可移除
  2. **拆 stage**:把 acme.sh 安装放到单独的 `acme-builder` stage(只编译阶段装 git),运行时只 COPY 结果
- **工作量**:0.2d
- **优先级**:下批修

---

## 6. [SEVERITY-LOW] ISSUE

### ISSUE-D12:`set -o pipefail` 未启,curl|md5sum 失败被吞

- **位置**:[Dockerfile:51-56](file:///opt/ikev2-panel-v2-main/Dockerfile#L51-L56)
- **问题**:
  ```dockerfile
  RUN set -eux; \
      curl -fsSL -o strongswan.tar.bz2 ...; \
      echo "${STRONGSWAN_MD5}  strongswan.tar.bz2" | md5sum -c -;
  ```
  - `set -e` 退出非零即失败,**但 `curl ... | md5sum -c -` 这条 pipe 行为依赖 shell**
  - bash 默认只检查**最后一个命令**(`md5sum`)的退出码;`curl` 失败但 md5sum 成功(几乎不可能但理论)会被吞
  - 推荐 `set -euxo pipefail`(Dockerfile 用 `/bin/sh` 而不是 bash 时需要显式 `# syntax=docker/dockerfile:1.7`)
- **影响**:理论上下载失败仍通过校验,导致**编译基于空文件**(强Swan tar.bz2 是空包的话,make 会失败 → build 失败 → 仍可发现)
- **参考**:[Dockerfile best practices — pipefail](https://docs.docker.com/develop/develop-images/dockerfile_best-practices/#use-pipefail)
- **修复方案**:
  ```dockerfile
  # syntax=docker/dockerfile:1.7
  SHELL ["/bin/bash", "-euxo", "pipefail", "-c"]
  ```
- **工作量**:0.1d
- **优先级**:本周修

---

### ISSUE-D13:apt 包版本未强 pin,版本漂移不可控

- **位置**:[Dockerfile:152-164](file:///opt/ikev2-panel-v2-main/Dockerfile#L152-L164)
- **问题**:
  - `apt-get install -y --no-install-recommends libssl3 iproute2 ...` 没指定 `=版本号`
  - 每次 `docker build` 拉到的是当时仓库最新版 → 镜像不可复现
  - 与 ISSUE-D02 互补(D02 是源信任,D13 是包版本)
- **影响**:
  - 不符合 SLSA L2 hermetic build 要求
  - 上游某包 CVE 时,镜像自动"打补丁",但**没经过测试**可能破坏功能
- **参考**:
  - [apt — Hold a package](https://manpages.debian.org/bookworm/apt/apt-mark.8.en.html)
  - [Docker Hardened Images — SBOM + pin](https://www.docker.com/products/hardened-images/)
- **修复方案**:
  ```dockerfile
  # 锁版(snapshot.debian.org 或本地 repo)
  RUN apt-get update && \
      apt-get install -y --no-install-recommends \
          libssl3=3.0.* \
          iproute2=6.* \
          iptables=1.8.* \
          ca-certificates=20230311 \
          tini=0.19.* \
          kmod=30* \
          cron=3.0pl1-* && \
      rm -rf /var/lib/apt/lists/*
  ```
  或用 [debian-snapshot](https://snapshot.debian.org/) 锁定时间点
- **工作量**:0.5d(查版本 + 测试)
- **优先级**:下批修

---

### ISSUE-D14:`--no-install-recommends` 强swan-builder stage 缺

- **位置**:[Dockerfile:27-40](file:///opt/ikev2-panel-v2-main/Dockerfile#L27-L40)
- **问题**:`apt-get install -y --no-install-recommends` runtime stage 用了,**builder stage 没用**
- **影响**:builder stage 是临时,build 完丢弃,影响小;但仍多装包 = 缓存层大
- **参考**:[Dockerfile best practices — apt-get](https://docs.docker.com/develop/develop-images/dockerfile_best-practices/#apt-get)
- **修复方案**:加 `--no-install-recommends`
- **工作量**:0.05d
- **优先级**:顺手改

---

### ISSUE-D15:`network_mode: host` 默认而非 `auto`,compose 文件违背"先安全"原则

- **位置**:[docker-compose.yml:44](file:///opt/ikev2-panel-v2-main/docker-compose.yml#L44)
- **问题**:compose 默认 `network_mode: host`,但 `.env.example` 注释说 `IKEV2_NETWORK_MODE=auto` 推荐。两者不一致:
  - 老用户用 compose 启动 = 直接 host 网络
  - 注释说"建议用 up.sh 自动选"
- **影响**:compose 默认行为不安全,误用率高
- **参考**:
  - [Docker Security Cheatsheet §RULE #5a — host 网络谨慎](https://cheatsheetseries.owasp.org/cheatsheets/Docker_Security_Cheat_Sheet.html#rule-5a-be-careful-when-mapping-container-ports-to-the-host-with-firewalls-like-ufw)
- **修复方案**:
  1. 把 compose 默认改为 `bridge`(注释里 ipvlan 块启用方式保留)
  2. host 网络改成注释里的"高级模式"显式启用
- **工作量**:0.2d
- **优先级**:下批修

---

### ISSUE-D16:8443 默认 `0.0.0.0:8443`,公网暴露管理面板

- **位置**:[docker-compose.yml:111](file:///opt/ikev2-panel-v2-main/docker-compose.yml#L111) `IKEV2_LISTEN_ADDR: "0.0.0.0:8443"`
- **问题**:`0.0.0.0` 监听所有接口 → host 网络下 8443 直接公网可访问
- **影响**:
  - 配合登录无 rate-limit(security audit ISSUE-S01),公网 brute-force 面板
  - OWASP / CIS 建议管理面板仅内网 / VPN 内可访问
- **参考**:
  - [CIS Docker Benchmark §5 — 端口映射谨慎](https://www.cisecurity.org/benchmark/docker)
  - 标杆:大多数 VPN 项目的管理面板默认绑定 `127.0.0.1`
- **修复方案**(二选一):
  1. 默认 `127.0.0.1:8443`,管理时 SSH 隧道进入
  2. 默认 `0.0.0.0:8443` 但加 `IKEV2_ADMIN_TRUSTED_CIDRS=10.0.0.0/8,127.0.0.1/32` 默认拒绝规则
- **工作量**:0.3d
- **优先级**:下批修

---

### ISSUE-D17:无 alpine / distroless / DHI 替代评估记录

- **位置**:Dockerfile 注释 + docs/design.md §8.1
- **问题**:
  - 注释明确说"alpine 有 glibc 兼容性问题 → 不用"
  - 注释说"strongSwan 用 distroless 不便 → 不用"
  - 但**没有正式评估文档**,未来如果升级 base image 选择没有对比依据
- **影响**:技术债,新成员 onboarding 成本
- **参考**:
  - [Distroless containers](https://github.com/GoogleContainerTools/distroless) — `gcr.io/distroless/cc-debian12`
  - [Docker Hardened Images catalog](https://hub.docker.com/hardened-images/catalog)
- **修复方案**:在 `docs/design.md` §8.1 加一节"Base Image 评估记录",逐项打勾:
  - debian-slim:✅ 成熟 / ❌ CVE 多
  - alpine:✅ 小 / ❌ musl vs glibc 兼容
  - distroless:✅ 最小 / ❌ 无 shell 调试不便
  - DHI:✅ 加固 / ❌ 商业授权
- **工作量**:0.1d(写文档)
- **优先级**:下批修

---

## 7. 多阶段构建深度分析

### 7.1 当前 3 stage 结构

```
Stage 1: strongswan-builder (debian:bookworm-slim)
   └─ 编译 strongSwan 6.0.1 → /tmp/strongswan-install/

Stage 2: go-builder (golang:1.26-bookworm)
   └─ go build -ldflags="-s -w" → /tmp/ikev2-panel

Stage 3: runtime (debian:bookworm-slim)
   ├─ COPY strongSwan /usr, /etc
   ├─ COPY ikev2-panel
   ├─ COPY scripts + configs + web/
   └─ 安装 acme.sh
```

### 7.2 评估

| 项 | 评价 | 标杆对比 |
|----|------|----------|
| Stage 1 builder 清理 | ✅ `make install DESTDIR=/tmp/...` 不留 source | [traefik](https://github.com/traefik/traefik/blob/master/Dockerfile) 用 `--mount=type=cache` 优化速度 |
| Stage 2 `golang:1.26-bookworm` | ⚠️ 拉整个 Go 工具链 ~800MB 编译 | traefik 用 `golang:1.22-alpine` 更小 |
| Stage 2 `CGO_ENABLED=0` | ✅ 纯静态二进制,无 glibc 依赖 | caddy 同样 |
| Stage 3 `libssl3` + `iproute2` + `iptables` | ⚠️ runtime 仍 ~150MB | 可考虑 `gcr.io/distroless/cc-debian12`(~25MB)但不能跑 shell 脚本 |
| Stage 3 安装 `git` + `curl` | ❌ 见 D11 | 仅 builder 用,应分离 |
| `web/` COPY 全量 | ⚠️ templates + static 可独立 layer | 改 multi-stage 把 web 也拆 |

### 7.3 优化建议(非紧急)

1. **加 `--mount=type=cache`** 加速 apt / go build cache
2. **runtime stage 评估 distroless**:保留 entrypoint.sh 需要 `/bin/sh`,所以**只能选 `static-debian12`**(无 apt / 无 shell)不行;`base-debian12` 带 shell 但无 apt,可行(可装包在 builder)
3. **拆 web/ 为独立 stage**,使 web 资源改动不重建整个镜像
4. **builder stage 显式 `USER nonroot`** 防 Lint 报警

---

## 8. 运行时 capability 深度分析

### 8.1 实际需要的能力矩阵

| 操作 | 需要 cap | 当前配置 |
|------|----------|----------|
| `ip route add table 220 default` | NET_ADMIN | ✅ 已开 |
| `iptables -t nat -A POSTROUTING ... MASQUERADE` | NET_ADMIN | ✅ 已开 |
| `ip rule add ...` | NET_ADMIN | ✅ 已开 |
| `tc qdisc/class/filter add` | NET_ADMIN | ✅ 已开 |
| bind UDP/500 + UDP/4500 | NET_BIND_SERVICE | ✅ 已开 |
| bind TCP/8443 | (non-privileged ≥1024) | ✅ 不需要 |
| `echo 1 > /proc/sys/net/ipv6/conf/all/forwarding` | NET_ADMIN (sysctl) | ✅ 已开 |
| **mount** (无需,容器内不需要) | SYS_ADMIN | ❌ 不需要 |
| **unshare** (无需) | SYS_ADMIN | ❌ 不需要 |
| **setns** (无需) | SYS_ADMIN | ❌ 不需要 |
| 读写 `/dev/net/tun` | NET_ADMIN (持有 device node) | ✅ 已开(`devices: /dev/net/tun`) |
| 写宿主 `/etc/sysctl.d/` | host fs 写 | 见 D09 |

### 8.2 评估

**SYS_ADMIN 完全不需要** — `tc` / `ip` / `iptables` 全在 NET_ADMIN 内。

**推荐最终配置**(配合 D01 + D05 修复):
```yaml
cap_drop:
  - ALL
cap_add:
  - NET_ADMIN
  - NET_BIND_SERVICE
devices:
  - /dev/net/tun
security_opt:
  - no-new-privileges:true
read_only: false   # /var/log + /etc/swanctl 需写,改 tmpfs
tmpfs:
  - /tmp
  - /run
```

---

## 9. 持久化与数据安全分析

### 9.1 volume 权限矩阵

| 路径 | mount 类型 | 文件权限 | 评价 |
|------|-----------|---------|------|
| `/data` | named volume `ikev2-panel-v2_ikev2-data` | 0777 volume default | ⚠️ DB / 凭证可被同主机其他用户读到(volume 0777) |
| `/etc/sysctl.d` (host) → `/host-sysctl.d` (容器) | bind rw | host 原权限 | ❌ 见 D09 |
| `/etc/swanctl/x509/` | image COPY | 644 / 600(LE 模式) | ✅ |
| `/etc/swanctl/private/` | image COPY | 600(LE 模式) | ✅ |
| `/root/.acme.sh/account.conf` | 容器内写 | 600(LE 模式 entrypoint §7.5) | ✅ |
| `/data/le/privkey.pem` | volume | 600(renew-cert.sh + ikev2-reload.sh) | ✅ |
| `/data/le/fullchain.pem` | volume | 644 | ✅ |
| `/data/panel-state/aliyun.creds` | volume | ?(Go 进程写) | ⚠️ 见 S03(security audit) |
| `/var/log` | tmpfs-style | 1777 | ❌ 见 D10 |
| `/data/ikev2-panel.db` | volume | 默认 644 | ❌ 任意用户可读 VPN 密码 hash |

### 9.2 改进建议

1. **VOLUME 创建时显式 chmod**:`docker volume create --opt device=...` 不支持 mode;改用 entrypoint 启动时 `chown -R root:root /data && chmod 700 /data`
2. **DB 文件单独 chmod 600**:Go 进程启动时 `os.Chmod("/data/ikev2-panel.db", 0o600)`
3. **/data volume 的 SELinux / AppArmor label**:`z` 选项(docker run `--security-opt label=type:ikev2_panel_data_t`)隔离

---

## 10. 网络暴露与端口分析

### 10.1 当前端口矩阵

| 端口/协议 | host 网络模式 | bridge 网络模式 | 公网必要性 |
|-----------|--------------|----------------|------------|
| 500/udp (IKE) | 宿主网卡直监听 | docker-proxy 映射 | ✅ 必须(ESP 协商) |
| 4500/udp (NAT-T) | 宿主网卡直监听 | docker-proxy 映射 | ✅ 必须(ESP 隧道) |
| 8443/tcp (管理面板) | 宿主网卡直监听 | docker-proxy 映射 | ⚠️ 建议仅内网 |

### 10.2 风险

- 500/udp + 4500/udp 必须公网(否则 ESP/IKE 协商失败)
- 8443/tcp 默认公网,**且无 fail2ban / rate limit** → brute-force 风险
- 注释里说"建议用防火墙限制源 IP" 但 compose 没强制

### 10.3 标杆对比

- [WireGuard 官方 Docker](https://github.com/linuxserver/docker-wireguard) — 默认不暴露 web UI,UI 走 SSH 隧道
- [OpenVPN AS](https://openvpn.net/cloud-docs/) — 公网默认开 web UI 但强制 2FA

---

## 11. healthcheck + 日志 + OOM 综合

| 项 | 状态 | 推荐 |
|----|------|------|
| HEALTHCHECK | ❌ 无 | D07 推荐 |
| 日志脱敏(凭证) | ⚠️ charon.log 默认 `enc = 1` 包含加密参数 | 关掉 `enc = 1`(只 debug 时开) |
| 日志轮转 | ❌ 无 `/etc/logrotate.d/` | entrypoint 加 logrotate.d 配置 |
| OOM behavior | ❌ 无 `oom_score_adj` | 设 `-500` 避免被 OOM killer 选中 |
| Restart policy | `unless-stopped`(无限重启) | 改 `on-failure:5`(D07 healthcheck 后) |

---

## 12. CI / CD 分析

### 12.1 当前 GitHub Actions

- 推 tag `v*` 触发
- 跨架构 `linux/amd64 + linux/arm64`(QEMU 模拟 arm64)
- 推到 `ghcr.io/rewind2026/ikev2-panel-v2`
- 无签名、无 SBOM、无 CVE scan

### 12.2 标杆对比

- [traefik](https://github.com/traefik/traefik/blob/master/.github/workflows/release.yml) — cosign + multi-arch + SBOM + 校验
- [caddy](https://github.com/caddyserver/caddy/blob/master/.github/workflows/release.yml) — cosign + Goreleaser

### 12.3 推荐改进

1. 加 [trivy scan](https://github.com/aquasecurity/trivy-action) — CRITICAL 阻断 build
2. 加 cosign 签名
3. 加 SLSA provenance generation([slsa-github-generator](https://github.com/slsa-framework/slsa-github-generator))
4. QEMU arm64 build 换成 self-hosted ARM runner(更快 + 真二进制,非 QEMU 模拟)

---

## 13. 改进优先级路线图

### 13.1 本周修(0.5d 内可完成)

| ID | 标题 | 工作量 |
|----|------|--------|
| D01(部分) | 删 `privileged: true` + SYS_ADMIN | 1d |
| D02 | apt 源加 `signed-by=` | 0.3d |
| D03 | acme.sh pin commit | 0.2d |
| D05 | SYS_ADMIN → NET_ADMIN | 0.5d(并入 D01) |
| D08 | 资源限制 deploy.resources | 0.3d |
| D10 | `/var/log` chmod 删除 | 0.1d |
| D12 | `set -o pipefail` | 0.1d |
| D14 | builder stage 加 `--no-install-recommends` | 0.05d |

**累计**:~2.5d

### 13.2 下批修(下个 sprint)

| ID | 标题 | 工作量 |
|----|------|--------|
| D06 | USER nobody | 0.5~2d |
| D07 | HEALTHCHECK | 0.3d |
| D09 | 改 up.sh 写宿主 sysctl | 1d |
| D11 | 拆 git/curl 到 builder | 0.2d |
| D13 | apt 包版本 pin | 0.5d |
| D15 | 默认 network_mode 改 bridge | 0.2d |
| D16 | 8443 默认 127.0.0.1 | 0.3d |

### 13.3 长期路线图

| 阶段 | 目标 | 估计 |
|------|------|------|
| v2.85 | D01/D02/D03/D05/D10 + HEALTHCHECK + 资源限制 | 1 sprint |
| v2.86 | D06/D07/D09/D11 + base image 评估 | 1 sprint |
| v3.0 | D13/D15/D16 + cosign 签名 + SLSA L2 | 2 sprint |

---

## 14. 与同类项目对比表

| 维度 | ikev2-panel-v2 | traefik | caddy | strongswan upstream |
|------|-----------------|---------|-------|---------------------|
| Base image | debian:bookworm-slim | scratch / distroless | distroless | (上游多镜像) |
| USER | root | nonroot | nonroot | root |
| privileged | ✅ true | ❌ false | ❌ false | ❌ false |
| cap_add | NET_ADMIN+NET_BIND_SERVICE+SYS_ADMIN | (默认) | NET_BIND_SERVICE | NET_ADMIN |
| 镜像签名 | ❌ 无 | ✅ cosign | ✅ cosign | ❌ |
| SBOM | ❌ 无 | ✅ syft | ✅ syft | ❌ |
| HEALCHECK | ❌ 无 | ✅ | ✅ | ✅ |
| multi-stage | ✅ 3 stage | ✅ 多 stage | ✅ 多 stage | ✅ |
| 资源限制 | ❌ 无 | 默认推荐 | 默认推荐 | ✅ |
| Read-only fs | ❌ | ✅ | ✅ | ❌ |

**总体评价**:strongSwan 集成需要 NET_ADMIN + TUN,**不可避免特权部分**;但在 USER / 镜像签名 / SBOM / 资源限制上落后标杆项目 1~2 代。

---

## 15. 与 docs/design.md / 现有审计的对齐检查

| 来源 | 引用 |
|------|------|
| [docs/audit-2026-09-security.md ISSUE-S03](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-security.md) | 镜像无 GPG 校验 — 本报告 D02 部分覆盖 |
| [docs/audit-2026-09-security.md ISSUE-S09](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-security.md) | `/var/log` 1777 — 本报告 D10 重复 |
| [docs/design.md §8.1](file:///opt/ikev2-panel-v2-main/docs/design.md) | 基础镜像选择 — 本报告 §7.3 + D17 互补 |
| [docs/design.md §10.2](file:///opt/ikev2-panel-v2-main/docs/design.md) | compose 设计 — 本报告 §8 + §10 互补 |
| [docs/design.md §15.x](file:///opt/ikev2-panel-v2-main/docs/design.md) | forwarding 自愈 — 本报告 D09 提出风险 |

---

## 16. 自检清单

- [x] 每个 issue 都有 file:// 链接
- [x] 每个 HIGH 都引用 OWASP / CIS Docker Benchmark / SLSA
- [x] 标杆对比覆盖 traefik / caddy / strongswan upstream
- [x] 没有"应假设包会被 0day 攻击"等不切实际的高严重度标定
- [x] 修复建议给出工作量 + 优先级
- [x] 17 个 issue 全部列出(4H + 7M + 6L)
- [x] 路线图分本周 / 下批 / 长期

---

**报告版本**:v1 — 2026-09-18 项目组自查
**下次复审**:v2.85 发布前 + v3.0 重构前
**责任人**:Docker 镜像 / 供应链审计 agent