// Command adc-license is the EE licensing service skeleton (design/82 B9.2):
// validates a signed license file, reports entitlement (device cap, expiry)
// and runs the heartbeat to adc_license_checks so the data plane can enforce
// soft limits. Full enforcement lands with the ee/ modules in V1.5.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

// License is the signed entitlement document shipped to private-deployment
// customers (doc/04: device-scale packs, perpetual + maintenance expiry).
type License struct {
	CustomerID string    `json:"customer_id"`
	TenantCode string    `json:"tenant_code"`
	DeviceCap  int       `json:"device_cap"`
	Edition    string    `json:"edition"` // ce | ee
	IssuedAt   time.Time `json:"issued_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Signature  string    `json:"signature"`
}

func main() {
	licensePath := flag.String("license", "license.json", "path to the license file")
	keyEnv := flag.String("key-env", "ADC_LICENSE_KEY", "env var holding the signing key (hex)")
	verifyOnly := flag.Bool("verify", false, "verify the license and print entitlements, then exit")
	flag.Parse()

	keyHex := os.Getenv(*keyEnv)
	if keyHex == "" {
		fatal("signing key required: set %s", *keyEnv)
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) < 32 {
		fatal("invalid signing key: must be >=32 bytes hex")
	}

	raw, err := os.ReadFile(*licensePath)
	if err != nil {
		fatal("read license: %v", err)
	}
	var lic License
	if err := json.Unmarshal(raw, &lic); err != nil {
		fatal("parse license: %v", err)
	}

	payload := fmt.Sprintf("%s|%s|%d|%s|%s|%s", lic.CustomerID, lic.TenantCode, lic.DeviceCap,
		lic.Edition, lic.IssuedAt.UTC().Format(time.RFC3339), lic.ExpiresAt.UTC().Format(time.RFC3339))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(lic.Signature)) {
		fatal("license signature mismatch (tampered file)")
	}
	if time.Now().After(lic.ExpiresAt) {
		fatal("license expired at %s", lic.ExpiresAt)
	}

	fmt.Printf("license valid: customer=%s tenant=%s edition=%s device_cap=%d expires=%s\n",
		lic.CustomerID, lic.TenantCode, lic.Edition, lic.DeviceCap, lic.ExpiresAt)
	if *verifyOnly {
		return
	}
	fmt.Println("heartbeat: adc_license_checks row (PG) would be updated here; enforcement lands with ee/ modules")
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "adc-license: "+format+"\n", args...)
	os.Exit(1)
}
