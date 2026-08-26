## 1. 基线与数据库迁移

- [x] 1.1 盘点默认 Compose、seed、Python Agent 路由、用户会话、A2A 内存状态和 workflow 引用，形成与六份 delta spec 逐项对应的改动清单；验证：清单覆盖全部 Requirement 且无 JetStream/outbox 实现项（不超过 1 小时）
- [x] 1.2 编写用户 `authz_version` 与 `adc_a2a_tasks` 连续 up/down 迁移，包含状态 CHECK、租户/应用幂等唯一约束、列表索引和外键（SEC-08/11）；验证：迁移静态门禁通过且 up/down 配对连续（不超过 2 小时）
- [x] 1.3 补充迁移 SQL 契约测试，验证 `authz_version` 非空默认值、A2A 状态范围、幂等唯一性、版本字段和租户索引（SEC-08/11）；验证：定向 Go 迁移测试通过（不超过 2 小时）
- [x] 1.4 在 PostgreSQL 16 实跑空库 up、down、再次 up 和并发迁移，确认事务回滚与账本连续；验证：迁移集成测试无失败、无跳过（不超过 2 小时）

## 2. Compose 运行时安全基线

- [x] 2.1 将 seed 从默认启动依赖移除并改为显式 `dev` profile 一次性服务，加入非 dev 拒绝和幂等退出（SEC-25）；验证：默认 `docker compose config` 无 seed 启动路径，dev/non-dev 定向测试通过（不超过 2 小时）
- [x] 2.2 实现生产默认管理员、设备、应用凭证和 KEK 启动 guard，覆盖缺失、默认/占位、格式和强度校验（SEC-03/13/25）；验证：每类不安全配置均非零退出，合法配置成功（不超过 2 小时）
- [x] 2.3 统一 guard 与 seed 的敏感日志脱敏，只输出变量名和规则类别（SEC-13/25）；验证：向测试配置注入唯一 canary secret 后扫描 stdout/stderr 和容器日志均无 canary 及其编码/哈希片段（不超过 2 小时）
- [x] 2.4 增加默认不 seed、显式 dev seed、非 dev 拒绝和生产 guard 的 Compose 集成测试（SEC-03/13/25）；验证：真实 Compose 拓扑测试全部通过且无 skip（不超过 2 小时）

## 3. Python Agent 认证闭环

- [x] 3.1 扩展 Go 开发者应用 scope 白名单与 introspection 契约，加入 `llm:invoke`、`evals:read`、`evals:write` 且不提供机器审批 scope（SEC-01/02）；验证：白名单正反单测和未知 scope 拒绝测试通过（不超过 2 小时）
- [x] 3.2 在 Python Agent 抽取统一实时 introspection 依赖，认证超时、不可用、非 200 和响应非法均 fail closed（SEC-02）；验证：401、403、超时、503、畸形响应测试通过（不超过 2 小时）
- [x] 3.3 为全部 LLM 路由绑定 `llm:invoke`，仅从认证主体注入租户和应用（SEC-02）；验证：匿名、scope 不足、伪造租户、合法调用四类 API 测试通过（不超过 2 小时）
- [x] 3.4 为 Eval 列表/读取绑定 `evals:read`，为运行创建绑定 `evals:write`（SEC-02）；验证：每个 Eval 路由的匿名、错 scope 和合法 scope 参数化测试通过（不超过 2 小时）
- [x] 3.5 核对 A2A task 路由统一认证并拒绝客户端租户字段覆盖，health、readiness、Agent Card 保持公开（SEC-02）；验证：全路由认证矩阵测试不存在未分类路由（不超过 2 小时）
- [x] 3.6 增加应用凭证停用、轮换和吊销后的 Python 下一请求测试（SEC-02）；验证：旧凭证立即 401且测试未使用 Python 认证缓存（不超过 2 小时）

## 4. 用户会话即时撤权

