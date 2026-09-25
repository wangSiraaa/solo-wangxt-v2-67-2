package registry

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"protocompat/internal/compat"
)

func analyze(t *testing.T, svc *Service, pkg, base, head string) *AnalyzeImpactResponse {
	t.Helper()
	resp, err := svc.AnalyzeImpact(context.Background(), connect.NewRequest(&AnalyzeImpactRequest{
		Package: pkg, BaseVersion: base, HeadVersion: head,
	}))
	if err != nil {
		t.Fatalf("analyze %s %s->%s: %v", pkg, base, head, err)
	}
	return resp.Msg
}

func impactOf(t *testing.T, resp *AnalyzeImpactResponse, pkg string) *PackageImpact {
	t.Helper()
	for i := range resp.Impacts {
		if resp.Impacts[i].Package == pkg {
			return &resp.Impacts[i]
		}
	}
	return nil
}

// pathPackages renders a reason path as "a@v1>b@v1" for comparison.
func pathPackages(p ReasonPath) string {
	var parts []string
	for _, h := range p.Hops {
		parts = append(parts, h.Package+"@"+h.Version)
	}
	return strings.Join(parts, ">")
}

func TestImpactDiamondSingleRecordMultipleReasonPaths(t *testing.T) {
	svc := newService()
	registerDiamond(t, svc)
	// base v2: int32 -> int64 on Shared.amount, a proven JSON break.
	registerFiles(t, svc, "acme.base", "v2", fileSpec{"base/common.proto", depBaseV2})

	resp := analyze(t, svc, "acme.base", "v1", "v2")
	if resp.Reused {
		t.Fatal("first analysis must not be marked reused")
	}
	if resp.Report.Verdict != compat.VerdictIncompatible {
		t.Fatalf("headline verdict = %s, want INCOMPATIBLE", resp.Report.Verdict)
	}

	// Every dependent appears exactly once.
	if len(resp.Impacts) != 3 {
		t.Fatalf("impacts = %+v, want mid, side, top", resp.Impacts)
	}
	seen := map[string]int{}
	for _, im := range resp.Impacts {
		seen[im.Package]++
	}
	for pkg, n := range seen {
		if n != 1 {
			t.Fatalf("package %s reported %d times, want exactly one record", pkg, n)
		}
	}

	mid := impactOf(t, resp, "acme.mid")
	if mid == nil || mid.Impact != ImpactDirect {
		t.Fatalf("mid impact = %+v, want DIRECT", mid)
	}
	side := impactOf(t, resp, "acme.side")
	if side == nil || side.Impact != ImpactDirect {
		t.Fatalf("side impact = %+v, want DIRECT", side)
	}

	top := impactOf(t, resp, "acme.top")
	if top == nil {
		t.Fatal("top missing from impacts")
	}
	if top.Impact != ImpactTransitive {
		t.Fatalf("top impact = %s, want TRANSITIVE", top.Impact)
	}
	// The diamond gives top two reason paths; both must be kept.
	if len(top.ReasonPaths) != 2 {
		t.Fatalf("top reason paths = %+v, want 2", top.ReasonPaths)
	}
	got := map[string]bool{pathPackages(top.ReasonPaths[0]): true, pathPackages(top.ReasonPaths[1]): true}
	for _, want := range []string{"acme.base@v2>acme.mid@v1>acme.top@v1", "acme.base@v2>acme.side@v1>acme.top@v1"} {
		if !got[want] {
			t.Fatalf("missing reason path %s in %+v", want, top.ReasonPaths)
		}
	}

	// The break is pinpointed to the symbol, even two hops away.
	found := false
	for _, f := range top.Findings {
		if f.Message == "acme.base.Shared" {
			found = true
		}
	}
	if !found {
		t.Fatalf("top findings should name acme.base.Shared: %+v", top.Findings)
	}
}

func TestImpactVerifiedUnaffectedAndUnrelated(t *testing.T) {
	svc := newService()
	registerFiles(t, svc, "acme.base", "v1", fileSpec{"base/common.proto", depBaseV1})
	// picky depends on base but only touches Other, which v2 does not change.
	registerFiles(t, svc, "acme.picky", "v1", fileSpec{"picky/view.proto", depPicky})
	// solo does not depend on base at all; its own upgrades are irrelevant.
	registerFiles(t, svc, "acme.solo", "v1", fileSpec{"solo/alone.proto", depSolo})
	registerFiles(t, svc, "acme.base", "v2", fileSpec{"base/common.proto", depBaseV2})
	registerFiles(t, svc, "acme.solo", "v2", fileSpec{"solo/alone.proto",
		`syntax = "proto3"; package acme.solo; message Alone { string id = 1; string extra = 2; }`})

	resp := analyze(t, svc, "acme.base", "v1", "v2")
	picky := impactOf(t, resp, "acme.picky")
	if picky == nil {
		t.Fatal("picky must appear as a dependent")
	}
	if picky.Impact != ImpactVerifiedUnaffected {
		t.Fatalf("picky impact = %s, want VERIFIED_UNAFFECTED (findings %+v)", picky.Impact, picky.Findings)
	}
	if len(picky.Findings) != 0 {
		t.Fatalf("picky findings = %+v, want none relevant", picky.Findings)
	}
	// The unrelated package must not be reported at all, even though it
	// published its own new version.
	if solo := impactOf(t, resp, "acme.solo"); solo != nil {
		t.Fatalf("unrelated package reported: %+v", solo)
	}
}

