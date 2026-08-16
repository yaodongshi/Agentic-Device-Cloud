# ADC 试点部署手册（A1.3）

> 适用版本：ADC V1.0 试点环境（单机 Docker Compose 形态）
> 依据基线：design/60 部署与交付方案、design/82 A1.3、deploy/compose.yaml、deploy/backup/、scripts/dev-smoke.sh
> 验收口径：新环境按本手册操作，30 分钟内完成上线自检（design/82 A1.3）
> 本文所有命令默认在仓库根目录执行；`docker compose` 一律带 `-f deploy/compose.yaml`

## 0. 环境与账号速查

| 项 | 值 | 说明 |
|---|---|---|
| 控制台地址 | http://127.0.0.1:18080 | console 容器对外端口，业务入口 |
| 业务统一入口 | http://127.0.0.1:18080 | 前端与 Agent API 均经此入口到网关 |
| 网关指标 | http://127.0.0.1:19091/metrics | 仅回环，供 Prometheus 采集 |
| Prometheus | http://127.0.0.1:19090 | observability profile |
| Grafana | http://127.0.0.1:19300 | 默认账号 admin |
| 平台管理员 | admin / admin123! | seed 生成（PLATFORM_ADMIN） |
| 租户管理员 | tenant-admin / tenant123! | seed 生成（TENANT_ADMIN，租户 tenant-demo） |
| 审批人 | approver / approver123! | seed 生成（APPROVER） |
| 演示 Agent Key | dev-agent-key | smoke 脚本使用 |

> 安全提示：seed 账号为演示口令，试点正式启用前必须在控制台修改全部默认口令。

## 1. 安装（单机 Compose）

### 1.1 前置检查清单

逐项执行并记录结果，任一不满足即停止安装并反馈。

| # | 检查项 | 命令 | 通过标准 |
|---|---|---|---|
| 1 | 操作系统 | `uname -m` | x86_64 或 aarch64（与镜像架构一致） |
| 2 | Docker 可用 | `docker version` | Docker CE 24+ 或 Docker Desktop 24+，服务运行中 |
| 3 | Compose 插件 | `docker compose version` | 输出版本号 |
| 4 | 内存 | `free -h` | 可用内存不低于 8GB |
| 5 | 磁盘 | `df -h /` | 可用空间不低于 40GB |
| 6 | 端口占用 | `ss -tlnp \| grep -E ':(18080\|18082\|19090\|19091\|19300\|19310)\b'` | 无输出（五端口均空闲） |
| 7 | 时间同步 | `date '+%F %T %z'` | 时区正确（Asia/Shanghai），时间与权威源一致 |
| 8 | 网络 | `curl -sf https://registry-1.docker.io/v2/ -o /dev/null && echo ok` | 输出 ok（可访问镜像仓库，仅首次拉取需要） |

### 1.2 端口与环境变量

**端口清单（deploy/compose.yaml）**

| 服务 | 容器名 | 宿主端口 | 用途 |
|---|---|---|---|
| console | adc-console | 18080 | 控制台与业务统一入口（对宿主开放） |
| adc（数据面） | adc-app | 127.0.0.1:18082 | 业务接口（仅回环，smoke 决策注入用） |
| gateway 指标 | adc-gateway | 127.0.0.1:19091 | /metrics（仅回环） |
| prometheus | adc-prometheus | 127.0.0.1:19090 | observability profile |
| grafana | adc-grafana | 127.0.0.1:19300 | observability profile |
| loki | adc-loki | 127.0.0.1:19310 | observability profile |
| postgres / valkey / py-agent | adc-postgres / adc-valkey / adc-py-agent | 无 | 仅内网 adc-net，零宿主暴露 |

**环境变量表（复制 deploy/.env.example 为 deploy/.env 后修改）**

| 变量 | 默认值 | 说明 |
|---|---|---|
| ADC_PG_USER / ADC_PG_PASSWORD / ADC_PG_DBNAME | adc / adc_dev_only / adc | PostgreSQL 连接（试点必须改口令） |
| ADC_VALKEY_USERNAME / ADC_VALKEY_PASSWORD | adc / adc_dev_only | Valkey ACL 用户，必须与 deploy/valkey-acl.conf 同步修改 |
| ADC_DEVICE_KEK | 开发固定值 | 设备凭证加密密钥（32 字节 hex，试点必须更换） |
| ADC_TLS_ENABLE / ADC_TLS_CERT / ADC_TLS_KEY | false / 空 / 空 | TLS 终结开关与证书路径（生产按 design/60 7.1 注入） |
| ADC_WECOM_WEBHOOK / ADC_DINGTALK_WEBHOOK | 空 | 审批卡片 webhook（A1.2 演练注入） |
| GF_ADMIN_USER / GF_ADMIN_PASSWORD | admin / adc_dev_only | Grafana 管理员（observability profile） |

