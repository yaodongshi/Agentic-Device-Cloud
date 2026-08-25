# ce/migrations 迁移说明

## 迁移工具选择

使用内置 Go `adc-migrate` 命令管理本目录迁移，理由：

1. 与 Go 技术栈同源，迁移文件为纯 SQL，可用 psql 直接校验与执行，评审门槛低；
2. 通过 `go:embed` 自动嵌入所有 `*.up.sql`，适配 ADC 私有化离线交付场景；
3. 使用 `adc_schema_migrations`、PostgreSQL advisory lock 和单迁移事务，支持并发互斥、失败回滚与版本审计。

## 文件命名

版本号单调递增、禁止复用：`0001_init.up.sql` / `0001_init.down.sql`，下一版本为 `0002_<名称>`。

## 执行命令

```bash
# 使用 DATABASE_URL
DATABASE_URL="postgres://adc:****@127.0.0.1:5432/adc?sslmode=disable" \
  go run ./ce/cmd/adc-migrate

# 或沿用应用的 PG_HOST/PG_PORT/PG_USER/PG_PASSWORD/PG_DBNAME/PG_SSLMODE
PG_PASSWORD="****" go run ./ce/cmd/adc-migrate

# 查看当前版本
psql "postgres://adc:****@127.0.0.1:5432/adc?sslmode=disable" \
  -c 'SELECT version, name, applied_at FROM adc_schema_migrations ORDER BY version'

# 新增迁移时同时创建连续版本的 up/down 文件；运行时仅自动向前迁移
touch ce/migrations/0007_<名称>.up.sql ce/migrations/0007_<名称>.down.sql
```

## 注意事项

- `gen_random_uuid()` 为 PG 13+ 核心内置函数，无需安装 pgcrypto 或 uuid-ossp 扩展。
- 审计分区：`0001_init` 建立"执行当月 + 未来两个月"初始分区（+08 月首零点边界）与 DEFAULT 兜底分区；后续按月预建由 pg_cron 或 Go 后台任务续接（design/32 5.1），DEFAULT 分区行数非零表示预建失效，立即告警。
- 本目录迁移只含 DDL；数据库角色与应用账号（adc_app/adc_admin，design/32 5.3）及平台预置角色种子数据（design/32 7.3）不在迁移文件中。
- `adc-migrate` 是空库安装和既有库升级的权威入口；生产不自动执行 down migration。
- 与 design/32 的两处对齐修正（详见 0001_init.up.sql 头部注释）：`adc_agent_api_keys.secret_hash` 按 ce/internal/agentauth/key.go 契约补充；`idx_tickets_pending_expire` 索引列修正为 `expires_at`（design/32 3.8 笔误，代码契约与 design 6.1 均为 expires_at）。
