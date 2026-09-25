package schema

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
)

func compile(t *testing.T, files ...SourceFile) *Compiled {
	t.Helper()
	c, err := Compile(context.Background(), files)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return c
}

func TestCompileNestedImports(t *testing.T) {
	c := compile(t,
		SourceFile{Path: "base/common.proto", Content: `syntax = "proto3"; package a.b; message Base { int64 id = 1; }`},
		SourceFile{Path: "mid/use.proto", Content: `syntax = "proto3"; package a.c; import "base/common.proto"; message Mid { a.b.Base b = 1; }`},
		SourceFile{Path: "top/app.proto", Content: `syntax = "proto3"; package a.d; import "mid/use.proto"; import "base/common.proto"; message Top { a.c.Mid m = 1; a.b.Base b = 2; }`},
	)
	if len(c.OwnedPaths) != 3 {
		t.Fatalf("owned paths = %v", c.OwnedPaths)
	}
	// The closure must contain the well-known-free 3 files.
	found := 0
	c.Files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		found++
		return true
	})
	if found != 3 {
		t.Fatalf("closure files = %d, want 3", found)
	}
}

func TestHashStabilityAndSensitivity(t *testing.T) {
	a := compile(t, SourceFile{Path: "m.proto", Content: `syntax = "proto3"; package p; message M { int32 x = 1; }`})
	b := compile(t, SourceFile{Path: "m.proto", Content: `syntax = "proto3"; package p; message M { int32 x = 1; }`})
	if string(a.Hash) != string(b.Hash) {
		t.Fatal("identical content produced different hashes")
	}
	c := compile(t, SourceFile{Path: "m.proto", Content: `syntax = "proto3"; package p; message M { int64 x = 1; }`})
	if string(a.Hash) == string(c.Hash) {
		t.Fatal("different content produced identical hashes")
	}
}

func TestCompileErrorPositions(t *testing.T) {
	_, err := Compile(context.Background(),
		[]SourceFile{{Path: "bad/bad.proto", Content: "syntax = \"proto3\";\nmessage M {\n  int32 = 1;\n}\n"}})
	if err == nil {
		t.Fatal("expected compile error")
	}
	ce, ok := err.(*CompileError)
	if !ok {
		t.Fatalf("error type = %T, want *CompileError", err)
	}
	if !strings.Contains(ce.Error(), "bad.proto") {
		t.Fatalf("error should name the file, got:\n%s", ce.Error())
	}
	if !strings.Contains(ce.Error(), ":3:") {
		t.Fatalf("error should carry a line number, got:\n%s", ce.Error())
	}
}

func TestLoadRoundTrip(t *testing.T) {
	c := compile(t, SourceFile{Path: "m.proto", Content: `syntax = "proto3"; package p; message M { int32 x = 1; }`})
	files, err := Load(c.DescriptorSet)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	d, err := files.FindDescriptorByName("p.M")
	if err != nil {
		t.Fatalf("find message: %v", err)
	}
	if d == nil {
		t.Fatal("message p.M missing after round trip")
	}
}
