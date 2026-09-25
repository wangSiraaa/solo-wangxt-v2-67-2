package registry

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"protocompat/internal/schema"
)

// Proto fixtures for a small dependency ecosystem. The registry package
// names mirror the proto packages for readability.
const (
	depBaseV1 = `syntax = "proto3"; package acme.base; message Shared { string id = 1; int32 amount = 2; } message Other { string name = 1; }`
	depBaseV2 = `syntax = "proto3"; package acme.base; message Shared { string id = 1; int64 amount = 2; } message Other { string name = 1; }`

	depMidV1  = `syntax = "proto3"; package acme.mid; import "base/common.proto"; message Invoice { acme.base.Shared shared = 1; string note = 2; }`
	depSideV1 = `syntax = "proto3"; package acme.side; import "base/common.proto"; message Order { acme.base.Shared shared = 1; }`
	depTopV1  = `syntax = "proto3"; package acme.top; import "mid/invoice.proto"; import "side/order.proto"; message Batch { acme.mid.Invoice inv = 1; acme.side.Order ord = 2; }`
	depPicky  = `syntax = "proto3"; package acme.picky; import "base/common.proto"; message View { acme.base.Other other = 1; }`
	depSolo   = `syntax = "proto3"; package acme.solo; message Alone { string id = 1; }`
)

// fileSpec is a (path, content) pair for registration requests.
type fileSpec struct {
	path    string
	content string
}

func registerFiles(t *testing.T, svc *Service, pkg, version string, files ...fileSpec) *RegisterVersionResponse {
	t.Helper()
	reqFiles := make([]schema.SourceFile, 0, len(files))
	for _, f := range files {
		reqFiles = append(reqFiles, schema.SourceFile{Path: f.path, Content: f.content})
	}
	resp, err := svc.RegisterVersion(context.Background(), connect.NewRequest(&RegisterVersionRequest{
		Package: pkg, Version: version, Files: reqFiles,
	}))
	if err != nil {
		t.Fatalf("register %s@%s: %v", pkg, version, err)
	}
	return resp.Msg
}

// registerDiamond builds: base <- mid, base <- side, mid+side <- top.
func registerDiamond(t *testing.T, svc *Service) {
	t.Helper()
	registerFiles(t, svc, "acme.base", "v1", fileSpec{"base/common.proto", depBaseV1})
	registerFiles(t, svc, "acme.mid", "v1", fileSpec{"mid/invoice.proto", depMidV1})
	registerFiles(t, svc, "acme.side", "v1", fileSpec{"side/order.proto", depSideV1})
	registerFiles(t, svc, "acme.top", "v1", fileSpec{"top/batch.proto", depTopV1})
}

