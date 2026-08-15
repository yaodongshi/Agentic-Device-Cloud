# ADC 备份恢复演练清单（design/80 A-08，design/60 第 8 章，design/32 8.2）

> 目标：RPO ≤ 5 分钟、RTO ≤ 30 分钟（NFR-001）。不演练的备份不是备份。
> 频率：每季度一次 PG 数据卷损坏恢复演练；每年一次全量容灾演练（等保"应急预案与演练"证据）。
> 每轮演练留书面记录（操作人、各步骤耗时、偏差、整改项），存 `adc_audit_logs`（admin_op 事件），形成"备份-演练-留痕"闭环。

## 1. 前置条件

- [ ] `deploy/backup/backup.sh` 已接入 cron（每日 02:00），最近一次执行退出码 0
- [ ] `deploy/backup/backup.sh wal-status` 通过：最新 WAL 段年龄 ≤ 6 分钟（archive_timeout=300 的 RPO 门）
- [ ] 最近全量 dump 存在且 `.sha256` 校验通过（`backup.sh verify`）
- [ ] 演练机与生产同架构（amd64/arm64），已安装 Docker Compose（design/60 1.2）
- [ ] 备份卷 `adc_backup` 已复制到演练机（`docker run --rm -v adc_backup:/backup -v "$PWD":/out alpine tar czf /out/adc-backup.tar.gz -C /backup .`）

## 2. RPO 验证（目标 ≤ 5 分钟）

| 步骤 | 操作 | 预期 | 实测耗时 |
|---|---|---|---|
| RPO-1 | 生产库写入标记：`INSERT INTO adc_tenants` 插入演练哨兵租户 `drill-rpo-<日期>`，记录写入时刻 T0 | 写入成功 | |
| RPO-2 | 等待 5-6 分钟（跨一个 archive_timeout 周期），观察 `/backup/wal` 出现新 WAL 段 | 新段文件名时间戳 ≥ T0 | |
| RPO-3 | 用演练机按第 4 节恢复，查询哨兵租户是否存在 | 存在：实测 RPO = 恢复库数据截止点 - T0，必须 ≤ 5 分钟 | |

RPO 判读口径：恢复库中出现哨兵数据的时间差 = 实际数据丢失窗口；若哨兵数据缺失，检查 `archive_timeout`、`archive_command` 返回码与 `/backup/wal` 目录权限（`backup.sh init-backup-dir` 修复）。

## 3. RTO 验证（目标 ≤ 30 分钟，计时自"故障宣告"起）

| 步骤 | 操作 | 目标耗时 | 实测 |
|---|---|---|---|
| RTO-1 | 故障注入：演练机 `docker volume rm` 空卷，或删除 `adc_pg_data` 卷内文件模拟损坏 | — | |
| RTO-2 | `docker compose -f deploy/compose.yaml --profile ee up -d postgres`（空数据卷，仅拉起 PG） | ≤ 2 分钟 | |
| RTO-3 | `RESTORE_DB=adc deploy/backup/restore.sh <最近dump>` 执行恢复（含 sha256 门禁与 pg_restore） | ≤ 15 分钟（1000 设备规模假设基线） | |
| RTO-4 | 冒烟清单（第 5 节）全过 | ≤ 10 分钟 | |
| RTO-5 | 拉起全部服务：`docker compose -f deploy/compose.yaml --profile ee up -d`，设备自动重连（SEC-15 退避） | ≤ 3 分钟 | |
| | **合计** | **≤ 30 分钟** | |

RTO 判读口径：自故障宣告到冒烟清单最后一项通过的总墙钟时间。超时优先排查 pg_restore 并行度（`-j 2` 可调）与磁盘吞吐；无状态服务（gateway/approval/admin/py-agent）恢复即拉起，不在恢复链内耗时。

## 4. 恢复比对方法（演练必须比对，不能只看恢复成功）

1. 行数比对：恢复完成后 `restore.sh` 输出四表计数（adc_tenants / adc_devices / adc_audit_logs / adc_usage_events），与生产库同查询结果 diff，允许差值 = 演练窗口内新增数据（RPO 窗口内），超出即失败。
2. 抽样比对：随机抽取 3 个租户，比对 `adc_audit_logs` 行数与 `adc_usage_events` 聚合值（design/32 8.2 口径）。
3. 应用层比对：用恢复库启动应用，冒烟清单第 5 节逐项验证（工具列表、审批链路、审计可查）。
4. 归档完整性：`ls /backup/wal | wc -l` 与生产侧一致（含演练窗口内新段）。

## 5. 冒烟清单（恢复后必过，design/60 9.1 子集）

- [ ] gateway/adc/approval/py-agent 全部 healthy（`docker compose ps`）
- [ ] mock 设备注册 → 30 秒内工具对 Agent 可见（FR-003）
- [ ] 一次免审工具调用成功，审计日志可查（FR-013）
- [ ] 一次高危调用进入 HITL → 审批通过 → 设备执行（FR-005）
- [ ] 审批推送失败重试与降级链路可触发（SEC-18）
- [ ] 设备在线数曲线在 Grafana ADC Overview 恢复展示（A-07 联动）

## 6. 时间点恢复（PITR，可选进阶）

全量 dump 恢复只能回到备份点；要恢复到任意时间点（利用 WAL 连续归档），在演练机执行：

```bash
# 1) 用 dump 恢复基线库 adc_pitr，恢复完成后立即停止
# 2) 以 WAL 归档回放至目标时间：
#    PG 数据目录下建 recovery.signal（PG16 无 recovery.conf）
#    postgresql.conf 追加：
#      restore_command = 'cp /backup/wal/%f %p'
#      recovery_target_time = '2026-08-15 10:31:05+08'  # 目标时刻，可选
# 3) 启动 postgres，回放完成自动生成 standby.signal 删除并进入读写
# 4) 按第 4 节比对；PITR 演练每半年一次即可
```

## 7. 演练记录模板

| 项 | 值 |
|---|---|
| 演练编号 / 日期 | drill-2026Q3-01 / |
| 操作人 / 复核人 | |
| 最近备份与 WAL 状态 | dump= / wal 最新段= |
| 实测 RPO | 秒（目标 ≤ 300） |
| 实测 RTO | 分钟（目标 ≤ 30，分段耗时见第 3 节） |
| 比对结果 | 行数 diff= / 抽样 3 租户= |
| 偏差与整改项 | |
