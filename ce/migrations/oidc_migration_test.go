package migrations

import (
	"io/fs"
	"strings"
	"testing"
)

func TestEmbeddedOIDCIdentityMigrationContract(t *testing.T) {
	t.Parallel()
	body, err := fs.ReadFile(embedded, "0005_oidc_identities.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(body)
	for _, required := range []string{
		"CREATE TABLE adc_oidc_identities",
		"issuer     TEXT NOT NULL",
		"subject    TEXT NOT NULL",
		"user_id    UUID NOT NULL REFERENCES adc_users(id) ON DELETE CASCADE",
		"UNIQUE (issuer, subject)",
		"CREATE TABLE adc_oidc_identity_quarantine",
		"metadata->>'oidc_issuer'",
		"metadata->>'oidc_subject'",
		"WHERE auth_source = 'OIDC'",
		"'MISSING_ISSUER'",
		"'DUPLICATE_IDENTITY'",
		"INSERT INTO adc_oidc_identities (issuer, subject, user_id)",
	} {
		if !strings.Contains(sql, required) {
			t.Errorf("0005 migration missing %q", required)
		}
	}
	if strings.Contains(sql, "tenant_admin") || strings.Contains(sql, "platform_admin") || strings.Contains(sql, "INSERT INTO adc_users") {
		t.Fatal("0005 migration must not auto-provision or grant administrator roles")
	}
}