- [x] 4.1 扩展 session payload 保存 user id、tenant id 和签发时 `authz_version`，登录只加载未过期角色（SEC-01/21）；验证：session 往返和过期角色过滤单测通过（不超过 2 小时）
- [x] 4.2 实现逐请求授权复核 repository，一次查询获得用户/租户当前状态、版本和按数据库时间有效的角色（SEC-01/21）；验证：ACTIVE、DISABLED、LOCKED、租户停用、角色过期和数据库错误表驱动测试通过（不超过 2 小时）
- [x] 4.3 将所有控制面受保护路由接入当前授权复核，版本不匹配或查询异常时删除 session 并 fail closed（SEC-01/21）；验证：路由矩阵无旁路且故障注入返回未授权（不超过 2 小时）
- [x] 4.4 在用户禁用、角色授予、撤销和降权事务中原子递增 `authz_version`（SEC-01/21）；验证：事务回滚版本不变、提交后版本仅递增一次（不超过 2 小时）
- [x] 4.5 增加禁用、降权、角色自然过期后使用原 session 发起下一请求的集成测试（SEC-01/21）；验证：下一请求立即拒绝旧权限且无重新登录窗口（不超过 2 小时）

## 5. A2A PostgreSQL 权威状态机

- [x] 5.1 实现 A2A PostgreSQL repository 的创建、租户列表和租户限定读取，移除进程内 `_tasks` 权威状态（SEC-08）；验证：repository 集成测试和跨租户 404 测试通过（不超过 2 小时）
- [x] 5.2 为创建任务实现必填幂等键、规范化请求摘要和数据库唯一冲突处理（SEC-08/11）；验证：同键同请求返回原任务、同键异请求 409、不同应用同键互不冲突（不超过 2 小时）
- [x] 5.3 实现基于 tenant、task id、期望 state 和 version 的条件更新并单调递增 version（SEC-11）；验证：过期 version 返回 409且不覆盖当前状态（不超过 2 小时）
- [x] 5.4 将 A2A 创建、列表和读取路由切换到异步 PostgreSQL repository，并确保 SQL 自带租户条件（SEC-02/08）；验证：API 集成测试与 SQL mock/实库租户断言通过（不超过 2 小时）
- [x] 5.5 增加两个 Python Agent 实例共享 PostgreSQL 的创建后跨实例读取测试（SEC-08）；验证：第二实例读取到相同 task/version 且无内存同步（不超过 2 小时）
- [x] 5.6 增加 Python Agent 容器重启后的任务与 `input-required` 状态恢复测试（SEC-08）；验证：重启前后任务字段和 version 一致且测试无 skip（不超过 2 小时）

## 6. HITL 人类审批边界

- [x] 6.1 在 Go 控制面新增或收紧人类 session 保护的 A2A decision 入口，只允许当前有效 `PLATFORM_ADMIN`、`TENANT_ADMIN` 或 `APPROVER`（SEC-01/21）；验证：角色矩阵、匿名和过期角色测试通过（不超过 2 小时）
- [x] 6.2 让人类 decision 通过租户限定 PG CAS 写入真实 approver、理由和版本，批准仅进入 `working`、拒绝进入 `rejected`（SEC-01/08/11/21）；验证：状态和审计字段实库测试通过且批准不出现 `completed`（不超过 2 小时）
- [x] 6.3 移除 Python 应用凭证 decision 能力并对机器应用自批固定返回 403（SEC-01/02）；验证：拥有 `a2a.tasks:write` 的应用仍无法审批且状态不变（不超过 1 小时）
- [x] 6.4 增加同任务双 approver 并发 CAS 与跨租户人类审批测试（SEC-01/11/21）；验证：仅一个决策成功，另一请求 409；跨租户 404且不泄露存在性（不超过 2 小时）
- [x] 6.5 增加“批准后等待真实执行结果”的端到端测试（SEC-01/21）；验证：批准响应与后续读取均为 `working`，没有代码路径自动写 `completed`（不超过 1 小时）

## 7. 不可变构建输入

