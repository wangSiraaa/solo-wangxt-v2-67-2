package compat

import (
	"context"
	"testing"

	"protocompat/internal/schema"
)

func compilePair(t *testing.T, oldSrc, newSrc string) (old, new_ *schema.Compiled) {
	t.Helper()
	ctx := context.Background()
	old, err := schema.Compile(ctx, []schema.SourceFile{{Path: "m.proto", Content: oldSrc}})
	if err != nil {
		t.Fatalf("compile old: %v", err)
	}
	new_, err = schema.Compile(ctx, []schema.SourceFile{{Path: "m.proto", Content: newSrc}})
	if err != nil {
		t.Fatalf("compile new: %v", err)
	}
	return old, new_
}

func TestSampleJSONMismatchInt32ToInt64(t *testing.T) {
	oldC, newC := compilePair(t,
		`syntax = "proto3"; package p; message C { string name = 1; int32 count = 2; }`,
		`syntax = "proto3"; package p; message C { string name = 1; int64 count = 2; }`)
	results := VerifySamples(oldC.Files, newC.Files, []Sample{
		{Name: "s", Message: "p.C", Encoding: "json", Data: `{"name":"hits","count":5}`},
	})
	if len(results) != 1 {
		t.Fatalf("results = %d", len(results))
	}
	r := results[0]
	if r.Status != "MISMATCH" {
		t.Fatalf("status = %s, want MISMATCH (%+v)", r.Status, r)
	}
	if !r.WireEqual {
		t.Fatal("wire should stay equal for int32 -> int64 varint widening")
	}
	if r.JSONEqual {
		t.Fatal("json must differ: number vs quoted string")
	}
	if len(r.Diffs) == 0 || r.Diffs[0].Path != "p.C.count" {
		t.Fatalf("diff path = %+v, want p.C.count", r.Diffs)
	}
}

func TestSampleWireRoundTripOK(t *testing.T) {
	src := `syntax = "proto3"; package p; message C { string name = 1; int32 count = 2; }`
	oldC, newC := compilePair(t, src, src)
	results := VerifySamples(oldC.Files, newC.Files, []Sample{
		{Name: "w", Message: "p.C", Encoding: "wire", Data: "CgRoaXRzEAU="},
	})
	if results[0].Status != "OK" {
		t.Fatalf("status = %s, want OK (%+v)", results[0].Status, results[0])
	}
}

func TestSampleParseErrorUnderNewSchema(t *testing.T) {
	oldC, newC := compilePair(t,
		`syntax = "proto3"; package p; message C { string a = 1; string b = 2; }`,
		`syntax = "proto3"; package p; message C { oneof pick { string a = 1; string b = 2; } }`)
	// JSON with both oneof members set is rejected by the new schema.
	results := VerifySamples(oldC.Files, newC.Files, []Sample{
		{Name: "both", Message: "p.C", Encoding: "json", Data: `{"a":"x","b":"y"}`},
	})
	if results[0].Status != "PARSE_ERROR" {
		t.Fatalf("status = %s, want PARSE_ERROR (%+v)", results[0].Status, results[0])
	}
	findings := sampleFindings(results)
	if len(findings) != 1 || findings[0].Code != "SAMPLE_PARSE_FAILED" || findings[0].Severity != SeverityFail {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestSampleFieldRemovedLeavesUnknown(t *testing.T) {
	oldC, newC := compilePair(t,
		`syntax = "proto3"; package p; message C { string keep = 1; string gone = 2; }`,
		`syntax = "proto3"; package p; message C { string keep = 1; reserved 2; reserved "gone"; }`)
	results := VerifySamples(oldC.Files, newC.Files, []Sample{
		{Name: "j", Message: "p.C", Encoding: "json", Data: `{"keep":"a","gone":"b"}`},
	})
	// New schema rejects the removed JSON name outright.
	if results[0].Status != "PARSE_ERROR" {
		t.Fatalf("status = %s (%+v)", results[0].Status, results[0])
	}
}

func TestSampleUnknownMessageIsWarnNotPass(t *testing.T) {
	oldC, newC := compilePair(t,
		`syntax = "proto3"; package p; message C { string a = 1; }`,
		`syntax = "proto3"; package p; message C { string a = 1; }`)
	results := VerifySamples(oldC.Files, newC.Files, []Sample{
		{Name: "bad", Message: "p.DoesNotExist", Encoding: "json", Data: `{}`},
	})
	if results[0].Status != "ERROR" {
		t.Fatalf("status = %s", results[0].Status)
	}
	findings := sampleFindings(results)
	if len(findings) != 1 || findings[0].Code != "SAMPLE_UNVERIFIABLE" || findings[0].Severity != SeverityWarn {
		t.Fatalf("unverifiable sample must be WARN, got %+v", findings)
	}
}

func TestSampleNestedDiffPath(t *testing.T) {
	oldC, newC := compilePair(t,
		`syntax = "proto3"; package p; message Inner { int32 v = 1; } message Outer { Inner inner = 1; }`,
		`syntax = "proto3"; package p; message Inner { int64 v = 1; } message Outer { Inner inner = 1; }`)
	results := VerifySamples(oldC.Files, newC.Files, []Sample{
		{Message: "p.Outer", Encoding: "json", Data: `{"inner":{"v":7}}`},
	})
	if results[0].Status != "MISMATCH" {
		t.Fatalf("status = %s (%+v)", results[0].Status, results[0])
	}
	found := false
	for _, d := range results[0].Diffs {
		if d.Path == "p.Outer.inner.v" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected diff at p.Outer.inner.v, got %+v", results[0].Diffs)
	}
}
