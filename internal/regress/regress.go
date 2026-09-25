// Package regress runs file-based compatibility regression cases. Each
// case is a directory with old/ and new/ proto trees plus a case.json
// describing the expected verdict and findings. The compatcheck CLI and
// the Go test suite share this runner.
package regress

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"protocompat/internal/compat"
	"protocompat/internal/schema"
)

// Case is one regression case definition (case.json).
type Case struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Samples     []compat.Sample `json:"samples,omitempty"`
	Expect      Expectation     `json:"expect"`
}

// Expectation describes what the checker must conclude.
type Expectation struct {
	Verdict  compat.Verdict    `json:"verdict"`
	Findings []ExpectedFinding `json:"findings"`
}

// ExpectedFinding matches a finding by code and (optionally) path and
// severity. Path may be the bare field path ("email"), the message-qualified
// path ("acme.user.Contact.email") or just the message name.
type ExpectedFinding struct {
	Code     string `json:"code"`
	Path     string `json:"path,omitempty"`
	Severity string `json:"severity,omitempty"`
}

// Result is the outcome of running one case.
type Result struct {
	Case     Case
	Dir      string
	Passed   bool
	Problems []string
	Report   *compat.Report
}

// RunCase executes the case in dir (containing case.json, old/, new/).
func RunCase(dir string) (*Result, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "case.json"))
	if err != nil {
		return nil, fmt.Errorf("read case.json: %w", err)
	}
	var kase Case
	if err := json.Unmarshal(raw, &kase); err != nil {
		return nil, fmt.Errorf("parse case.json: %w", err)
	}

	oldFiles, err := LoadTree(filepath.Join(dir, "old"))
	if err != nil {
		return nil, fmt.Errorf("load old tree: %w", err)
	}
	newFiles, err := LoadTree(filepath.Join(dir, "new"))
	if err != nil {
		return nil, fmt.Errorf("load new tree: %w", err)
	}

	ctx := context.Background()
	oldCompiled, err := schema.Compile(ctx, oldFiles)
	if err != nil {
		return nil, fmt.Errorf("compile old schema: %w", err)
	}
	newCompiled, err := schema.Compile(ctx, newFiles)
	if err != nil {
		return nil, fmt.Errorf("compile new schema: %w", err)
	}

	owned := append(append([]string{}, oldCompiled.OwnedPaths...), newCompiled.OwnedPaths...)
	report := compat.Check(compat.Input{
		Old: oldCompiled.Files, New: newCompiled.Files,
		OwnedPaths: owned, Samples: kase.Samples,
	})

	res := &Result{Case: kase, Dir: dir, Report: report}
	res.evaluate()
	return res, nil
}

// RunDir runs every case directory directly under root (or root itself if
// it contains a case.json).
func RunDir(root string) ([]*Result, error) {
	if _, err := os.Stat(filepath.Join(root, "case.json")); err == nil {
		res, err := RunCase(root)
		if err != nil {
			return nil, err
		}
		return []*Result{res}, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var results []*Result
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "case.json")); err != nil {
			continue
		}
		res, err := RunCase(dir)
		if err != nil {
			return nil, fmt.Errorf("case %s: %w", e.Name(), err)
		}
		results = append(results, res)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no cases found under %s", root)
	}
	return results, nil
}

// evaluate compares the report against the expectation.
func (r *Result) evaluate() {
	if r.Report.Verdict != r.Case.Expect.Verdict {
		r.Problems = append(r.Problems,
			fmt.Sprintf("verdict: got %s, want %s", r.Report.Verdict, r.Case.Expect.Verdict))
	}
	for _, want := range r.Case.Expect.Findings {
		if !r.hasFinding(want) {
			r.Problems = append(r.Problems,
				fmt.Sprintf("missing expected finding: code=%s path=%q severity=%s", want.Code, want.Path, want.Severity))
		}
	}
	// Unexpected WARN/FAIL findings fail the case: silence is not a pass.
	for _, f := range r.Report.Findings {
		if f.Severity == compat.SeverityInfo {
			continue
		}
		if !r.expected(f) {
			r.Problems = append(r.Problems,
				fmt.Sprintf("unexpected %s finding: %s at %s (%s)", f.Severity, f.Code, displayPath(f), f.Detail))
		}
	}
	r.Passed = len(r.Problems) == 0
}

func (r *Result) hasFinding(want ExpectedFinding) bool {
	for _, f := range r.Report.Findings {
		if f.Code != want.Code {
			continue
		}
		if want.Path != "" && !pathMatches(want.Path, f) {
			continue
		}
		if want.Severity != "" && string(f.Severity) != want.Severity {
			continue
		}
		return true
	}
	return false
}

func (r *Result) expected(f compat.Finding) bool {
	for _, want := range r.Case.Expect.Findings {
		if want.Code != f.Code {
			continue
		}
		if want.Path != "" && !pathMatches(want.Path, f) {
			continue
		}
		return true
	}
	return false
}

// pathMatches allows expectations to name a finding by its field path,
// its message-qualified path, or its message alone.
func pathMatches(want string, f compat.Finding) bool {
	if want == f.Path || want == f.Message {
		return true
	}
	if f.Message != "" && f.Path != "" && want == f.Message+"."+f.Path {
		return true
	}
	return false
}

func displayPath(f compat.Finding) string {
	if f.Message == "" {
		return f.Path
	}
	if f.Path == "" {
		return f.Message
	}
	return f.Message + "." + f.Path
}

// LoadTree reads every .proto file under root, keyed by slash-separated
// path relative to root so imports resolve naturally.
func LoadTree(root string) ([]schema.SourceFile, error) {
	var files []schema.SourceFile
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, schema.SourceFile{
			Path:    filepath.ToSlash(rel),
			Content: string(content),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .proto files under %s", root)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}
