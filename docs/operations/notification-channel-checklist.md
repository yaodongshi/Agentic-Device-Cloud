# ADC 通知通道接入演练清单（A1.2：企微 / 钉钉）

> 目标：企微/钉钉真实通道冒烟一次通过——审批卡片真实送达测试群（design/82 A1.2 验收口径）
> 通道实现：approval 服务 Notifier seam（ce/internal/approval/notifiers.go），企微 template_card 按钮交互卡片、钉钉 actionCard 卡片；推送 3 次重试（退避 1s/2s/4s）、单次 HTTP 超时 5 秒，并校验通道业务返回码
> 前置：ADC 试点环境已按 docs/operations/deployment-guide.md 安装，控制台可登录

## 1. 创建测试群机器人

### 1.1 企微（企业微信）

1. 用企业管理员创建内部测试群，群成员含审批演练人。
2. 群设置 → 群机器人 → 添加机器人，命名为"ADC 审批测试"。
3. 复制生成的 Webhook 地址（形如 `https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxxx`），保存到安全载体。

### 1.2 钉钉

1. 创建内部测试群，群成员含审批演练人。
2. 群设置 → 智能群助手 → 添加机器人 → 自定义机器人。
3. 安全设置选择"自定义关键词"并填入"ADC"（卡片标题含 ADC 字样，命中关键词）。
4. 复制 Webhook 地址（形如 `https://oapi.dingtalk.com/robot/send?access_token=xxxx`），保存到安全载体。

> 安全约束：Webhook 地址属敏感凭证（SEC-13），不入库、不入聊天记录、不入工单附件；仅以环境变量注入平台（第 2 节）。

## 2. Webhook 注入（compose 环境变量）

通道配置经环境变量 `ADC_WECOM_WEBHOOK` / `ADC_DINGTALK_WEBHOOK` 注入 adc 数据面容器（ce/internal/config/config.go），二者可同时配置（MultiNotifier 扇出）。

1. 编辑 deploy/compose.yaml，在 adc 服务 environment 块追加：

```yaml
      ADC_WECOM_WEBHOOK: ${ADC_WECOM_WEBHOOK:-}
      ADC_DINGTALK_WEBHOOK: ${ADC_DINGTALK_WEBHOOK:-}
```

2. 在 deploy/.env 中填入真实地址（.env 不入库，注意文件权限 600）：

```bash
ADC_WECOM_WEBHOOK=https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxxx
ADC_DINGTALK_WEBHOOK=
```

3. 重建容器并确认配置生效：

```bash
docker compose -f deploy/compose.yaml up -d adc
docker exec adc-app env | grep -E 'ADC_(WECOM|DINGTALK)_WEBHOOK'
```

第 3 步输出包含所配 webhook 即注入成功。

## 3. 触发测试工单（smoke 步骤）

高危工具调用即生成审批工单并触发卡片推送（调用返回 202 与 ticket_id，不等待审批）。

```bash
# 1. 运行全链路 smoke（第 4 步即高危调用 set_spindle_speed 触发 HITL）
scripts/dev-smoke.sh

# 2. 或单独触发一次高危调用并保留 ticket_id
curl -s -H "X-ADC-Key: dev-agent-key" -H "Content-Type: application/json" \
  -d '{"name":"cnc-demo-01::set_spindle_speed","arguments":{"rpm":3000}}' \
  'http://127.0.0.1:18080/v1/agent/mcp/tools/call'
```

预期：响应含 `ticket_id`；30 秒内测试群收到审批卡片。

## 4. 卡片送达验证

| # | 检查项 | 通过标准 |
|---|---|---|
| 1 | 卡片出现 | 触发后 30 秒内群内出现卡片 |
| 2 | 标题 | "ADC 高危设备操作审批" |
| 3 | 字段 | 含目标设备（cnc-demo-01）、触发工具（set_spindle_speed）、调用参数（rpm:3000）、风险等级（2（高危，须人工审批））、申请来源 |
| 4 | 按钮 | 企微含"核实并执行""拦截终止"两按钮；钉钉含同标题双按钮 |
| 5 | 按钮可达 | 点击"核实并执行"打开签名落地页（无需实际提交决策） |
| 6 | 落库 | smoke 第 10 步审计记录存在，工单可在控制台"审批工单"页查到 |

## 5. 失败排查表

| 现象 | 可能原因 | 处置 |
|---|---|---|
| 群内无卡片，容器日志报 notify rejected with HTTP 4xx/5xx | webhook 地址无效、机器人被删除或群解散 | 重新创建机器人，替换 .env 后重建容器 |
| 日志报 channel business error errcode=xxx（企微 93000 类） | 企微机器人被禁用或推送频率超限 | 检查机器人状态与推送频率限制（每分钟不超过 20 条），错峰重试 |
| 容器网络无法连接 webhook 域名 | 服务器出站网络不通、DNS 失败 | 服务器侧 `curl -sS -o /dev/null -w '%{http_code}' <webhook地址>` 验证；放行 443 出站 |
| 重试 3 次（日志 3 条 send 失败）后放弃 | 通道持续不可达（SEC-18 行为：重试耗尽后错误上抛，工单仍按超时收敛） | 修复通道后重新触发一次高危调用验证 |
| 配置未生效（env 输出为空） | 未加 environment 行或未重建容器 | 按第 2 节第 3 步重做并核对输出 |
| 钉钉报"keyword 不匹配" | 安全设置关键词与卡片标题不匹配 | 钉钉机器人关键词填"ADC"，或改 IP 白名单 |
| 卡片按钮打不开 | 落地页域名不可达或证书不受信 | 确认 ADC_PUBLIC_URL 配置与浏览器可达性 |

## 6. 演练记录

| 项 | 值 |
|---|---|
| 演练日期 / 操作人 | |
| 通道 | 企微 / 钉钉（可并列） |
| 测试群机器人名称 | |
| 触发工单 ticket_id | |
| 送达时长 | 秒（目标 ≤ 30） |
| 卡片验证结果 | 通过 / 不通过（附排查项） |

记录归档后本清单演练完成（A1.2 验收：审批卡片真实送达一次）。

## 7. 相关文档

- docs/operations/deployment-guide.md：环境安装与端口基线（A1.3）
- docs/operations/pilot-onboarding-sop.md：试点设备接入 SOP（A1.5）
- design/60 6.3：HITL 推送失败告警规则（SEC-18）