- [x] 7.1 将所有 Dockerfile `FROM` 固定为 `name:version@sha256:digest` 并校验目标架构可拉取（SEC-13/25）；验证：三类镜像在目标平台构建成功且静态检查无 tag-only 引用（不超过 2 小时）
- [x] 7.2 将 Compose 中 PostgreSQL、Valkey及三类应用生产镜像固定合法 digest，同时保留可追溯版本（SEC-13）；验证：`docker compose config` 展开后全部受管镜像含 digest（不超过 2 小时）
- [x] 7.3 将全部 GitHub Actions `uses:` 固定 40 位完整 SHA并为 workflow/job 声明最小 `permissions`（SEC-13）；验证：actionlint、YAML 解析和权限静态检查通过（不超过 2 小时）
- [x] 7.4 实现不可变引用门禁脚本，检查 Dockerfile、Compose、Actions 的缺失/类型/格式和禁用浮动引用路径（SEC-13/25）；验证：合法样例通过且 tag-only、短 SHA、非法 digest 样例均失败（不超过 2 小时）
- [x] 7.5 配置 Dependabot 的 Docker 与 GitHub Actions 更新入口并限制合理更新频率（SEC-13）；验证：配置语法检查通过且覆盖仓库实际 Dockerfile/Actions 路径（不超过 1 小时）

## 8. Guard、运行时与质量门禁

- [x] 8.1 将参数 guard 接入真实 Compose smoke，独立执行缺失、类型错误、范围越界和合法配置四路径；验证：前三条非零、合法路径为零且脚本汇总 `FAIL=0 SKIP=0`（不超过 2 小时）
- [x] 8.2 建立 M9 runtime tests 聚合入口，覆盖默认 seed、日志脱敏、生产 guard、Python 路由、凭证吊销、用户撤权、HITL 和 A2A 韧性（SEC-01/02/03/08/11/13/21/25）；验证：缺依赖或未执行用例计 FAIL，不产生 SKIP（不超过 2 小时）
- [x] 8.3 将不可变引用、runtime tests 和真实 Compose smoke 加入必经 CI/release jobs，并收紧 job 依赖使失败时不构建候选制品（SEC-13/25）；验证：workflow DAG 静态测试及失败注入测试通过（不超过 2 小时）
- [x] 8.4 执行 Python ruff、pytest 和 lock 校验，修复本变更引入问题；验证：全部 Python 门禁通过且 pytest 无 skip（不超过 2 小时）
- [x] 8.5 执行 Go gofmt、vet、build、race、覆盖率与 PostgreSQL 迁移集成测试；验证：全部 Go 门禁通过且集成测试无 skip（不超过 2 小时）
- [x] 8.6 执行 Console lint/typecheck/Vitest/build/Playwright、SDK 矩阵、i18n、安全扫描和版本一致性门禁；验证：所有既有候选发布门禁通过（不超过 2 小时）

## 9. 升级、回滚与交付验收

- [x] 9.1 对 V2.1 既有命名卷制作升级前最终 PostgreSQL 自定义格式备份并生成 SHA-256；验证：校验和复核成功且 `pg_restore -l` 可读取（不超过 1 小时）
- [x] 9.2 使用 V2.1 哨兵卷执行候选迁移，核对迁移账本连续以及租户、设备、用户、角色、开发者应用和工具包数据保留；验证：升级 SQL 断言全部通过（不超过 2 小时）
- [x] 9.3 基于固定 digest 执行三类应用镜像 Compose rebuild/up，确认默认未 seed、迁移只执行一次且所有服务 ready；验证：第二次 up 无重复 DDL且核心健康检查通过（不超过 2 小时）
- [x] 9.4 在升级后的真实 Compose 环境执行 M9 runtime tests、四路径 guard、主 smoke和专项 smoke；验证：最终汇总严格为 `FAIL=0 SKIP=0`（不超过 2 小时）
- [x] 9.5 演练保留 PostgreSQL/Valkey 卷并切回升级前不可变应用镜像的应用回滚；验证：不执行 down migration、不删除卷且回滚说明记录兼容结论（不超过 2 小时）
- [x] 9.6 执行 OpenSpec `openspec validate v2-2-runtime-resilience --strict` 和全量文档矛盾扫描；验证：严格校验通过且无 Redis、企业版闭源、默认 seed 或机器自批等残留冲突表述（不超过 1 小时）
- [x] 9.7 编写 V2.2 M9 验收报告，记录备份、迁移账本、既有卷数据、镜像 digest、runtime tests、全门禁、回滚与残余风险证据；验证：报告逐项映射六份 delta spec 且明确未创建 tag（不超过 2 小时）
- [ ] 9.8 检查 `git status`、完整 diff 和最近提交后仅暂存本变更文件，提交到 `dev` 并推送远端 `dev`；验证：远端提交 SHA 与本地一致且未创建或推送任何 tag（不超过 1 小时）
