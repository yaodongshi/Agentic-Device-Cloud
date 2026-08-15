# 能力：Python Agent 面（python-agent-plane）

## ADDED Requirements

### Requirement: 评测工具链（V1.0 最小集）
平台 MUST 提供 Python 评测工具链（EvalHarness），支持评测集管理、执行与指标报告，以 CLI 与 API 双入口提供。

#### Scenario: CLI 评测执行
- **WHEN** 用户以 CLI 运行评测工具链并指定评测集
- **THEN** 执行全部用例并输出结构化指标报告（通过率/失败清单）

#### Scenario: 评测 API 查询
- **WHEN** 客户端经网关 /v2/agents/evals/* 调用评测 API
- **THEN** 返回评测任务状态与结果

### Requirement: 项目隔离与依赖治理
Python Agent 面 MUST 使用项目内虚拟环境与 uv 锁定依赖，不得依赖全局 Python 环境。

#### Scenario: 可复现安装
- **WHEN** 在干净环境执行 uv sync（uv.lock 固定版本）
- **THEN** 依赖完整安装，测试与 CLI 可运行
