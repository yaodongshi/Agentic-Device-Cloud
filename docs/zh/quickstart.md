# 快速开始

> 用 Docker Compose 在本地跑起完整的 ADC 栈，然后验证完整链路：设备隧道、工具聚合、HITL 审批、执行与审计。全程约 5 分钟。

## 前置条件

| 要求 | 版本 | 说明 |
| --- | --- | --- |
| Docker Engine | 24+ | 需带 Compose v2 插件（`docker compose`） |
| Go 工具链 | 1.24+ | 仅 smoke 测试需要（mock 设备经 `go run` 启动） |
| curl | 任意 | 用于健康检查 |
| 空闲端口 | 18080、18082 | 18080 为网关入口；18082 仅 smoke 脚本内部使用 |

栈共五个容器：`gateway`、`adc`（Go 数据面）、`py-agent`（Python Agent 面）、`postgres`（16）、`valkey`（8）。数据全部落在专属 Compose 卷（`adc_pg_data`、`adc_valkey_data`）与专属网络中，与本机其他栈互不干扰。

## 第一步：克隆并启动

```bash
git clone https://github.com/yaodongshi/Agentic-Device-Cloud.git
cd Agentic-Device-Cloud
docker compose -f deploy/compose.yaml up -d --build
```

首次运行需构建 Go 与 Python 镜像，耗时数分钟；之后启动只需几秒。

确认五个容器全部健康：

```bash
docker compose -f deploy/compose.yaml ps
```

## 第二步：验证网关

统一 API 网关是所有客户端流量的唯一入口，其健康端点聚合全栈状态：

```bash
curl http://localhost:18080/healthz
```

预期输出：

```json
{"status":"ok"}
```

也可在浏览器打开 `http://localhost:18080/`。

## 第三步：运行 smoke 测试

smoke 脚本经网关驱动完整业务链路：健康检查、mock 设备 WSS 隧道接入、工具聚合、触发 HITL 审批的高危工具调用、审批回调、执行与审计日志核验。

```bash
bash scripts/dev-smoke.sh
```

预期输出以 PASS/FAIL 汇总收尾，健康栈全部通过（10/10）。

```text
--- 1. 网关健康检查
  PASS: healthz
--- 2. mock 设备接入隧道
  PASS: mock 设备在线（经网关 WSS 透传）
...
  PASS: 审计日志可查
SUMMARY: PASS=10 FAIL=0
```

## 栈内服务

| 服务 | 容器 | 职责 | 端口 |
| --- | --- | --- | --- |
| gateway | `adc-gateway` | 统一 API 网关，唯一对外入口 | 18080 |
| adc | `adc-app` | Go 数据面：设备连接器、Agent API、审批、审计 | 内部 |
| py-agent | `adc-pyagent` | Python Agent 面（FastAPI，V1.0 交付评测工具链） | 内部 |
| postgres | `adc-postgres` | 状态与审计主存（PostgreSQL 16） | 内部 |
| valkey | `adc-valkey` | 路由索引、审批唤醒、审计缓冲（Valkey 8） | 内部 |

开发期默认凭证（`adc` / `adc_dev_only`）仅限本地开发。生产使用前务必经环境变量覆盖（`ADC_PG_PASSWORD`、`ADC_VALKEY_PASSWORD` 等）。

## 停止与清理

```bash
docker compose -f deploy/compose.yaml down            # 停止，保留数据卷
docker compose -f deploy/compose.yaml down -v         # 停止并删除数据卷
```

## 常见问题

| 现象 | 原因 | 解决办法 |
| --- | --- | --- |
| `bind: address already in use` | 18080 或 18082 被占用 | 释放端口，或修改 `deploy/compose.yaml` 的端口映射 |
| smoke 在"mock 设备接入隧道"失败 | mock 设备经网关连不上隧道 | 等待全部容器健康后重跑 |
| smoke 报"无法获取设备密钥" | `adc` 容器尚未完成 seed | 等待 `adc-app` 健康（`docker compose ps`） |
| 镜像构建失败 | Docker 缓存或工具链不匹配 | 执行 `docker compose -f deploy/compose.yaml build --no-cache` |
| smoke 报 `go: command not found` | 缺少 Go 工具链 | 安装 Go 1.24+（仅 mock 设备需要） |

## 下一步

- [架构](/zh/architecture) - 网关、数据面、Agent 面与边缘 SDK 如何协作
- [API 参考](/zh/api-reference) - 端点和错误码全集
- [设备 SDK](/zh/device-sdk) - 用 Rust SDK 注册自己的设备工具
- [开源](/zh/open-source) - 许可证、贡献流程与社区规则
