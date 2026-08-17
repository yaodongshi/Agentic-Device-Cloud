# ADC 部署手册（V1.0 单机版，design/60 落地）

> 适用：Linux x86_64 / ARM64（信创麒麟/统信/UOS），Docker 24+。
> 目标：新环境 30 分钟内上线；本手册覆盖 安装 / 升级 / 回滚 / 备份 四章。

## 1. 前置检查

```bash
docker --version          # >= 24
uname -m                  # x86_64 或 aarch64（镜像已双架构）
free -h                   # 建议 >= 8GB RAM
df -h /var/lib/docker     # 建议 >= 50GB 可用
```

网络要求：设备侧需可达本机 18080 端口（WSS 隧道）；出站需可达企业微信/钉钉 webhook。

## 2. 安装（单机 compose）

```bash
# 1. 获取代码
git clone https://github.com/yaodongshi/Agentic-Device-Cloud.git adc
cd adc/deploy

# 2. 配置环境
cp .env.example .env
vim .env   # 必改：ADC_PG_PASSWORD、ADC_VALKEY_PASSWORD、ADC_DEVICE_KEK（64位hex）、
           # ADC_HITL_CALLBACK_KEY、ADC_WECOM_WEBHOOK / ADC_DINGTALK_WEBHOOK

# 3. 启动（含构建，约 5-10 分钟）
docker compose -f compose.yaml up -d --build

# 4. 验证
docker compose -f compose.yaml ps          # 6 容器全部 healthy
curl http://127.0.0.1:18080/healthz        # backends 全部 ok
```

首次启动后：登录控制台 `http://<主机IP>:18080`（admin/admin123!，**立即修改**）。

## 3. 升级

```bash
cd deploy
git pull
docker compose -f compose.yaml pull        # 或 up -d --build（源码部署时）
docker compose -f compose.yaml up -d       # 滚动重建
docker compose -f compose.yaml ps          # 确认 healthy
bash ../scripts/dev-smoke.sh               # 冒烟验证（可选）
```

升级顺序（design/60 4.4）：先网关、后数据面/Agent 面；跨大版本先看 CHANGELOG 迁移说明。

## 4. 回滚

```bash
# 回滚到指定 tag
git checkout v0.1.0
docker compose -f compose.yaml up -d --build
```

数据库迁移是向前兼容设计（expand-contract），回滚应用代码不要求回滚 schema；若需回滚 schema 见 ce/migrations/ 的 down 脚本（需人工执行）。

## 5. 备份与恢复

```bash
# 每日备份（cron 建议 02:00）
bash backup/backup.sh                      # 产物在 /backup：dump + WAL 归档

# 恢复演练（季度必做，RPO 5min / RTO 30min 目标）
bash backup/restore.sh                     # 恢复至 adc_restore 演练库并比对计数
```

详见 `backup/RECOVERY-DRILL.md`（哨兵租户法、PITR 指引）。

## 6. 端口与安全

| 端口 | 用途 | 暴露范围 |
|---|---|---|
| 18080 | 统一入口（控制台+API+WSS） | 对外 |
| 18082 | dev 决策旁路 | 仅 127.0.0.1（生产移除） |
| 19090/19300/19310 | Prometheus/Grafana/Loki | 仅 127.0.0.1 |
| 19091 | 网关 metrics | 仅 127.0.0.1 |

生产硬化清单：修改全部默认密码、启用网关 TLS（ADC_TLS_ENABLE）、防火墙仅放行 18080、定期 `docker scout`/trivy 扫描镜像。
