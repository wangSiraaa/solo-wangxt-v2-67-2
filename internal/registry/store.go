package registry

import (
	"context"
	"errors"
	"time"

	"protocompat/internal/compat"
)

// ErrVersionConflict is returned when a (package, version) pair already
// exists with different content. Versions are immutable: same version,
// different content is always rejected, never overwritten.
var ErrVersionConflict = errors.New("version already exists with different content")

// ErrNotFound is returned for unknown packages, versions or consumers.
var ErrNotFound = errors.New("not found")

// Version is one immutable registered schema version.
type Version struct {
	Package       string
	Version       string
	ContentHash   []byte
	DescriptorSet []byte
	OwnedPaths    []string
	CreatedAt     time.Time
}

// ConsumerDecl is a consumer's declared usage of a package: which
// messages/fields it reads and over which encoding. Declarations let the
// service project global findings onto the surface a consumer actually
// depends on.
type ConsumerDecl struct {
	Package   string
	Consumer  string
	Encoding  string // "wire", "json" or "both"
	Usages    []Usage
	UpdatedAt time.Time
}

// StoredReport is a persisted compatibility verdict.
type StoredReport struct {
	Package     string
	BaseVersion string
	HeadVersion string
	Report      *compat.Report
	CreatedAt   time.Time
}

// Store persists packages, versions, consumer declarations and reports.
type Store interface {
	// PutVersion registers an immutable version. It returns created=false
	// when the exact same content was already registered (idempotent
	// retry), and ErrVersionConflict when the version exists with
	// different content.
	PutVersion(ctx context.Context, v Version) (created bool, err error)
	GetVersion(ctx context.Context, pkg, version string) (*Version, error)
	// LatestVersion returns the most recently registered version.
	LatestVersion(ctx context.Context, pkg string) (*Version, error)
	ListVersions(ctx context.Context, pkg string) ([]Version, error)

	PutReport(ctx context.Context, rep StoredReport) error

	UpsertConsumer(ctx context.Context, decl ConsumerDecl) error
	GetConsumer(ctx context.Context, pkg, consumer string) (*ConsumerDecl, error)
}