func TestCrossPackageRegistrationLocksDependencies(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	registerFiles(t, svc, "acme.base", "v1", fileSpec{"base/common.proto", depBaseV1})
	registerFiles(t, svc, "acme.mid", "v1", fileSpec{"mid/invoice.proto", depMidV1})

	resp, err := svc.ListDependencies(ctx, connect.NewRequest(&ListDependenciesRequest{Package: "acme.mid", Version: "v1"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Dependencies) != 1 {
		t.Fatalf("dependencies = %+v, want exactly one", resp.Msg.Dependencies)
	}
	dep := resp.Msg.Dependencies[0]
	if dep.Package != "acme.base" || dep.Version != "v1" || dep.ContentHash == "" {
		t.Fatalf("lock = %+v, want acme.base@v1 with a digest", dep)
	}

	// The lock digest must equal the registered content hash of base v1.
	versions, err := svc.ListVersions(ctx, connect.NewRequest(&ListVersionsRequest{Package: "acme.base"}))
	if err != nil {
		t.Fatal(err)
	}
	if versions.Msg.Versions[0].ContentHash != dep.ContentHash {
		t.Fatalf("lock digest %s != registered hash %s", dep.ContentHash, versions.Msg.Versions[0].ContentHash)
	}
}

func TestMissingDependencyRejected(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	_, err := svc.RegisterVersion(ctx, connect.NewRequest(&RegisterVersionRequest{
		Package: "acme.ghost", Version: "v1",
		Files: []schema.SourceFile{{Path: "ghost/g.proto", Content: `syntax = "proto3"; package acme.ghost; import "no/such.proto"; message G { no.such.M m = 1; }`}},
	}))
	if err == nil {
		t.Fatal("expected rejection for missing dependency")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %s (%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "no/such.proto") {
		t.Fatalf("error should name the missing import: %v", err)
	}
	// Nothing may be stored.
	if _, err := svc.store.GetVersion(ctx, "acme.ghost", "v1"); err != ErrNotFound {
		t.Fatalf("rejected registration must not be stored, err = %v", err)
	}
}

func TestDependencyCycleRejectedAtomically(t *testing.T) {
	newSvc := func() *Service { return NewService(NewMemStore()) }

	// Two shapes of the same cycle, both must be refused:
	//  - same file path: the compiler hits the file-level import cycle
	//    through the pinned closure;
	//  - new file path: compilation succeeds, and the lock-graph check
	//    rejects the package-level cycle.
	t.Run("file level", func(t *testing.T) {
		svc := newSvc()
		ctx := context.Background()
		registerFiles(t, svc, "acme.cyca", "v1", fileSpec{"cyca/a.proto",
			`syntax = "proto3"; package acme.cyca; message A { string id = 1; }`})
		registerFiles(t, svc, "acme.cycb", "v1", fileSpec{"cycb/b.proto",
			`syntax = "proto3"; package acme.cycb; import "cyca/a.proto"; message B { acme.cyca.A a = 1; }`})

		_, err := svc.RegisterVersion(ctx, connect.NewRequest(&RegisterVersionRequest{
			Package: "acme.cyca", Version: "v2",
			Files: []schema.SourceFile{{Path: "cyca/a.proto", Content: `syntax = "proto3"; package acme.cyca; import "cycb/b.proto"; message A { string id = 1; acme.cycb.B b = 2; }`}},
		}))
		assertCycleRejectedAtomically(t, svc, err)
	})

	t.Run("package level", func(t *testing.T) {
		svc := newSvc()
		ctx := context.Background()
		registerFiles(t, svc, "acme.cyca", "v1", fileSpec{"cyca/a.proto",
			`syntax = "proto3"; package acme.cyca; message A { string id = 1; }`})
		registerFiles(t, svc, "acme.cycb", "v1", fileSpec{"cycb/b.proto",
			`syntax = "proto3"; package acme.cycb; import "cyca/a.proto"; message B { acme.cyca.A a = 1; }`})

		// cyca v2 keeps a.proto untouched and adds a2.proto importing
		// cycb: no file-level cycle, but the package lock graph closes
		// acme.cyca -> acme.cycb -> acme.cyca.
		_, err := svc.RegisterVersion(ctx, connect.NewRequest(&RegisterVersionRequest{
			Package: "acme.cyca", Version: "v2",
			Files: []schema.SourceFile{
				{Path: "cyca/a.proto", Content: `syntax = "proto3"; package acme.cyca; message A { string id = 1; }`},
				{Path: "cyca/a2.proto", Content: `syntax = "proto3"; package acme.cyca; import "cycb/b.proto"; message A2 { acme.cycb.B b = 1; }`},
			},
		}))
		assertCycleRejectedAtomically(t, svc, err)
	})
}

func assertCycleRejectedAtomically(t *testing.T, svc *Service, err error) {
	t.Helper()
	ctx := context.Background()
	if err == nil {
		t.Fatal("expected cycle rejection")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %s (%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "dependency cycle") {
		t.Fatalf("error should explain the cycle: %v", err)
	}

	// Atomic: no trace of cyca@v2 anywhere.
	if _, err := svc.store.GetVersion(ctx, "acme.cyca", "v2"); err != ErrNotFound {
		t.Fatalf("cyca v2 must not be stored, err = %v", err)
	}
	versions, err := svc.ListVersions(ctx, connect.NewRequest(&ListVersionsRequest{Package: "acme.cyca"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(versions.Msg.Versions) != 1 || versions.Msg.Versions[0].Version != "v1" {
		t.Fatalf("cyca versions = %+v, want only v1", versions.Msg.Versions)
	}
	// Path ownership still points at v1, not at the rejected v2.
	owner, ownerVer, err := svc.store.FindPathOwner(ctx, "cyca/a.proto")
	if err != nil || owner != "acme.cyca" || ownerVer != "v1" {
		t.Fatalf("path owner = %s@%s err=%v, want acme.cyca@v1", owner, ownerVer, err)
	}
	// cyca@v1 has no locks, and the rejected attempt added none.
	if deps, _ := svc.store.GetDeps(ctx, "acme.cyca", "v1"); len(deps) != 0 {
		t.Fatalf("cyca v1 deps = %+v, want none", deps)
	}
	// Re-registering cyca v1 with identical content is still idempotent.
	resp := registerFiles(t, svc, "acme.cyca", "v1", fileSpec{"cyca/a.proto",
		`syntax = "proto3"; package acme.cyca; message A { string id = 1; }`})
	if !resp.AlreadyExisted {
		t.Fatal("idempotent re-registration of cyca v1 expected")
	}
}

// tamperStore corrupts the content hash of chosen versions on the second
// read, simulating a store whose registered content no longer matches the
// digest captured when a lock was taken.
type tamperStore struct {
	Store
	corrupt map[string]bool
	reads   map[string]int
}

func (t *tamperStore) GetVersion(ctx context.Context, pkg, version string) (*Version, error) {
	v, err := t.Store.GetVersion(ctx, pkg, version)
	if err != nil {
		return nil, err
	}
	key := pkg + "@" + version
	t.reads[key]++
	if t.corrupt[key] && t.reads[key] > 1 {
		v.ContentHash = []byte("corrupted-digest")
	}
	return v, nil
}

func TestDigestMismatchRejected(t *testing.T) {
	mem := NewMemStore()
	svc := NewService(&tamperStore{Store: mem, corrupt: map[string]bool{"acme.base@v1": true}, reads: map[string]int{}})
	ctx := context.Background()
	registerFiles(t, svc, "acme.base", "v1", fileSpec{"base/common.proto", depBaseV1})

	_, err := svc.RegisterVersion(ctx, connect.NewRequest(&RegisterVersionRequest{
		Package: "acme.mid", Version: "v1",
		Files: []schema.SourceFile{{Path: "mid/invoice.proto", Content: depMidV1}},
	}))
	if err == nil {
		t.Fatal("expected digest mismatch rejection")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("error should report the digest mismatch: %v", err)
	}
	if _, err := mem.GetVersion(ctx, "acme.mid", "v1"); err != ErrNotFound {
		t.Fatalf("rejected registration must not be stored, err = %v", err)
	}
}

func TestDiamondVersionConflictRejected(t *testing.T) {
	svc := newService()
	// mid locks base v1; then base v2 is published; side locks base v2.
	registerFiles(t, svc, "acme.base", "v1", fileSpec{"base/common.proto", depBaseV1})
	registerFiles(t, svc, "acme.mid", "v1", fileSpec{"mid/invoice.proto", depMidV1})
	registerFiles(t, svc, "acme.base", "v2", fileSpec{"base/common.proto", depBaseV2})
	registerFiles(t, svc, "acme.side", "v1", fileSpec{"side/order.proto", depSideV1})

	// top imports both mid and side: their pinned closures disagree on
	// base/common.proto (v1 vs v2 content).
	_, err := svc.RegisterVersion(context.Background(), connect.NewRequest(&RegisterVersionRequest{
		Package: "acme.top", Version: "v1",
		Files: []schema.SourceFile{{Path: "top/batch.proto", Content: depTopV1}},
	}))
	if err == nil {
		t.Fatal("expected diamond version conflict")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %s (%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("error should explain the conflict: %v", err)
	}
}

func TestWellKnownTypesAcrossDependencyBoundary(t *testing.T) {
	svc := newService()
	// base uses a well-known type; mid depends on base. The WKT must be
	// supplied by the compiler's standard imports on both sides, never
	// conflict between the two pinned closures.
	registerFiles(t, svc, "acme.wkt", "v1", fileSpec{"wkt/base.proto",
		`syntax = "proto3"; package acme.wkt; import "google/protobuf/timestamp.proto"; message Event { string id = 1; google.protobuf.Timestamp at = 2; }`})
	registerFiles(t, svc, "acme.wktmid", "v1", fileSpec{"wktmid/mid.proto",
		`syntax = "proto3"; package acme.wktmid; import "google/protobuf/duration.proto"; import "wkt/base.proto"; message Window { acme.wkt.Event ev = 1; google.protobuf.Duration span = 2; }`})

	resp, err := svc.ListDependencies(context.Background(), connect.NewRequest(&ListDependenciesRequest{
		Package: "acme.wktmid", Version: "v1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Dependencies) != 1 || resp.Msg.Dependencies[0].Package != "acme.wkt" {
		t.Fatalf("locks = %+v, want only acme.wkt (WKTs are not registry dependencies)", resp.Msg.Dependencies)
	}
}

func TestHistoricalLocksSurviveDependencyUpdates(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	registerFiles(t, svc, "acme.base", "v1", fileSpec{"base/common.proto", depBaseV1})
	registerFiles(t, svc, "acme.mid", "v1", fileSpec{"mid/invoice.proto", depMidV1})

	// base publishes v2; mid v1's lock must still point at base v1.
	registerFiles(t, svc, "acme.base", "v2", fileSpec{"base/common.proto", depBaseV2})
	resp, err := svc.ListDependencies(ctx, connect.NewRequest(&ListDependenciesRequest{Package: "acme.mid", Version: "v1"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Dependencies) != 1 || resp.Msg.Dependencies[0].Version != "v1" {
		t.Fatalf("mid v1 lock = %+v, want base v1", resp.Msg.Dependencies)
	}

	// A new mid version registered now locks the latest base (v2).
	registerFiles(t, svc, "acme.mid", "v2", fileSpec{"mid/invoice.proto", depMidV1})
	resp, err = svc.ListDependencies(ctx, connect.NewRequest(&ListDependenciesRequest{Package: "acme.mid", Version: "v2"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Dependencies) != 1 || resp.Msg.Dependencies[0].Version != "v2" {
		t.Fatalf("mid v2 lock = %+v, want base v2", resp.Msg.Dependencies)
	}

	// Re-registering mid v1 with identical own content stays idempotent
	// even though the import now resolves to base v2: a version's identity
	// is its own content, its dependency context is the lock.
	again := registerFiles(t, svc, "acme.mid", "v1", fileSpec{"mid/invoice.proto", depMidV1})
	if !again.AlreadyExisted {
		t.Fatal("re-registration of mid v1 must be idempotent")
	}
	// And the historical lock is still base v1.
	resp, err = svc.ListDependencies(ctx, connect.NewRequest(&ListDependenciesRequest{Package: "acme.mid", Version: "v1"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Dependencies[0].Version != "v1" {
		t.Fatalf("mid v1 lock changed to %+v", resp.Msg.Dependencies[0])
	}
}

func TestRegisterWithUnknownExplicitBaseRejected(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	registerFiles(t, svc, "acme.solo", "v1", fileSpec{"solo/alone.proto", depSolo})
	// An explicitly requested base that does not exist must fail loudly —
	// silently skipping the check would defeat require_compatible.
	_, err := svc.RegisterVersion(ctx, connect.NewRequest(&RegisterVersionRequest{
		Package: "acme.solo", Version: "v2", BaseVersion: "typo",
		Files:             []schema.SourceFile{{Path: "solo/alone.proto", Content: depSolo}},
		RequireCompatible: true,
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("code = %s (%v), want NotFound", connect.CodeOf(err), err)
	}
	if _, err := svc.store.GetVersion(ctx, "acme.solo", "v2"); err != ErrNotFound {
		t.Fatalf("v2 must not be stored, err = %v", err)
	}
}
