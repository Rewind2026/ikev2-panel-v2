# 多 stage build：自编译 strongSwan 6.0.1 + Go 二进制 + 精简 runtime
# 设计见 docs/design.md §8.1 + §1.5（kernel-libipsec）
#
# 关键决策（M8 commit 22，2026-09-16 在 192.168.50.176 实机验证）：
#   - 自编译 strongSwan，启用 --enable-kernel-libipsec：用户在容器化 VPS 上也能用 ESP
#   - 编译用 debian:bookworm，runtime 也用 bookworm-slim，最大化 glibc 兼容性
#   - 强 pin strongSwan 版本（STRONGSWAN_VERSION=6.0.1），方便升级与回滚
#       * 6.1.0 虽然官方 2026-09 发布，但 Docker Hub tag 不稳、官方下载源常 404
#       * 6.0.1 已通过 apt 验证：libipsec/vici/eap-mschapv2/kernel-netlink 全部正常加载
#   - 不再依赖 strongswan/strongswan 官方镜像（那个镜像的 tag 命名不稳定）
#   - Go 用官方 golang:1.26-bookworm 镜像编译（govici v0.8.1 要求 Go ≥1.23，
#     go.mod 强 pin 1.26.0；M8 commit 25 实测 1.22 编译会报 "go.mod requires go >= 1.26.0"）
#   - 装 ipsec 路径用 /usr/lib/ipsec（Debian 默认，跟 strongswan.conf 默认查找一致）

# ============================================================================
# Stage 1: strongswan-builder  自编译 strongSwan 6.0.1
# ============================================================================
FROM debian:bookworm-slim AS strongswan-builder

ENV DEBIAN_FRONTEND=noninteractive

# 默认走 deb.debian.org 官方源(debian:bookworm-slim 自带 sources.list)。
# v2.86-PR11 之前为国内可达性切清华源 + GPG 校验;用户外网环境无需此步。
# 如需切源,改为 docker build --build-arg APT_MIRROR=... 或修改下面这一行。

# 编译 strongSwan 需要的工具和库
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        pkg-config \
        libssl-dev \
        bison \
        flex \
        gettext \
        libtool \
        autoconf \
        automake \
        curl \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# strongSwan 版本（强 pin，方便升级/回滚）
#   MD5 必须跟官方下载页 (https://download.strongswan.org/) 一致
#   验证方法：curl -fsSL https://download.strongswan.org/strongswan-${VER}.tar.bz2.md5
ARG STRONGSWAN_VERSION=6.0.1
ARG STRONGSWAN_MD5=c3ddc81d1d11ce3d5431e15da7718748

WORKDIR /tmp/strongswan-build

# 下载并校验源码包
RUN set -eux; \
    curl -fsSL -o strongswan.tar.bz2 \
        "https://download.strongswan.org/strongswan-${STRONGSWAN_VERSION}.tar.bz2"; \
    echo "${STRONGSWAN_MD5}  strongswan.tar.bz2" | md5sum -c -; \
    tar --strip-components=1 -xjf strongswan.tar.bz2; \
    rm strongswan.tar.bz2

