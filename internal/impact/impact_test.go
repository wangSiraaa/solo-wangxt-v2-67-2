package impact

import (
	"testing"

	"protocompat/internal/compat"
	"protocompat/internal/deps"
	"protocompat/internal/schema"
)

// layer is one package version compiled against already-built layers.
type layer struct {
	pkg, version, path string
	content            string
	compiled           *schema.Compiled
	hash               []byte
	locks              []Lock
}

// buildLayer compiles one layer, resolving every import from the
// previously built layers (which carry their full closures).
func buildLayer(t *testing.T, l *layer, prev []*layer, locks []Lock) {
	t.Helper()
	byPkg := map[string]*layer{}
	for _, p := range prev {
		byPkg[p.pkg] = p
	}
	var rd []deps.ResolvedDependency
	for _, lk := range locks {
		p, ok := byPkg[lk.Package]
		if !ok || p.version != lk.Version {
			t.Fatalf("lock %s@%s has no built layer", lk.Package, lk.Version)
		}
		rd = append(rd, deps.ResolvedDependency{
			Package: p.pkg, Version: p.version, Digest: p.hash,
			Closure: p.compiled.DescriptorSet, OwnedPaths: []string{p.path},
		})
	}
	owners, err := deps.BuildOwnerPaths(rd)
	if err != nil {
		t.Fatalf("owners for %s: %v", l.pkg, err)
	}
	c, err := deps.CompilePinned(t.Context(),
		[]schema.SourceFile{{Path: l.path, Content: l.content}}, owners)
	if err != nil {
		t.Fatalf("compile %s: %v", l.pkg, err)
	}
	h, err := schema.OwnedOnlyHash(c.DescriptorSet, []string{l.path})
	if err != nil {
		t.Fatal(err)
	}
	l.compiled, l.hash, l.locks = c, h, locks
}

// TestDeepTransitiveImpact builds base -> l1 -> l2 -> l3, with l2 also
// having a side branch l2b. A break in base must locate l1 directly, l2
// and l3 transitively, while l2b (which locks l2 but references only
// l2's own unchanged surface) is verified unaffected.
func TestDeepTransitiveImpact(t *testing.T) {
	base1 := &layer{pkg: "acme.base", version: "v1", path: "base/base.proto",
		content: `syntax = "proto3"; package acme.base; message M { string a = 1; }`}
	base1.compiled = mustCompile(t, map[string]string{
		"base/base.proto": base1.content,
	})
	base1.hash = []byte("base-v1-hash")
	baseV2 := mustCompile(t, map[string]string{
		"base/base.proto": `syntax = "proto3"; package acme.base; message M { int32 a = 1; }`,
	})

	l1 := &layer{pkg: "acme.l1", version: "v1", path: "l1/l1.proto",
		content: `syntax = "proto3"; package acme.l1; import "base/base.proto"; message W1 { acme.base.M m = 1; }`}
	l2 := &layer{pkg: "acme.l2", version: "v1", path: "l2/l2.proto",
		content: `syntax = "proto3"; package acme.l2; import "l1/l1.proto"; message W2 { acme.l1.W1 w = 1; }`}
	l2b := &layer{pkg: "acme.l2b", version: "v1", path: "l2b/l2b.proto",
		content: `syntax = "proto3"; package acme.l2b; import "l2/l2.proto"; message Other { int64 z = 1; }`}
	l3 := &layer{pkg: "acme.l3", version: "v1", path: "l3/l3.proto",
		content: `syntax = "proto3"; package acme.l3; import "l2/l2.proto"; message W3 { acme.l2.W2 w = 1; }`}

	buildLayer(t, l1, []*layer{base1}, []Lock{{Package: "acme.base", Version: "v1"}})
	buildLayer(t, l2, []*layer{base1, l1}, []Lock{{Package: "acme.l1", Version: "v1"}})
	buildLayer(t, l2b, []*layer{base1, l1, l2}, []Lock{{Package: "acme.l2", Version: "v1"}})
	buildLayer(t, l3, []*layer{base1, l1, l2}, []Lock{{Package: "acme.l2", Version: "v1"}})

	snap := Snapshot{Versions: map[NodeKey]VersionData{
		{Package: "acme.base", Version: "v1"}: {ContentHash: base1.hash, Files: base1.compiled.Files, OwnedPaths: []string{"base/base.proto"}},
		{Package: "acme.base", Version: "v2"}: {ContentHash: []byte("base-v2"), Files: baseV2.Files, OwnedPaths: []string{"base/base.proto"}},
	}}
	for _, l := range []*layer{l1, l2, l2b, l3} {
		snap.Versions[NodeKey{Package: l.pkg, Version: l.version}] = VersionData{
			ContentHash: l.hash, Files: l.compiled.Files,
			OwnedPaths: []string{l.path}, Locks: l.locks,
		}
	}

	checker := func() *compat.Report {
		return compat.Check(compat.Input{
			Old: base1.compiled.Files, New: baseV2.Files, OwnedPaths: []string{"base/base.proto"},
		})
	}
	a := Analyze(snap, "acme.base", "v1", "v2", checker)
	if a.Verdict != compat.VerdictIncompatible {
		t.Fatalf("verdict = %s", a.Verdict)
	}
	status := map[string]Status{}
	for _, n := range a.Nodes {
		status[n.Package] = n.Status
	}
	if status["acme.l1"] != StatusDirect {
		t.Fatalf("l1 = %s", status["acme.l1"])
	}
	if status["acme.l2"] != StatusTransitive {
		t.Fatalf("l2 = %s", status["acme.l2"])
	}
	if status["acme.l3"] != StatusTransitive {
		t.Fatalf("l3 = %s, want TRANSITIVE_IMPACTED", status["acme.l3"])
	}
	if status["acme.l2b"] != StatusUnaffected {
		t.Fatalf("l2b = %s, want VERIFIED_UNAFFECTED", status["acme.l2b"])
	}
	counts := map[string]int{}
	for _, n := range a.Nodes {
		counts[n.Package]++
	}
	for pkg, n := range counts {
		if n != 1 {
			t.Fatalf("%s reported %d times", pkg, n)
		}
	}
}

func mustCompile(t *testing.T, files map[string]string) *schema.Compiled {
	t.Helper()
	srcs := make([]schema.SourceFile, 0, len(files))
	for p, c := range files {
		srcs = append(srcs, schema.SourceFile{Path: p, Content: c})
	}
	c, err := schema.Compile(t.Context(), srcs)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return c
}
