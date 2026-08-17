#!/usr/bin/env bash
# ADC 私有化离线安装脚本（design/82 B9.1，design/60 4.2）。
# 在客户内网机器执行：解包 -> 校验 manifest -> 载入镜像 -> 启动。
set -euo pipefail

DIST="$(cd "$(dirname "$0")" && pwd)"
echo "==> 校验安装包完整性"
if [ -f "$DIST/manifest.txt" ]; then
  (cd "$DIST" && shasum -a 256 -c manifest.txt >/dev/null 2>&1 || {
    echo "ERROR: manifest 校验失败，安装包可能被篡改或损坏"; exit 1; })
  echo "   manifest 校验通过"
else
  echo "WARN: 无 manifest.txt，跳过完整性校验"
fi

echo "==> 载入镜像"
docker load -i "$DIST/adc-images.tar.gz"

echo "==> 配置环境"
[ -f "$DIST/.env" ] || cp "$DIST/.env.example" "$DIST/.env"
if ! grep -q "ADC_DEVICE_KEK" "$DIST/.env"; then
  KEK=$(python3 -c "import secrets; print(secrets.token_hex(32))" 2>/dev/null || openssl rand -hex 32)
  echo "ADC_DEVICE_KEK=$KEK" >> "$DIST/.env"
  echo "   已生成 ADC_DEVICE_KEK"
fi
echo "   请编辑 $DIST/.env 修改全部默认密码"

echo "==> 启动"
docker compose -f "$DIST/compose.yaml" up -d

echo "==> 验证"
sleep 15
docker compose -f "$DIST/compose.yaml" ps
curl -sf http://127.0.0.1:18080/healthz && echo && echo "==> 安装完成，控制台：http://<本机IP>:18080"
