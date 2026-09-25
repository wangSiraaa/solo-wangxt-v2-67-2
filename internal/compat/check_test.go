package compat

import (
	"context"
	"testing"

	"protocompat/internal/schema"
)

// checkSources compiles old and new inline sources and runs Check.
func checkSources(t *testing.T, oldSrc, newSrc string, samples ...Sample) *Report {
	t.Helper()
	ctx := context.Background()
	oldC, err := schema.Compile(ctx, []schema.SourceFile{{Path: "m.proto", Content: oldSrc}})
	if err != nil {
		t.Fatalf("compile old: %v", err)
	}
	newC, err := schema.Compile(ctx, []schema.SourceFile{{Path: "m.proto", Content: newSrc}})
	if err != nil {
		t.Fatalf("compile new: %v", err)
	}
	return Check(Input{
		Old: oldC.Files, New: newC.Files,
		OwnedPaths: []string{"m.proto"}, Samples: samples,
	})
}

func findingsByCode(r *Report, code string) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Code == code {
			out = append(out, f)
		}
	}
	return out
}

func mustFinding(t *testing.T, r *Report, code string, sev Severity, path string) Finding {
	t.Helper()
	for _, f := range r.Findings {
		if f.Code == code && f.Severity == sev && (path == "" || f.Path == path) {
			return f
		}
	}
	t.Fatalf("no finding %s/%s at %q in %+v", code, sev, path, r.Findings)
	return Finding{}
}

func TestIdenticalSchemasPass(t *testing.T) {
	src := `syntax = "proto3"; package p; message M { int32 a = 1; string b = 2; }`
	r := checkSources(t, src, src)
	if r.Verdict != VerdictCompatible {
		t.Fatalf("verdict = %s, want COMPATIBLE; findings: %+v", r.Verdict, r.Findings)
	}
}

func TestFieldRemovedUnreserved(t *testing.T) {
	r := checkSources(t,
		`syntax = "proto3"; package p; message M { int32 a = 1; string gone = 2; }`,
		`syntax = "proto3"; package p; message M { int32 a = 1; }`)
	f := mustFinding(t, r, "FIELD_REMOVED_UNRESERVED", SeverityFail, "gone")
	if f.Dimension != DimensionWire {
		t.Fatalf("dimension = %s, want WIRE", f.Dimension)
	}
	if r.Verdict != VerdictIncompatible {
		t.Fatalf("verdict = %s", r.Verdict)
	}
}

func TestFieldRemovedProperlyReserved(t *testing.T) {
	r := checkSources(t,
		`syntax = "proto3"; package p; message M { int32 a = 1; string gone = 2; }`,
		`syntax = "proto3"; package p; message M { int32 a = 1; reserved 2; reserved "gone"; }`)
	mustFinding(t, r, "FIELD_REMOVED_RESERVED", SeverityInfo, "gone")
	if r.Verdict != VerdictCompatible {
		t.Fatalf("verdict = %s, want COMPATIBLE; findings: %+v", r.Verdict, r.Findings)
	}
}

func TestFieldRemovedNumberReservedOnly(t *testing.T) {
	r := checkSources(t,
		`syntax = "proto3"; package p; message M { int32 a = 1; string gone = 2; }`,
		`syntax = "proto3"; package p; message M { int32 a = 1; reserved 2; }`)
	mustFinding(t, r, "FIELD_REMOVED_NAME_UNRESERVED", SeverityWarn, "gone")
	if r.Verdict != VerdictNeedsReview {
		t.Fatalf("verdict = %s, want NEEDS_REVIEW", r.Verdict)
	}
}

func TestFieldNumberReuse(t *testing.T) {
	r := checkSources(t,
		`syntax = "proto3"; package p; message M { string name = 2; }`,
		`syntax = "proto3"; package p; message M { int32 age = 2; }`)
	mustFinding(t, r, "FIELD_NUMBER_REUSED", SeverityFail, "name")
}

