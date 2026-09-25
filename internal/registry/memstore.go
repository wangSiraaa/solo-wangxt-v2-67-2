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
	mu        sync.Mutex
	versions  map[string]map[string]*Version // package -> version -> record
	reports   []StoredReport
	consumers map[string]map[string]*ConsumerDecl // package -> consumer -> decl
}

func NewMemStore() *MemStore {
	return &MemStore{
		versions:  map[string]map[string]*Version{},
		consumers: map[string]map[string]*ConsumerDecl{},
	}
}

func (m *MemStore) PutVersion(_ context.Context, v Version) (bool, error) {
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
