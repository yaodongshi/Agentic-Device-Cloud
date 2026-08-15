package config

import (
	"strings"
	"testing"
)

// envKeys covers every variable the Load function consults. Tests start from
// a clean slate so a developer's shell environment cannot leak into results.
var envKeys = []string{
	"ADC_ENV", "ADC_LOG_LEVEL", "ADC_HTTP_ADDR", "ADC_NODE_ID", "ADC_PUBLIC_URL",
	"ADC_TLS_ENABLE", "ADC_TLS_CERT", "ADC_TLS_KEY",
	"ADC_HTTP_READ_TIMEOUT_SEC", "ADC_HTTP_WRITE_TIMEOUT_SEC", "ADC_HTTP_IDLE_TIMEOUT_SEC",
	"ADC_HTTP_MAX_BODY_BYTES",
	"PG_HOST", "PG_PORT", "PG_USER", "PG_PASSWORD", "PG_DBNAME", "PG_SSLMODE",
	"VALKEY_ADDR", "VALKEY_PASSWORD", "VALKEY_DB",
	"ADC_WECOM_WEBHOOK", "ADC_DINGTALK_WEBHOOK", "ADC_HITL_TIMEOUT_SEC",
	"ADC_HITL_CALLBACK_KEY", "ADC_DEVICE_TTL_SEC", "ADC_WS_ALLOWED_ORIGINS",
}

// resetEnv neutralizes all known configuration variables (empty value is
// treated as unset by every getEnv helper).
func resetEnv(t *testing.T) {
	t.Helper()
	for _, k := range envKeys {
		t.Setenv(k, "")
	}
}

// setSecrets satisfies the mandatory credential checks of Load.
func setSecrets(t *testing.T) {
	t.Helper()
	t.Setenv("PG_PASSWORD", "pg-secret")
	t.Setenv("VALKEY_PASSWORD", "valkey-secret")
}

func TestLoadDefaults(t *testing.T) {
	resetEnv(t)
	setSecrets(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.TLSEnable {
		t.Fatal("TLSEnable default = true, want false")
	}
	if c.TLSCertFile != "" || c.TLSKeyFile != "" {
		t.Fatalf("TLS files default = (%q, %q), want empty", c.TLSCertFile, c.TLSKeyFile)
	}
	if c.ReadTimeoutSec != 10 {
		t.Fatalf("ReadTimeoutSec = %d, want 10", c.ReadTimeoutSec)
	}
	if c.WriteTimeoutSec != 30 {
		t.Fatalf("WriteTimeoutSec = %d, want 30", c.WriteTimeoutSec)
	}
	if c.IdleTimeoutSec != 60 {
		t.Fatalf("IdleTimeoutSec = %d, want 60", c.IdleTimeoutSec)
	}
	if c.MaxBodyBytes != 1<<20 {
		t.Fatalf("MaxBodyBytes = %d, want %d", c.MaxBodyBytes, 1<<20)
	}
	if c.Env != "dev" {
		t.Fatalf("Env = %q, want %q", c.Env, "dev")
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	resetEnv(t)
	setSecrets(t)
	t.Setenv("ADC_TLS_ENABLE", "true")
	t.Setenv("ADC_TLS_CERT", "/certs/tls.crt")
	t.Setenv("ADC_TLS_KEY", "/certs/tls.key")
	t.Setenv("ADC_HTTP_READ_TIMEOUT_SEC", "5")
	t.Setenv("ADC_HTTP_WRITE_TIMEOUT_SEC", "15")
	t.Setenv("ADC_HTTP_IDLE_TIMEOUT_SEC", "30")
	t.Setenv("ADC_HTTP_MAX_BODY_BYTES", "2048")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !c.TLSEnable {
		t.Fatal("TLSEnable = false, want true")
	}
	if c.TLSCertFile != "/certs/tls.crt" || c.TLSKeyFile != "/certs/tls.key" {
		t.Fatalf("TLS files = (%q, %q)", c.TLSCertFile, c.TLSKeyFile)
	}
	if c.ReadTimeoutSec != 5 || c.WriteTimeoutSec != 15 || c.IdleTimeoutSec != 30 {
		t.Fatalf("timeouts = (%d, %d, %d), want (5, 15, 30)",
			c.ReadTimeoutSec, c.WriteTimeoutSec, c.IdleTimeoutSec)
	}
	if c.MaxBodyBytes != 2048 {
		t.Fatalf("MaxBodyBytes = %d, want 2048", c.MaxBodyBytes)
	}
}

func TestLoadTLSEnableRequiresCertAndKey(t *testing.T) {
	resetEnv(t)
	setSecrets(t)
	t.Setenv("ADC_TLS_ENABLE", "true")

	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded without cert/key, want error")
	} else if !strings.Contains(err.Error(), "ADC_TLS_CERT") {
		t.Fatalf("error = %q, want mention of ADC_TLS_CERT", err)
	}

	t.Setenv("ADC_TLS_CERT", "/certs/tls.crt")
	t.Setenv("ADC_TLS_KEY", "/certs/tls.key")
	if _, err := Load(); err != nil {
		t.Fatalf("Load() with complete TLS config error = %v", err)
	}
}

func TestLoadNegativeTimeoutRejected(t *testing.T) {
	resetEnv(t)
	setSecrets(t)
	t.Setenv("ADC_HTTP_READ_TIMEOUT_SEC", "-1")

	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded with negative read timeout, want error")
	} else if !strings.Contains(err.Error(), "SEC-19") {
		t.Fatalf("error = %q, want mention of SEC-19", err)
	}
}

func TestLoadNegativeMaxBodyRejected(t *testing.T) {
	resetEnv(t)
	setSecrets(t)
	t.Setenv("ADC_HTTP_MAX_BODY_BYTES", "-1024")

	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded with negative max body, want error")
	} else if !strings.Contains(err.Error(), "ADC_HTTP_MAX_BODY_BYTES") {
		t.Fatalf("error = %q, want mention of ADC_HTTP_MAX_BODY_BYTES", err)
	}
}

func TestLoadBadBoolFallsBackToDefault(t *testing.T) {
	resetEnv(t)
	setSecrets(t)
	t.Setenv("ADC_TLS_ENABLE", "not-a-bool")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.TLSEnable {
		t.Fatal("TLSEnable = true for unparsable value, want default false")
	}
}
