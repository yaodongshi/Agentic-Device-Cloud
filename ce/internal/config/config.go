// Package config 提供统一配置加载：环境变量注入（SEC-13：密钥不入代码、不入进程参数）。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Env        string
	LogLevel   string
	HTTPAddr   string // 服务监听地址，如 :8080
	NodeID     string
	PublicURL  string // 对外访问地址（回调签名与落地页用）

	// TLS 终结配置（SEC-05）：证书与私钥经环境变量注入，仅挂载于边缘节点
	//（统一 API 网关）；部署细节见 design/60。
	TLSEnable   bool
	TLSCertFile string
	TLSKeyFile  string

	// http.Server 全量超时预算（SEC-19：slowloris 防御），单位秒。
	ReadTimeoutSec  int
	WriteTimeoutSec int
	IdleTimeoutSec  int

	// MaxBodyBytes 限制请求体大小（SEC-19），默认 1 MiB，HTTP 层经
	// httpx.BodyLimit 中间件执行。
	MaxBodyBytes int64

	Postgres DSN
	Valkey   ValkeyConfig

	WeComWebhookURL  string
	DingTalkWebhook  string
	HITLTimeoutSec   int
	HITLCallbackKey  string // 回调签名密钥（环境变量注入）

	DeviceTTLSeconds int
	// WSS Origin 白名单（SEC-04），逗号分隔，经 ADC_WS_ALLOWED_ORIGINS 注入；
	// 为空时仅放行无 Origin 头的设备 SDK 客户端（浏览器跨站请求必带 Origin）。
	AllowedOrigins []string
}

type DSN struct {
	Host     string
	Port     int
	User     string
	Password string
	DBName   string
	SSLMode  string
}

func (d DSN) String() string {
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		d.Host, d.Port, d.User, d.Password, d.DBName, d.SSLMode)
}

// ValkeyConfig 仅承载连接凭据。Valkey ACL 最小权限与通道 TLS 属部署层配置
//（SEC-06，由 design/60 落地），代码只读取注入的凭据，不管理 ACL。
type ValkeyConfig struct {
	Addr     string
	Password string
	DB       int
}

// Load 从环境变量读取配置；缺失必填项返回错误，避免静默使用不安全默认值。
func Load() (*Config, error) {
	c := &Config{
		Env:              getEnv("ADC_ENV", "dev"),
		LogLevel:         getEnv("ADC_LOG_LEVEL", "info"),
		HTTPAddr:         getEnv("ADC_HTTP_ADDR", ":8080"),
		NodeID:           getEnv("ADC_NODE_ID", "adc-node-01"),
		PublicURL:        getEnv("ADC_PUBLIC_URL", "http://localhost:8080"),
		TLSEnable:        getEnvBool("ADC_TLS_ENABLE", false),
		TLSCertFile:      getEnv("ADC_TLS_CERT", ""),
		TLSKeyFile:       getEnv("ADC_TLS_KEY", ""),
		ReadTimeoutSec:   getEnvInt("ADC_HTTP_READ_TIMEOUT_SEC", 10),
		WriteTimeoutSec:  getEnvInt("ADC_HTTP_WRITE_TIMEOUT_SEC", 30),
		IdleTimeoutSec:   getEnvInt("ADC_HTTP_IDLE_TIMEOUT_SEC", 60),
		MaxBodyBytes:     getEnvInt64("ADC_HTTP_MAX_BODY_BYTES", 1<<20),
		Postgres:         DSN{Host: getEnv("PG_HOST", "127.0.0.1"), Port: getEnvInt("PG_PORT", 5432), User: getEnv("PG_USER", "adc"), Password: getEnv("PG_PASSWORD", ""), DBName: getEnv("PG_DBNAME", "adc"), SSLMode: getEnv("PG_SSLMODE", "disable")},
		Valkey:           ValkeyConfig{Addr: getEnv("VALKEY_ADDR", "127.0.0.1:6379"), Password: getEnv("VALKEY_PASSWORD", ""), DB: getEnvInt("VALKEY_DB", 0)},
		WeComWebhookURL:  getEnv("ADC_WECOM_WEBHOOK", ""),
		DingTalkWebhook:  getEnv("ADC_DINGTALK_WEBHOOK", ""),
		HITLTimeoutSec:   getEnvInt("ADC_HITL_TIMEOUT_SEC", 300),
		HITLCallbackKey:  getEnv("ADC_HITL_CALLBACK_KEY", ""),
		DeviceTTLSeconds: getEnvInt("ADC_DEVICE_TTL_SEC", 90),
		AllowedOrigins:   getEnvList("ADC_WS_ALLOWED_ORIGINS"),
	}

	if c.Postgres.Password == "" {
		return nil, fmt.Errorf("config: PG_PASSWORD is required (env injection, never hardcode)")
	}
	if c.Valkey.Password == "" {
		return nil, fmt.Errorf("config: VALKEY_PASSWORD is required (env injection, never hardcode)")
	}
	if c.TLSEnable && (c.TLSCertFile == "" || c.TLSKeyFile == "") {
		return nil, fmt.Errorf("config: ADC_TLS_ENABLE requires ADC_TLS_CERT and ADC_TLS_KEY (SEC-05)")
	}
	if c.ReadTimeoutSec < 0 || c.WriteTimeoutSec < 0 || c.IdleTimeoutSec < 0 {
		return nil, fmt.Errorf("config: HTTP timeouts must not be negative (SEC-19)")
	}
	if c.MaxBodyBytes < 0 {
		return nil, fmt.Errorf("config: ADC_HTTP_MAX_BODY_BYTES must not be negative (SEC-19)")
	}
	return c, nil
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// getEnvList 解析逗号分隔的配置项列表（如 WSS Origin 白名单）。
func getEnvList(key string) []string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getEnvInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func getEnvInt64(key string, def int64) int64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return n
		}
	}
	return def
}

func getEnvBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}
