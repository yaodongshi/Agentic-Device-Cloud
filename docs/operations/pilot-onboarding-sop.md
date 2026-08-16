# ADC 试点客户接入 SOP（A1.5）

> 适用：种子工厂试点设备接入（B 类 SDK 桥接设备，WSS 反向隧道）
> 依据基线：design/82 A1.5、doc/07 设备接入方案、design/60 网络端口、core-sdk/rust 边缘 SDK
> 范围：设备注册、凭证下发、网络放行、边缘 SDK 部署、上线自检；原生 MCP 设备（A 类）接入属 V1.5（FR-021），不适用本文
> 角色：客户 IT 管理员（控制台操作）、ADC 实施工程师（凭证交付与验证）

## 1. 设备注册（控制台操作步骤）

由客户租户管理员在 ADC 控制台完成，每台设备注册一次。

1. 打开控制台：浏览器访问 `http://<ADC 控制台地址>:18080`。
2. 使用租户管理员账号登录（试点租户初始账号 tenant-admin，登录后立即修改口令）。
3. 进入左侧菜单"设备管理"，点击"新增设备"。
4. 填写设备表单（字段与后端校验一致，design/33 3.1.6）：

| 字段 | 必填 | 规则 |
|---|---|---|
| 设备码 device_code | 是 | 1-128 字符，仅字母、数字、短横线、下划线（如 cnc-plant3-01） |
| 设备名称 name | 是 | 最长 255 字符，便于台账辨认（如"三车间 2 号 CNC"） |
| 设备类型 device_type | 是 | 最长 64 字符（如 cnc / robot / plc） |
| 鉴权方式 auth_type | 是 | token / hmac / mtls，试点默认 hmac |
| 分组 | 否 | V1.0 仅存单一 group_id |
| 元数据 metadata | 否 | 自由键值（如站点、产线、责任人） |

5. 点击"注册"。注册响应在界面一次性显示设备凭证（credential.secret），只显示这一次。
6. 复制凭证内容到安全载体（见第 2 节），关闭页面后无法再次查看。

API 方式（批量或脚本化时使用，等价流程）：

```bash
curl -s -H "Authorization: Bearer <tenant-admin-token>" \
  -H "Content-Type: application/json" \
  -d '{"device_code":"cnc-plant3-01","name":"三车间 2 号 CNC","device_type":"cnc","auth_type":"hmac"}' \
  'http://127.0.0.1:18080/v1/admin/devices'
```

注册结果核对：控制台设备台账出现新设备，状态为 offline（待设备上线后转 online）。

## 2. 凭证下发（一次性明文交付规范）

凭证为一次性明文：平台只存哈希/加密态（SEC-13/25），明文仅注册响应中出现一次。

**交付规则**

1. 只交付一次：明文凭证不得重复抄写、转发、截图留存于聊天记录。
2. 交付通道按优先级：当面交付（打印纸质信封，密封）→ 企业加密邮件/内网加密盘 → 客户密码管理工具（Vault 类）。
3. 交付后双方签《凭证接收确认单》：设备码、交付时间、接收人、载体编号。
4. 设备侧写入后即销毁明文：SDK 配置完成后删除临时文件与命令行历史。
5. 遗失/泄露处置：立即在控制台吊销该设备凭证（PATCH /v1/admin/devices/{deviceID}，op=rotate），重新注册换发新凭证，旧凭证随即失效（SEC-03）。
6. 禁止事项：凭证写入代码仓库、放入工单附件、微信/钉钉明文传输。

## 3. 网络放行清单

设备出站方向仅需一条长连接通路（反向隧道，设备主动连出，客户无需开放入站端口）。

| # | 方向 | 协议 | 目的地 | 端口 | 说明 |
|---|---|---|---|---|---|
| 1 | 设备 → ADC 网关 | WSS | ADC 网关域名（如 tunnel.<客户域名> 或网关公网 IP） | 443 | 唯一必需通路；试点若未上 TLS 为 ws 协议加网关端口 18080 |
| 2 | 设备 → 时间源 | NTP | 客户内网 NTP 或网关所在域 | 123 | 时间同步（HMAC 时间窗依赖正确时钟） |
| 3 | 设备 → DNS | UDP/TCP | 客户内网 DNS | 53 | 网关域名解析 |

**防火墙放行示例（设备侧出口，按需二选一）**

```bash
# ufw（Ubuntu 设备）
sudo ufw allow out 443/tcp
sudo ufw allow out 123/udp

# 白名单式（仅放行网关，更严格，推荐）
sudo ufw allow out to <网关IP> port 443 proto tcp
```

**验证放行**

