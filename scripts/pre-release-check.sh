#!/usr/bin/env bash
# C5.4 月度发布纪律：release 前检查清单（CI release.yml 同源逻辑）。
# 用法：bash scripts/pre-release-check.sh <version>
set -euo pipefail
VERSION="${1:-}"
if [ -z "$VERSION" ]; then echo "usage: pre-release-check.sh vX.Y.Z"; exit 1; fi

cd "$(dirname "$0")/.."

echo "==> 1/5 CHANGELOG 条目"
grep -q "## \[${VERSION#v}\]" CHANGELOG.md || { echo "FAIL: CHANGELOG 缺 $VERSION 条目"; exit 1; }

echo "==> 2/5 Go 门禁（vet + 全量 -race）"
(cd ce && go vet ./... && go test ./... -race -count=1 >/dev/null) || { echo "FAIL: go"; exit 1; }

echo "==> 3/5 前端门禁"
(cd ce/console && npm run lint >/dev/null 2>&1 && npm run typecheck >/dev/null 2>&1 && npx vitest run >/dev/null 2>&1 && npm run build >/dev/null 2>&1) || { echo "FAIL: console"; exit 1; }

echo "==> 4/5 Python 门禁"
PYTHONPATH=core-sdk/python .venv/bin/pytest core-sdk/python/tests ce/py-agent/tests ce/py-agent/app -q >/dev/null || { echo "FAIL: python"; exit 1; }

echo "==> 5/5 SDK 兼容矩阵"
bash scripts/sdk-matrix.sh >/dev/null || { echo "FAIL: sdk matrix"; exit 1; }

echo "==> pre-release 检查全部通过：可打 tag $VERSION"
