package registry

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"protocompat/internal/deps"
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
	// Unique prefix per run so re-runs never collide on package_paths.
	runPrefix := "test_pgstore_" + time.Now().Format("20060102150405_000000000")
	pkg := "test.pgstore." + runPrefix + ".a"
	pkgB := "test.pgstore." + runPrefix + ".b"
	pathA := runPrefix + "/a.proto"
	pathB := runPrefix + "/b.proto"
	pathA2 := runPrefix + "/a2.proto"
	store := NewPGStore(db)

	v1 := Version{
		Package:       pkg,
		Version:       "v1",
		ContentHash:   []byte("hash-v1"),
		DescriptorSet: mustDescriptorSet(t, pkg, pathA),
		OwnedPaths:    []string{pathA},
	}
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
	other := v1
	other.ContentHash = []byte("hash-other")
	if _, err = store.PutVersion(ctx, other); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("conflicting put: err=%v, want ErrVersionConflict", err)
	}

	got, err := store.GetVersion(ctx, pkg, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.ContentHash) != "hash-v1" {
		t.Fatalf("stored content changed: %+v", got)
	}
	if got.OwnedPaths[0] != pathA {
		t.Fatalf("owned paths = %v", got.OwnedPaths)
	}

	// Path ownership lookup.
	owner, latest, err := store.PathOwner(ctx, pathA)
	if err != nil || owner != pkg || latest.Version != "v1" {
		t.Fatalf("path owner = %s %+v err=%v", owner, latest, err)
	}
	if _, _, err := store.PathOwner(ctx, runPrefix+"/no_such.proto"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing path owner: err=%v", err)
	}

	// Second package, one version with a lock on pkg v1.
	vB := Version{
		Package:       pkgB,
		Version:       "v1",
		ContentHash:   []byte("hash-b"),
		DescriptorSet: mustDescriptorSet(t, pkgB, pathB),
		OwnedPaths:    []string{pathB},
		Locks:         []deps.Lock{{Package: pkg, Version: "v1", Digest: hex.EncodeToString(v1.ContentHash)}},
		LockDigest:    "digest-b-v1",
	}
	if _, err := store.PutVersion(ctx, vB); err != nil {
		t.Fatalf("put b v1 with lock: %v", err)
	}
	gotB, err := store.GetVersion(ctx, pkgB, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(gotB.Locks) != 1 || gotB.Locks[0].Package != pkg || gotB.LockDigest != "digest-b-v1" {
		t.Fatalf("stored locks = %+v digest=%q", gotB.Locks, gotB.LockDigest)
	}
	all, err := store.AllVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, v := range all {
		if v.Package == pkg || v.Package == pkgB {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("AllVersions found %d of the 2 test versions", found)
	}

	// A would-be package cycle (a v2 locks b; b already locks a) must
	// roll back atomically: no version, no path claim, no edge.
	cyclic := Version{
		Package:       pkg,
		Version:       "v2",
		ContentHash:   []byte("hash-v2"),
		DescriptorSet: mustDescriptorSet(t, pkg, pathA2),
		OwnedPaths:    []string{pathA2},
		Locks:         []deps.Lock{{Package: pkgB, Version: "v1", Digest: hex.EncodeToString(vB.ContentHash)}},
		LockDigest:    "digest-a-v2",
	}
	if _, err := store.PutVersion(ctx, cyclic); !errors.Is(err, ErrDependencyCycle) {
		t.Fatalf("cycle put: err=%v, want ErrDependencyCycle", err)
	}
	if _, err := store.GetVersion(ctx, pkg, "v2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cyclic version persisted: err=%v", err)
	}
	if _, _, err := store.PathOwner(ctx, pathA2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cyclic path claim persisted: err=%v", err)
	}
	// A's lock set must be untouched (still no edges).
	var depCount int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM version_dependencies d
		JOIN versions v ON v.id = d.version_id
		JOIN packages p ON p.id = v.package_id
		WHERE p.name = $1`, pkg).Scan(&depCount); err != nil {
		t.Fatal(err)
	}
	if depCount != 0 {
		t.Fatalf("rejected cycle left %d dependency edge(s) for %s", depCount, pkg)
	}

	if _, err := store.LatestVersion(ctx, pkg); err != nil {
		t.Fatalf("latest: %v", err)
	}
	list, err := store.ListVersions(ctx, pkg)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v err=%v", list, err)
	}

	if err := store.UpsertConsumer(ctx, ConsumerDecl{
		Package: pkg, Consumer: "svc", Encoding: "json",
		Usages: []Usage{{Message: "a.M", Fields: []string{"f"}}},
	}); err != nil {
		t.Fatal(err)
	}
	decl, err := store.GetConsumer(ctx, pkg, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if decl.Encoding != "json" || len(decl.Usages) != 1 || decl.Usages[0].Fields[0] != "f" {
		t.Fatalf("decl = %+v", decl)
	}

	// Impact memoization round-trips.
	analysis := map[string]any{
		"package": pkg, "base_version": "v1", "candidate_version": "v2",
		"verdict": "INCOMPATIBLE",
		"nodes": []any{
			map[string]any{"package": pkgB, "version": "v1", "status": "DIRECT_IMPACTED"},
		},
	}
	body, _ := json.Marshal(analysis)
	in := StoredImpact{InputDigest: "digest-" + runPrefix, Package: pkg, BaseVersion: "v1", Analysis: body}
	if err := store.PutImpact(ctx, in); err != nil {
		t.Fatalf("put impact: %v", err)
	}
	stored, err := store.GetImpact(ctx, in.InputDigest)
	if err != nil {
		t.Fatalf("get impact: %v", err)
	}
	var gotJSON, wantJSON map[string]any
	if err := json.Unmarshal(stored.Analysis, &gotJSON); err != nil {
		t.Fatalf("stored analysis is not JSON: %v", err)
	}
	if err := json.Unmarshal(body, &wantJSON); err != nil {
		t.Fatal(err)
	}
	if !mapsEqual(gotJSON, wantJSON) {
		t.Fatalf("impact body changed:\n got %s\nwant %s", stored.Analysis, body)
	}
	if err := store.PutImpact(ctx, in); err != nil {
		t.Fatalf("idempotent put impact: %v", err)
	}
}

// mustDescriptorSet builds a minimal valid FileDescriptorSet so PG tests
// do not depend on the compiler.
func mustDescriptorSet(t *testing.T, pkgName, path string) []byte {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String(path),
		Package: proto.String(pkgName),
		Syntax:  proto.String("proto3"),
	}
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fdp}}
	b, err := proto.Marshal(set)
	if err != nil {
		t.Fatalf("marshal descriptor set for %s: %v", path, err)
	}
	return b
}

func mapsEqual(a, b map[string]any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}