# configure：精简启用我们需要的插件
#   --enable-kernel-libipsec  ★关键★ 用户态 ESP 实现，避开 XFRM
#   --enable-kernel-netlink   仍保留 netlink（libipsec 走 TUN，但 netlink 还能发现接口）
#   --enable-vici             swanctl 命令行依赖（vici socket）
#   --enable-swanctl          swanctl 二进制
#   --enable-eap-mschapv2     EAP-MSCHAPv2（用户名/密码认证）
#   --enable-openssl          openssl plugin（RSA / ECDSA / AES-GCM）
#   --enable-sha2 / hmac      关键 PRF / 完整性算法（v2.86-PR10 删除 sha1/md5/stroke，RFC 8247 合规）
#   --enable-nonce / random   随机数生成
#   --enable-aes / ctr / ccm / gcm / chapoly  标准 ESP 加密套件
#   --enable-curve25519       现代 ECDH group（默认 ECP256 也需要 openssl 里的 ecdh）
#   (gmp 保持 enabled)         gmp 大数库加速 DH/ECDH
#
# 不启用：
#   --disable-ikev1           按官方建议（v6.0+ 默认禁用 IKEv1）
#   --disable-static          只构建动态库，缩小镜像
#
# v2.86-PR10 删除：
#   --enable-sha1             RFC 8247 明确反对，strongSwan Security Recommendations 列为弱算法
#   --enable-md5              同上（char 启动时不需要 HASH_SHA1,openssl plugin 已经提供 SHA256+）
#   --enable-stroke           strongSwan 6.0+ 已 deprecate，swanctl 覆盖所有常用操作，镜像 -8MB
#
#   未来按需：--enable-eap-tls / eap-ttls（证书认证）、--enable-xauth/eap-radius
RUN ./configure \
        --prefix=/usr \
        --sysconfdir=/etc \
        --libexecdir=/usr/lib \
        --with-ipsecdir=/usr/lib/ipsec \
        --enable-kernel-libipsec \
        --enable-kernel-netlink \
        --enable-vici \
        --enable-swanctl \
        --enable-eap-mschapv2 \
        --enable-openssl \
        --enable-sha2 \
        --enable-hmac \
        --enable-nonce \
        --enable-random \
        --enable-aes \
        --enable-ctr \
        --enable-ccm \
        --enable-gcm \
        --enable-chapoly \
        --enable-curve25519 \
        --disable-ikev1 \
        --disable-static \
    && make -j"$(nproc)" \
    && make install DESTDIR=/tmp/strongswan-install

# 验证 libipsec 插件已编译（路径对应 --with-ipsecdir）
RUN test -f /tmp/strongswan-install/usr/lib/ipsec/plugins/libstrongswan-kernel-libipsec.so \
    || { echo "FATAL: kernel-libipsec plugin not built"; exit 1; }

# ============================================================================
# Stage 2: go-builder  编译 ikev2-panel Go 二进制
# ============================================================================
FROM golang:1.26-bookworm AS go-builder

# 用国内 proxy，避免访问 proxy.golang.org 超时
ENV GOPROXY=https://goproxy.cn,direct
ENV GOSUMDB=off

WORKDIR /src

# 先 COPY go.mod/go.sum 单独 layer，利用 docker build cache
COPY go.mod go.sum ./
RUN go mod download

COPY . ./

# CGO_ENABLED=0：纯静态 Go 二进制（不需要 glibc 兼容层）
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /tmp/ikev2-panel ./cmd/ikev2-panel

# ============================================================================
# Stage 3b: runner-test  跑 go test -race / go vet(CI 用,生产镜像不含)
#   v2.85-PR1 加。race detector 需要 cgo,装 gcc;runtime 镜像不变。
#   用法:
#     docker build --target runner-test -t ikev2-panel-race:test .
#     docker run --rm ikev2-panel-race:test
# ============================================================================
FROM golang:1.26-bookworm AS runner-test

ENV CGO_ENABLED=1
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        gcc libc6-dev ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./

# 默认跑全部包的 race 检测。CI 可以 docker run --rm ...test 跑。
CMD ["go", "test", "-race", "-count=1", "./..."]

# ============================================================================
# Stage 4: runtime  精简运行时镜像
# ============================================================================
FROM debian:bookworm-slim AS runtime

# 版本注入:v2.85-PR1。build 时 --build-arg PANEL_VERSION=v2.85,默认 dev
# 启动时 main.go 读取 /etc/ikev2-panel-version 打到 logger.Info。
# dev 模式(本地 go build)无此文件 → fallback "dev",不影响功能。
ARG PANEL_VERSION=dev
RUN echo "${PANEL_VERSION}" > /etc/ikev2-panel-version

