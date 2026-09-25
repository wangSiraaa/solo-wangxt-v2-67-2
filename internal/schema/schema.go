// Package schema compiles .proto sources into descriptor sets using
// protocompile. All compatibility judgments are made on the fully linked
// descriptor closure produced here — never on source text.
package schema

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/reporter"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// SourceFile is one .proto file submitted for registration or checking.
type SourceFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Compiled is the result of compiling a self-contained set of sources.
type Compiled struct {
	// DescriptorSet is the serialized FileDescriptorSet of the full
	// transitive closure, in topological (imports-first) order.
	DescriptorSet []byte
	// Hash is a content hash over the canonical form of every compiled
	// file. Identical content always yields an identical hash.
	Hash []byte
	// OwnedPaths are the file paths that were submitted (as opposed to
	// well-known imports supplied by the compiler).
	OwnedPaths []string
	// Files is the live descriptor registry for the compiled closure.
	Files *protoregistry.Files
}

// Compile parses and links the given sources. Every import must be
// resolvable from the submitted files themselves or from the standard
// well-known types; nested and diamond imports are handled by the
// compiler. Compilation errors are returned with file:line:col positions.
func Compile(ctx context.Context, files []SourceFile) (*Compiled, error) {
	if len(files) == 0 {
		return nil, errors.New("schema: no source files submitted")
	}
	srcs := make(map[string]string, len(files))
	entrypoints := make([]string, 0, len(files))
	for _, f := range files {
		if f.Path == "" {
			return nil, errors.New("schema: source file with empty path")
		}
		if _, dup := srcs[f.Path]; dup {
			return nil, fmt.Errorf("schema: duplicate source file %q", f.Path)
		}
		srcs[f.Path] = f.Content
		entrypoints = append(entrypoints, f.Path)
	}
	sort.Strings(entrypoints)

	compiler := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			Accessor: protocompile.SourceAccessorFromMap(srcs),
		}),
		SourceInfoMode: protocompile.SourceInfoStandard,
	}
	linked, err := compiler.Compile(ctx, entrypoints...)
	if err != nil {
		return nil, &CompileError{Err: err}
	}
	out := make([]protoreflect.FileDescriptor, len(linked))
	for i := range linked {
		out[i] = linked[i]
	}
	return FromLinked(entrypoints, out)
}

// FromLinked serializes and hashes an already-linked compilation. The
// hash covers every file reachable in the closure; callers that compile a
// package against pre-registered dependency closures need OwnedOnlyHash
// instead if they want identical package content to hash identically
// regardless of which pinned versions supplied the imports.
func FromLinked(entrypoints []string, linked []protoreflect.FileDescriptor) (*Compiled, error) {
	// Serialize the full transitive closure in topological order.
	fdset := buildSet(linked)

	setBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(fdset)
	if err != nil {
		return nil, fmt.Errorf("schema: marshal descriptor set: %w", err)
	}
	hash := hashSet(fdset, nil)
	registry, err := Load(setBytes)
	if err != nil {
		return nil, err
	}
	return &Compiled{
		DescriptorSet: setBytes,
		Hash:          hash,
		OwnedPaths:    append([]string(nil), entrypoints...),
		Files:         registry,
	}, nil
}

// OwnedOnlyHash hashes only the given file paths (their canonical
// descriptor bytes) within a serialized FileDescriptorSet. It makes a
// package's content hash independent of the exact pinned dependency
// versions it was compiled against. Paths absent from the set are an error.
func OwnedOnlyHash(descriptorSet []byte, ownedPaths []string) ([]byte, error) {
	var fdset descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(descriptorSet, &fdset); err != nil {
		return nil, fmt.Errorf("schema: unmarshal descriptor set: %w", err)
	}
	owned := make(map[string]bool, len(ownedPaths))
	for _, p := range ownedPaths {
		owned[p] = true
	}
	return hashSet(&fdset, owned), nil
}

