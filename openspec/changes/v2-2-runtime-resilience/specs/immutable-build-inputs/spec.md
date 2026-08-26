## ADDED Requirements

### Requirement: 容器构建与部署镜像必须固定 digest
系统 MUST 使全部 Dockerfile 基础镜像和 Compose 生产镜像使用带可读版本且固定 `sha256` digest 的不可变引用；浮动 tag、缺失 digest 或非法 digest 必须被门禁拒绝。

#### Scenario: Dockerfile 使用浮动基础镜像
- **WHEN** Dockerfile 的任一 `FROM` 仅使用 tag 或使用 `latest`
- **THEN** 不可变引用检查失败且候选制品不得发布

#### Scenario: Compose 镜像缺少 digest
- **WHEN** 生产 Compose 的任一镜像未固定合法 `sha256` digest
- **THEN** 不可变引用检查失败

#### Scenario: 镜像引用完整
- **WHEN** 全部基础镜像和生产镜像均使用 `name:version@sha256:digest`
- **THEN** 静态检查通过且记录的 digest 可用于复现构建与部署

### Requirement: GitHub Actions 必须固定完整提交 SHA
系统 MUST 使全部 workflow 的 `uses:` 固定到 40 位完整提交 SHA，禁止 tag、分支和短 SHA；workflow 与 job MUST 显式声明完成任务所需的最小权限。

#### Scenario: Action 使用 tag
- **WHEN** workflow 使用 `owner/action@vN`、分支或短 SHA
- **THEN** 不可变引用检查失败

#### Scenario: Job 请求过宽权限
- **WHEN** workflow 或 job 授予未被步骤使用的写权限或 `write-all`
- **THEN** 最小权限检查失败

#### Scenario: Action 与权限均合规
- **WHEN** 所有 Action 固定完整 SHA且每个 job 仅声明必需权限
- **THEN** workflow 静态门禁通过

### Requirement: 不可变依赖必须具有自动更新入口
系统 SHALL 配置 Dependabot 检查 Docker 与 GitHub Actions 引用更新，更新后的 digest 或完整 SHA MUST 经过与普通候选提交相同的完整门禁。

#### Scenario: 上游镜像或 Action 发布更新
- **WHEN** Dependabot 检测到受管 Docker 或 GitHub Actions 依赖有新版本
- **THEN** 系统创建可审查的更新入口且不得绕过测试、扫描和不可变引用检查
