package regress

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"connectrpc.com/connect"

	"protocompat/internal/registry"
	"protocompat/internal/schema"
)

// ImpactCase is one dependency-locking / impact-analysis regression case.
// The case directory holds one proto tree per registration step plus a
// case.json describing the steps and the expected analysis.
type ImpactCase struct {
	Name          string             `json:"name"`
	Description   string             `json:"description,omitempty"`
	Registrations []RegistrationStep `json:"registrations"`
	Analyze       *AnalyzeStep       `json:"analyze,omitempty"`
	Expect        ImpactExpectation  `json:"expect"`
}

// RegistrationStep registers one package version from a proto tree in dir
// (relative to the case directory). ExpectError asserts the registration
// is rejected with a message containing the given substring.
type RegistrationStep struct {
	Package     string `json:"package"`
	Version     string `json:"version"`
	Dir         string `json:"dir"`
	ExpectError string `json:"expect_error,omitempty"`
}

// AnalyzeStep asks for an impact analysis of package base->head.
type AnalyzeStep struct {
	Package     string `json:"package"`
	BaseVersion string `json:"base_version"`
	HeadVersion string `json:"head_version"`
}

// ImpactExpectation describes the required analysis outcome and,
// optionally, exact registry state (versions and dependency locks).
type ImpactExpectation struct {
	Verdict string           `json:"verdict,omitempty"`
	Impacts []ExpectedImpact `json:"impacts,omitempty"`
	// Versions asserts the exact registered version list per package.
	Versions map[string][]string `json:"versions,omitempty"`
	// Dependencies asserts exact locks: "pkg@ver" -> ["dep@ver", ...].
	Dependencies map[string][]string `json:"dependencies,omitempty"`
}

// ExpectedImpact matches one reported package impact. ReasonPaths are
// package-name sequences ("acme.base" -> "acme.mid" -> "acme.top").
type ExpectedImpact struct {
	Package     string            `json:"package"`
	Impact      string            `json:"impact"`
	ReasonPaths [][]string        `json:"reason_paths,omitempty"`
	Findings    []ExpectedFinding `json:"findings,omitempty"`
}

// ImpactResult is the outcome of running one impact case.
type ImpactResult struct {
	Case     ImpactCase
	Dir      string
	Passed   bool
	Problems []string
	Response *registry.AnalyzeImpactResponse
}

// RunImpactCase executes the impact case in dir against a fresh service
// backed by the in-memory store.
func RunImpactCase(dir string) (*ImpactResult, error) {
	return RunImpactCaseWithStore(dir, registry.NewMemStore())
}

// RunImpactCaseWithStore is RunImpactCase against a caller-supplied
// store, letting the same cases verify the PostgreSQL implementation.
func RunImpactCaseWithStore(dir string, store registry.Store) (*ImpactResult, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "case.json"))
	if err != nil {
		return nil, fmt.Errorf("read case.json: %w", err)
	}
	var kase ImpactCase
	if err := json.Unmarshal(raw, &kase); err != nil {
		return nil, fmt.Errorf("parse case.json: %w", err)
	}

	svc := registry.NewService(store)
	ctx := context.Background()
	res := &ImpactResult{Case: kase, Dir: dir}

	for i, step := range kase.Registrations {
		files, err := LoadTree(filepath.Join(dir, step.Dir))
		if err != nil {
			return nil, fmt.Errorf("registration %d (%s@%s): %w", i, step.Package, step.Version, err)
		}
		reqFiles := make([]schema.SourceFile, len(files))
		copy(reqFiles, files)
		_, err = svc.RegisterVersion(ctx, connect.NewRequest(&registry.RegisterVersionRequest{
			Package: step.Package, Version: step.Version, Files: reqFiles,
		}))
		if step.ExpectError != "" {
			if err == nil {
				res.Problems = append(res.Problems,
					fmt.Sprintf("registration %s@%s: expected error containing %q, got success",
						step.Package, step.Version, step.ExpectError))
			} else if !strings.Contains(err.Error(), step.ExpectError) {
				res.Problems = append(res.Problems,
					fmt.Sprintf("registration %s@%s: error %q does not contain %q",
						step.Package, step.Version, err.Error(), step.ExpectError))
			}
			continue
		}
		if err != nil {
			res.Problems = append(res.Problems,
				fmt.Sprintf("registration %s@%s: unexpected error: %v", step.Package, step.Version, err))
		}
	}

	if kase.Analyze != nil {
		res.runAnalyze(ctx, svc, kase.Analyze)
	}
	res.checkState(ctx, svc)
	res.Passed = len(res.Problems) == 0
	return res, nil
}

