#!/bin/bash
# acme.sh --reloadcmd hook：续签成功后被调，让 charon + 面板 HTTPS 重新加载证书。
# 设计见 docs/design.md §1.4.1 + §19.7 (v2-83) + v2.86-PR12
#
# 设计意图：
#   - charon 默认不会自动重新读取证书，需要显式调 swanctl --load-creds
#     注意:不要用 --load-all(会 reload 所有 conns,重置 active SA → 用户断流)
#   - 面板 HTTPS (Go http.Server) 在 v2-83 加了 SIGHUP 热重载
#     (atomic.Pointer 缓存 cert,SIGHUP handler 重新读 panel-tls/)
#   - 这两个都要在本 hook 里触发
#
# v2.86-PR12.1 改动:
#   - 改 install 直接覆盖为 atomic rename(write tmp + rename)
#   - 防止续签中途 OOM kill 留下半截 PEM → charon 读半截 cert 全部 SA 挂
#   - 改 swanctl --load-all 为只 --load-creds(不重置 active SA,断流 0)
#
# Go 进程的 VICI ReloadAll() 内部其实就是调 swanctl 二进制，我们这里也走 CLI
# （acme.sh reloadcmd 不走 VICI 协议，hook 越简单越稳）。

set -e

LE_CERT_DIR="/data/le"
DOMAIN="${IKEV2_DOMAIN:?IKEV2_DOMAIN not set}"
PANEL_DATA_DIR="${IKEV2_DATA_DIR:-/data}"

LE_FULLCHAIN="${LE_CERT_DIR}/fullchain.pem"
LE_PRIVKEY="${LE_CERT_DIR}/privkey.pem"
SWAN_X509="/etc/swanctl/x509/${DOMAIN}.pem"
SWAN_KEY="/etc/swanctl/private/${DOMAIN}.key"

# v2-83:同时复制到 /data/panel-tls/(面板 HTTPS 用)
PANEL_TLS_DIR="${PANEL_DATA_DIR}/panel-tls"
PANEL_CERT="${PANEL_TLS_DIR}/cert.pem"
PANEL_KEY="${PANEL_TLS_DIR}/key.pem"

# v2.86-PR12.1:atomic_rename 替代 install
# 写 .tmp → rename,失败时原文件保留(charon 仍可读旧 cert)
atomic_rename() {
  local src="$1" dst="$2" mode="$3"
  if [ ! -f "$src" ]; then
    return 0
  fi
  local tmp="${dst}.tmp.$$"
  install -m "$mode" "$src" "$tmp"
  mv -f "$tmp" "$dst"
}

# v2.86-PR12.1:atomic copy to swanctl
if [ -f "${LE_FULLCHAIN}" ]; then
  atomic_rename "${LE_FULLCHAIN}" "${SWAN_X509}" "644"
fi
if [ -f "${LE_PRIVKEY}" ]; then
  atomic_rename "${LE_PRIVKEY}" "${SWAN_KEY}" "600"
fi

# v2-83:复制到面板 HTTPS 用的 panel-tls/(用 install 确保权限正确)
# 注意:Go 进程 SIGHUP 重读这两个文件,所以必须先复制再 HUP
# v2.86-PR12.1:同样改 atomic_rename
if [ -f "${LE_FULLCHAIN}" ]; then
  mkdir -p "${PANEL_TLS_DIR}"
  atomic_rename "${LE_FULLCHAIN}" "${PANEL_CERT}" "644"
fi
if [ -f "${LE_PRIVKEY}" ]; then
  mkdir -p "${PANEL_TLS_DIR}"
  atomic_rename "${LE_PRIVKEY}" "${PANEL_KEY}" "600"
fi

# v2.86-PR12.1:只调 --load-creds(不调 --load-all)
# --load-all 会 reload 所有连接定义,触发 active SA 全部重建 → 用户 VPN 断流 500ms
# --load-creds 只重读 cert/key,SAs 沿用现有,新建立/重协商的连接才用新 cert
# 实际效果:VPN 用户 0 断流,只在重协商时(24h rekey 或重连)用新 cert
swanctl --load-creds >/dev/null 2>&1 || true

# v2-83:通知 Go 进程重读面板 HTTPS 证书
#   - pidof 找 ikev2-panel PID
#   - kill -HUP 触发 main.go 里的 SIGHUP handler
#   - atomic.Pointer 替换新 cert,GetCertificate 回调下一 handshake 用新 cert
if command -v pidof >/dev/null 2>&1; then
  PANEL_PID=$(pidof ikev2-panel 2>/dev/null || true)
  if [ -n "${PANEL_PID}" ]; then
    kill -HUP "${PANEL_PID}" 2>/dev/null || true
    echo "[ikev2-reload] sent SIGHUP to ikev2-panel pid=${PANEL_PID}"
  fi
fi

echo "[ikev2-reload] reloaded at $(date -Iseconds)" >> /var/log/ikev2-reload.log
exit 0