func TestFieldRenameSameKind(t *testing.T) {
	r := checkSources(t,
		`syntax = "proto3"; package p; message M { string display_name = 1; }`,
		`syntax = "proto3"; package p; message M { string label = 1; }`)
	// JSON name changes (displayName -> label): proven JSON break.
	mustFinding(t, r, "FIELD_RENAMED", SeverityFail, "display_name")
}

func TestTypeChangeWireIncompatible(t *testing.T) {
	r := checkSources(t,
		`syntax = "proto3"; package p; message M { int32 a = 1; }`,
		`syntax = "proto3"; package p; message M { string a = 1; }`)
	mustFinding(t, r, "FIELD_TYPE_CHANGED", SeverityFail, "a")
}

func TestTypeChangeWidening(t *testing.T) {
	r := checkSources(t,
		`syntax = "proto3"; package p; message M { int32 a = 1; }`,
		`syntax = "proto3"; package p; message M { int64 a = 1; }`)
	// Wire-safe widening, but JSON number -> quoted string: FAIL via JSON.
	mustFinding(t, r, "FIELD_TYPE_CHANGED", SeverityFail, "a")
}

func TestTypeChangeFixedPair(t *testing.T) {
	r := checkSources(t,
		`syntax = "proto3"; package p; message M { fixed32 a = 1; }`,
		`syntax = "proto3"; package p; message M { sfixed32 a = 1; }`)
	mustFinding(t, r, "FIELD_TYPE_CHANGED", SeverityInfo, "a")
	if r.Verdict != VerdictCompatible {
		t.Fatalf("verdict = %s, want COMPATIBLE", r.Verdict)
	}
}

func TestSingularToRepeated(t *testing.T) {
	r := checkSources(t,
		`syntax = "proto3"; package p; message M { string tag = 1; }`,
		`syntax = "proto3"; package p; message M { repeated string tag = 1; }`)
	// JSON shape changes value -> array.
	mustFinding(t, r, "FIELD_CARDINALITY_CHANGED", SeverityFail, "tag")
}

func TestPackedRepeatedToSingular(t *testing.T) {
	r := checkSources(t,
		`syntax = "proto3"; package p; message M { repeated int32 nums = 1; }`,
		`syntax = "proto3"; package p; message M { int32 nums = 1; }`)
	// proto3 repeated scalars are packed: singular readers drop the data.
	mustFinding(t, r, "FIELD_CARDINALITY_CHANGED", SeverityFail, "nums")
}

func TestOneofMigrationInto(t *testing.T) {
	r := checkSources(t,
		`syntax = "proto3"; package p; message M { string a = 1; string b = 2; }`,
		`syntax = "proto3"; package p; message M { oneof pick { string a = 1; string b = 2; } }`)
	mustFinding(t, r, "FIELD_MOVED_INTO_ONEOF", SeverityWarn, "a")
	mustFinding(t, r, "FIELD_MOVED_INTO_ONEOF", SeverityWarn, "b")
	if r.Verdict != VerdictNeedsReview {
		t.Fatalf("verdict = %s, want NEEDS_REVIEW (unprovable must not pass)", r.Verdict)
	}
}

func TestEnumZeroValueRenamed(t *testing.T) {
	r := checkSources(t,
		`syntax = "proto3"; package p; enum E { E_UNKNOWN = 0; E_ON = 1; } message M { E e = 1; }`,
		`syntax = "proto3"; package p; enum E { E_UNSPECIFIED = 0; E_ON = 1; } message M { E e = 1; }`)
	mustFinding(t, r, "ENUM_VALUE_RENAMED", SeverityFail, "E_UNKNOWN")
	mustFinding(t, r, "ENUM_DEFAULT_VALUE_CHANGED", SeverityWarn, "E_UNKNOWN")
}

func TestEnumValueAddedClosedProto2(t *testing.T) {
	r := checkSources(t,
		`syntax = "proto2"; package p; enum E { A = 0; B = 1; } message M { optional E e = 1; }`,
		`syntax = "proto2"; package p; enum E { A = 0; B = 1; C = 2; } message M { optional E e = 1; }`)
	mustFinding(t, r, "ENUM_VALUE_ADDED", SeverityWarn, "C")
	if r.Verdict != VerdictNeedsReview {
		t.Fatalf("verdict = %s, want NEEDS_REVIEW", r.Verdict)
	}
}

