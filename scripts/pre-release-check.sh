#!/usr/bin/env bash
# C5.4 月度发布纪律：release 前检查清单（CI release.yml 同源逻辑）。
# 用法：bash scripts/pre-release-check.sh <version>
set -euo pipefail
VERSION="${1:-}"
if [ -z "$VERSION" ]; then echo "usage: pre-release-check.sh vX.Y.Z"; exit 1; fi

cd "$(dirname "$0")/.."

echo "==> 1/9 不可变构建输入"
bash scripts/immutable-check-selftest.sh >/dev/null && bash scripts/immutable-check.sh >/dev/null || { echo "FAIL: immutable inputs"; exit 1; }

echo "==> 2/9 版本与 CHANGELOG 一致性"
bash scripts/version-check.sh "$VERSION" || { echo "FAIL: version"; exit 1; }

echo "==> 3/9 Go 门禁（vet + 全量 -race）"
(cd ce && go vet ./... && go test ./... -race -count=1 >/dev/null) || { echo "FAIL: go"; exit 1; }

echo "==> 4/9 前端与 i18n 门禁"
(cd ce/console && npm run lint >/dev/null 2>&1 && npm run typecheck >/dev/null 2>&1 && npm run i18n:test >/dev/null 2>&1 && npx vitest run >/dev/null 2>&1 && npm run build >/dev/null 2>&1) || { echo "FAIL: console/i18n"; exit 1; }

echo "==> 5/9 Python 门禁"
PYTHONPATH=core-sdk/python .venv/bin/pytest core-sdk/python/tests ce/py-agent/tests ce/py-agent/app -q >/dev/null || { echo "FAIL: python"; exit 1; }

echo "==> 6/9 SDK 兼容矩阵"
bash scripts/sdk-matrix.sh >/dev/null || { echo "FAIL: sdk matrix"; exit 1; }

echo "==> 7/9 数据库迁移静态门禁"
bash scripts/migration-check.sh >/dev/null || { echo "FAIL: migrations"; exit 1; }

echo "==> 8/9 Compose 配置解析"
docker compose -f deploy/compose.yaml config -q || { echo "FAIL: compose"; exit 1; }

echo "==> 9/9 安全扫描由 release workflow 执行（Trivy + SBOM + cosign）"
echo "==> pre-release 源码检查全部通过：$VERSION 可进入候选制品构建；正式 tag 仍需人工批准"
