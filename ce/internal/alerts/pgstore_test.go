package alerts

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

const storedRulesJSON = `[{"id":"11111111-2222-3333-4444-555555555555","name":"pool watermark",` +
	`"metric":"adc_pg_pool_connections","operator":">","threshold":0.8,"duration_sec":300,` +
	`"severity":"P2","enabled":true,"created_at":"2026-08-14T08:00:00Z","updated_at":"2026-08-14T08:00:00Z"}]`

func TestPGRuleStoreList(t *testing.T) {
	m := newMockPool(t)
	m.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(metadata->'alert_rules', '[]'::jsonb) FROM adc_tenants
		 WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testTenantA).
		WillReturnRows(pgxmock.NewRows([]string{"coalesce"}).AddRow([]byte(storedRulesJSON)))

	s := NewPGRuleStore(m)
	rules, err := s.List(context.Background(), testTenantA)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rules) != 1 || rules[0].Name != "pool watermark" || rules[0].Operator != OpGreater {
		t.Fatalf("List = %+v", rules)
	}
	if rules[0].Severity != SeverityP2 || rules[0].DurationSec != 300 {
		t.Fatalf("List = %+v", rules)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGRuleStoreListTenantNotFound(t *testing.T) {
	m := newMockPool(t)
	m.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(metadata->'alert_rules', '[]'::jsonb) FROM adc_tenants
		 WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testTenantA).
		WillReturnError(pgx.ErrNoRows)

	s := NewPGRuleStore(m)
	_, err := s.List(context.Background(), testTenantA)
	if !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("List err = %v, want ErrTenantNotFound", err)
	}
}

func TestPGRuleStoreReplaceMergesJSONB(t *testing.T) {
	m := newMockPool(t)
	// The merge must preserve sibling metadata keys: assert the exact SQL
	// shape (metadata || jsonb_build_object('alert_rules', $2::jsonb)).
	m.ExpectExec(regexp.QuoteMeta(`UPDATE adc_tenants
		 SET metadata = metadata || jsonb_build_object('alert_rules', $2::jsonb), updated_at = now()
		 WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testTenantA, pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	s := NewPGRuleStore(m)
	rule := sampleRule("r1", testTenantA)
	rule.ID = "11111111-2222-3333-4444-555555555555"
	rule.CreatedAt = fixedClock
	rule.UpdatedAt = fixedClock
	if err := s.Replace(context.Background(), testTenantA, []Rule{rule}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGRuleStoreReplaceTenantNotFound(t *testing.T) {
	m := newMockPool(t)
	m.ExpectExec(regexp.QuoteMeta(`UPDATE adc_tenants
		 SET metadata = metadata || jsonb_build_object('alert_rules', $2::jsonb), updated_at = now()
		 WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testTenantA, pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	s := NewPGRuleStore(m)
	err := s.Replace(context.Background(), testTenantA, []Rule{sampleRule("r1", testTenantA)})
	if !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("Replace err = %v, want ErrTenantNotFound", err)
	}
}

func TestPGRuleStoreAll(t *testing.T) {
	m := newMockPool(t)
	rows := pgxmock.NewRows([]string{"id", "coalesce"}).
		AddRow(testTenantA, []byte(storedRulesJSON)).
		AddRow(testTenantB, nil) // tenant without rules: NULL coalesces to []
	m.ExpectQuery(regexp.QuoteMeta(`SELECT id::text, COALESCE(metadata->'alert_rules', '[]'::jsonb)
		 FROM adc_tenants WHERE deleted_at IS NULL`)).
		WillReturnRows(rows)

	s := NewPGRuleStore(m)
	all, err := s.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("All len = %d, want 1", len(all))
	}
	if all[0].TenantID != testTenantA {
		t.Fatalf("All[0].TenantID = %q, want %q", all[0].TenantID, testTenantA)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestPGRuleStoreAllSkipsCorruptMetadata: a broken JSON payload on one
// tenant must not abort the sweep.
func TestPGRuleStoreAllSkipsCorruptMetadata(t *testing.T) {
	m := newMockPool(t)
	rows := pgxmock.NewRows([]string{"id", "coalesce"}).
		AddRow(testTenantA, []byte(`not-json`)).
		AddRow(testTenantB, []byte(storedRulesJSON))
	m.ExpectQuery(regexp.QuoteMeta(`SELECT id::text, COALESCE(metadata->'alert_rules', '[]'::jsonb)
		 FROM adc_tenants WHERE deleted_at IS NULL`)).
		WillReturnRows(rows)

	s := NewPGRuleStore(m)
	all, err := s.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 1 || all[0].TenantID != testTenantB {
		t.Fatalf("All = %+v, want only tenant B's rule", all)
	}
}
