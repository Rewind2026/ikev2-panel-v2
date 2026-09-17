#!/bin/bash
# acme.sh --reloadcmd hook：续签成功后被调，让 charon 重新加载证书。
# 设计见 docs/design.md §1.4.1
#
# 设计意图：charon 默认不会自动重新读取证书，需要显式调 swanctl --load-creds。
# Go 进程的 VICI ReloadAll() 内部其实就是调 swanctl 二进制，我们这里也走 CLI
# （acme.sh reloadcmd 不走 VICI 协议，hook 越简单越稳）。

set -e

LE_CERT_DIR="/data/le"
DOMAIN="${IKEV2_DOMAIN:?IKEV2_DOMAIN not set}"

LE_FULLCHAIN="${LE_CERT_DIR}/fullchain.pem"
LE_PRIVKEY="${LE_CERT_DIR}/privkey.pem"
SWAN_X509="/etc/swanctl/x509/${DOMAIN}.pem"
SWAN_KEY="/etc/swanctl/private/${DOMAIN}.key"

# 新证书可能还没复制 → 复制
if [ -f "${LE_FULLCHAIN}" ]; then
  install -m 644 "${LE_FULLCHAIN}" "${SWAN_X509}"
fi
if [ -f "${LE_PRIVKEY}" ]; then
  install -m 600 "${LE_PRIVKEY}" "${SWAN_KEY}"
fi

# 通知 charon 重读证书
swanctl --load-creds >/dev/null 2>&1 || true
swanctl --load-all   >/dev/null 2>&1 || true

echo "[ikev2-reload] reloaded at $(date -Iseconds)" >> /var/log/ikev2-reload.log
exit 0