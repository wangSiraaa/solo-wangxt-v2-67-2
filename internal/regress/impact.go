// Cross-package dependency/impact regression scenarios. Unlike the
// single-tree cases, each scenario drives the registry service through a
// sequence of registrations and impact analyses against an in-memory
// store, then asserts locks, rejections and the de-duplicated impact
// classification. The same scenarios run under the compatcheck CLI and
// go test.
package regress

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"connectrpc.com/connect"

	"protocompat/internal/deps"
	"protocompat/internal/registry"
)

// ImpactScenario is one cross-package case (scenario.json).
type ImpactScenario struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Register    []RegisterStep `json:"register"`
	Analyze     *AnalyzeStep   `json:"analyze,omitempty"`
	// ThenAnalyze runs further analyses after post-registration steps
	// (the first Analyze's Then list). Used to verify that the break in
	// a newer dependency version locates only consumers locked to the
	// right base version.
	ThenAnalyze []AnalyzeStep `json:"then_analyze,omitempty"`
}

// RegisterStep registers one package version, optionally expecting the
// call to be refused.
type RegisterStep struct {
	Package        string     `json:"package"`
	Version        string     `json:"version"`
	Dir            string     `json:"dir"` // directory holding this version's .proto tree
	Pins           []deps.Pin `json:"pins,omitempty"`
	ExpectRejected bool       `json:"expect_rejected,omitempty"`
	RejectCode     string     `json:"reject_code,omitempty"` // connect error code, e.g. failed_precondition
	// ExpectLocks asserts the stored dependency summary after a
	// successful registration.
	ExpectLocks []ExpectedLock `json:"expect_locks,omitempty"`
	// ExpectMissing asserts that a rejected registration stored nothing:
	// the version row is absent afterwards.
	ExpectMissing bool `json:"expect_missing,omitempty"`
}

// ExpectedLock matches one lock tuple.
type ExpectedLock struct {
	Package string `json:"package"`
	Version string `json:"version"`
}

// AnalyzeStep runs one impact analysis and asserts the de-duplicated
// nodes. Repeat > 0 repeats the same request and asserts the digest
// stays identical (memoization).
type AnalyzeStep struct {
	Package          string               `json:"package"`
	BaseVersion      string               `json:"base_version"`
	CandidateVersion string               `json:"candidate_version"`
	Verdict          string               `json:"verdict,omitempty"`
	Nodes            []ExpectedImpactNode `json:"nodes,omitempty"`
	Repeat           int                  `json:"repeat,omitempty"`
	// Then registers extra package versions before repeating the
	// analysis; the digest must remain unchanged when those versions are
	// outside the analyzed subgraph.
	Then []RegisterStep `json:"then,omitempty"`
}

// ExpectedImpactNode asserts one node exactly once.
type ExpectedImpactNode struct {
	Package      string `json:"package"`
	Version      string `json:"version"`
	Status       string `json:"status"`
	Verdict      string `json:"verdict,omitempty"`
	MinPaths     int    `json:"min_paths,omitempty"`
	ExactlyPaths int    `json:"exactly_paths,omitempty"`
}

// ImpactResult is the outcome of running one scenario.
type ImpactResult struct {
	Scenario ImpactScenario
	Dir      string
	Passed   bool
	Problems []string
	Digest   string
}

// LoadImpactScenario reads scenario.json from dir.
func LoadImpactScenario(dir string) (*ImpactScenario, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "scenario.json"))
	if err != nil {
		return nil, fmt.Errorf("read scenario.json: %w", err)
	}
	var s ImpactScenario
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse scenario.json: %w", err)
	}
	return &s, nil
}

