# 安全 OIDC 登录

## Purpose

定义生产环境 OIDC 登录从发起到会话恢复的安全边界，覆盖令牌完整信任链、PKCE 与事务防重放、外部端点限制、最小权限身份映射，以及回调响应和会话凭证的防泄漏要求。

## Requirements

### Requirement: OIDC 令牌必须建立完整信任链
系统 MUST 使用受信 issuer 的 JWKS 验证 `id_token` 签名，并校验 issuer、audience、有效期、允许算法和 nonce；任一校验失败不得创建用户或会话。

#### Scenario: 拒绝伪造令牌
- **WHEN** 回调收到签名无效、受众错误、过期或 nonce 不匹配的 `id_token`
- **THEN** 系统返回统一认证失败且用户、身份映射和会话数量不变

### Requirement: OIDC 登录事务必须防重放和登录 CSRF
系统 MUST 使用 PKCE S256、随机 state、nonce 和浏览器绑定事务 cookie，并原子地单次消费服务端事务。

#### Scenario: 并发重放同一事务
- **WHEN** 多个回调并发提交相同 state 或使用其他浏览器的事务 cookie
- **THEN** 最多一个绑定正确的回调成功，其余全部失败

### Requirement: OIDC 外呼必须遵守安全端点策略
系统 MUST 在生产模式仅信任 HTTPS issuer，校验 discovery issuer，并阻止未允许的 loopback、链路本地和私网端点及跨源重定向。

#### Scenario: Discovery 返回不安全 token endpoint
- **WHEN** discovery 返回 HTTP、未允许私网或不受信跨源 token endpoint
- **THEN** OIDC 配置或登录 MUST fail closed，且不得向该端点发送 client secret

### Requirement: OIDC 身份映射必须最小权限
系统 MUST 以 issuer 与 subject 的组合唯一映射本地用户；未知身份默认拒绝，自动开户不得默认授予 `tenant_admin` 或 `platform_admin`。

#### Scenario: 未知外部身份登录
- **WHEN** 未预映射的 subject 完成 IdP 认证且未显式启用安全开户策略
- **THEN** 系统拒绝登录且不创建管理用户

### Requirement: OIDC 回调不得泄漏凭证
系统 MUST 使用 HttpOnly Secure 会话 cookie，并在所有回调响应设置禁止缓存和 Referrer 防泄漏头；会话 token 不得出现在 URL 或响应正文。

#### Scenario: 登录成功重定向
- **WHEN** OIDC 登录成功
- **THEN** 浏览器通过安全 cookie 恢复会话且 Location 不包含 token、code 或 state
