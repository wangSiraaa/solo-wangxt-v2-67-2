package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"protocompat/internal/schema"
)

// depProvider implements schema.FileProvider by resolving import paths to
// immutable registered versions. The first time a path resolves to a
// package version, that version's entire stored descriptor closure is
// merged into the provider's cache, so transitive imports resolve to the
// versions the direct dependency was itself locked against — never to a
// newer registration. Two locked closures contributing different content
// for the same path is a diamond version conflict and fails the compile.
type depProvider struct {
	store Store

	mu       sync.Mutex
	cache    map[string]*descriptorpb.FileDescriptorProto
	conflict error
	missing  []string                 // paths requested but unknown to the registry
	used     map[string]lockedVersion // path -> owning version, for paths served from the store
	versions map[string]*Version      // "pkg\x00version" -> record (for digests)
}

type lockedVersion struct {
	pkg     string
	version string
}

func newDepProvider(store Store) *depProvider {
	return &depProvider{
		store:    store,
		cache:    map[string]*descriptorpb.FileDescriptorProto{},
		used:     map[string]lockedVersion{},
		versions: map[string]*Version{},
	}
}

// FindFile serves one pinned dependency file. It returns (nil, nil) when
// no registered package owns the path; the compiler then reports the
// import as unresolved. Well-known types are left to the standard
// imports and are never recorded as missing.
func (p *depProvider) FindFile(ctx context.Context, path string) (*descriptorpb.FileDescriptorProto, error) {
	if strings.HasPrefix(path, "google/protobuf/") {
		return nil, nil
	}
	p.mu.Lock()
	if fdp, ok := p.cache[path]; ok {
		p.mu.Unlock()
		return fdp, nil
	}
	if p.conflict != nil {
		err := p.conflict
		p.mu.Unlock()
		return nil, err
	}
	p.mu.Unlock()

	pkg, version, err := p.store.FindPathOwner(ctx, path)
	if errors.Is(err, ErrNotFound) {
		p.mu.Lock()
		p.missing = append(p.missing, path)
		p.mu.Unlock()
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ver, err := p.store.GetVersion(ctx, pkg, version)
	if err != nil {
		return nil, fmt.Errorf("load pinned dependency %s@%s: %w", pkg, version, err)
	}
	var fdset descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(ver.DescriptorSet, &fdset); err != nil {
		return nil, fmt.Errorf("decode pinned dependency %s@%s: %w", pkg, version, err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conflict != nil {
		return nil, p.conflict
	}
	key := pkg + "\x00" + version
	p.versions[key] = ver
	for _, f := range fdset.GetFile() {
		name := f.GetName()
		if strings.HasPrefix(name, "google/protobuf/") {
			// Well-known types always come from the compiler's standard
			// imports; merging them from pinned closures could only cause
			// false conflicts between protobuf runtime versions.
			continue
		}
		if existing, ok := p.cache[name]; ok {
			if !fdpEqual(existing, f) {
				p.conflict = fmt.Errorf(
					"dependency conflict: %s is provided by both %s and %s@%s with different content; "+
						"align the diamond dependency on a single version of %s",
					name, p.used[name].pkg+"@"+p.used[name].version, pkg, version, ownerOf(p.used, name))
				return nil, p.conflict
			}
			continue
		}
		p.cache[name] = f
		p.used[name] = lockedVersion{pkg: pkg, version: version}
	}
	fdp, ok := p.cache[path]
	if !ok {
		// The path owner no longer carries the file in its descriptor
		// set: the store is inconsistent; refuse rather than guess.
		return nil, fmt.Errorf("registered version %s@%s owns path %s but its descriptor set does not contain it", pkg, version, path)
	}
	return fdp, nil
}

func ownerOf(used map[string]lockedVersion, path string) string {
	if lv, ok := used[path]; ok {
		return lv.pkg
	}
	return "the conflicting package"
}

func fdpEqual(a, b *descriptorpb.FileDescriptorProto) bool {
	ab, err := proto.MarshalOptions{Deterministic: true}.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := proto.MarshalOptions{Deterministic: true}.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

// missingImports reports the import paths that no registered package
// could supply during the compile.
func (p *depProvider) missingImports() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.missing...)
}

// conflictErr reports a diamond version conflict encountered while
// merging pinned closures, if any.
func (p *depProvider) conflictErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conflict
}

// directLocks computes the dependency locks for a compiled package: the
// pinned (package, version, digest) of every external file directly
// imported by the package's own files. Transitive files pulled in through
// a dependency's closure are not locked here — they are implied by the
// direct dependency's own stored locks.
func (p *depProvider) directLocks(compiled *schema.Compiled) ([]DepLock, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	direct := map[string]lockedVersion{}
	for _, owned := range compiled.OwnedPaths {
		fd, err := compiled.Files.FindFileByPath(owned)
		if err != nil {
			return nil, fmt.Errorf("compiled closure missing owned file %s: %w", owned, err)
		}
		imports := fd.Imports()
		for i := 0; i < imports.Len(); i++ {
			path := imports.Get(i).Path()
			lv, ok := p.used[path]
			if !ok {
				continue // own file or well-known type: not a registry dependency
			}
			direct[lv.pkg] = lv
		}
	}
	locks := make([]DepLock, 0, len(direct))
	for _, lv := range direct {
		ver, ok := p.versions[lv.pkg+"\x00"+lv.version]
		if !ok {
			return nil, fmt.Errorf("internal: pinned version %s@%s not recorded", lv.pkg, lv.version)
		}
		locks = append(locks, DepLock{
			DepPackage: lv.pkg,
			DepVersion: lv.version,
			DepHash:    ver.ContentHash,
		})
	}
	sort.Slice(locks, func(i, j int) bool { return locks[i].DepPackage < locks[j].DepPackage })
	return locks, nil
}

// checkDepCycle refuses to let pkg depend on a package whose own locked
// dependency closure already contains pkg. Cycles are detected at package
// granularity over the stored locks of the pinned versions; the error
// renders the offending chain.
func checkDepCycle(ctx context.Context, store Store, pkg, newVersion string, locks []DepLock) error {
	type frame struct {
		pkg, version string
		chain        []string
	}
	queue := make([]frame, 0, len(locks))
	for _, l := range locks {
		queue = append(queue, frame{l.DepPackage, l.DepVersion, []string{pkg + "@" + newVersion}})
	}
	visited := map[string]bool{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		chain := append(append([]string(nil), cur.chain...), cur.pkg+"@"+cur.version)
		if cur.pkg == pkg {
			return fmt.Errorf("dependency cycle: %s", strings.Join(chain, " -> "))
		}
		key := cur.pkg + "\x00" + cur.version
		if visited[key] {
			continue
		}
		visited[key] = true
		if len(visited) > 1024 {
			return fmt.Errorf("dependency closure too deep to verify acyclicity")
		}
		deps, err := store.GetDeps(ctx, cur.pkg, cur.version)
		if err != nil {
			return fmt.Errorf("load locks of %s@%s: %w", cur.pkg, cur.version, err)
		}
		for _, d := range deps {
			queue = append(queue, frame{d.DepPackage, d.DepVersion, chain})
		}
	}
	return nil
}

// verifyDepDigests re-reads every locked version and refuses the
// registration when a locked digest no longer matches the stored content.
// Versions are immutable, so a mismatch means the store was tampered with;
// refusing is the only safe response.
func verifyDepDigests(ctx context.Context, store Store, locks []DepLock) error {
	for _, l := range locks {
		ver, err := store.GetVersion(ctx, l.DepPackage, l.DepVersion)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("locked dependency %s@%s is not registered", l.DepPackage, l.DepVersion)
		}
		if err != nil {
			return err
		}
		if !bytes.Equal(ver.ContentHash, l.DepHash) {
			return fmt.Errorf("digest mismatch for dependency %s@%s: locked digest does not match the registered content",
				l.DepPackage, l.DepVersion)
		}
	}
	return nil
}
