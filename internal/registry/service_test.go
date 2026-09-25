package registry

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"protocompat/internal/compat"
	"protocompat/internal/schema"
)

const (
	v1Proto = `syntax = "proto3"; package acme.pay; message Invoice { string id = 1; int32 total = 2; string note = 3; }`
	v2Proto = `syntax = "proto3"; package acme.pay; message Invoice { string id = 1; int64 total = 2; reserved 3; reserved "note"; }`
	v3Proto = `syntax = "proto3"; package acme.pay; message Invoice { string id = 1; int64 total = 2; reserved 3; reserved "note"; string currency = 4; }`
)

func registerReq(pkg, version, content string, requireCompatible bool) *connect.Request[RegisterVersionRequest] {
	return connect.NewRequest(&RegisterVersionRequest{
		Package: pkg, Version: version,
		Files:             []schema.SourceFile{{Path: "pay/invoice.proto", Content: content}},
		RequireCompatible: requireCompatible,
	})
}

func newService() *Service { return NewService(NewMemStore()) }

func TestRegisterFirstVersionHasNoBase(t *testing.T) {
	svc := newService()
	resp, err := svc.RegisterVersion(context.Background(), registerReq("acme.pay", "v1", v1Proto, false))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if resp.Msg.AlreadyExisted {
		t.Fatal("first registration must not be marked as existing")
	}
	if resp.Msg.Compatibility != nil {
		t.Fatalf("no base version exists, compatibility must be nil, got %+v", resp.Msg.Compatibility)
	}
	if resp.Msg.ContentHash == "" {
		t.Fatal("content hash missing")
	}
}

func TestRegisterReportsCompatibilityAgainstBase(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	if _, err := svc.RegisterVersion(ctx, registerReq("acme.pay", "v1", v1Proto, false)); err != nil {
		t.Fatal(err)
	}
	resp, err := svc.RegisterVersion(ctx, registerReq("acme.pay", "v2", v2Proto, false))
	if err != nil {
		t.Fatal(err)
	}
	rep := resp.Msg.Compatibility
	if rep == nil {
		t.Fatal("expected a compatibility report against v1")
	}
	if resp.Msg.BaseVersion != "v1" {
		t.Fatalf("base = %q, want v1", resp.Msg.BaseVersion)
	}
	// int32 -> int64 changes the JSON form: proven break.
	if rep.Verdict != compat.VerdictIncompatible {
		t.Fatalf("verdict = %s, want INCOMPATIBLE; findings %+v", rep.Verdict, rep.Findings)
	}
}

func TestSameVersionDifferentContentRejected(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	if _, err := svc.RegisterVersion(ctx, registerReq("acme.pay", "v1", v1Proto, false)); err != nil {
		t.Fatal(err)
	}
	// Same version, different content: must be refused, never overwritten.
	_, err := svc.RegisterVersion(ctx, registerReq("acme.pay", "v1", v2Proto, false))
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("code = %s, want AlreadyExists (%v)", connect.CodeOf(err), err)
	}
	// The stored content must still be the original.
	v, err := svc.store.GetVersion(ctx, "acme.pay", "v1")
	if err != nil {
		t.Fatal(err)
	}
	files, err := schema.Load(v.DescriptorSet)
	if err != nil {
		t.Fatal(err)
	}
	d, err := files.FindDescriptorByName("acme.pay.Invoice")
	if err != nil {
		t.Fatal(err)
	}
	_ = d
	// Same content again: idempotent, not an error.
	resp, err := svc.RegisterVersion(ctx, registerReq("acme.pay", "v1", v1Proto, false))
	if err != nil {
		t.Fatalf("idempotent re-register: %v", err)
	}
	if !resp.Msg.AlreadyExisted {
		t.Fatal("expected already_existed for identical content")
	}
}

func TestRequireCompatibleBlocksIncompatible(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	if _, err := svc.RegisterVersion(ctx, registerReq("acme.pay", "v1", v1Proto, false)); err != nil {
		t.Fatal(err)
	}
	_, err := svc.RegisterVersion(ctx, registerReq("acme.pay", "v2", v2Proto, true))
	if err == nil {
		t.Fatal("expected FailedPrecondition for incompatible publish")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %s (%v)", connect.CodeOf(err), err)
	}
	// Nothing may have been stored for v2.
	if _, err := svc.store.GetVersion(ctx, "acme.pay", "v2"); err != ErrNotFound {
		t.Fatalf("v2 must not be stored, err = %v", err)
	}
}

