## ADDED Requirements

### Requirement: 用户授权变更必须推进授权版本
系统 MUST 为每个用户维护非空且单调递增的 `authz_version`，并在禁用、角色授予、撤销或降权等授权变更事务中推进版本。

#### Scenario: 管理员降权用户
- **WHEN** 管理员撤销或替换用户角色
- **THEN** 角色变更与 `authz_version` 递增在同一事务中提交

#### Scenario: 管理员禁用用户
- **WHEN** 管理员将用户状态改为非 ACTIVE
- **THEN** 状态变更与 `authz_version` 递增在同一事务中提交

### Requirement: 受保护请求必须复核用户当前授权
系统 MUST 在每个受保护请求上按 session 中的 user id 和 tenant id 查询用户当前状态、租户状态、`authz_version` 与有效角色；任一查询失败、状态异常、版本不匹配或授权数据矛盾均须 fail closed。

#### Scenario: 禁用用户发起下一请求
- **WHEN** 已登录用户被禁用后使用原 session 发起下一受保护请求
- **THEN** 系统拒绝请求并使当前 session 失效

#### Scenario: 用户降权后发起下一请求
- **WHEN** 已登录用户的高权限角色被撤销后使用原 session 发起下一请求
- **THEN** 系统不得沿用 session 中旧角色并拒绝已失去权限的操作

#### Scenario: 授权数据库不可用
- **WHEN** 系统无法读取用户当前状态、版本或角色
- **THEN** 请求被拒绝且不得回退到 session 内角色

#### Scenario: session 授权版本匹配
- **WHEN** 用户、租户均有效且 session 版本等于当前版本
- **THEN** 系统仅使用数据库当前有效角色执行授权判定

### Requirement: 过期角色必须在下一请求过滤
系统 MUST 仅将 `expires_at` 为空或晚于数据库当前时间的角色视为有效，不得因 session 已签发而延长角色有效期。

#### Scenario: 临时审批角色到期
- **WHEN** 用户临时 `APPROVER` 角色到期后使用原 session 发起审批请求
- **THEN** 下一请求不包含该角色并拒绝审批

#### Scenario: 用户仍有其他有效角色
- **WHEN** 一个角色过期但用户仍有其他未过期角色
- **THEN** 系统只按剩余有效角色授权
