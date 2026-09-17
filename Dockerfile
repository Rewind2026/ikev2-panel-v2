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

# 用清华源（默认 deb.debian.org 在国内网络慢/卡）
RUN sed -i 's|deb.debian.org|mirrors.tuna.tsinghua.edu.cn|g; s|security.debian.org|mirrors.tuna.tsinghua.edu.cn|g' /etc/apt/sources.list.d/debian.sources 2>/dev/null \
    || sed -i 's|deb.debian.org|mirrors.tuna.tsinghua.edu.cn|g; s|security.debian.org|mirrors.tuna.tsinghua.edu.cn|g' /etc/apt/sources.list

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
#   --enable-stroke           ipsec/starter 命令（entrypoint.sh 用 `ipsec start` 启 charon）
#   --enable-sha1 / sha2 / md5 / hmac   关键 PRF / 完整性算法（charon 启动需要 NONCE_GEN + HASH_SHA1）
#   --enable-nonce / random   随机数生成
#   --enable-aes / ctr / ccm / gcm / chapoly  标准 ESP 加密套件
#   --enable-curve25519       现代 ECDH group（默认 ECP256 也需要 openssl 里的 ecdh）
#   (gmp 保持 enabled)         gmp 大数库加速 DH/ECDH
#
# 不启用：
#   --disable-ikev1           按官方建议（v6.0+ 默认禁用 IKEv1）
#   --disable-static          只构建动态库，缩小镜像
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
        --enable-stroke \
        --enable-sha1 \
        --enable-sha2 \
        --enable-md5 \
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
# Stage 3: runtime  精简运行时镜像
# ============================================================================
FROM debian:bookworm-slim AS runtime

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
# 用清华源
RUN sed -i 's|deb.debian.org|mirrors.tuna.tsinghua.edu.cn|g; s|security.debian.org|mirrors.tuna.tsinghua.edu.cn|g' /etc/apt/sources.list.d/debian.sources 2>/dev/null \
    || sed -i 's|deb.debian.org|mirrors.tuna.tsinghua.edu.cn|g; s|security.debian.org|mirrors.tuna.tsinghua.edu.cn|g' /etc/apt/sources.list

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        libssl3 \
        iproute2 \
        iptables \
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
COPY configs/swanctl-ipv6-only.conf           /etc/swanctl/swanctl.conf
COPY configs/strongswan-filelog.conf          /etc/strongswan.d/filelog.conf

# Web 资源（HTML 模板 + CSS），main.go 启动时在 /app/web/ 查找
COPY web/                                    /app/web/

# 构建期 sanity check：确认关键二进制都在 PATH 里
# 注意：默认 configure 不加 --enable-systemd 时编译的是 charon（不是 charon-systemd）
# entrypoint.sh 用 `ipsec start --nofork` 启动的就是普通 charon
# strongSwan 6.0.1 把 charon 安装到 /usr/lib/ipsec/charon（不在 PATH 里），
# `ipsec` 脚本运行时通过 strongswan.conf 的 setenv 临时加 PATH；这里直接验证文件存在。
RUN set -eux; \
    for bin in ipsec swanctl ikev2-panel tini; do \
        command -v "$bin" >/dev/null || { echo "FATAL: missing $bin"; exit 1; }; \
    done; \
    test -x /usr/lib/ipsec/charon || { echo "FATAL: missing /usr/lib/ipsec/charon"; exit 1; }; \
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
    chmod 1777 /var/log

# ----------------------------------------------------------------------------
# 安装 acme.sh（仅 LE 模式需要用到，但镜像里直接装好，避免容器启动再下）：
#   - 优先用 Gitee 镜像（国内可达）；失败 fallback 到 GitHub
#   - 装到 /root/.acme.sh
#   - 装完后软链 /usr/local/bin/acme.sh（entrypoint.sh 直接命令名调用）
#   - 不注册 cron（acme.sh 默认会注册 crontab；我们自己用 cron.d 管理）
#   - 不写 account.conf（凭证由 entrypoint.sh 运行时从 env 注入，不进镜像）
# 设计见 docs/design.md §1.4.1
# ----------------------------------------------------------------------------
RUN set -eux; \
    cd /tmp && \
    if git clone --depth 1 https://gitee.com/neilpang/acme.sh.git 2>/dev/null; then \
        ACME_SRC=/tmp/acme.sh; \
    elif git clone --depth 1 https://github.com/acmesh-official/acme.sh.git 2>/dev/null; then \
        ACME_SRC=/tmp/acme.sh; \
    else \
        echo "FATAL: cannot clone acme.sh (gitee + github both failed)"; exit 1; \
    fi; \
    cd "$ACME_SRC" && ./acme.sh --install --home /root/.acme.sh --config-home /root/.acme.sh --no-cron --no-profile; \
    rm -rf /tmp/acme.sh; \
    ln -sf /root/.acme.sh/acme.sh /usr/local/bin/acme.sh; \
    acme.sh --version

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
