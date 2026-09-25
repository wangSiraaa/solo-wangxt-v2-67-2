package registry

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// PGStore is the PostgreSQL-backed Store. Packages, immutable versions,
// consumer declarations and compatibility reports live here.
type PGStore struct {
	db *sql.DB
}

func NewPGStore(db *sql.DB) *PGStore {
	return &PGStore{db: db}
}

// ensurePackage returns the package id, creating the row if needed.
func (s *PGStore) ensurePackage(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
		WITH ins AS (
			INSERT INTO packages (name) VALUES ($1)
			ON CONFLICT (name) DO NOTHING
			RETURNING id
		)
		SELECT id FROM ins
		UNION ALL SELECT id FROM packages WHERE name = $1
		LIMIT 1`, name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ensure package %q: %w", name, err)
	}
	return id, nil
}

func (s *PGStore) PutVersion(ctx context.Context, v Version, deps []DepLock) (bool, error) {
	owned, err := json.Marshal(v.OwnedPaths)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	pkgID, err := s.ensurePackage(ctx, tx, v.Package)
	if err != nil {
		return false, err
	}

	var insertedID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO versions (package_id, version, content_hash, descriptor_set, owned_paths)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (package_id, version) DO NOTHING
		RETURNING id`, pkgID, v.Version, v.ContentHash, v.DescriptorSet, owned).Scan(&insertedID)
	switch {
	case err == nil:
		// New row inserted.
	case errors.Is(err, sql.ErrNoRows):
		// (package, version) exists: identical content is an idempotent
		// retry; different content is rejected, never overwritten. Locks
		// and path ownership stay as first written.
		var existingHash []byte
		qerr := tx.QueryRowContext(ctx, `
			SELECT content_hash FROM versions WHERE package_id = $1 AND version = $2`,
			pkgID, v.Version).Scan(&existingHash)
		if qerr != nil {
			return false, fmt.Errorf("check existing version: %w", qerr)
		}
		if !bytes.Equal(existingHash, v.ContentHash) {
			return false, ErrVersionConflict
		}
		return false, nil
	default:
		return false, fmt.Errorf("insert version: %w", err)
	}

	// Dependency locks, path ownership and the version row commit or roll
	// back together: a rejected registration leaves no dirty data.
	for _, d := range deps {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO version_deps (package_id, version, dep_package, dep_version, dep_hash)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (package_id, version, dep_package) DO NOTHING`,
			pkgID, v.Version, d.DepPackage, d.DepVersion, d.DepHash); err != nil {
			return false, fmt.Errorf("insert dependency lock: %w", err)
		}
	}
	for _, path := range v.OwnedPaths {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO path_owners (path, package_id, version)
			VALUES ($1, $2, $3)
			ON CONFLICT (path)
			DO UPDATE SET package_id = EXCLUDED.package_id, version = EXCLUDED.version, updated_at = now()`,
			path, pkgID, v.Version); err != nil {
			return false, fmt.Errorf("upsert path owner: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *PGStore) GetVersion(ctx context.Context, pkg, version string) (*Version, error) {
	var v Version
	var owned []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT p.name, v.version, v.content_hash, v.descriptor_set, v.owned_paths, v.created_at
		FROM versions v JOIN packages p ON p.id = v.package_id
		WHERE p.name = $1 AND v.version = $2`, pkg, version).
		Scan(&v.Package, &v.Version, &v.ContentHash, &v.DescriptorSet, &owned, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(owned, &v.OwnedPaths); err != nil {
		return nil, fmt.Errorf("decode owned paths: %w", err)
	}
	return &v, nil
}

func (s *PGStore) LatestVersion(ctx context.Context, pkg string) (*Version, error) {
	var v Version
	var owned []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT p.name, v.version, v.content_hash, v.descriptor_set, v.owned_paths, v.created_at
		FROM versions v JOIN packages p ON p.id = v.package_id
		WHERE p.name = $1
		ORDER BY v.created_at DESC, v.id DESC
		LIMIT 1`, pkg).
		Scan(&v.Package, &v.Version, &v.ContentHash, &v.DescriptorSet, &owned, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(owned, &v.OwnedPaths); err != nil {
		return nil, fmt.Errorf("decode owned paths: %w", err)
	}
	return &v, nil
}

func (s *PGStore) ListVersions(ctx context.Context, pkg string) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.name, v.version, v.content_hash, v.owned_paths, v.created_at
		FROM versions v JOIN packages p ON p.id = v.package_id
		WHERE p.name = $1
		ORDER BY v.created_at, v.id`, pkg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		var owned []byte
		if err := rows.Scan(&v.Package, &v.Version, &v.ContentHash, &owned, &v.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(owned, &v.OwnedPaths); err != nil {
			return nil, fmt.Errorf("decode owned paths: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *PGStore) FindPathOwner(ctx context.Context, path string) (string, string, error) {
	var pkg, version string
	err := s.db.QueryRowContext(ctx, `
		SELECT p.name, o.version
		FROM path_owners o JOIN packages p ON p.id = o.package_id
		WHERE o.path = $1`, path).Scan(&pkg, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	return pkg, version, nil
}

func (s *PGStore) GetDeps(ctx context.Context, pkg, version string) ([]DepLock, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.dep_package, d.dep_version, d.dep_hash
		FROM version_deps d JOIN packages p ON p.id = d.package_id
		WHERE p.name = $1 AND d.version = $2
		ORDER BY d.dep_package`, pkg, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DepLock
	for rows.Next() {
		var d DepLock
		if err := rows.Scan(&d.DepPackage, &d.DepVersion, &d.DepHash); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *PGStore) Dependents(ctx context.Context, pkg string) ([]DependentEdge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.name, d.version, d.dep_version
		FROM version_deps d JOIN packages p ON p.id = d.package_id
		WHERE d.dep_package = $1
		ORDER BY p.name, d.version`, pkg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DependentEdge
	for rows.Next() {
		var e DependentEdge
		if err := rows.Scan(&e.Package, &e.Version, &e.DepVersion); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *PGStore) GetImpactAnalysis(ctx context.Context, pkg, base, head string) (*StoredImpact, error) {
	var a StoredImpact
	var result []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT p.name, a.base_version, a.head_version, a.input_hash, a.result, a.created_at
		FROM impact_analyses a JOIN packages p ON p.id = a.package_id
		WHERE p.name = $1 AND a.base_version = $2 AND a.head_version = $3`, pkg, base, head).
		Scan(&a.Package, &a.BaseVersion, &a.HeadVersion, &a.InputHash, &result, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Result = &ImpactResult{}
	if err := json.Unmarshal(result, a.Result); err != nil {
		return nil, fmt.Errorf("decode impact analysis: %w", err)
	}
	return &a, nil
}

func (s *PGStore) PutImpactAnalysis(ctx context.Context, a StoredImpact) (bool, error) {
	body, err := json.Marshal(a.Result)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	pkgID, err := s.ensurePackage(ctx, tx, a.Package)
	if err != nil {
		return false, err
	}
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO impact_analyses (package_id, base_version, head_version, input_hash, result)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (package_id, base_version, head_version) DO NOTHING
		RETURNING id`, pkgID, a.BaseVersion, a.HeadVersion, a.InputHash, body).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// Same input was analyzed concurrently; the stored row wins.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("insert impact analysis: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *PGStore) PutReport(ctx context.Context, rep StoredReport) error {
	body, err := json.Marshal(rep.Report)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	pkgID, err := s.ensurePackage(ctx, tx, rep.Package)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO reports (package_id, base_version, head_version, verdict, report)
		VALUES ($1, $2, $3, $4, $5)`,
		pkgID, rep.BaseVersion, rep.HeadVersion, string(rep.Report.Verdict), body)
	if err != nil {
		return fmt.Errorf("insert report: %w", err)
	}
	return tx.Commit()
}

func (s *PGStore) UpsertConsumer(ctx context.Context, decl ConsumerDecl) error {
	usages, err := json.Marshal(decl.Usages)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	pkgID, err := s.ensurePackage(ctx, tx, decl.Package)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO consumers (package_id, consumer, encoding, usages)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (package_id, consumer)
		DO UPDATE SET encoding = EXCLUDED.encoding, usages = EXCLUDED.usages, updated_at = now()`,
		pkgID, decl.Consumer, decl.Encoding, usages)
	if err != nil {
		return fmt.Errorf("upsert consumer: %w", err)
	}
	return tx.Commit()
}

func (s *PGStore) GetConsumer(ctx context.Context, pkg, consumer string) (*ConsumerDecl, error) {
	var d ConsumerDecl
	var usages []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT p.name, c.consumer, c.encoding, c.usages, c.updated_at
		FROM consumers c JOIN packages p ON p.id = c.package_id
		WHERE p.name = $1 AND c.consumer = $2`, pkg, consumer).
		Scan(&d.Package, &d.Consumer, &d.Encoding, &usages, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(usages, &d.Usages); err != nil {
		return nil, fmt.Errorf("decode usages: %w", err)
	}
	return &d, nil
}