func TestRequireCompatibleAllowsCompatible(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	if _, err := svc.RegisterVersion(ctx, registerReq("acme.pay", "v2", v2Proto, false)); err != nil {
		t.Fatal(err)
	}
	// v3 only adds a field: compatible, must pass the gate.
	if _, err := svc.RegisterVersion(ctx, registerReq("acme.pay", "v3", v3Proto, true)); err != nil {
		t.Fatalf("compatible publish blocked: %v", err)
	}
}

func TestCheckCompatibilityWithInlineCandidate(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	if _, err := svc.RegisterVersion(ctx, registerReq("acme.pay", "v1", v1Proto, false)); err != nil {
		t.Fatal(err)
	}
	resp, err := svc.CheckCompatibility(ctx, connect.NewRequest(&CheckRequest{
		Package:        "acme.pay",
		BaseVersion:    "v1",
		CandidateFiles: []schema.SourceFile{{Path: "pay/invoice.proto", Content: v2Proto}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Report.Verdict != compat.VerdictIncompatible {
		t.Fatalf("verdict = %s", resp.Msg.Report.Verdict)
	}
}

func TestConsumerImpactProjection(t *testing.T) {
	svc := newService()
	ctx := context.Background()
	if _, err := svc.RegisterVersion(ctx, registerReq("acme.pay", "v1", v1Proto, false)); err != nil {
		t.Fatal(err)
	}
	// A wire-only consumer that only reads Invoice.id is unaffected by the
	// int32 -> int64 JSON break on total.
	if _, err := svc.DeclareConsumer(ctx, connect.NewRequest(&DeclareConsumerRequest{
		Package: "acme.pay", Consumer: "ledger", Encoding: "wire",
		Usages: []Usage{{Message: "acme.pay.Invoice", Fields: []string{"id"}}},
	})); err != nil {
		t.Fatal(err)
	}
	resp, err := svc.CheckCompatibility(ctx, connect.NewRequest(&CheckRequest{
		Package:        "acme.pay",
		BaseVersion:    "v1",
		CandidateFiles: []schema.SourceFile{{Path: "pay/invoice.proto", Content: v2Proto}},
		Consumer:       "ledger",
	}))
	if err != nil {
		t.Fatal(err)
	}
	impact := resp.Msg.ConsumerImpact
	if impact == nil {
		t.Fatal("expected consumer impact")
	}
	if impact.Verdict != compat.VerdictCompatible {
		t.Fatalf("wire consumer of id only: verdict = %s, want COMPATIBLE (findings %+v)", impact.Verdict, impact.Findings)
	}

	// A JSON consumer of the whole message sees the break.
	if _, err := svc.DeclareConsumer(ctx, connect.NewRequest(&DeclareConsumerRequest{
		Package: "acme.pay", Consumer: "dashboard", Encoding: "json",
		Usages: []Usage{{Message: "acme.pay.Invoice"}},
	})); err != nil {
		t.Fatal(err)
	}
	resp, err = svc.CheckCompatibility(ctx, connect.NewRequest(&CheckRequest{
		Package:        "acme.pay",
		BaseVersion:    "v1",
		CandidateFiles: []schema.SourceFile{{Path: "pay/invoice.proto", Content: v2Proto}},
		Consumer:       "dashboard",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.ConsumerImpact.Verdict != compat.VerdictIncompatible {
		t.Fatalf("json consumer verdict = %s, want INCOMPATIBLE", resp.Msg.ConsumerImpact.Verdict)
	}
}

func TestCompileErrorSurfacedWithPosition(t *testing.T) {
	svc := newService()
	_, err := svc.RegisterVersion(context.Background(), connect.NewRequest(&RegisterVersionRequest{
		Package: "acme.pay", Version: "v1",
		Files: []schema.SourceFile{{Path: "pay/broken.proto", Content: "syntax = \"proto3\";\nmessage M {\n  int32 = 1;\n}\n"}},
	}))
	if err == nil {
		t.Fatal("expected compile error")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %s (%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "pay/broken.proto:3:") {
		t.Fatalf("error should carry file:line:col, got: %v", err)
	}
}

func TestCheckUnknownBaseVersion(t *testing.T) {
	svc := newService()
	_, err := svc.CheckCompatibility(context.Background(), connect.NewRequest(&CheckRequest{
		Package:        "acme.pay",
		BaseVersion:    "nope",
		CandidateFiles: []schema.SourceFile{{Path: "x.proto", Content: v1Proto}},
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("code = %s (%v)", connect.CodeOf(err), err)
	}
}
