package registry

import (
	"bytes"
	"context"
	"sort"
	"sync"
	"time"
)

// MemStore is an in-memory Store with the same semantics as the
// PostgreSQL store. It backs unit tests and local dry-runs.
type MemStore struct {
	mu         sync.Mutex
	versions   map[string]map[string]*Version  // package -> version -> record
	deps       map[string]map[string][]DepLock // package -> version -> locks
	pathOwners map[string]pathOwner            // import path -> owning version
	impacts    map[string]*StoredImpact        // pkg\x00base\x00head -> analysis
	reports    []StoredReport
	consumers  map[string]map[string]*ConsumerDecl // package -> consumer -> decl
}

type pathOwner struct {
	pkg     string
	version string
}

func NewMemStore() *MemStore {
	return &MemStore{
		versions:   map[string]map[string]*Version{},
		deps:       map[string]map[string][]DepLock{},
		pathOwners: map[string]pathOwner{},
		impacts:    map[string]*StoredImpact{},
		consumers:  map[string]map[string]*ConsumerDecl{},
	}
}

func (m *MemStore) PutVersion(_ context.Context, v Version, deps []DepLock) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pkg, ok := m.versions[v.Package]
	if !ok {
		pkg = map[string]*Version{}
		m.versions[v.Package] = pkg
	}
	if existing, ok := pkg[v.Version]; ok {
		if bytes.Equal(existing.ContentHash, v.ContentHash) {
			return false, nil // idempotent re-registration of identical content
		}
		return false, ErrVersionConflict
	}
	cp := v
	cp.CreatedAt = time.Now()
	pkg[v.Version] = &cp

	// Dependency locks and path ownership are written in the same critical
	// section as the version itself: the three either all exist or none do.
	if m.deps[v.Package] == nil {
		m.deps[v.Package] = map[string][]DepLock{}
	}
	depCopy := append([]DepLock(nil), deps...)
	sort.Slice(depCopy, func(i, j int) bool { return depCopy[i].DepPackage < depCopy[j].DepPackage })
	m.deps[v.Package][v.Version] = depCopy
	for _, path := range v.OwnedPaths {
		m.pathOwners[path] = pathOwner{pkg: v.Package, version: v.Version}
	}
	return true, nil
}

func (m *MemStore) GetVersion(_ context.Context, pkg, version string) (*Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.versions[pkg]; ok {
		if v, ok := p[version]; ok {
			cp := *v
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) LatestVersion(_ context.Context, pkg string) (*Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.versions[pkg]
	if !ok {
		return nil, ErrNotFound
	}
	var latest *Version
	for _, v := range p {
		if latest == nil || v.CreatedAt.After(latest.CreatedAt) {
			latest = v
		}
	}
	if latest == nil {
		return nil, ErrNotFound
	}
	cp := *latest
	return &cp, nil
}

func (m *MemStore) ListVersions(_ context.Context, pkg string) ([]Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Version
	for _, v := range m.versions[pkg] {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *MemStore) FindPathOwner(_ context.Context, path string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.pathOwners[path]; ok {
		return o.pkg, o.version, nil
	}
	return "", "", ErrNotFound
}

func (m *MemStore) GetDeps(_ context.Context, pkg, version string) ([]DepLock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.deps[pkg]; ok {
		if deps, ok := p[version]; ok {
			return append([]DepLock(nil), deps...), nil
		}
	}
	return nil, nil
}

func (m *MemStore) Dependents(_ context.Context, pkg string) ([]DependentEdge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []DependentEdge
	for depPkg, versions := range m.deps {
		for ver, locks := range versions {
			for _, l := range locks {
				if l.DepPackage == pkg {
					out = append(out, DependentEdge{Package: depPkg, Version: ver, DepVersion: l.DepVersion})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Package != out[j].Package {
			return out[i].Package < out[j].Package
		}
		return out[i].Version < out[j].Version
	})
	return out, nil
}

func impactKey(pkg, base, head string) string { return pkg + "\x00" + base + "\x00" + head }

func (m *MemStore) GetImpactAnalysis(_ context.Context, pkg, base, head string) (*StoredImpact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.impacts[impactKey(pkg, base, head)]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, ErrNotFound
}

func (m *MemStore) PutImpactAnalysis(_ context.Context, a StoredImpact) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := impactKey(a.Package, a.BaseVersion, a.HeadVersion)
	if _, ok := m.impacts[key]; ok {
		return false, nil
	}
	a.CreatedAt = time.Now()
	cp := a
	m.impacts[key] = &cp
	return true, nil
}

func (m *MemStore) PutReport(_ context.Context, rep StoredReport) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rep.CreatedAt = time.Now()
	m.reports = append(m.reports, rep)
	return nil
}

func (m *MemStore) UpsertConsumer(_ context.Context, decl ConsumerDecl) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pkg, ok := m.consumers[decl.Package]
	if !ok {
		pkg = map[string]*ConsumerDecl{}
		m.consumers[decl.Package] = pkg
	}
	decl.UpdatedAt = time.Now()
	cp := decl
	pkg[decl.Consumer] = &cp
	return nil
}

func (m *MemStore) GetConsumer(_ context.Context, pkg, consumer string) (*ConsumerDecl, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.consumers[pkg]; ok {
		if d, ok := p[consumer]; ok {
			cp := *d
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}
