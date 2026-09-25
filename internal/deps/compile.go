package deps

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protocompile/reporter"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"protocompat/internal/schema"
)

// errNotFound is returned by the resolver for unknown paths; the standard
// import wrapper uses the signal to fall back to well-known descriptors.
var errNotFound = protoregistry.NotFound

// ExtractImports returns the import paths of every source file keyed by
// file path. Parsing here needs no import resolution: it runs before any
// pinned package is fetched so missing dependencies can be reported
// precisely instead of as a compiler "file not found" deep in the tree.
func ExtractImports(files []schema.SourceFile) (map[string][]string, error) {
	out := make(map[string][]string, len(files))
	ordered := append([]schema.SourceFile(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	handler := reporter.NewHandler(reporter.NewReporter(
		func(err reporter.ErrorWithPos) error { return err },
		nil,
	))
	for _, f := range ordered {
		fd, err := parser.Parse(f.Path, strings.NewReader(f.Content), handler)
		if err != nil {
			return nil, &schema.CompileError{Err: err}
		}
		seen := map[string]bool{}
		for _, el := range fd.Decls {
			imp, ok := el.(*ast.ImportNode)
			if !ok {
				continue
			}
			p := imp.Name.AsString()
			if seen[p] {
				continue
			}
			seen[p] = true
			out[f.Path] = append(out[f.Path], p)
		}
	}
	return out, nil
}

// CompilePinned compiles sources with every external import resolved from
// the pinned dependency closures. Files that are neither submitted nor
// resolvable through a pin nor well-known are rejected by the compiler.
// The returned Compiled carries the full linked closure; its hash covers
// only the submitted (owned) files, so the same package content hashes
// identically even when compiled against different pinned versions.
func CompilePinned(ctx context.Context, files []schema.SourceFile, owners OwnerPaths) (*schema.Compiled, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("deps: no source files submitted")
	}
	srcs := make(map[string]string, len(files))
	entrypoints := make([]string, 0, len(files))
	for _, f := range files {
		if f.Path == "" {
			return nil, fmt.Errorf("deps: source file with empty path")
		}
		if _, dup := srcs[f.Path]; dup {
			return nil, fmt.Errorf("deps: duplicate source file %q", f.Path)
		}
		srcs[f.Path] = f.Content
		entrypoints = append(entrypoints, f.Path)
	}
	sort.Strings(entrypoints)

	// Cache unmarshaled closures by digest; a diamond pins the same
	// dependency many times but unmarshals it once.
	cache := map[string]*descriptorpb.FileDescriptorSet{}
	findProto := func(owner pathOwner, path string) (*descriptorpb.FileDescriptorProto, error) {
		set, ok := cache[owner.digest]
		if !ok {
			set = &descriptorpb.FileDescriptorSet{}
			if err := proto.Unmarshal(owner.closure, set); err != nil {
				return nil, fmt.Errorf("deps: unmarshal pinned closure: %w", err)
			}
			cache[owner.digest] = set
		}
		for _, fdp := range set.File {
			if fdp.GetName() == path {
				return fdp, nil
			}
		}
		return nil, errNotFound
	}

	resolver := protocompile.ResolverFunc(func(path string) (protocompile.SearchResult, error) {
		if src, ok := srcs[path]; ok {
			return protocompile.SearchResult{Source: strings.NewReader(src)}, nil
		}
		if owner, ok := owners[path]; ok {
			fdp, err := findProto(owner, path)
			if err != nil {
				return protocompile.SearchResult{}, err
			}
			return protocompile.SearchResult{Proto: fdp}, nil
		}
		return protocompile.SearchResult{}, errNotFound
	})

	compiler := protocompile.Compiler{
		Resolver:       protocompile.WithStandardImports(resolver),
		SourceInfoMode: protocompile.SourceInfoStandard,
	}
	linked, err := compiler.Compile(ctx, entrypoints...)
	if err != nil {
		return nil, &schema.CompileError{Err: err}
	}
	fds := make([]protoreflect.FileDescriptor, len(linked))
	for i := range linked {
		fds[i] = linked[i]
	}
	return schema.FromLinked(entrypoints, fds)
}
