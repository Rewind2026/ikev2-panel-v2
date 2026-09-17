#!/bin/bash
# Let's Encrypt 证书续签脚本（含失败回退）
# 设计见 docs/design.md §1.4.1
#
# 流程：
#   1. 证书还有 >30 天 → 跳过
#   2. 备份当前证书到 /data/le/backup/<timestamp>/
#   3. acme.sh --renew（会自动通过 --reloadcmd /usr/local/bin/ikev2-reload.sh 重载 charon）
#   4. 验证新证书有效期 ≥ 30 天
#      - 是 → 删除 FAIL_FLAG
#      - 否 → 回退到备份 + 设置 FAIL_FLAG + 写日志
#
# 注：acme.sh 自身续签成功后自动触发 --reloadcmd（已在 entrypoint.sh 中配置），
# 本脚本不再手动调用 swanctl --load-creds，避免双重重载。
set -e

DOMAIN="${1:-${IKEV2_DOMAIN:-}}"
if [ -z "$DOMAIN" ]; then
  echo "[renew] FATAL: no domain given" >&2
  exit 2
fi

# 持久化路径（/data/le/），不是容器内 /etc/swanctl/
LE_DIR="/data/le"
CERT_PATH="${LE_DIR}/fullchain.pem"
KEY_PATH="${LE_DIR}/privkey.pem"
BACKUP_DIR="${LE_DIR}/backup"
FAIL_FLAG="${LE_DIR}/LAST_RENEW_FAILED"
TIMESTAMP=$(date +%Y%m%d-%H%M%S)

mkdir -p "$BACKUP_DIR"

# 检查证书有效期（< 30 天 = 需要续签）
check_days_left() {
    openssl x509 -in "$1" -noout -checkend 2592000 2>/dev/null && echo "ok" || echo "renew"
}

if [ ! -f "$CERT_PATH" ]; then
  echo "[renew] FATAL: cert not found at $CERT_PATH" >&2
  exit 3
fi

if [ "$(check_days_left "$CERT_PATH")" = "ok" ]; then
  echo "[renew] cert still has >30 days, skip"
  exit 0
fi

echo "[renew] cert needs renewal, backing up..."

# 备份
mkdir -p "${BACKUP_DIR}/${TIMESTAMP}"
cp "$CERT_PATH" "${BACKUP_DIR}/${TIMESTAMP}/fullchain.pem"
cp "$KEY_PATH" "${BACKUP_DIR}/${TIMESTAMP}/privkey.pem"
echo "[renew] backed up to ${BACKUP_DIR}/${TIMESTAMP}"

# 续签（--reloadcmd 由 entrypoint.sh 装证书时设过，自动触发）
if acme.sh --renew -d "$DOMAIN" --keylength 2048 --force \
       --home /root/.acme.sh --config-home /root/.acme.sh \
       > /var/log/acme-renew.log 2>&1; then
  if [ "$(check_days_left "$CERT_PATH")" = "ok" ]; then
    rm -f "$FAIL_FLAG"
    echo "[renew] success, new cert loaded (reloadcmd triggered charon reload)"
    exit 0
  fi
fi

# 续签失败 → 回退
echo "[renew] FATAL: renew failed, rolling back" >&2
cp "${BACKUP_DIR}/${TIMESTAMP}/fullchain.pem" "$CERT_PATH"
cp "${BACKUP_DIR}/${TIMESTAMP}/privkey.pem" "$KEY_PATH"
chmod 644 "$CERT_PATH"
chmod 600 "$KEY_PATH"
# 回退后也要 reload，让 charon 读旧证书
/usr/local/bin/ikev2-reload.sh >/dev/null 2>&1 || true
touch "$FAIL_FLAG"
echo "[renew] rolled back to backup, FAIL_FLAG set" >&2
exit 1