func TestImpactAnalysisIsIdempotentAndReproducible(t *testing.T) {
	svc := newService()
	registerDiamond(t, svc)
	registerFiles(t, svc, "acme.base", "v2", fileSpec{"base/common.proto", depBaseV2})

	first := analyze(t, svc, "acme.base", "v1", "v2")
	// More registrations afterwards must not change a historical analysis.
	registerFiles(t, svc, "acme.base", "v3", fileSpec{"base/common.proto",
		`syntax = "proto3"; package acme.base; message Shared { string id = 1; string amount = 2; } message Other { string name = 1; }`})
	second := analyze(t, svc, "acme.base", "v1", "v2")

	if !second.Reused {
		t.Fatal("second analysis of the same input must reuse the stored result")
	}
	if !reflect.DeepEqual(first.Report, second.Report) || !reflect.DeepEqual(first.Impacts, second.Impacts) {
		t.Fatalf("recomputed analysis differs:\nfirst  %+v\nsecond %+v", first, second)
	}
}

func TestImpactDependentPinnedToOlderThanBase(t *testing.T) {
	svc := newService()
	// mid locks base v1. base then publishes v2 (compatible) and v3
	// (breaking). Analyzing v2->v3 must still evaluate mid's world, which
	// is pinned to v1: the effective question for mid is v1->v3.
	registerFiles(t, svc, "acme.base", "v1", fileSpec{"base/common.proto", depBaseV1})
	registerFiles(t, svc, "acme.mid", "v1", fileSpec{"mid/invoice.proto", depMidV1})
	registerFiles(t, svc, "acme.base", "v2", fileSpec{"base/common.proto",
		`syntax = "proto3"; package acme.base; message Shared { string id = 1; int32 amount = 2; } message Other { string name = 1; } message NewGuy { string x = 1; }`})
	registerFiles(t, svc, "acme.base", "v3", fileSpec{"base/common.proto", depBaseV2})

	resp := analyze(t, svc, "acme.base", "v2", "v3")
	mid := impactOf(t, resp, "acme.mid")
	if mid == nil || mid.Impact != ImpactDirect {
		t.Fatalf("mid impact = %+v, want DIRECT (its pinned v1 world breaks under v3)", mid)
	}
}

func TestImpactDependencySymbolRemoved(t *testing.T) {
	svc := newService()
	registerFiles(t, svc, "acme.base", "v1", fileSpec{"base/common.proto", depBaseV1})
	registerFiles(t, svc, "acme.mid", "v1", fileSpec{"mid/invoice.proto", depMidV1})
	// base v2 deletes Shared entirely.
	registerFiles(t, svc, "acme.base", "v2", fileSpec{"base/common.proto",
		`syntax = "proto3"; package acme.base; message Other { string name = 1; }`})

	resp := analyze(t, svc, "acme.base", "v1", "v2")
	mid := impactOf(t, resp, "acme.mid")
	if mid == nil || mid.Impact != ImpactDirect {
		t.Fatalf("mid impact = %+v, want DIRECT", mid)
	}
	var proven bool
	for _, f := range mid.Findings {
		if f.Code == "DEPENDENCY_SYMBOL_REMOVED" && f.Message == "acme.base.Shared" && f.Severity == compat.SeverityFail {
			proven = true
		}
	}
	if !proven {
		t.Fatalf("expected proven DEPENDENCY_SYMBOL_REMOVED for acme.base.Shared: %+v", mid.Findings)
	}
}

func TestImpactDependentAlreadyBeyondHead(t *testing.T) {
	svc := newService()
	// mid is registered after base v3, so it locks v3. Analyzing the older
	// v1->v2 transition must not flag it: its world is already past head.
	registerFiles(t, svc, "acme.base", "v1", fileSpec{"base/common.proto", depBaseV1})
	registerFiles(t, svc, "acme.base", "v2", fileSpec{"base/common.proto", depBaseV2})
	registerFiles(t, svc, "acme.base", "v3", fileSpec{"base/common.proto",
		`syntax = "proto3"; package acme.base; message Shared { string id = 1; string amount = 2; } message Other { string name = 1; }`})
	registerFiles(t, svc, "acme.mid", "v1", fileSpec{"mid/invoice.proto", depMidV1})

	resp := analyze(t, svc, "acme.base", "v1", "v2")
	mid := impactOf(t, resp, "acme.mid")
	if mid == nil || mid.Impact != ImpactVerifiedUnaffected {
		t.Fatalf("mid impact = %+v, want VERIFIED_UNAFFECTED (already past head)", mid)
	}
}

func TestAnalyzeImpactUnknownVersions(t *testing.T) {
	svc := newService()
	registerFiles(t, svc, "acme.base", "v1", fileSpec{"base/common.proto", depBaseV1})
	_, err := svc.AnalyzeImpact(context.Background(), connect.NewRequest(&AnalyzeImpactRequest{
		Package: "acme.base", BaseVersion: "v1", HeadVersion: "v9",
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("code = %s (%v), want NotFound", connect.CodeOf(err), err)
	}
}