// runAnalyze runs the analysis twice: the first run must compute, the
// second must return the identical stored result (reused=true).
func (r *ImpactResult) runAnalyze(ctx context.Context, svc *registry.Service, step *AnalyzeStep) {
	call := func() *registry.AnalyzeImpactResponse {
		resp, err := svc.AnalyzeImpact(ctx, connect.NewRequest(&registry.AnalyzeImpactRequest{
			Package: step.Package, BaseVersion: step.BaseVersion, HeadVersion: step.HeadVersion,
		}))
		if err != nil {
			r.Problems = append(r.Problems, fmt.Sprintf("analyze %s %s->%s: %v", step.Package, step.BaseVersion, step.HeadVersion, err))
			return nil
		}
		return resp.Msg
	}
	first := call()
	if first == nil {
		return
	}
	r.Response = first
	if first.Reused {
		r.Problems = append(r.Problems, "first analysis must compute (reused=false)")
	}
	second := call()
	if second == nil {
		return
	}
	if !second.Reused {
		r.Problems = append(r.Problems, "second analysis of the same input must reuse the stored result (reused=true)")
	}
	if !reflect.DeepEqual(first.Report, second.Report) || !reflect.DeepEqual(first.Impacts, second.Impacts) {
		r.Problems = append(r.Problems, "repeated analysis returned a different result")
	}

	if r.Case.Expect.Verdict != "" && string(first.Report.Verdict) != r.Case.Expect.Verdict {
		r.Problems = append(r.Problems,
			fmt.Sprintf("verdict: got %s, want %s", first.Report.Verdict, r.Case.Expect.Verdict))
	}
	r.checkImpacts(first)
}

func (r *ImpactResult) checkImpacts(resp *registry.AnalyzeImpactResponse) {
	want := r.Case.Expect.Impacts
	// Exact set match: every expected impact present once, and no extras.
	matched := make([]bool, len(resp.Impacts))
	for _, w := range want {
		idx := -1
		for i, im := range resp.Impacts {
			if !matched[i] && im.Package == w.Package {
				matched[i] = true
				idx = i
				break
			}
		}
		if idx < 0 {
			r.Problems = append(r.Problems, fmt.Sprintf("missing expected impact for package %s", w.Package))
			continue
		}
		r.checkOneImpact(w, &resp.Impacts[idx])
	}
	for i, im := range resp.Impacts {
		if !matched[i] {
			r.Problems = append(r.Problems, fmt.Sprintf("unexpected impact reported for package %s (%s)", im.Package, im.Impact))
		}
	}
}

