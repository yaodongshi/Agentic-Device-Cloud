#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
VERSION="${1:-v0.3.0}"
VERSION="${VERSION#v}"

[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || {
  echo "FAIL: 非法版本号 $VERSION"
  exit 1
}

grep -Fq "## [$VERSION]" CHANGELOG.md || { echo "FAIL: CHANGELOG 缺少 [$VERSION]"; exit 1; }
node -e '
  const fs = require("fs");
  const expected = process.argv[1];
  for (const file of ["ce/console/package.json", "ce/console/package-lock.json"]) {
    const actual = JSON.parse(fs.readFileSync(file, "utf8")).version;
    if (actual !== expected) throw new Error(`${file}: ${actual} != ${expected}`);
  }
' "$VERSION"

grep -Fq 'image: ${ADC_CE_IMAGE:-adc/ce:dev}' deploy/compose.yaml || {
  echo "FAIL: Compose CE 镜像未使用 ADC_CE_IMAGE"
  exit 1
}
grep -Fq 'image: ${ADC_PY_AGENT_IMAGE:-adc/py-agent:dev}' deploy/compose.yaml || {
  echo "FAIL: Compose Python Agent 镜像未使用 ADC_PY_AGENT_IMAGE"
  exit 1
}
grep -Fq 'image: ${ADC_CONSOLE_IMAGE:-adc/console:dev}' deploy/compose.yaml || {
  echo "FAIL: Compose Console 镜像未使用 ADC_CONSOLE_IMAGE"
  exit 1
}

for image in ce py-agent console; do
  grep -Fq "/$image:sha-\${{ github.sha }}" .github/workflows/release.yml || {
    echo "FAIL: release workflow 缺少 $image 的 commit SHA 暂存镜像"
    exit 1
  }
  grep -Fq "promote $image" .github/workflows/release.yml || {
    echo "FAIL: release workflow 缺少 $image 的正式 tag 晋升"
    exit 1
  }
done

grep -Fq 'TAG="${{ steps.meta.outputs.version }}"' .github/workflows/release.yml || {
  echo "FAIL: release workflow 正式镜像未使用与 ADC_VERSION 一致的无 v 版本号"
  exit 1
}

if grep -Fq '${{ steps.meta.outputs.image_base }}/adc:' .github/workflows/release.yml; then
  echo "FAIL: release workflow 残留与 Compose 不一致的 adc 镜像名"
  exit 1
fi

echo "版本一致性校验通过：$VERSION"
