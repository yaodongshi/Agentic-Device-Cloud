# ADC 中英核心术语表

> 适用版本：v0.3.0 及以后。英文是设计源语言；冻结术语如需修改，必须由产品与中英文审校人共同批准。

| English | 中文 | 使用范围 | 状态 |
|---|---|---|---|
| Agent | Agent | 全局 | 冻结 |
| Device | 设备 | 全局 | 冻结 |
| Tool | 工具 | MCP、工具市场 | 冻结 |
| Tool Market | 工具市场 | 控制台、文档 | 冻结 |
| Approval | 审批 | HITL、工单 | 冻结 |
| Approver | 审批人 | HITL、RBAC | 冻结 |
| High-risk operation | 高危操作 | 设备写操作、审批 | 冻结 |
| Permission | 权限 | RBAC、凭证 scope | 冻结 |
| Credential | 凭证 | 设备、应用、API Key | 冻结 |
| Revoke | 吊销 | 凭证生命周期 | 冻结 |
| Tenant | 租户 | 全局 | 冻结 |
| Audit log | 审计日志 | 治理、合规 | 冻结 |
| Budget | 预算 | 计量与告警 | 冻结 |
| Legacy Bridge | Legacy Bridge | B 类 SDK 桥接设备 | 冻结 |
| Community Edition (CE) | 社区版（CE） | 产品版本 | 冻结 |
| Enterprise Edition (EE) | 企业版（EE） | 产品版本 | 冻结 |

## 门禁范围

`npm run i18n:test` 校验中英 key、值类型、命名占位符、空值，并检查凭证、权限、审批、高危、吊销、执行和下发等高风险术语。确有语境例外时，将具体 key 加入 `scripts/i18n-term-allowlist.json`，并在评审中说明原因；禁止使用通配符绕过检查。
