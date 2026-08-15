#!/usr/bin/env bash
# ADC PostgreSQL restore (design/80 A-08, design/60 8.2, design/32 8.2).
#
# Restores a full pg_dump into a database. The default target is the scratch
# database `adc_restore`, so quarterly drills never touch production data;
# set RESTORE_DB=adc for an in-place production restore (stop the adc app
# container first: docker stop adc-app).
#
# Usage:
#   deploy/backup/restore.sh                    # drill: latest dump -> adc_restore
#   deploy/backup/restore.sh adc-full-20260815.dump
#   RESTORE_DB=adc deploy/backup/restore.sh     # in-place production restore
#
# Point-in-time recovery (replay WAL archives up to a timestamp) is described
# in RECOVERY-DRILL.md; the WAL segments live in /backup/wal (adc_backup
# volume) and the restore_command template is documented there.
#
# Env overrides (defaults match deploy/.env.example):
#   ADC_DB_CONTAINER, ADC_PG_USER, ADC_PG_PASSWORD, ADC_PG_DBNAME
#   RESTORE_DB   target database name (default adc_restore)

set -euo pipefail

DB_CONTAINER="${ADC_DB_CONTAINER:-adc-postgres}"
PG_USER="${ADC_PG_USER:-adc}"
PG_PASSWORD="${ADC_PG_PASSWORD:-adc_dev_only}"
PG_DB="${ADC_PG_DBNAME:-adc}"
RESTORE_DB="${RESTORE_DB:-adc_restore}"
BACKUP_DIR="/backup"

DUMP="${1:-}"

if [ "$RESTORE_DB" = "$PG_DB" ]; then
  echo "[warn] in-place restore into production db '${PG_DB}'"
  echo "[warn] stop application containers first: docker stop adc-app adc-gateway"
  read -r -p "type '${PG_DB}' to continue: " CONFIRM
  [ "$CONFIRM" = "$PG_DB" ] || { echo "[fatal] aborted"; exit 1; }
fi

if [ -z "$DUMP" ]; then
  DUMP="$(docker exec "$DB_CONTAINER" sh -c \
    "ls -1t ${BACKUP_DIR}/adc-full-*.dump 2>/dev/null | head -1 || true")"
  [ -n "$DUMP" ] || { echo "[fatal] no dump found in ${BACKUP_DIR}"; exit 1; }
fi
case "$DUMP" in
  /*) ;;
  *) DUMP="${BACKUP_DIR}/${DUMP}" ;;
esac

# Integrity gate before touching any database.
docker exec "$DB_CONTAINER" sh -c \
  "cd ${BACKUP_DIR} && sha256sum -c \$(basename ${DUMP}).sha256"

START_EPOCH="$(date +%s)"

docker exec -e PGPASSWORD="$PG_PASSWORD" "$DB_CONTAINER" \
  dropdb -U "$PG_USER" --if-exists "$RESTORE_DB"
docker exec -e PGPASSWORD="$PG_PASSWORD" "$DB_CONTAINER" \
  createdb -U "$PG_USER" "$RESTORE_DB"

echo "[info] restore start: $(basename "$DUMP") -> ${RESTORE_DB}"
docker exec -e PGPASSWORD="$PG_PASSWORD" "$DB_CONTAINER" \
  pg_restore -U "$PG_USER" -d "$RESTORE_DB" --no-owner --no-acl -j 2 "$DUMP"

END_EPOCH="$(date +%s)"
echo "[ok] restore finished in $((END_EPOCH - START_EPOCH)) s (RTO gate: 30 min)"

# Drill comparison report: row counts of the key tables (RECOVERY-DRILL.md
# step 4); run the same query on the production db and diff the numbers.
echo "---- restore verification counts (compare with production) ----"
docker exec -e PGPASSWORD="$PG_PASSWORD" "$DB_CONTAINER" \
  psql -U "$PG_USER" -d "$RESTORE_DB" -At -c "
    SELECT 'adc_tenants', count(*) FROM adc_tenants
    UNION ALL SELECT 'adc_devices', count(*) FROM adc_devices
    UNION ALL SELECT 'adc_audit_logs', count(*) FROM adc_audit_logs
    UNION ALL SELECT 'adc_usage_events', count(*) FROM adc_usage_events;"
