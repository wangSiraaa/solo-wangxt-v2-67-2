package registry_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"protocompat/internal/registry"
	"protocompat/internal/regress"
)

// TestImpactCasesOnPostgres reruns the whole file-based impact regression
// suite against a real PostgreSQL store, proving the dependency graph and
// the stored analyses behave identically to the in-memory store. Skipped
// unless DATABASE_URL is set.
func TestImpactCasesOnPostgres(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, registry.SchemaSQL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	root := filepath.Join("..", "..", "testdata", "impact")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "case.json")); err != nil {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			// Isolate every case: truncating packages cascades to
			// versions, locks, path owners, analyses, reports, consumers.
			if _, err := db.ExecContext(ctx, "TRUNCATE packages CASCADE"); err != nil {
				t.Fatalf("truncate: %v", err)
			}
			res, err := regress.RunImpactCaseWithStore(filepath.Join(root, e.Name()), registry.NewPGStore(db))
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range res.Problems {
				t.Errorf("  - %s", p)
			}
		})
	}
}