### 1.3 首次启动

```bash
# 1. 生成环境变量文件并修改密码与 KEK
cp deploy/.env.example deploy/.env
vi deploy/.env

# 2. 构建并启动全部服务（adc 容器带 -seed 自动初始化数据）
docker compose -f deploy/compose.yaml up -d --build

# 3. 等待健康（约 1-2 分钟，首次需拉取基础镜像）
docker compose -f deploy/compose.yaml ps

# 4. 初始化备份卷目录属主（一次性，必做，否则 WAL 归档失败）
deploy/backup/backup.sh init-backup-dir

# 5. 可选：启动可观测组件（Grafana/Prometheus/Loki）
docker compose -f deploy/compose.yaml --profile observability up -d
```

### 1.4 seed 说明

- 启动命令中 adc 服务带 `-seed` 参数：幂等执行，重复启动不会覆盖已有业务数据，仅补齐演示账号与角色。
- seed 写入内容：租户 tenant-demo、平台管理员 admin、租户管理员 tenant-admin、审批人 approver、演示 Agent Key（dev-agent-key）与演示设备。
- seed 的演示口令仅用于首次登录验证，试点启用前全部修改。

### 1.5 上线自检（30 分钟口径）

```bash
# 1. 健康检查：全部服务 healthy
curl -sf http://127.0.0.1:18080/healthz && echo ok

# 2. 全链路冒烟（10 步，PASS 计数输出）
scripts/dev-smoke.sh

# 3. 备份链路可用（WAL 归档新鲜）
deploy/backup/backup.sh wal-status

# 4. 控制台登录验证：浏览器打开 http://127.0.0.1:18080
#    分别用 admin 与 tenant-admin 登录，确认可见对应数据范围
```

冒烟脚本输出 `FAIL=0`、wal-status 输出 `[ok]`、两账号可登录即视为上线自检通过。

## 2. 升级（镜像更新流程）

### 2.1 升级原则（design/60 4.4）

- 升级一律安排维护窗口（与试点客户约定），窗口内设备断连可接受。
- 升级前 48 小时冻结配置变更。
- 数据备份先行：未完成第 2.2 节第 1 步备份，禁止继续。
- 升级后发布人值守 2 小时，盯设备在线数与告警。

### 2.2 升级步骤

```bash
# 1. 备份先行（全量 dump + WAL 状态确认）
deploy/backup/backup.sh
deploy/backup/backup.sh wal-status

# 2. 记录当前版本（回滚依据）
git rev-parse HEAD
docker inspect adc-app --format '{{.Image}}'
docker inspect adc-gateway --format '{{.Image}}'

# 3. 拉取新代码并校验 Compose 配置
git pull
docker compose -f deploy/compose.yaml config --quiet && echo "compose config ok"

# 4. 重新构建并滚动替换（postgres/valkey 不重建，数据零丢失）
docker compose -f deploy/compose.yaml up -d --build

# 5. 等待全部服务 healthy
docker compose -f deploy/compose.yaml ps

# 6. 升级后冒烟（全量 10 步）
scripts/dev-smoke.sh

# 7. 观察 24 小时：设备在线数曲线与告警（Grafana ADC Overview）
```

> 注意：数据面与网关共用构建产物，先升数据面再升网关的顺序由 `up -d --build` 统一处理；数据库迁移随 seed/迁移步骤向前兼容执行，禁止手工改库。

## 3. 回滚

### 3.1 回滚触发条件（design/60 9.2）

满足任一即触发：升级后冒烟 FAIL 不为 0；P1 告警且根因指向新版本；高危拦截率下降（HITL 拦截异常）；异常 15 分钟未定位。

### 3.2 镜像 tag/版本回退

```bash
# 1. 回退代码到升级前提交（使用第 2.2 节第 2 步记录的版本）
git checkout '<升级前 commit>'

# 2. 重建并替换服务（数据面与网关回退到旧版本）
docker compose -f deploy/compose.yaml up -d --build

# 3. 回滚后冒烟
scripts/dev-smoke.sh

# 4. 确认设备在线数恢复（Grafana 或 /metrics）
curl -sf http://127.0.0.1:19091/metrics | grep adc_device_online_total
```

