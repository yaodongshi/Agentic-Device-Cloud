# 候选发布门禁

## Purpose

定义候选版本进入正式发布前必须满足的统一质量门禁，覆盖源码检查、多架构镜像一致性与不可变性、SBOM 和 digest 追溯、空库及既有卷升级验证，以及保留数据卷的应用回滚流程。

## Requirements

### Requirement: 候选发布必须通过统一源码门禁
候选发布 MUST 通过 Go race/覆盖率、Python、Console、SDK 矩阵、迁移、i18n、安全扫描和 Compose 冒烟。

#### Scenario: 任一门禁失败
- **WHEN** 候选提交的任一必需检查失败
- **THEN** CI 不得生成可发布制品

### Requirement: 三类应用镜像必须版本一致且不可变
发布流程 SHALL 构建 ADC、Python Agent 和 Console 的版本化多架构镜像，生成 SBOM 并记录 digest；正式部署不得现场重新构建。

#### Scenario: Compose 使用候选版本
- **WHEN** 操作员设置 `ADC_VERSION` 启动候选 Compose
- **THEN** 三类应用镜像使用同一版本标识且可追溯到发布 digest

### Requirement: 候选升级必须覆盖既有卷和 V2.0 专项链路
发布门禁 MUST 验证空库安装、既有卷升级、幂等重建、主冒烟以及 OIDC、工具市场、预算和参数校验专项冒烟。

#### Scenario: 既有卷候选升级
- **WHEN** 使用 V2.0 哨兵数据卷升级到候选版本
- **THEN** 数据保留、迁移成功且全部自动化冒烟通过

### Requirement: 回滚必须保留数据卷
发布说明 MUST 定义使用升级前不可变镜像回滚应用且不执行 down migration；数据恢复仅作为损坏兜底。

#### Scenario: 应用候选回滚
- **WHEN** 专项冒烟失败触发回滚
- **THEN** 操作员切回旧镜像、不删除命名卷，并能重新通过主链路冒烟