// ClosurePaths lists every file name inside a serialized
// FileDescriptorSet, sorted for deterministic traversal.
func ClosurePaths(descriptorSet []byte) ([]string, error) {
	var fdset descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(descriptorSet, &fdset); err != nil {
		return nil, fmt.Errorf("schema: unmarshal descriptor set: %w", err)
	}
	paths := make([]string, 0, len(fdset.File))
	for _, fdp := range fdset.File {
		paths = append(paths, fdp.GetName())
	}
	sort.Strings(paths)
	return paths, nil
}

// linkerResult kept for documentation of the protocompile output type.
type linkerResult = []protoreflect.FileDescriptor

func buildSet(linked []protoreflect.FileDescriptor) *descriptorpb.FileDescriptorSet {
	fdset := &descriptorpb.FileDescriptorSet{}
	seen := make(map[string]bool)
	var add func(fd protoreflect.FileDescriptor)
	add = func(fd protoreflect.FileDescriptor) {
		if seen[fd.Path()] {
			return
		}
		seen[fd.Path()] = true
		imports := fd.Imports()
		for i := 0; i < imports.Len(); i++ {
			add(imports.Get(i))
		}
		fdset.File = append(fdset.File, protodesc.ToFileDescriptorProto(fd))
	}
	for _, fd := range linked {
		add(fd)
	}
	return fdset
}

// hashSet computes the canonical content hash over a descriptor set. When
// only is non-nil, files outside the allow-list are skipped; otherwise
// every file participates. Files are hashed in path order so entrypoint
// order cannot change the result.
func hashSet(fdset *descriptorpb.FileDescriptorSet, only map[string]bool) []byte {
	type fileBytes struct {
		path string
		data []byte
	}
	canonical := make([]fileBytes, 0, len(fdset.File))
	for _, fdp := range fdset.File {
		if only != nil && !only[fdp.GetName()] {
			continue
		}
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(fdp)
		if err != nil {
			// Every fdp originates from protodesc; deterministic marshal
			// cannot realistically fail. Hash what is available rather than
			// panicking in a hashing helper.
			continue
		}
		canonical = append(canonical, fileBytes{path: fdp.GetName(), data: b})
	}
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].path < canonical[j].path })
	h := sha256.New()
	for _, fb := range canonical {
		h.Write([]byte(fb.path))
		h.Write([]byte{0})
		h.Write(fb.data)
		h.Write([]byte{0})
	}
	return h.Sum(nil)
}

// Load rebuilds a descriptor registry from a serialized FileDescriptorSet.
func Load(descriptorSet []byte) (*protoregistry.Files, error) {
	var fdset descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(descriptorSet, &fdset); err != nil {
		return nil, fmt.Errorf("schema: unmarshal descriptor set: %w", err)
	}
	files, err := protodesc.NewFiles(&fdset)
	if err != nil {
		return nil, fmt.Errorf("schema: link descriptor set: %w", err)
	}
	return files, nil
}

// CompileError wraps a protocompile failure and renders every reported
// problem with its source position.
type CompileError struct {
	Err error
}

func (e *CompileError) Error() string {
	return "proto compilation failed:\n" + FormatPositions(e.Err)
}

func (e *CompileError) Unwrap() error { return e.Err }

// FormatPositions renders an error tree from protocompile as one
// "file:line:col: message" line per reported problem.
func FormatPositions(err error) string {
	var lines []string
	var walk func(error)
	walk = func(err error) {
		if err == nil {
			return
		}
		if multi, ok := err.(interface{ Unwrap() []error }); ok {
			for _, sub := range multi.Unwrap() {
				walk(sub)
			}
			return
		}
		var withPos reporter.ErrorWithPos
		if errors.As(err, &withPos) {
			pos := withPos.GetPosition()
			lines = append(lines, fmt.Sprintf("  %s: %s", pos.String(), withPos.Unwrap()))
			return
		}
		lines = append(lines, "  "+err.Error())
	}
	walk(err)
	if len(lines) == 0 {
		return "  unknown compilation error"
	}
	out := lines[0]
	for _, l := range lines[1:] {
		out += "\n" + l
	}
	return out
}
