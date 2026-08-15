#!/usr/bin/env bash
# ADC PostgreSQL backup (design/80 A-08, design/60 8.1, design/32 8.1).
#
# Daily full pg_dump (custom format, compressed) + retention + integrity
# checks + WAL freshness check (RPO gate).
#
# WAL archiving itself runs continuously inside postgres: compose.yaml mounts
# deploy/backup/postgres.conf (archive_mode=on, archive_timeout=300) so a WAL
# segment lands in /backup/wal at least every 5 minutes (RPO <= 5 min,
# NFR-001). This script only verifies that pipeline stays fresh.
#
# Actions:
#   backup              daily full + verify + RPO check   (default)
#   init-backup-dir     one-time: fix /backup ownership on a fresh volume
#   verify              re-verify the latest dump (pg_restore TOC + sha256)
#   wal-status          print WAL archive freshness (RPO evidence)
#
# Cron (host, docker-capable user):
#   0 2 * * * root /opt/adc/deploy/backup/backup.sh >> /var/log/adc-backup.log 2>&1
#
# Off-site copy: set BACKUP_DIR_HOST to a host dir (NFS mount or local disk
# rsynced to another machine per design/60 3.6); dumps are also copied there
# with checksum verification.
#
# Env overrides (defaults match deploy/.env.example):
#   ADC_DB_CONTAINER   postgres container name       (default adc-postgres)
#   ADC_PG_USER / ADC_PG_PASSWORD / ADC_PG_DBNAME
#   BACKUP_KEEP_DAYS   full dump retention in days   (default 14)
#   BACKUP_DIR_HOST    optional host-side copy dir   (default empty)
#   WAL_MAX_AGE_MIN    RPO freshness gate in minutes (default 6; archive
#                      timeout is 5, one extra minute of slack)
#
# Exit codes: 0 ok, 1 fatal, 3 RPO/verification failure (P3 backup alert,
# design/60 6.3).

set -euo pipefail

DB_CONTAINER="${ADC_DB_CONTAINER:-adc-postgres}"
PG_USER="${ADC_PG_USER:-adc}"
PG_PASSWORD="${ADC_PG_PASSWORD:-adc_dev_only}"
PG_DB="${ADC_PG_DBNAME:-adc}"
BACKUP_KEEP_DAYS="${BACKUP_KEEP_DAYS:-14}"
BACKUP_DIR_HOST="${BACKUP_DIR_HOST:-}"
WAL_MAX_AGE_MIN="${WAL_MAX_AGE_MIN:-6}"
BACKUP_DIR="/backup"

ACTION="${1:-backup}"

# ensure_dir_ownership: named volumes are root-owned on first mount; the
# postgres process (uid postgres) must be able to write WAL segments.
ensure_dir_ownership() {
  docker exec -u root "$DB_CONTAINER" sh -c \
    "mkdir -p ${BACKUP_DIR}/wal && chown -R postgres:postgres ${BACKUP_DIR}"
}

latest_dump() {
  docker exec "$DB_CONTAINER" sh -c \
    "ls -1t ${BACKUP_DIR}/adc-full-*.dump 2>/dev/null | head -1 || true"
}

check_checksum() {
  local dump="$1"
  docker exec "$DB_CONTAINER" sh -c \
    "cd ${BACKUP_DIR} && sha256sum -c \$(basename ${dump}).sha256"
}

wal_status() {
  local newest age_min
  newest="$(docker exec "$DB_CONTAINER" sh -c \
    "ls -1t ${BACKUP_DIR}/wal/ 2>/dev/null | head -1 || true")"
  if [ -z "$newest" ]; then
    echo "[error] no WAL archives present in ${BACKUP_DIR}/wal"
    return 3
  fi
  age_min="$(docker exec "$DB_CONTAINER" sh -c \
    "expr \( \$(date +%s) - \$(stat -c %Y ${BACKUP_DIR}/wal/${newest}) \) / 60")"
  echo "newest WAL: ${newest} (${age_min} min old, gate ${WAL_MAX_AGE_MIN} min)"
  if [ "$age_min" -gt "$WAL_MAX_AGE_MIN" ]; then
    echo "[error] RPO gate failed: WAL archive stale (> ${WAL_MAX_AGE_MIN} min)"
    return 3
  fi
  echo "[ok] WAL archive fresh -> RPO <= 5 min holds"
}

case "$ACTION" in
  init-backup-dir)
    ensure_dir_ownership
    echo "[ok] backup dir ready in ${DB_CONTAINER}"
    exit 0
    ;;

  verify)
    DUMP="$(latest_dump)"
    [ -n "$DUMP" ] || { echo "[fatal] no dump found in ${BACKUP_DIR}"; exit 1; }
    check_checksum "$DUMP"
    # TOC listing fails on a corrupt dump; this is the cheap restore-readiness
    # probe that runs daily.
    docker exec -e PGPASSWORD="$PG_PASSWORD" "$DB_CONTAINER" \
      pg_restore -l "$DUMP" > /dev/null
    echo "[ok] latest dump verified: $(basename "$DUMP")"
    exit 0
    ;;

  wal-status)
    wal_status
    exit $?
    ;;

  backup)
    ;;
  *)
    echo "usage: $0 [backup|init-backup-dir|verify|wal-status]" >&2
    exit 1
    ;;
esac

ensure_dir_ownership

TODAY="$(date +%Y%m%d)"
DUMP_NAME="adc-full-${TODAY}.dump"
DUMP_PATH="${BACKUP_DIR}/${DUMP_NAME}"

echo "[info] full dump start: ${DUMP_NAME}"
docker exec -e PGPASSWORD="$PG_PASSWORD" "$DB_CONTAINER" \
  pg_dump -U "$PG_USER" -d "$PG_DB" -Fc -Z 6 -f "$DUMP_PATH"

docker exec "$DB_CONTAINER" sh -c \
  "cd ${BACKUP_DIR} && sha256sum ${DUMP_NAME} > ${DUMP_NAME}.sha256"
check_checksum "$DUMP_PATH"
docker exec -e PGPASSWORD="$PG_PASSWORD" "$DB_CONTAINER" \
  pg_restore -l "$DUMP_PATH" > /dev/null
echo "[ok] full dump + checksum + TOC verify passed: ${DUMP_NAME}"

if [ -n "$BACKUP_DIR_HOST" ]; then
  mkdir -p "$BACKUP_DIR_HOST"
  docker cp "$DB_CONTAINER:${DUMP_PATH}" "$BACKUP_DIR_HOST/"
  docker cp "$DB_CONTAINER:${DUMP_PATH}.sha256" "$BACKUP_DIR_HOST/"
  (cd "$BACKUP_DIR_HOST" && sha256sum -c "${DUMP_NAME}.sha256")
  echo "[ok] host-side copy verified: ${BACKUP_DIR_HOST}/${DUMP_NAME}"
fi

docker exec "$DB_CONTAINER" sh -c \
  "find ${BACKUP_DIR} -name 'adc-full-*.dump' -mtime +${BACKUP_KEEP_DAYS} -delete"

wal_status