func TestEnumValueRemovedUnreserved(t *testing.T) {
	r := checkSources(t,
		`syntax = "proto3"; package p; enum E { A = 0; B = 1; } message M { E e = 1; }`,
		`syntax = "proto3"; package p; enum E { A = 0; } message M { E e = 1; }`)
	mustFinding(t, r, "ENUM_VALUE_REMOVED_UNRESERVED", SeverityFail, "B")
}

func TestMessageRemoved(t *testing.T) {
	r := checkSources(t,
		`syntax = "proto3"; package p; message Gone { int32 a = 1; } message Stay { int32 b = 1; }`,
		`syntax = "proto3"; package p; message Stay { int32 b = 1; }`)
	mustFinding(t, r, "MESSAGE_REMOVED", SeverityWarn, "")
	if r.Verdict != VerdictNeedsReview {
		t.Fatalf("verdict = %s, want NEEDS_REVIEW", r.Verdict)
	}
}

func TestMessageTypeChanged(t *testing.T) {
	old := `syntax = "proto3"; package p; message A { int32 x = 1; } message B { int32 x = 1; } message M { A a = 1; }`
	new := `syntax = "proto3"; package p; message A { int32 x = 1; } message B { int32 x = 1; } message M { B a = 1; }`
	r := checkSources(t, old, new)
	mustFinding(t, r, "FIELD_MESSAGE_TYPE_CHANGED", SeverityFail, "a")
}

func TestJSONNameExplicitChange(t *testing.T) {
	r := checkSources(t,
		`syntax = "proto3"; package p; message M { string a = 1 [json_name = "alpha"]; }`,
		`syntax = "proto3"; package p; message M { string a = 1 [json_name = "alpa"]; }`)
	mustFinding(t, r, "FIELD_JSON_NAME_CHANGED", SeverityFail, "a")
}

func TestProto3OptionalIsNotAOneofMigration(t *testing.T) {
	// proto3 optional fields are synthetic oneofs; adding or removing
	// `optional` must not trigger oneof-migration findings.
	r := checkSources(t,
		`syntax = "proto3"; package p; message M { string a = 1; }`,
		`syntax = "proto3"; package p; message M { optional string a = 1; }`)
	for _, f := range r.Findings {
		if f.Code == "FIELD_MOVED_INTO_ONEOF" || f.Code == "FIELD_MOVED_OUT_OF_ONEOF" || f.Code == "ONEOF_ADDED" || f.Code == "ONEOF_REMOVED" {
			t.Fatalf("synthetic oneof leaked into findings: %+v", f)
		}
	}
	if r.Verdict != VerdictCompatible {
		t.Fatalf("verdict = %s, want COMPATIBLE; findings %+v", r.Verdict, r.Findings)
	}
}

func TestSameNameDifferentPackagesIndependent(t *testing.T) {
	oldFiles := []schema.SourceFile{
		{Path: "a/u.proto", Content: `syntax = "proto3"; package x; message U { int32 f = 1; }`},
		{Path: "b/u.proto", Content: `syntax = "proto3"; package y; message U { int32 f = 1; }`},
	}
	newFiles := []schema.SourceFile{
		{Path: "a/u.proto", Content: `syntax = "proto3"; package x; message U { int32 f = 1; }`},
		{Path: "b/u.proto", Content: `syntax = "proto3"; package y; message U { string f = 1; }`},
	}
	ctx := context.Background()
	oldC, err := schema.Compile(ctx, oldFiles)
	if err != nil {
		t.Fatal(err)
	}
	newC, err := schema.Compile(ctx, newFiles)
	if err != nil {
		t.Fatal(err)
	}
	r := Check(Input{Old: oldC.Files, New: newC.Files, OwnedPaths: []string{"a/u.proto", "b/u.proto"}})
	if len(r.Findings) != 1 {
		t.Fatalf("expected exactly 1 finding, got %+v", r.Findings)
	}
	if r.Findings[0].Message != "y.U" {
		t.Fatalf("finding attached to %s, want y.U", r.Findings[0].Message)
	}
}