// RunImpactScenario executes one scenario against a fresh in-memory
// registry service.
func RunImpactScenario(dir string) (*ImpactResult, error) {
	s, err := LoadImpactScenario(dir)
	if err != nil {
		return nil, err
	}
	res := &ImpactResult{Scenario: *s, Dir: dir}

	svc := registry.NewService(registry.NewMemStore())
	ctx := context.Background()

	for i := range s.Register {
		if err := runRegister(ctx, svc, dir, s.Register[i], res); err != nil {
			return res, err
		}
	}

	if s.Analyze != nil {
		digest, err := runAnalyze(ctx, svc, *s.Analyze, res)
		if err != nil {
			return res, err
		}
		res.Digest = digest
		// Register post-analysis versions and repeat: the historical
		// digest must survive.
		for i := range s.Analyze.Then {
			if err := runRegister(ctx, svc, dir, s.Analyze.Then[i], res); err != nil {
				return res, err
			}
		}
		for n := 0; n < s.Analyze.Repeat; n++ {
			again, err := runAnalyzeNoExpect(ctx, svc, *s.Analyze)
			if err != nil {
				return res, err
			}
			if again != digest {
				res.Problems = append(res.Problems,
					fmt.Sprintf("analysis digest not reproducible after repeat %d: %s != %s", n+1, again, digest))
			}
		}
		for i := range s.ThenAnalyze {
			if _, err := runAnalyze(ctx, svc, s.ThenAnalyze[i], res); err != nil {
				return res, err
			}
		}
	}

	res.Passed = len(res.Problems) == 0
	return res, nil
}

func runRegister(ctx context.Context, svc *registry.Service, root string, step RegisterStep, res *ImpactResult) error {
	files, err := LoadTree(filepath.Join(root, step.Dir))
	if err != nil {
		return fmt.Errorf("load %s: %w", step.Dir, err)
	}
	req := connect.NewRequest(&registry.RegisterVersionRequest{
		Package: step.Package, Version: step.Version,
		Files: files, Pins: step.Pins,
	})
	resp, err := svc.RegisterVersion(ctx, req)
	if step.ExpectRejected {
		if err == nil {
			res.Problems = append(res.Problems,
				fmt.Sprintf("register %s@%s: expected rejection but it succeeded", step.Package, step.Version))
			return nil
		}
		if step.RejectCode != "" && connect.CodeOf(err).String() != step.RejectCode {
			res.Problems = append(res.Problems,
				fmt.Sprintf("register %s@%s: code = %s, want %s (%v)",
					step.Package, step.Version, connect.CodeOf(err), step.RejectCode, err))
		}
	} else if err != nil {
		res.Problems = append(res.Problems,
			fmt.Sprintf("register %s@%s: unexpected error: %v", step.Package, step.Version, err))
		return nil
	}

	if step.ExpectMissing {
		// Use GetDependencies as the "does the version exist?" probe; a
		// missing version is NotFound.
		_, gerr := svc.GetDependencies(ctx, connect.NewRequest(&registry.GetDependenciesRequest{
			Package: step.Package, Version: step.Version,
		}))
		if gerr == nil {
			res.Problems = append(res.Problems,
				fmt.Sprintf("register %s@%s rejected but the version was stored (dirty data)",
					step.Package, step.Version))
		}
	}

	if len(step.ExpectLocks) > 0 && err == nil {
		dresp, gerr := svc.GetDependencies(ctx, connect.NewRequest(&registry.GetDependenciesRequest{
			Package: step.Package, Version: step.Version,
		}))
		if gerr != nil {
			res.Problems = append(res.Problems,
				fmt.Sprintf("get dependencies %s@%s: %v", step.Package, step.Version, gerr))
			return nil
		}
		got := map[string]string{}
		for _, l := range dresp.Msg.Summary.Locks {
			got[l.Package] = l.Version
		}
		for _, want := range step.ExpectLocks {
			if got[want.Package] != want.Version {
				res.Problems = append(res.Problems,
					fmt.Sprintf("locks of %s@%s: %s -> %s, want %s",
						step.Package, step.Version, want.Package, got[want.Package], want.Version))
			}
		}
		if len(got) != len(step.ExpectLocks) {
			res.Problems = append(res.Problems,
				fmt.Sprintf("locks of %s@%s: got %d edges, want %d",
					step.Package, step.Version, len(got), len(step.ExpectLocks)))
		}
		if dresp.Msg.Summary.Digest == "" {
			res.Problems = append(res.Problems,
				fmt.Sprintf("locks of %s@%s: empty dependency summary digest", step.Package, step.Version))
		}
	}
	_ = resp
	return nil
}

