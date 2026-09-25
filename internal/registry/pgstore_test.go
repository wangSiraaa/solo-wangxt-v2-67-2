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
	created, err := store.PutVersion(ctx, v1, nil)
	if err != nil || !created {
		t.Fatalf("first put: created=%v err=%v", created, err)
	}
	// Identical retry is idempotent.
	created, err = store.PutVersion(ctx, v1, nil)
	if err != nil || created {
		t.Fatalf("idempotent put: created=%v err=%v", created, err)
	}
	// Different content under the same version is rejected.
	created, err = store.PutVersion(ctx, Version{Package: pkg, Version: "v1", ContentHash: []byte("hash-other"), DescriptorSet: []byte("set-other"), OwnedPaths: []string{"a.proto"}}, nil)
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

	if _, err := store.PutVersion(ctx, Version{Package: pkg, Version: "v2", ContentHash: []byte("hash-v2"), DescriptorSet: []byte("set-v2"), OwnedPaths: []string{"a.proto"}}, nil); err != nil {
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

// TestPGStoreDependencyGraph covers the cross-package tables: path
// ownership, locks, reverse edges and idempotent impact analyses.
func TestPGStoreDependencyGraph(t *testing.T) {
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
	suffix := time.Now().Format("20060102150405.000000000")
	basePkg := "test.pg.deps.base." + suffix
	midPkg := "test.pg.deps.mid." + suffix
	store := NewPGStore(db)

	if _, err := store.PutVersion(ctx, Version{
		Package: basePkg, Version: "v1", ContentHash: []byte("base-v1"),
		DescriptorSet: []byte("set"), OwnedPaths: []string{"base/common.proto"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	owner, ownerVer, err := store.FindPathOwner(ctx, "base/common.proto")
	if err != nil || owner != basePkg || ownerVer != "v1" {
		t.Fatalf("path owner = %s@%s err=%v", owner, ownerVer, err)
	}
	if _, _, err := store.FindPathOwner(ctx, "no/such.proto"); err != ErrNotFound {
		t.Fatalf("unknown path: err=%v, want ErrNotFound", err)
	}

	locks := []DepLock{{DepPackage: basePkg, DepVersion: "v1", DepHash: []byte("base-v1")}}
	if _, err := store.PutVersion(ctx, Version{
		Package: midPkg, Version: "v1", ContentHash: []byte("mid-v1"),
		DescriptorSet: []byte("set"), OwnedPaths: []string{"mid/invoice.proto"},
	}, locks); err != nil {
		t.Fatal(err)
	}
	gotLocks, err := store.GetDeps(ctx, midPkg, "v1")
	if err != nil || len(gotLocks) != 1 || gotLocks[0].DepPackage != basePkg || gotLocks[0].DepVersion != "v1" {
		t.Fatalf("locks = %+v err=%v", gotLocks, err)
	}
	if string(gotLocks[0].DepHash) != "base-v1" {
		t.Fatalf("lock digest = %q", gotLocks[0].DepHash)
	}
	edges, err := store.Dependents(ctx, basePkg)
	if err != nil || len(edges) != 1 || edges[0].Package != midPkg || edges[0].Version != "v1" || edges[0].DepVersion != "v1" {
		t.Fatalf("dependents = %+v err=%v", edges, err)
	}

	// Impact analyses: same input is stored once, second put is a no-op.
	a := StoredImpact{
		Package: basePkg, BaseVersion: "v1", HeadVersion: "v2",
		InputHash: []byte("input-hash"),
		Result:    &ImpactResult{Report: nil, Impacts: []PackageImpact{{Package: midPkg, Version: "v1", Impact: ImpactDirect}}},
	}
	created, err := store.PutImpactAnalysis(ctx, a)
	if err != nil || !created {
		t.Fatalf("first analysis put: created=%v err=%v", created, err)
	}
	created, err = store.PutImpactAnalysis(ctx, a)
	if err != nil || created {
		t.Fatalf("repeat analysis put: created=%v err=%v, want created=false", created, err)
	}
	stored, err := store.GetImpactAnalysis(ctx, basePkg, "v1", "v2")
	if err != nil {
		t.Fatal(err)
	}
	if string(stored.InputHash) != "input-hash" || len(stored.Result.Impacts) != 1 || stored.Result.Impacts[0].Impact != ImpactDirect {
		t.Fatalf("stored analysis = %+v", stored)
	}
	if _, err := store.GetImpactAnalysis(ctx, basePkg, "v1", "v9"); err != ErrNotFound {
		t.Fatalf("unknown analysis: err=%v, want ErrNotFound", err)
	}
}
