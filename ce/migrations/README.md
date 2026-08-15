# ce/migrations 迁移说明

## 迁移工具选择

使用 **golang-migrate**（github.com/golang-migrate/migrate）管理本目录迁移，理由：

1. 与 Go 技术栈同源，迁移文件为纯 SQL，可用 psql 直接校验与执行，评审门槛低；
2. 支持 go:embed 嵌入式迁移，适配 ADC 私有化离线交付场景；
3. 版本表 schema_migrations 结构简单可审计，up/down 配对纪律与 design/32 7.1 一致（design/32 原文：放弃 goose 因其函数式迁移会诱导业务逻辑进迁移）。

## 文件命名

版本号单调递增、禁止复用：`0001_init.up.sql` / `0001_init.down.sql`，下一版本为 `0002_<名称>`。

## 执行命令

```bash
# golang-migrate CLI（推荐，自动记录版本）
migrate -path ce/migrations \
  -database "postgres://adc:****@127.0.0.1:5432/adc?sslmode=disable" up

# 查看当前版本
migrate -path ce/migrations -database "postgres://...:5432/adc?sslmode=disable" version

# 开发环境回滚一步；生产禁止 down（design/32 7.2 只向前）
migrate -path ce/migrations -database "postgres://...:5432/adc?sslmode=disable" down 1

# 新增迁移（生成空 up/down 模板）
migrate create -ext sql -dir ce/migrations 0002_<名称>

# 无 migrate CLI 时用 psql 直接校验/执行（不记录版本表）
docker exec -i <pg容器名> psql -U postgres -v ON_ERROR_STOP=1 -d adc \
  -f - < 0001_init.up.sql
```

## 注意事项

- `gen_random_uuid()` 为 PG 13+ 核心内置函数，无需安装 pgcrypto 或 uuid-ossp 扩展。
- 审计分区：`0001_init` 建立"执行当月 + 未来两个月"初始分区（+08 月首零点边界）与 DEFAULT 兜底分区；后续按月预建由 pg_cron 或 Go 后台任务续接（design/32 5.1），DEFAULT 分区行数非零表示预建失效，立即告警。
- 本目录迁移只含 DDL；数据库角色与应用账号（adc_app/adc_admin，design/32 5.3）及平台预置角色种子数据（design/32 7.3）不在迁移文件中。
- 与 design/32 的两处对齐修正（详见 0001_init.up.sql 头部注释）：`adc_agent_api_keys.secret_hash` 按 ce/internal/agentauth/key.go 契约补充；`idx_tickets_pending_expire` 索引列修正为 `expires_at`（design/32 3.8 笔误，代码契约与 design 6.1 均为 expires_at）。
