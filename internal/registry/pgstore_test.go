package registry

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestPGStore exercises the PostgreSQL store end to end. It is skipped
// unless DATABASE_URL points at a database (see docker-compose.yml).
func TestPGStore(t *testing.T) {
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
	if _, err := db.ExecContext(ctx, SchemaSQL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	// Isolate the test in its own package rows.
	pkg := "test.pgstore." + time.Now().Format("20060102150405.000000000")
	store := NewPGStore(db)

	v1 := Version{Package: pkg, Version: "v1", ContentHash: []byte("hash-v1"), DescriptorSet: []byte("set-v1"), OwnedPaths: []string{"a.proto"}}
	created, err := store.PutVersion(ctx, v1)
	if err != nil || !created {
		t.Fatalf("first put: created=%v err=%v", created, err)
	}
	// Identical retry is idempotent.
	created, err = store.PutVersion(ctx, v1)
	if err != nil || created {
		t.Fatalf("idempotent put: created=%v err=%v", created, err)
	}
	// Different content under the same version is rejected.
	created, err = store.PutVersion(ctx, Version{Package: pkg, Version: "v1", ContentHash: []byte("hash-other"), DescriptorSet: []byte("set-other"), OwnedPaths: []string{"a.proto"}})
	if err != ErrVersionConflict {
		t.Fatalf("conflicting put: created=%v err=%v, want ErrVersionConflict", created, err)
	}

	got, err := store.GetVersion(ctx, pkg, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.ContentHash) != "hash-v1" || string(got.DescriptorSet) != "set-v1" {
		t.Fatalf("stored content changed: %+v", got)
	}
	if got.OwnedPaths[0] != "a.proto" {
		t.Fatalf("owned paths = %v", got.OwnedPaths)
	}

	if _, err := store.PutVersion(ctx, Version{Package: pkg, Version: "v2", ContentHash: []byte("hash-v2"), DescriptorSet: []byte("set-v2"), OwnedPaths: []string{"a.proto"}}); err != nil {
		t.Fatal(err)
	}
	latest, err := store.LatestVersion(ctx, pkg)
	if err != nil || latest.Version != "v2" {
		t.Fatalf("latest = %+v err=%v", latest, err)
	}
	list, err := store.ListVersions(ctx, pkg)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v err=%v", list, err)
	}

	if err := store.UpsertConsumer(ctx, ConsumerDecl{Package: pkg, Consumer: "svc", Encoding: "json", Usages: []Usage{{Message: "a.M", Fields: []string{"f"}}}}); err != nil {
		t.Fatal(err)
	}
	decl, err := store.GetConsumer(ctx, pkg, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if decl.Encoding != "json" || len(decl.Usages) != 1 || decl.Usages[0].Fields[0] != "f" {
		t.Fatalf("decl = %+v", decl)
	}
}