func (r *ImpactResult) checkOneImpact(want ExpectedImpact, got *registry.PackageImpact) {
	if got.Impact != want.Impact {
		r.Problems = append(r.Problems,
			fmt.Sprintf("%s: impact = %s, want %s", want.Package, got.Impact, want.Impact))
	}
	if want.ReasonPaths != nil {
		gotPaths := map[string]bool{}
		for _, p := range got.ReasonPaths {
			var pkgs []string
			for _, h := range p.Hops {
				pkgs = append(pkgs, h.Package)
			}
			gotPaths[strings.Join(pkgs, ">")] = true
		}
		wantPaths := map[string]bool{}
		for _, seq := range want.ReasonPaths {
			wantPaths[strings.Join(seq, ">")] = true
		}
		if !reflect.DeepEqual(gotPaths, wantPaths) {
			r.Problems = append(r.Problems,
				fmt.Sprintf("%s: reason paths = %v, want %v", want.Package, keys(gotPaths), keys(wantPaths)))
		}
	}
	for _, wf := range want.Findings {
		found := false
		for _, f := range got.Findings {
			if f.Code == wf.Code && (wf.Path == "" || pathMatches(wf.Path, f)) &&
				(wf.Severity == "" || string(f.Severity) == wf.Severity) {
				found = true
				break
			}
		}
		if !found {
			r.Problems = append(r.Problems,
				fmt.Sprintf("%s: missing expected finding code=%s path=%q", want.Package, wf.Code, wf.Path))
		}
	}
	// Unlisted WARN/FAIL findings fail the case: silence is not a pass.
	for _, f := range got.Findings {
		if f.Severity != "FAIL" && f.Severity != "WARN" {
			continue
		}
		listed := false
		for _, wf := range want.Findings {
			if wf.Code == f.Code && (wf.Path == "" || pathMatches(wf.Path, f)) {
				listed = true
				break
			}
		}
		if !listed {
			r.Problems = append(r.Problems,
				fmt.Sprintf("%s: unexpected %s finding: %s at %s (%s)", want.Package, f.Severity, f.Code, displayPath(f), f.Detail))
		}
	}
}

// checkState verifies exact registry contents (versions and locks).
func (r *ImpactResult) checkState(ctx context.Context, svc *registry.Service) {
	for pkg, wantVersions := range r.Case.Expect.Versions {
		resp, err := svc.ListVersions(ctx, connect.NewRequest(&registry.ListVersionsRequest{Package: pkg}))
		if err != nil {
			r.Problems = append(r.Problems, fmt.Sprintf("list versions of %s: %v", pkg, err))
			continue
		}
		var got []string
		for _, v := range resp.Msg.Versions {
			got = append(got, v.Version)
		}
		if !reflect.DeepEqual(got, wantVersions) {
			r.Problems = append(r.Problems, fmt.Sprintf("%s: versions = %v, want %v", pkg, got, wantVersions))
		}
	}
	for key, wantDeps := range r.Case.Expect.Dependencies {
		pkg, ver, ok := strings.Cut(key, "@")
		if !ok {
			r.Problems = append(r.Problems, fmt.Sprintf("invalid dependencies key %q, want pkg@version", key))
			continue
		}
		resp, err := svc.ListDependencies(ctx, connect.NewRequest(&registry.ListDependenciesRequest{Package: pkg, Version: ver}))
		if err != nil {
			r.Problems = append(r.Problems, fmt.Sprintf("list dependencies of %s: %v", key, err))
			continue
		}
		var got []string
		for _, d := range resp.Msg.Dependencies {
			got = append(got, d.Package+"@"+d.Version)
		}
		sort.Strings(got)
		sortedWant := append([]string(nil), wantDeps...)
		sort.Strings(sortedWant)
		if !reflect.DeepEqual(got, sortedWant) {
			r.Problems = append(r.Problems, fmt.Sprintf("%s: dependencies = %v, want %v", key, got, sortedWant))
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RunImpactDir runs every impact case directory directly under root (or
// root itself if it contains a case.json).
func RunImpactDir(root string) ([]*ImpactResult, error) {
	if _, err := os.Stat(filepath.Join(root, "case.json")); err == nil {
		res, err := RunImpactCase(root)
		if err != nil {
			return nil, err
		}
		return []*ImpactResult{res}, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var results []*ImpactResult
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "case.json")); err != nil {
			continue
		}
		res, err := RunImpactCase(dir)
		if err != nil {
			return nil, fmt.Errorf("case %s: %w", e.Name(), err)
		}
		results = append(results, res)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no impact cases found under %s", root)
	}
	return results, nil
}
