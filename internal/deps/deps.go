// Package deps implements cross-package dependency locking: it extracts
// imports from sources before compilation, pins every imported package to
// an already registered immutable version, compiles against exactly those
// pinned descriptors, and summarizes the resolved lock set with a digest.
//
// Locks are immutable history: every stored package version records the
// exact dependency package/version/digest tuples it was compiled against.
// Impact analysis (see sibling package impact) walks those locks.
package deps

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"protocompat/internal/schema"
)

// WellKnownPrefix marks the well-known types supplied by the compiler
// itself; they are never locked as a registered dependency.
const WellKnownPrefix = "google/protobuf/"

// Pin is one requested dependency pin: package X must be registered at
// Version. When Digest is non-empty it must also match that version's
// content digest, otherwise registration is refused (lockfile-style
// verification).
type Pin struct {
	Package string `json:"package"`
	Version string `json:"version"`
	Digest  string `json:"digest,omitempty"`
}

// Lock is one resolved, verified dependency edge. It is stored with the
// dependent version and is the unit later digested and walked for
// transitive impact.
type Lock struct {
	Package string `json:"package"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

// ResolvedDependency is a dependency package plus every path the new
// version imports from it. Only imported paths are locked, so a pin on a
// package that contributes nothing is rejected.
type ResolvedDependency struct {
	Package string
	Version string
	Digest  []byte
	// Closure is that version's serialized FileDescriptorSet (the full
	// transitive closure as it was compiled against its own locks).
	Closure []byte
	// OwnedPaths are the file paths owned by that package version.
	OwnedPaths []string
	// ImportedPaths are the paths actually imported by the sources being
	// registered; it is a subset of OwnedPaths.
	ImportedPaths []string
}

// IsExternalImport reports whether an import path names something outside
// both the submitted sources and the well-known types.
func IsExternalImport(path string, submitted map[string]bool) bool {
	if submitted[path] {
		return false
	}
	return len(path) < len(WellKnownPrefix) || path[:len(WellKnownPrefix)] != WellKnownPrefix
}

// Digest summarizes a resolved lock set. The same unordered set of
// (package, version, digest) tuples always yields the same digest, and
// changing, adding or removing one tuple changes it. A package with no
// dependencies has the empty digest "".
func Digest(locks []Lock) string {
	if len(locks) == 0 {
		return ""
	}
	sorted := append([]Lock(nil), locks...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Package < sorted[j].Package })
	b, _ := json.Marshal(sorted)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// FromLocked builds Lock records in deterministic package order.
func FromLocked(rd []ResolvedDependency) []Lock {
	locks := make([]Lock, 0, len(rd))
	for _, d := range rd {
		locks = append(locks, Lock{
			Package: d.Package, Version: d.Version, Digest: hex.EncodeToString(d.Digest),
		})
	}
	sort.Slice(locks, func(i, j int) bool { return locks[i].Package < locks[j].Package })
	return locks
}

// OwnerPaths is the resolver index used while compiling: path -> one
// closure providing it. Actual ownership comes from the store's
// package_paths table; here closures are merged so diamonds (the same
// dependency reached through two paths) compile transparently while
// conflicting pins for the same file do not.
type OwnerPaths map[string]pathOwner

type pathOwner struct {
	// digest identifies the closure that supplies the path; it doubles
	// as the unmarshal cache key.
	digest  string
	closure []byte
}

// BuildOwnerPaths constructs the resolver index. Direct ownership comes
// from each dependency's OwnedPaths; other files in a dependency closure
// are transitive dependencies supplied through that closure. Two
// closures supplying the same path must carry the identical file
// descriptor (a diamond); different content means the pins conflict.
func BuildOwnerPaths(resolved []ResolvedDependency) (OwnerPaths, error) {
	idx := OwnerPaths{}
	for _, d := range resolved {
		digest := hex.EncodeToString(d.Digest)
		paths, err := schema.ClosurePaths(d.Closure)
		if err != nil {
			return nil, fmt.Errorf("dependency %s@%s: %w", d.Package, d.Version, err)
		}
		for _, p := range paths {
			if len(p) >= len(WellKnownPrefix) && p[:len(WellKnownPrefix)] == WellKnownPrefix {
				continue
			}
			existing, ok := idx[p]
			if !ok {
				idx[p] = pathOwner{digest: digest, closure: d.Closure}
				continue
			}
			// Same path through another dependency's closure: the file
			// descriptor itself must be identical, even when the two
			// closures have different overall content digests.
			if !sameDescriptor(existing.closure, d.Closure, p) {
				return nil, fmt.Errorf("import path %q resolves to conflicting content through pinned dependencies; pin consistent versions",
					p)
			}
		}
	}
	return idx, nil
}

// sameDescriptor reports whether a named file has identical canonical
// bytes in two serialized descriptor sets.
func sameDescriptor(a, b []byte, path string) bool {
	var sa, sb descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(a, &sa); err != nil {
		return false
	}
	if err := proto.Unmarshal(b, &sb); err != nil {
		return false
	}
	var fa, fb *descriptorpb.FileDescriptorProto
	for _, f := range sa.File {
		if f.GetName() == path {
			fa = f
		}
	}
	for _, f := range sb.File {
		if f.GetName() == path {
			fb = f
		}
	}
	if fa == nil || fb == nil {
		return false
	}
	ba, _ := proto.MarshalOptions{Deterministic: true}.Marshal(fa)
	bb, _ := proto.MarshalOptions{Deterministic: true}.Marshal(fb)
	return bytes.Equal(ba, bb)
}
