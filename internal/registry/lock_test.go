package registry

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"protocompat/internal/deps"
	"protocompat/internal/schema"
)

func registerCross(t *testing.T, svc *Service, pkg, version, path, content string, pins []deps.Pin) *RegisterVersionResponse {
	t.Helper()
	resp, err := svc.RegisterVersion(context.Background(), connect.NewRequest(&RegisterVersionRequest{
		Package: pkg, Version: version,
		Files: []schema.SourceFile{{Path: path, Content: content}}, Pins: pins,
	}))
	if err != nil {
		t.Fatalf("register %s@%s: %v", pkg, version, err)
	}
	return resp.Msg
}

func TestRegisterLocksAndSummary(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	r1 := registerCross(t, svc, "acme.common", "v1", "base/common.proto",
		`syntax = "proto3"; package acme.common; message Money { int64 units = 1; }`, nil)
	if len(r1.Locks) != 0 || r1.LockDigest != "" {
		t.Fatalf("root package should have no locks, got %+v", r1)
	}

	r2 := registerCross(t, svc, "acme.billing", "v1", "middle/invoice.proto",
		`syntax = "proto3"; package acme.billing; import "base/common.proto";
message Line { acme.common.Money amount = 1; }`, nil)
	if len(r2.Locks) != 1 || r2.Locks[0].Package != "acme.common" || r2.Locks[0].Version != "v1" {
		t.Fatalf("locks = %+v", r2.Locks)
	}
	if r2.Locks[0].Digest != r1.ContentHash {
		t.Fatalf("lock digest %s != dependency content hash %s", r2.Locks[0].Digest, r1.ContentHash)
	}
	if r2.LockDigest == "" {
		t.Fatal("empty lock summary digest")
	}

	// GetDependencies echoes the stored summary.
	dresp, err := svc.GetDependencies(ctx, connect.NewRequest(&GetDependenciesRequest{
		Package: "acme.billing", Version: "v1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if dresp.Msg.Summary.Digest != r2.LockDigest || len(dresp.Msg.Summary.Locks) != 1 {
		t.Fatalf("summary = %+v", dresp.Msg.Summary)
	}
}

func TestExplicitPinVersionAndDigest(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	r1 := registerCross(t, svc, "acme.common", "v1", "base/common.proto",
		`syntax = "proto3"; package acme.common; message M { string a = 1; }`, nil)
	registerCross(t, svc, "acme.common", "v2", "base/common.proto",
		`syntax = "proto3"; package acme.common; message M { string a = 1; string b = 2; }`, nil)

	// Explicit pin to v1 even though v2 is latest.
	resp := registerCross(t, svc, "acme.user", "v1", "u/u.proto",
		`syntax = "proto3"; package acme.user; import "base/common.proto";
message U { acme.common.M m = 1; }`,
		[]deps.Pin{{Package: "acme.common", Version: "v1", Digest: r1.ContentHash}})
	if resp.Locks[0].Version != "v1" {
		t.Fatalf("pin not honored: %+v", resp.Locks)
	}

	// Wrong digest is rejected before storage.
	_, err := svc.RegisterVersion(context.Background(), connect.NewRequest(&RegisterVersionRequest{
		Package: "acme.baddigest", Version: "v1",
		Files: []schema.SourceFile{{Path: "b/b.proto",
			Content: `syntax = "proto3"; package acme.baddigest; import "base/common.proto"; message B { acme.common.M m = 1; }`}},
		Pins: []deps.Pin{{Package: "acme.common", Version: "v1", Digest: strings.Repeat("0", 64)}},
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("digest mismatch code = %s (%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "dependency summary mismatch") {
		t.Fatalf("error should name the mismatch: %v", err)
	}
	if _, err := svc.store.GetVersion(ctx, "acme.baddigest", "v1"); err != ErrNotFound {
		t.Fatal("rejected registration left a version behind")
	}

	// Pin of an unregistered version is rejected.
	_, err = svc.RegisterVersion(context.Background(), connect.NewRequest(&RegisterVersionRequest{
		Package: "acme.missing", Version: "v1",
		Files: []schema.SourceFile{{Path: "m/m.proto",
			Content: `syntax = "proto3"; package acme.missing; import "base/common.proto"; message M { acme.common.M m = 1; }`}},
		Pins: []deps.Pin{{Package: "acme.common", Version: "v99"}},
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("missing pin code = %s (%v)", connect.CodeOf(err), err)
	}
}

func TestUnusedPinRejected(t *testing.T) {
	svc := newService()
	registerCross(t, svc, "acme.common", "v1", "base/common.proto",
		`syntax = "proto3"; package acme.common; message M { string a = 1; }`, nil)
	_, err := svc.RegisterVersion(context.Background(), connect.NewRequest(&RegisterVersionRequest{
		Package: "acme.user", Version: "v1",
		Files: []schema.SourceFile{{Path: "u/u.proto",
			Content: `syntax = "proto3"; package acme.user; message U { string x = 1; }`}},
		Pins: []deps.Pin{{Package: "acme.common", Version: "v1"}},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("unused pin code = %s (%v)", connect.CodeOf(err), err)
	}
}

func TestPackageCycleAtomicRejection(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	registerCross(t, svc, "acme.common", "v1", "base/common.proto",
		`syntax = "proto3"; package acme.common; message Money { int64 units = 1; }`, nil)
	registerCross(t, svc, "acme.billing", "v1", "middle/invoice.proto",
		`syntax = "proto3"; package acme.billing; import "base/common.proto";
message Line { acme.common.Money amount = 1; }`, nil)

	// New common version imports billing -> package cycle.
	_, err := svc.RegisterVersion(ctx, connect.NewRequest(&RegisterVersionRequest{
		Package: "acme.common", Version: "v2",
		Files: []schema.SourceFile{
			{Path: "base/common.proto", Content: `syntax = "proto3"; package acme.common; message Money { int64 units = 1; int64 c = 2; }`},
			{Path: "base/ref.proto", Content: `syntax = "proto3"; package acme.common; import "middle/invoice.proto";
message Ref { acme.billing.Line item = 1; }`},
		},
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("cycle code = %s (%v)", connect.CodeOf(err), err)
	}
	if _, err := svc.store.GetVersion(ctx, "acme.common", "v2"); err != ErrNotFound {
		t.Fatalf("rejected cycle left dirty version: %v", err)
	}
	// The new path claim must have rolled back too: a fresh package can
	// own base/ref.proto.
	registerCross(t, svc, "acme.fresh", "v1", "base/ref.proto",
		`syntax = "proto3"; package acme.fresh; message Ref { int64 n = 1; }`, nil)
}

func TestAnalyzeImpactMemoized(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	registerCross(t, svc, "acme.common", "v1", "base/common.proto",
		`syntax = "proto3"; package acme.common; message Money { string currency = 1; }`, nil)
	registerCross(t, svc, "acme.billing", "v1", "middle/invoice.proto",
		`syntax = "proto3"; package acme.billing; import "base/common.proto";
message Line { acme.common.Money amount = 1; }`, nil)
	registerCross(t, svc, "acme.common", "v2", "base/common.proto",
		`syntax = "proto3"; package acme.common; message Money { int64 currency = 1; }`, nil)

	first, err := svc.AnalyzeImpact(ctx, connect.NewRequest(&AnalyzeImpactRequest{
		Package: "acme.common", BaseVersion: "v1", CandidateVersion: "v2",
	}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.AnalyzeImpact(ctx, connect.NewRequest(&AnalyzeImpactRequest{
		Package: "acme.common", BaseVersion: "v1", CandidateVersion: "v2",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if first.Msg.InputDigest == "" || first.Msg.InputDigest != second.Msg.InputDigest {
		t.Fatalf("digests differ: %s vs %s", first.Msg.InputDigest, second.Msg.InputDigest)
	}
	if len(first.Msg.Analysis.Nodes) != 1 {
		t.Fatalf("nodes = %d", len(first.Msg.Analysis.Nodes))
	}
	n := first.Msg.Analysis.Nodes[0]
	if n.Package != "acme.billing" || n.Status != "DIRECT_IMPACTED" {
		t.Fatalf("node = %+v", n)
	}
	if hex.DecodedLen(len(first.Msg.InputDigest)) != 32 && len(first.Msg.InputDigest) != 64 {
		t.Fatalf("digest not a sha256 hex: %s", first.Msg.InputDigest)
	}
}

func TestPathOwnedByOtherPackageRejected(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	registerCross(t, svc, "acme.first", "v1", "shared/x.proto",
		`syntax = "proto3"; package acme.first; message X { int32 a = 1; }`, nil)
	// A different package submitting the same path must fail, even with
	// no imports involved.
	_, err := svc.RegisterVersion(ctx, connect.NewRequest(&RegisterVersionRequest{
		Package: "acme.second", Version: "v1",
		Files: []schema.SourceFile{{Path: "shared/x.proto",
			Content: `syntax = "proto3"; package acme.second; message X { int32 a = 1; }`}},
	}))
	if err == nil {
		t.Fatal("path theft accepted")
	}
	if code := connect.CodeOf(err); code != connect.CodeFailedPrecondition {
		t.Fatalf("path theft code = %s (%v)", code, err)
	}
	if _, err := svc.store.GetVersion(ctx, "acme.second", "v1"); err != ErrNotFound {
		t.Fatal("rejected second package was stored")
	}
}

func TestAnalyzeImpactUnknownVersions(t *testing.T) {
	svc := newService()
	_, err := svc.AnalyzeImpact(context.Background(), connect.NewRequest(&AnalyzeImpactRequest{
		Package: "acme.common", BaseVersion: "v1", CandidateVersion: "v2",
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("code = %s (%v)", connect.CodeOf(err), err)
	}
}

func TestIdempotentReregisterPinImmutability(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	registerCross(t, svc, "acme.common", "v1", "base/common.proto",
		`syntax = "proto3"; package acme.common; message M { string a = 1; }`, nil)
	registerCross(t, svc, "acme.common", "v2", "base/common.proto",
		`syntax = "proto3"; package acme.common; message M { string a = 1; string b = 2; }`, nil)
	registerCross(t, svc, "acme.user", "v1", "u/u.proto",
		`syntax = "proto3"; package acme.user; import "base/common.proto";
message U { acme.common.M m = 1; }`,
		[]deps.Pin{{Package: "acme.common", Version: "v1"}})

	// Same content, no pins: idempotent success.
	resp, err := svc.RegisterVersion(ctx, connect.NewRequest(&RegisterVersionRequest{
		Package: "acme.user", Version: "v1",
		Files: []schema.SourceFile{{Path: "u/u.proto",
			Content: `syntax = "proto3"; package acme.user; import "base/common.proto";
message U { acme.common.M m = 1; }`}},
	}))
	if err != nil || resp.Msg.AlreadyExisted != true {
		t.Fatalf("idempotent retry: err=%v existed=%v", err, resp != nil && resp.Msg.AlreadyExisted)
	}

	// Same content, same pin: idempotent success and echoes stored lock.
	resp, err = svc.RegisterVersion(ctx, connect.NewRequest(&RegisterVersionRequest{
		Package: "acme.user", Version: "v1",
		Files: []schema.SourceFile{{Path: "u/u.proto",
			Content: `syntax = "proto3"; package acme.user; import "base/common.proto";
message U { acme.common.M m = 1; }`}},
		Pins: []deps.Pin{{Package: "acme.common", Version: "v1"}},
	}))
	if err != nil {
		t.Fatalf("matching pin retry: %v", err)
	}
	if resp.Msg.Locks[0].Version != "v1" {
		t.Fatalf("stored lock changed: %+v", resp.Msg.Locks)
	}

	// Same content, pin now demands v2: immutability refusal.
	_, err = svc.RegisterVersion(ctx, connect.NewRequest(&RegisterVersionRequest{
		Package: "acme.user", Version: "v1",
		Files: []schema.SourceFile{{Path: "u/u.proto",
			Content: `syntax = "proto3"; package acme.user; import "base/common.proto";
message U { acme.common.M m = 1; }`}},
		Pins: []deps.Pin{{Package: "acme.common", Version: "v2"}},
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("pin rewrite code = %s (%v)", connect.CodeOf(err), err)
	}
}
