package deps

import (
	"context"
	"strings"
	"testing"

	"protocompat/internal/schema"
)

func compileOwn(t *testing.T, path, content string) *schema.Compiled {
	t.Helper()
	c, err := schema.Compile(context.Background(), []schema.SourceFile{{Path: path, Content: content}})
	if err != nil {
		t.Fatalf("compile %s: %v", path, err)
	}
	return c
}

func TestDigestStableAndOrderIndependent(t *testing.T) {
	a := Lock{Package: "acme.a", Version: "v1", Digest: "aa"}
	b := Lock{Package: "acme.b", Version: "v9", Digest: "bb"}
	d1 := Digest([]Lock{a, b})
	d2 := Digest([]Lock{b, a})
	if d1 != d2 {
		t.Fatalf("digest depends on order: %s != %s", d1, d2)
	}
	if d1 == Digest([]Lock{a}) {
		t.Fatal("adding a lock must change the digest")
	}
	c := Lock{Package: "acme.a", Version: "v2", Digest: "aa"}
	if d1 == Digest([]Lock{c, b}) {
		t.Fatal("changing a locked version must change the digest")
	}
}

func TestExtractImports(t *testing.T) {
	files := []schema.SourceFile{
		{Path: "a/a.proto", Content: `syntax = "proto3"; package a;
import "b/b.proto";
import public "google/protobuf/empty.proto";
import "b/b.proto"; // duplicate, collapsed
message A {}`},
	}
	got, err := ExtractImports(files)
	if err != nil {
		t.Fatal(err)
	}
	imps := got["a/a.proto"]
	if len(imps) != 2 || imps[0] != "b/b.proto" || imps[1] != "google/protobuf/empty.proto" {
		t.Fatalf("imports = %v", imps)
	}

	if _, err := ExtractImports([]schema.SourceFile{{Path: "x.proto", Content: "syntax = \"proto3\";\nmessage M {\n int32 = 1;\n}\n"}}); err == nil {
		t.Fatal("expected syntax error")
	} else if !strings.Contains(err.Error(), "x.proto:3:") {
		t.Fatalf("syntax error should carry position: %v", err)
	}
}

func TestIsExternalImport(t *testing.T) {
	submitted := map[string]bool{"own/x.proto": true}
	cases := map[string]bool{
		"own/x.proto":                 false,
		"google/protobuf/empty.proto": false,
		"dep/dep.proto":               true,
	}
	for path, want := range cases {
		if got := IsExternalImport(path, submitted); got != want {
			t.Errorf("IsExternalImport(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestCompilePinnedDiamond(t *testing.T) {
	base := compileOwn(t, "base/base.proto",
		`syntax = "proto3"; package acme.common; message Money { int64 units = 1; }`)
	baseHash, err := schema.OwnedOnlyHash(base.DescriptorSet, base.OwnedPaths)
	if err != nil {
		t.Fatal(err)
	}
	owners, err := BuildOwnerPaths([]ResolvedDependency{{
		Package: "acme.common", Version: "v1", Digest: baseHash,
		Closure: base.DescriptorSet, OwnedPaths: base.OwnedPaths,
	}})
	if err != nil {
		t.Fatal(err)
	}
	middle, err := CompilePinned(context.Background(), []schema.SourceFile{{
		Path: "mid/mid.proto",
		Content: `syntax = "proto3"; package acme.mid; import "base/base.proto";
message Line { acme.common.Money amount = 1; }`,
	}}, owners)
	if err != nil {
		t.Fatalf("compile middle: %v", err)
	}
	midHash, err := schema.OwnedOnlyHash(middle.DescriptorSet, middle.OwnedPaths)
	if err != nil {
		t.Fatal(err)
	}

	// top sees the same base through its direct pin AND through middle's
	// closure: same descriptor content, must compile as a diamond.
	owners2, err := BuildOwnerPaths([]ResolvedDependency{
		{
			Package: "acme.common", Version: "v1", Digest: baseHash,
			Closure: base.DescriptorSet, OwnedPaths: base.OwnedPaths,
		},
		{
			Package: "acme.mid", Version: "v1", Digest: midHash,
			Closure: middle.DescriptorSet, OwnedPaths: middle.OwnedPaths,
		},
	})
	if err != nil {
		t.Fatalf("diamond owners: %v", err)
	}
	if _, err := CompilePinned(context.Background(), []schema.SourceFile{{
		Path: "top/top.proto",
		Content: `syntax = "proto3"; package acme.top;
import "mid/mid.proto";
import "base/base.proto";
message Invoice { repeated acme.mid.Line lines = 1; acme.common.Money total = 2; }`,
	}}, owners2); err != nil {
		t.Fatalf("compile top diamond: %v", err)
	}
}

func TestCompilePinnedMissingImport(t *testing.T) {
	_, err := CompilePinned(context.Background(), []schema.SourceFile{{
		Path:    "x/x.proto",
		Content: `syntax = "proto3"; package x; import "ghost/ghost.proto"; message X {}`,
	}}, OwnerPaths{})
	if err == nil {
		t.Fatal("expected compile failure for unresolved import")
	}
}
