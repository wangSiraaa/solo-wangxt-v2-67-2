package regress

import (
	"path/filepath"
	"testing"
)

// TestImpactCases runs the dependency-locking / impact-analysis
// regression cases under testdata/impact, the same set the
// `compatcheck impact` CLI runs.
func TestImpactCases(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "impact")
	results, err := RunImpactDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range results {
		if !res.Passed {
			t.Errorf("impact case %s failed:", res.Case.Name)
			for _, p := range res.Problems {
				t.Errorf("  - %s", p)
			}
		}
	}
}
