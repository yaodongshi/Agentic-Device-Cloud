## MODIFIED Requirements

### Requirement: 候选发布必须通过统一源码门禁
候选发布 MUST 通过 Go race/覆盖率、Python、Console、SDK 矩阵、迁移、i18n、安全扫描、不可变构建输入检查、运行时测试和真实 Compose 冒烟；必需运行时门禁汇总 MUST 为 `FAIL=0 SKIP=0`。

#### Scenario: 任一门禁失败
- **WHEN** 候选提交的任一必需检查失败、未执行或产生 skip
- **THEN** CI 不得生成可发布制品

#### Scenario: 全部门禁通过
- **WHEN** 全部源码、供应链、迁移、运行时和 Compose 检查成功且汇总为 `FAIL=0 SKIP=0`
- **THEN** CI 才可继续生成候选制品

### Requirement: 三类应用镜像必须版本一致且不可变
发布流程 SHALL 构建 ADC、Python Agent 和 Console 的版本化多架构镜像，生成 SBOM 并记录 digest；Dockerfile 基础镜像、Compose 生产镜像和 GitHub Actions MUST 使用不可变引用，正式部署不得现场重新构建。

#### Scenario: Compose 使用候选版本
- **WHEN** 操作员设置 `ADC_VERSION` 启动候选 Compose
- **THEN** 三类应用镜像使用同一版本标识、固定合法 digest且可追溯到发布 digest

#### Scenario: 构建输入存在浮动引用
- **WHEN** Dockerfile、生产 Compose 或 GitHub Actions 中存在 tag-only、分支、短 SHA 或缺失 digest 的受管引用
- **THEN** 候选发布门禁失败

### Requirement: 候选升级必须覆盖既有卷和 V2.0 专项链路
发布门禁 MUST 验证空库安装、既有卷升级、幂等重建、主冒烟以及 OIDC、工具市场、预算、参数 guard 和运行时权限专项冒烟。参数 guard MUST 在真实 Compose 拓扑独立覆盖缺失、类型错误、范围越界和合法配置四条路径，且全部测试不得 skip。

#### Scenario: 既有卷候选升级
- **WHEN** 使用 V2.0 或 V2.1 哨兵数据卷升级到候选版本
- **THEN** 数据保留、迁移成功且全部自动化冒烟通过

#### Scenario: Guard 四路径验证
- **WHEN** 候选流水线在真实 Compose 拓扑执行参数 guard
- **THEN** 缺失、类型错误、范围越界三条路径均以非零状态阻断，合法路径成功，汇总为 `FAIL=0 SKIP=0`
