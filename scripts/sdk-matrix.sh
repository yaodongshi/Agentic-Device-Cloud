#!/usr/bin/env bash
# C2.2 SDK 认证计划：兼容矩阵自动验证。
# 验证 SDK 版本 × 网关协议版本的组合：core-sdk/protocol 的 Go/Python/Rust
# 三实现共享同一 wire 契约，任何一方的契约测试失败即阻断。
# 用法：bash scripts/sdk-matrix.sh
set -euo pipefail
cd "$(dirname "$0")/.."

echo "==> 协议契约矩阵（ProtocolVersion=1.0）"
FAIL=0

echo "  1/3 Go 契约测试"
(cd ce && go test ../core-sdk/protocol/... -race -count=1) || { echo "  FAIL: go protocol"; FAIL=1; }

echo "  2/3 Python 契约测试"
PYTHONPATH=core-sdk/python .venv/bin/pytest core-sdk/python/tests -q || { echo "  FAIL: python protocol"; FAIL=1; }

echo "  3/3 Rust 契约测试"
export PATH="$HOME/.cargo/bin:$PATH"
(cd core-sdk/rust && cargo test -q) || { echo "  FAIL: rust protocol"; FAIL=1; }

echo "==> 网关协议协商测试（缺 version=1.0、未知版本拒绝）"
(cd ce && go test ../core-sdk/protocol/... -run TestVersionNegotiation -count=1) || FAIL=1

if [ "$FAIL" -ne 0 ]; then
  echo "==> 兼容矩阵：FAIL（SDK 认证不通过）"
  exit 1
fi
echo "==> 兼容矩阵：PASS（三实现 + 协商一致）"
