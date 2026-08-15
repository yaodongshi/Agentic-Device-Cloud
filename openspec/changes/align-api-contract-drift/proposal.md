# 变更：契约漂移对齐（集成测试暴露）

## Why

B-12 集成契约测试（ce/integration）在真实 PG/Valkey 上发现三处实现与 design/33 契约的漂移，需要统一决策并对齐（实现或文档二选一）。

## What Changes

1. 直调（免审）tools/call 的 200 响应无 request_id/status 信封——决定：实现补充信封还是 design/33 接受裸结果
2. 无签名回调返回 400/10001 而非 design/33 的 401/12004——对齐错误码
3. 跨租户调用 fail-closed 为 500——design/33 要求 11001/10003 区分

## Capabilities

- **New Capabilities**: 无
- **Modified Capabilities**: 无（实现细节对齐，经测试固化后记录）

## Impact

- 修改：ce/internal/agentapi、ce/internal/approval（按决策对齐错误码/信封）
- 测试：ce/integration 相应断言更新
- 非目标：不动 design/33 编号体系（若对齐文档则本次仅更新文档措辞）