### 3.3 数据恢复兜底

当回滚代码不足以恢复（数据库异常、迁移失败）时，走备份恢复：

```bash
# 停止应用容器后原地恢复（restore.sh 有二次确认）
docker stop adc-app adc-gateway
RESTORE_DB=adc deploy/backup/restore.sh '<最近 dump 文件名>'

# 恢复后拉起并冒烟
docker start adc-app adc-gateway
scripts/dev-smoke.sh
```

恢复演练完整流程见 deploy/backup/RECOVERY-DRILL.md（第 3 节 RTO 五步）。

### 3.4 回滚后动作

- 24 小时观察窗口：设备在线数、HITL 拦截率、告警通道三项无异常方可关闭窗口。
- 记录回滚原因、耗时与根因，写入试点问题清单。

## 4. 备份

### 4.1 backup.sh 用法（引用 deploy/backup/backup.sh）

| 动作 | 命令 | 说明 |
|---|---|---|
| 初始化备份卷 | `deploy/backup/backup.sh init-backup-dir` | 新卷属主修复，一次性 |
| 全量备份 | `deploy/backup/backup.sh` | 每日全量 pg_dump + sha256 + TOC 校验 + RPO 检查 |
| 校验最近备份 | `deploy/backup/backup.sh verify` | 恢复就绪探针，建议随每日备份后执行 |
| WAL 新鲜度 | `deploy/backup/backup.sh wal-status` | RPO ≤ 5 分钟证据（退出码 3 即 P3 备份告警） |

备份机制说明：postgres 开启连续归档（archive_mode=on、archive_timeout=300），WAL 每 5 分钟强制切段落入 adc_backup 卷的 /backup/wal；每日全量 dump 落 /backup/adc-full-YYYYMMDD.dump 并生成 .sha256。Valkey 纯缓存不备份。

**接入定时任务（每日 02:00，RPO 保证）**

```bash
crontab -e
# 追加一行：
0 2 * * * root /opt/adc/deploy/backup/backup.sh >> /var/log/adc-backup.log 2>&1
```

**异机备份（可选，推荐）**：设置 `BACKUP_DIR_HOST`（NFS 挂载或本机目录）后，每次全量备份自动复制并校验一份到宿主目录，再经 rsync 到异机。

**保留策略**：`BACKUP_KEEP_DAYS=14`（默认保留 14 天全量），WAL 随卷增长，试点期每季度检查一次卷容量。

### 4.2 恢复演练步骤（每季度一次，deploy/backup/RECOVERY-DRILL.md）

```bash
# 1. 前置确认：最近备份可校验、WAL 新鲜
deploy/backup/backup.sh verify
deploy/backup/backup.sh wal-status

# 2. 在演练库恢复（默认目标 adc_restore，不触碰生产数据）
deploy/backup/restore.sh

# 3. 核对恢复比对报告（脚本输出四表计数，与生产比对）
# 4. 按 RECOVERY-DRILL.md 第 5 节执行冒烟清单
scripts/dev-smoke.sh
```

RTO 目标 ≤ 30 分钟：恢复耗时由 restore.sh 输出（RTO gate: 30 min）。演练留书面记录（操作人、耗时、偏差、整改项），模板见 RECOVERY-DRILL.md 第 7 节。

### 4.3 故障恢复速查

| 场景 | 动作 |
|---|---|
| WAL 归档失败（wal-status 退出码 3） | `deploy/backup/backup.sh init-backup-dir` 修复属主后重试；仍失败检查磁盘与 archive_command |
| 数据卷损坏 | 第 3.3 节原地恢复流程（RTO ≤ 30 分钟） |
| 误升级需回滚 | 第 3.2 节版本回退，无需数据恢复 |
| 备份脚本退出码 1 | 查看 /var/log/adc-backup.log，确认容器名与数据库口令 |

## 5. 相关文档

- deploy/backup/RECOVERY-DRILL.md：恢复演练完整清单（RPO/RTO 口径、PITR 进阶）
- docs/operations/notification-channel-checklist.md：审批通道接入演练（A1.2）
- docs/operations/pilot-onboarding-sop.md：试点客户接入 SOP（A1.5）
- docs/operations/pilot-weekly-report-template.md：试点周报模板（A1.4）
- design/60：部署与交付方案基线