```bash
# 从设备侧验证 WSS 端口可达（TLS 握手成功即放行，无需业务报文）
nc -vz -w 5 <网关IP或域名> 443
# 或
openssl s_client -connect <网关IP或域名>:443 </dev/null 2>&1 | head -5
```

设备无需任何入站端口；拒绝客户侧任何"平台主动连设备"的放行要求（架构上即出站隧道，design/60 3.2）。

## 4. 边缘 SDK 部署（Rust SDK 快速步骤）

SDK：core-sdk/rust（crate 名 adc-edge-sdk，Apache-2.0），内置两个参考设备程序 echo_device 与 modbus_device。

**快速步骤（在设备上）**

```bash
# 1. 前置：Rust 工具链（rustup 安装 stable 即可）
rustc --version && cargo --version

# 2. 取 SDK 并运行参考设备（echo_device 注册 get_status 与 set_speed 两工具）
git clone '<core-sdk 仓库地址>' && cd core-sdk/rust

# 3. 写入环境变量（凭证见第 2 节交付内容，写入后销毁明文）
export ADC_TUNNEL_URL='wss://<网关域名>:443/v1/devices/tunnel'
export ADC_DEVICE_CODE='cnc-plant3-01'
export ADC_DEVICE_SECRET='<一次性凭证>'

# 4. 前台运行验证
cargo run --bin echo_device
```

**环境变量表**

| 变量 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| ADC_TUNNEL_URL | 是 | ws://127.0.0.1:18080/v1/devices/tunnel | 隧道端点；生产用 wss + 443 |
| ADC_DEVICE_CODE | 是 | cnc-demo-01 | 与控制台注册的设备码一致 |
| ADC_DEVICE_SECRET | 是 | 无 | 注册响应的一次性凭证，禁止写日志与代码 |

**接入自有设备程序**：依赖 `adc-edge-sdk`，用 `ClientConfig`/`DeviceClient` 连接隧道、`ToolRegistry` 注册工具（参考 echo_device.rs 与 modbus_device.rs；modbus_device 演示 read_register 风险 0 / write_register 风险 2 的 HITL 拦截）。

**运行要点**

- 程序应配置为开机自启常驻（systemd 单元或客户既有进程管理），断线由 SDK 指数退避自动重连（1 秒起步、上限 60 秒）。
- 凭证经环境变量或受保护的配置文件注入，不得出现在进程命令行参数（SEC-13）。
- 工具风险等级以平台配置为准，SDK 上报仅作初始值（RISK-006）。

## 5. 验证 checklist（上线自检 10 条）

逐条执行并记录，全部通过后设备计为试点在线设备。

| # | 检查项 | 操作 | 通过标准 |
|---|---|---|---|
| 1 | 隧道建立 | 查看 SDK 程序输出 | 出现 tunnel online 事件 |
| 2 | 台账在线 | 控制台设备管理页 | 设备状态为 online |
| 3 | 工具同步 | 控制台查看设备工具或 Agent tools/list | 30 秒内看到"设备码__工具名"聚合工具 |
| 4 | 免审调用 | 经 Agent 调用风险等级 0 工具（如 get_status） | 无审批工单直接返回执行结果 |
| 5 | 高危拦截 | 经 Agent 调用风险等级 2 工具（如 set_speed） | 返回 202 与 ticket_id，指令未下发 |
| 6 | 审批链路 | 审批人群机器人点"核实并执行" | 指令下发、设备执行、Agent 收到结果 |
| 7 | 拒绝链路 | 再次高危调用后点"拦截终止" | Agent 收到 BLOCKED_BY_HITL，设备未执行 |
| 8 | 断线重连 | 拔网线 90 秒后恢复 | 设备自动重连，台账恢复 online，工具重新可见 |
| 9 | 审计留痕 | 控制台审计查询按设备码过滤 | 上述调用与审批记录完整（含审批人工号） |
| 10 | 凭证清理 | 检查设备进程与文件 | 明文凭证已销毁，进程参数无密钥 |

自检异常处理：第 2/3 条失败查网络放行（第 3 节）与 ADC_DEVICE_CODE 一致性；第 5/6/7 条失败查通知通道（docs/operations/notification-channel-checklist.md）与审批策略；第 8 条失败查 SDK 重连配置。全部通过后填写接入记录表归档，进入周报统计范围（docs/operations/pilot-weekly-report-template.md）。

## 6. 相关文档

- docs/operations/deployment-guide.md：平台侧部署与端口基线（A1.3）
- docs/operations/notification-channel-checklist.md：审批通道演练（A1.2）
- design/60：网络与 TLS 基线；doc/07：设备接入方案
