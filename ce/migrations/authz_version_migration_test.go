package migrations

import (
	"io/fs"
	"strings"
	"testing"
)

func TestEmbeddedAuthzVersionMigrationContract(t *testing.T) {
	t.Parallel()
	up, err := fs.ReadFile(embedded, "0007_authz_version.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	down, err := fs.ReadFile(fs.FS(embedded), "0007_authz_version.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"ADD COLUMN authz_version BIGINT NOT NULL DEFAULT 1",
		"CHECK (authz_version > 0)",
		"AFTER INSERT OR UPDATE OR DELETE ON adc_user_roles",
		"BEFORE UPDATE OF status, deleted_at ON adc_users",
		"NEW.authz_version := OLD.authz_version + 1",
	} {
		if !strings.Contains(string(up), required) {
			t.Errorf("0007 migration missing %q", required)
		}
	}
	if strings.Contains(string(up), "adc_a2a_tasks") || strings.Contains(string(down), "adc_a2a_tasks") {
		t.Fatal("0007 must contain only authz_version changes; A2A is reserved for 0008")
	}
	for _, required := range []string{"DROP TRIGGER", "DROP FUNCTION", "DROP COLUMN IF EXISTS authz_version"} {
		if !strings.Contains(string(down), required) {
			t.Errorf("0007 down migration missing %q", required)
		}
	}
}