func runAnalyze(ctx context.Context, svc *registry.Service, step AnalyzeStep, res *ImpactResult) (string, error) {
	resp, err := svc.AnalyzeImpact(ctx, connect.NewRequest(&registry.AnalyzeImpactRequest{
		Package: step.Package, BaseVersion: step.BaseVersion, CandidateVersion: step.CandidateVersion,
	}))
	if err != nil {
		return "", fmt.Errorf("analyze %s %s->%s: %w", step.Package, step.BaseVersion, step.CandidateVersion, err)
	}
	an := resp.Msg.Analysis
	if step.Verdict != "" && string(an.Verdict) != step.Verdict {
		res.Problems = append(res.Problems,
			fmt.Sprintf("analyze %s %s->%s: verdict = %s, want %s",
				step.Package, step.BaseVersion, step.CandidateVersion, an.Verdict, step.Verdict))
	}
	byKey := map[string]registry.ImpactNode{}
	for _, n := range an.Nodes {
		key := n.Package + "@" + n.Version
		if _, dup := byKey[key]; dup {
			res.Problems = append(res.Problems,
				fmt.Sprintf("node %s reported more than once", key))
		}
		byKey[key] = n
	}
	for _, want := range step.Nodes {
		key := want.Package + "@" + want.Version
		n, ok := byKey[key]
		if !ok {
			res.Problems = append(res.Problems,
				fmt.Sprintf("expected node %s (%s) is missing; got nodes: %s",
					key, want.Status, strings.Join(nodeKeys(an.Nodes), ", ")))
			continue
		}
		if n.Status != want.Status {
			res.Problems = append(res.Problems,
				fmt.Sprintf("node %s: status = %s, want %s", key, n.Status, want.Status))
		}
		if want.Verdict != "" && string(n.Verdict) != want.Verdict {
			res.Problems = append(res.Problems,
				fmt.Sprintf("node %s: verdict = %s, want %s", key, n.Verdict, want.Verdict))
		}
		if want.MinPaths > 0 && len(n.ReasonPaths) < want.MinPaths {
			res.Problems = append(res.Problems,
				fmt.Sprintf("node %s: %d reason paths, want at least %d", key, len(n.ReasonPaths), want.MinPaths))
		}
		if want.ExactlyPaths > 0 && len(n.ReasonPaths) != want.ExactlyPaths {
			res.Problems = append(res.Problems,
				fmt.Sprintf("node %s: %d reason paths, want exactly %d", key, len(n.ReasonPaths), want.ExactlyPaths))
		}
	}
	if len(byKey) != len(step.Nodes) {
		res.Problems = append(res.Problems,
			fmt.Sprintf("analyze %s: got %d nodes, expected %d (got: %s)",
				step.Package, len(byKey), len(step.Nodes), strings.Join(nodeKeys(an.Nodes), ", ")))
	}
	return resp.Msg.InputDigest, nil
}

func runAnalyzeNoExpect(ctx context.Context, svc *registry.Service, step AnalyzeStep) (string, error) {
	resp, err := svc.AnalyzeImpact(ctx, connect.NewRequest(&registry.AnalyzeImpactRequest{
		Package: step.Package, BaseVersion: step.BaseVersion, CandidateVersion: step.CandidateVersion,
	}))
	if err != nil {
		return "", err
	}
	return resp.Msg.InputDigest, nil
}

func nodeKeys(nodes []registry.ImpactNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Package+"@"+n.Version+"="+n.Status)
	}
	sort.Strings(out)
	return out
}

// RunImpactDir runs every scenario directory directly under root (or
// root itself when it contains a scenario.json).
func RunImpactDir(root string) ([]*ImpactResult, error) {
	if _, err := os.Stat(filepath.Join(root, "scenario.json")); err == nil {
		res, err := RunImpactScenario(root)
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
		if _, err := os.Stat(filepath.Join(dir, "scenario.json")); err != nil {
			continue
		}
		res, err := RunImpactScenario(dir)
		if err != nil {
			return nil, fmt.Errorf("scenario %s: %w", e.Name(), err)
		}
		results = append(results, res)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Scenario.Name < results[j].Scenario.Name })
	if len(results) == 0 {
		return nil, fmt.Errorf("no scenarios found under %s", root)
	}
	return results, nil
}
