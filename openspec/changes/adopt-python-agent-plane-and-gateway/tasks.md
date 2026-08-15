# 任务分解（每项 ≤ 2 小时粒度）

## 1. core-sdk Python 协议包（前置）

- [x] 1.1 新建 core-sdk/python/ 包：JSON-RPC/MCP 协议类型（与 core-sdk/protocol Go 版同一契约，Pydantic 模型），pyproject.toml + uv 初始化，契约来源注释指向 design/33
- [x] 1.2 契约一致性：生成/手写 JSON Schema 样例，Go 与 Python 序列化结果对照验证（单测）

## 2. 统一 API 网关（Go，与数据面并行）

- [x] 2.1 ce/internal/gateway 骨架：路由表（前缀→后端服务映射，含 /v2/agents/* 预留）、httputil.ReverseProxy 封装、配置项（后端地址表）
- [x] 2.2 中间件链：鉴权头透传、trace_id 生成与注入、限流挂载（复用 pkg/ratelimit 接口，接口先行实现可空）、统一错误码 10001
- [x] 2.3 网关单测：路由命中/未命中、鉴权头透传、trace_id 贯通、限流头输出（httptest 起假后端）

## 3. Python Agent 面最小集（ce/py-agent）

- [x] 3.1 ce/py-agent 骨架：FastAPI 应用 + uv 工程（pyproject.toml、uv.lock、.venv 约定）、/healthz、依赖 core-sdk/python
- [x] 3.2 评测工具链核心：评测集模型（用例/输入/期望）、执行器、指标报告（通过率/失败清单），CLI 入口（python -m evals）
- [x] 3.3 评测 API：/v2/agents/evals/* 端点（经网关路由预留），Pydantic 请求/响应契约
- [x] 3.4 pytest 单测（评测执行/报告生成/输入校验）+ ruff/mypy 门禁通过

## 4. 集成与验证

- [x] 4.1 网关→py-agent 联通：本地起 uvicorn + 网关转发验证（curl 经网关调 /v2/agents/evals/*）
- [x] 4.2 前端无感验证：设计/33 契约样例经网关全链路跑通（mock 前端脚本）
- [x] 4.3 openspec validate 通过 + 本变更验收记录

## 依赖与顺序

1（协议包）→ 2/3（网关与 Python 面可并行）→ 4（集成验证）。2.2 依赖 2.1；3.x 依赖 1.x。