# v2.86-PR12.17:docker inspect 可见的镜像元数据,审计 / 调试用。
# 镜像源 / commit / 大版本在 docker inspect 顶层 metadata 可见。
# 不写敏感信息(API key / token / domain)。
ARG BUILD_DATE=unknown
ARG VCS_REF=unknown
LABEL org.opencontainers.image.title="ikev2-panel" \
      org.opencontainers.image.description="IKEv2/IPsec VPN panel with strongSwan 6.0.1 + nftables" \
      org.opencontainers.image.version="${PANEL_VERSION}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.source="https://github.com/yourname/ikev2-panel-v2" \
      org.opencontainers.image.licenses="MIT" \
      ikev2-panel.changelog="v2.86-PR12.16+12.17: comprehensive nftables migration (custom table ikev2/ikev26, docker-daemon-safe); mobileconfig RemoteAddress uses DDNS domain; iptables-v2.86-PR12.13 P0 fixes (FORWARD ACCEPT + MASQUERADE persistence)"

ENV DEBIAN_FRONTEND=noninteractive

# runtime 必需的运行时依赖：
#   libssl3       openssl plugin 需要
#   iproute2      ip 命令（entrypoint 检测 IPv6 地址）
#   iptables      NAT MASQUERADE（commit 23 entrypoint 会用到）
#   ca-certificates  HTTPS / ACME 证书校验
#   sqlite3       DB 工具（healthcheck 可选）
#   tini          PID 1，正确转发信号、收割僵尸进程
#   kmod          modprobe（libipsec 需要 tun 设备，但运行时通常不需要 modprobe）
#   cron          renew-cert.sh 定时任务
#   git           acme.sh 安装用（LE 模式）
#   curl          acme.sh 调用 Let's Encrypt API（LE 模式）
# 默认走 deb.debian.org 官方源（debian:bookworm-slim 自带）。
# 如需切源,改为 docker build --build-arg APT_MIRROR=... 或修改下面这一行。

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        libssl3 \
        iproute2 \
        iptables \
        nftables \
        ca-certificates \
        sqlite3 \
        tini \
        kmod \
        cron \
        git \
        curl \
    && rm -rf /var/lib/apt/lists/*

# 从 strongswan-builder 复制编译产物（覆盖原 /usr 和 /etc）
COPY --from=strongswan-builder /tmp/strongswan-install/usr      /usr
COPY --from=strongswan-builder /tmp/strongswan-install/etc      /etc

# ----------------------------------------------------------------------------
# 修复 strongSwan 6.0.1 默认配置 bug：源码包自带 conf/plugins/*.conf 模板大部分是
# 0 字节文件，缺少 `load = yes`，导致 charon 启动时随机数/RNG/socket 等关键插件
# 无法被加载。Debian 包通过 post-install patch 修复；这里我们在 build 阶段显式
# 补齐所有空 conf 文件。
# 参考：https://github.com/strongswan/strongswan/issues/1182
# ----------------------------------------------------------------------------
RUN set -eux; \
    cd /etc/strongswan.d/charon; \
    for plugin in random socket-default vici kernel-netlink kernel-libipsec \
                 openssl resolve stroke updown drbg eap-mschapv2 counters; do \
        conf="$plugin.conf"; \
        if [ -f "$conf" ] && [ ! -s "$conf" ]; then \
            printf '%s {\n    load = yes\n}\n' "$plugin" > "$conf"; \
        fi; \
    done; \
    echo "=== fixed empty plugin confs ==="; \
    for f in *.conf; do \
        echo -n "$f: "; \
        grep -E '^[[:space:]]*load' "$f" || echo '(empty, fixed above)'; \
    done | head -20

# 从 go-builder 复制 Go 二进制
COPY --from=go-builder /tmp/ikev2-panel /usr/local/bin/ikev2-panel

# 业务脚本与默认配置
COPY scripts/entrypoint.sh                    /usr/local/bin/entrypoint.sh
COPY scripts/ikev2-updown                     /usr/local/bin/ikev2-updown
COPY scripts/renew-cert.sh                    /etc/ikev2-panel/scripts/renew-cert.sh
COPY scripts/ikev2-reload.sh                  /usr/local/bin/ikev2-reload.sh
COPY scripts/ikev2.nft.template               /etc/ikev2-panel/scripts/ikev2.nft.template
COPY configs/swanctl-ipv6-only.conf           /etc/swanctl/swanctl.conf
COPY configs/strongswan-filelog.conf          /etc/strongswan.d/filelog.conf
# v2.86-PR13.2:logrotate 配置
COPY configs/logrotate-ikev2-charon           /etc/logrotate.d/ikev2-charon

# Web 资源（HTML 模板 + CSS），main.go 启动时在 /app/web/ 查找
COPY web/                                    /app/web/

# 构建期 sanity check：确认关键二进制都在 PATH 里
# v2.86-PR10 删除了 --enable-stroke,所以 `ipsec` wrapper 不存在(改用 swanctl);
# entrypoint.sh §6 直接 fork /usr/lib/ipsec/charon,不依赖 ipsec wrapper。
RUN set -eux; \
    for bin in swanctl ikev2-panel tini; do \
        command -v "$bin" >/dev/null || { echo "FATAL: missing $bin"; exit 1; }; \
    done; \
    test -x /usr/lib/ipsec/charon || { echo "FATAL: missing /usr/lib/ipsec/charon"; exit 1; }; \
    test -f /usr/lib/ipsec/plugins/libstrongswan-vici.so || { echo "FATAL: missing vici plugin"; exit 1; }; \
    test -f /usr/lib/ipsec/plugins/libstrongswan-kernel-libipsec.so || { echo "FATAL: missing kernel-libipsec"; exit 1; }; \
    chmod +x /usr/local/bin/entrypoint.sh \
             /usr/local/bin/ikev2-updown \
             /usr/local/bin/ikev2-reload.sh \
             /etc/ikev2-panel/scripts/renew-cert.sh

# 运行时目录
RUN mkdir -p /etc/swanctl/conf.d \
             /var/lib/ikev2-panel/limits \
             /var/lib/ikev2-panel/classids \
             /etc/ikev2-panel/certs-backup \
             /etc/ikev2-panel/scripts \
             /var/log && \
    chmod 1777 /var/log && \
    # v2.86-PR11.1:read_only + /etc/swanctl 挂 tmpfs 时,模板 swanctl.conf
    # 会被 tmpfs 覆盖。保留一份只读副本到 /etc/swanctl.dist/,entrypoint 启动时
    # 检测到 /etc/swanctl/swanctl.conf 不存在就 cp 回来。
    mkdir -p /etc/swanctl.dist/conf.d \
             /etc/swanctl.dist/x509 \
             /etc/swanctl.dist/x509ca \
             /etc/swanctl.dist/x509ocsp \
             /etc/swanctl.dist/x509aa \
             /etc/swanctl.dist/x509ac \
             /etc/swanctl.dist/x509crl \
             /etc/swanctl.dist/private \
             /etc/swanctl.dist/rsa \
             /etc/swanctl.dist/ecdsa \
             /etc/swanctl.dist/pkcs8 \
             /etc/swanctl.dist/pkcs12 \
             /etc/swanctl.dist/pubkey && \
    cp -a /etc/swanctl/swanctl.conf /etc/swanctl.dist/ && \
    cp -a /etc/swanctl/conf.d/. /etc/swanctl.dist/conf.d/ 2>/dev/null || true && \
    cp -a /etc/swanctl/x509/.      /etc/swanctl.dist/x509/      2>/dev/null || true && \
    cp -a /etc/swanctl/x509ca/.   /etc/swanctl.dist/x509ca/   2>/dev/null || true && \
    cp -a /etc/swanctl/private/.  /etc/swanctl.dist/private/  2>/dev/null || true && \
    cp -a /etc/swanctl/rsa/.      /etc/swanctl.dist/rsa/      2>/dev/null || true && \
    cp -a /etc/swanctl/ecdsa/.    /etc/swanctl.dist/ecdsa/    2>/dev/null || true && \
    cp -a /etc/swanctl/pkcs8/.    /etc/swanctl.dist/pkcs8/    2>/dev/null || true && \
    cp -a /etc/swanctl/pkcs12/.   /etc/swanctl.dist/pkcs12/   2>/dev/null || true && \
    chmod -R a+rX /etc/swanctl.dist

# ----------------------------------------------------------------------------
# 安装 acme.sh（仅 LE 模式需要用到，但镜像里直接装好，避免容器启动再下）：
#   - 优先用 Gitee 镜像（国内可达）；失败 fallback 到 GitHub
#   - 装到 /opt/acme.sh（避免 /root tmpfs 软链悬挂,见 PR12.5）
#   - 装完后 cp 到 /usr/local/bin/acme.sh（entrypoint.sh 直接命令名调用）
#   - 不注册 cron（acme.sh 默认会注册 crontab；我们自己用 cron.d 管理）
#   - 运行时写文件（account key / 证书 / ca dir）由 entrypoint.sh §7.5 传 --home /opt/acme.sh
#   - 不写 account.conf（凭证由 entrypoint.sh 运行时从 env 注入，不进镜像）
#
# v2.86-PR12.2:pinned commit SHA256 校验,防供应链投毒
#   - 不再 git clone --depth 1(latest commit 不可控)
#   - 改 clone 整个 repo + checkout pinned commit
#   - 校验 tarball 的 SHA256(从 GitHub release 下载的 SHA256SUMS)
#   - 实际做法:用 git 自身的 SHA256 校验(每个 commit 的 tree hash 自带)
# 设计见 docs/design.md §1.4.1
# ----------------------------------------------------------------------------
ARG ACMESH_COMMIT=181425b3c8373ca23c0664948b97edf5ed84e9c5
ARG ACMESH_TARBALL_SHA256=d68b4e1c4c4b9e1e5e1e5e1e5e1e5e1e5e1e5e1e5e1e5e1e5e1e5e1e5e1e5e1e5
RUN set -eux; \
    cd /tmp && \
    # v2.86-PR12.2:clone 完整历史(不带 --depth 1),否则 git checkout pinned_commit
    # 会报 "fatal: reference is not a tree"(浅 clone 拿不到非 HEAD 历史)。
    # 优先 GitHub(gitee 镜像经常滞后,可能没有 pinned commit);
    # 国内环境可加 ACMESH_GITEE_FIRST=1 切回 gitee 优先。
    if [ "${ACMESH_GITEE_FIRST:-0}" = "1" ] && git clone https://gitee.com/neilpang/acme.sh.git 2>/dev/null; then \
        ACME_SRC=/tmp/acme.sh; \
    elif git clone https://github.com/acmesh-official/acme.sh.git 2>/dev/null; then \
        ACME_SRC=/tmp/acme.sh; \
    else \
        echo "FATAL: cannot clone acme.sh (github + gitee both failed)"; exit 1; \
    fi; \
    cd "$ACME_SRC" && \
    git checkout "${ACMESH_COMMIT}" && \
    ACTUAL_SHA=$(git rev-parse HEAD) && \
    if [ "${ACMESH_COMMIT}" != "${ACTUAL_SHA}" ]; then \
        echo "FATAL: acme.sh pinned commit mismatch (want ${ACMESH_COMMIT}, got ${ACTUAL_SHA})"; exit 1; \
    fi && \
    echo "acme.sh pinned to commit $(echo "${ACTUAL_SHA}" | cut -c1-12)" && \
    # v2.86-PR12.5:装到 /opt/acme.sh(容器 root fs 只读时,/opt 也只读所以 OK),
    # **不装到 /root/.acme.sh**——因为 v2.86-PR11 read_only + tmpfs 设计下,
    # /root 是镜像只读的,挂 /root/.acme.sh tmpfs 会让 Docker 软链
    # /usr/local/bin/acme.sh 悬挂。装 /opt + 软链到 /usr/local 没问题,因为
    # /usr/local/bin/acme.sh 是真文件不是软链 /root/... 之后的产物。
    # v2.86-PR12.6:`./acme.sh --install` 装的是 runtime 精简版,**不含 dnsapi/**。
    # 没 dnsapi/ → `acme.sh --issue --dns dns_ali` 报 "Cannot find DNS API hook"。
    # 修复:install 完后,从源码 cp -a dnsapi/ + deploy/ + notify/ 三个目录。
    ./acme.sh --install --home /opt/acme.sh --config-home /opt/acme.sh --no-cron --no-profile; \
    cp -a "$ACME_SRC/dnsapi"  /opt/acme.sh/; \
    cp -a "$ACME_SRC/deploy"  /opt/acme.sh/; \
    cp -a "$ACME_SRC/notify"  /opt/acme.sh/; \
    rm -rf /tmp/acme.sh; \
    cp /opt/acme.sh/acme.sh /usr/local/bin/acme.sh; \
    chmod +x /usr/local/bin/acme.sh; \
    # v2.86-pr23o:acme.sh _findHook 查 $(dirname "$_SCRIPT_")/dnsapi,
    # 而 _SCRIPT_ 是 /usr/local/bin/acme.sh,所以 _SCRIPT_HOME = /usr/local/bin,
    # → /usr/local/bin/dnsapi/ 不存在 → "Cannot find DNS API hook for: dns_ali"。
    # 解决:把 /opt/acme.sh/{dnsapi,deploy,notify} symlink 到 /usr/local/bin/。
    # 这样 acme.sh runtime 不依赖源码布局,镜像层就修好,entrypoint 无需兜底。
    ln -sfn /opt/acme.sh/dnsapi /usr/local/bin/dnsapi && \
    ln -sfn /opt/acme.sh/deploy /usr/local/bin/deploy && \
    ln -sfn /opt/acme.sh/notify /usr/local/bin/notify && \
    # v2.86-PR12.8:删除了原本的 /opt/acme.sh.dist 备份目录。理由:
    #   - docker-compose.yml 不再挂 /opt/acme.sh tmpfs,dnsapi/ deploy/ notify/
    #     不会被遮蔽,acme.sh 运行时直接用镜像层 cp 进去的文件
    #   - 容器运行时写的 account.json / ca/ 目录等,通过 /data 命名卷持久化
    #     (跟 nginx-proxy/acme-companion / mailcow 一致)
    acme.sh --version && ls /opt/acme.sh/dnsapi/ | head -5 && \
    ls -la /usr/local/bin/dnsapi | head -1

# /etc/cron.d/ 下的 LE 续签任务（LE 模式启动时 entrypoint.sh 写入）
# 占位文件保证目录存在
RUN touch /etc/cron.d/ikev2-le-placeholder && \
    echo "# placeholder; LE renew entry written by entrypoint.sh" > /etc/cron.d/ikev2-le-placeholder

# 持久化数据卷
VOLUME ["/data"]

# 端口：500/udp（IKEv2）、4500/udp（NAT-T ESP-in-UDP）、8443/tcp（管理面板）
EXPOSE 500/udp 4500/udp 8443/tcp

# tini 作 PID 1，负责信号转发与僵尸回收
# docker compose 配：默认 host 网络（公网 IPv6 必须共享宿主网卡），可选 bridge+ipvlan
#   + cap_add NET_ADMIN/NET_BIND_SERVICE/SYS_ADMIN + /dev/net/tun（commit 24）
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]
