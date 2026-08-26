## ADDED Requirements

### Requirement: 部署必须自动执行版本化迁移
系统 SHALL 在业务服务启动前按版本顺序执行未应用迁移，并将成功版本记录在数据库中。

#### Scenario: 空库首次安装
- **WHEN** Compose 在空 PostgreSQL 数据库启动
- **THEN** 迁移服务自动到达目标版本后业务服务才进入健康状态

### Requirement: 并发迁移必须互斥且幂等
迁移器 MUST 使用数据库级锁确保多实例仅有一个执行者，已应用版本重复执行不得修改业务数据。

#### Scenario: 两个迁移器同时启动
- **WHEN** 两个实例同时检测到相同待执行迁移
- **THEN** 仅一个实例执行 DDL，最终版本记录唯一且数据完整

### Requirement: 不兼容版本必须阻断启动
迁移失败或数据库版本高于应用支持版本时，系统 MUST fail closed，业务服务不得对外提供流量。

#### Scenario: 迁移 SQL 失败
- **WHEN** 任一迁移事务执行失败
- **THEN** 失败迁移不记录成功版本且依赖该迁移的应用不启动

### Requirement: 既有卷升级必须保留数据
系统 SHALL 支持从 V2.0 既有 schema 向前迁移并保留租户、用户、设备、审批、审计、计量和工具包数据。

#### Scenario: V2.0 数据卷升级
- **WHEN** 对包含哨兵数据的既有卷执行 V2.1 Compose 重建
- **THEN** 迁移达到目标版本且升级前关键数据仍可查询
