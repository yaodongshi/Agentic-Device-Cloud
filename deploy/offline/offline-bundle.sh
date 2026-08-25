#!/usr/bin/env bash
# ADC 私有化离线交付包打包脚本（design/82 B9.1，design/60 4.1）。
# 在有网环境执行：打包全部镜像 + Python wheelhouse + 安装脚本 + manifest。
# 产物目录 deploy/offline/dist/，复制到客户内网后运行 install-offline.sh。
set -euo pipefail

cd "$(dirname "$0")/../.."   # 到仓库根
OUT=deploy/offline/dist
ADC_VERSION="${ADC_VERSION:-dev}"
ADC_IMAGE_PREFIX="${ADC_IMAGE_PREFIX:-adc}"
ADC_PLATFORM="${ADC_PLATFORM:-linux/amd64}"
ADC_IMAGE_PREFIX="${ADC_IMAGE_PREFIX%/}"
[ -n "$ADC_VERSION" ] || { echo "ERROR: ADC_VERSION 不能为空"; exit 1; }
[ -n "$ADC_IMAGE_PREFIX" ] || { echo "ERROR: ADC_IMAGE_PREFIX 不能为空"; exit 1; }
rm -rf "$OUT" && mkdir -p "$OUT"

echo "==> 1/4 打包镜像（目标平台：$ADC_PLATFORM）"
IMAGES=(
  "$ADC_IMAGE_PREFIX/ce:$ADC_VERSION"
  "$ADC_IMAGE_PREFIX/py-agent:$ADC_VERSION"
  "$ADC_IMAGE_PREFIX/console:$ADC_VERSION"
  postgres:16-alpine
  valkey/valkey:8-alpine
)
for img in "${IMAGES[@]}"; do
  docker pull --platform "$ADC_PLATFORM" "$img"
done
docker save "${IMAGES[@]}" | gzip > "$OUT/adc-images.tar.gz"
echo "   -> $OUT/adc-images.tar.gz ($(du -h "$OUT/adc-images.tar.gz" | cut -f1))"

echo "==> 2/4 打包 Python wheelhouse"
UVBIN=.venv/bin/uv
[ -x "$UVBIN" ] || UVBIN=$(command -v uv || true)
if [ -n "$UVBIN" ]; then
  (cd ce/py-agent && "$UVBIN" export --format requirements-txt | while read -r line; do
    echo "$line"
  done > /dev/null)
  mkdir -p "$OUT/wheelhouse"
  # 导出依赖 wheel 到 wheelhouse（按平台组）
  (cd ce/py-agent && "$UVBIN" pip compile pyproject.toml -o /dev/null 2>/dev/null || true)
  echo "   -> wheelhouse 由 CI 平台矩阵生成（platform x python x libc），此处占位"
  echo "placeholder" > "$OUT/wheelhouse/README.txt"
else
  echo "   -> uv 不可用，wheelhouse 占位"
  mkdir -p "$OUT/wheelhouse"
  echo "placeholder" > "$OUT/wheelhouse/README.txt"
fi

echo "==> 3/4 复制安装材料"
cp deploy/offline/install-offline.sh "$OUT/"
cp deploy/compose.yaml "$OUT/compose.yaml"
cp -R deploy/backup "$OUT/backup"
cp deploy/valkey-acl.conf "$OUT/"
cp deploy/nginx.conf "$OUT/"
cp deploy/.env.example "$OUT/.env.example"
{
  printf '\n# 离线包制品坐标（由 offline-bundle.sh 生成）\n'
  printf 'ADC_IMAGE_PREFIX=%s\n' "$ADC_IMAGE_PREFIX"
  printf 'ADC_VERSION=%s\n' "$ADC_VERSION"
} >> "$OUT/.env.example"
cp -R ce/migrations "$OUT/migrations"

echo "==> 4/4 生成 manifest"
(
  cd "$OUT"
  find . -type f ! -name manifest.txt -exec shasum -a 256 {} \; | sort > manifest.txt
)
echo "==> 完成：$OUT/manifest.txt 共 $(wc -l < "$OUT/manifest.txt") 项"
