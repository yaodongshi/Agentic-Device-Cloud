## V2.2 M9 改动清单

| Delta spec Requirement | 现状入口 | 实现任务 |
|---|---|---|
| 默认 Compose 不自动 seed | `deploy/compose.yaml`、`ce/cmd/adc/main.go` | 2.1、2.4：移除默认 `-seed`，增加 `dev-seed` profile 和环境拒绝 |
| 生产凭证与 KEK guard | `deploy/compose.yaml`、`scripts/runtime-guard.sh` | 2.2、2.3、2.4：迁移前 fail closed，日志仅含变量名和规则 |
| Python Agent 非公开路由逐请求鉴权 | `ce/py-agent/app/`、开发者应用 introspection | 3.1-3.6：统一依赖、scope、可信租户注入和吊销验证 |
| HITL 决策来自当前人类主体 | Go 管理会话、Python A2A decision | 6.1-6.5：人类角色复核、租户 PG CAS、机器固定拒绝 |
| A2A 任务持久化、幂等、CAS、SQL 租户隔离 | Python `_tasks` 内存状态 | 1.2-1.4、5.1-5.6、6.2、6.4：连续迁移和 PostgreSQL repository |
| 用户授权版本推进与逐请求复核 | Go session、用户/角色 repository | 1.2-1.4、4.1-4.5：`authz_version`、数据库时间有效角色、fail closed |
| Docker/Compose 镜像固定 digest | 三份 Dockerfile、`deploy/compose.yaml` | 7.1、7.2、7.4、7.5：不可变引用检查和 Dependabot |
| Actions 完整 SHA 与最小权限 | `.github/workflows/` | 7.3-7.5：完整提交 SHA、权限静态门禁和自动更新 |
| 候选发布统一源码门禁 | `ci.yml`、`release.yml`、`pre-release-check.sh` | 8.1-8.6：immutable、runtime、真实 Compose smoke 必经且失败阻断制品 |
| 既有卷升级与四路径 Guard smoke | 迁移脚本、`scripts/dev-smoke.sh` | 8.1、9.1-9.7：缺失/类型/范围/合法 HTTP 路径，`FAIL=0 SKIP=0` |

本清单不包含 NATS JetStream、outbox、事件总线或异步投递实现。
