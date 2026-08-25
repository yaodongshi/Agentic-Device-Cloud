#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
shopt -s nullglob
up=(ce/migrations/*.up.sql)
down=(ce/migrations/*.down.sql)
[ "${#up[@]}" -gt 0 ] || { echo "FAIL: 未找到数据库迁移"; exit 1; }
[ "${#up[@]}" -eq "${#down[@]}" ] || { echo "FAIL: up/down 迁移数量不一致"; exit 1; }

previous=0
for file in "${up[@]}"; do
  name="${file##*/}"
  version="${name%%_*}"
  [[ "$version" =~ ^[0-9]{4}$ ]] || { echo "FAIL: 迁移文件版本非法：$file"; exit 1; }
  [ "$((10#$version))" -eq "$((previous + 1))" ] || { echo "FAIL: 迁移版本不连续，期望 $(printf '%04d' "$((previous + 1))")，实际 $version：$file"; exit 1; }
  [ -f "${file%.up.sql}.down.sql" ] || { echo "FAIL: 缺少 down 迁移：$file"; exit 1; }
  previous=$((10#$version))
done

echo "迁移静态校验通过：${#up[@]} 个版本，最新 $(printf '%04d' "$previous")"
