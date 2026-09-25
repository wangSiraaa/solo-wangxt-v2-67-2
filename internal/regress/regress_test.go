package regress

import (
	"path/filepath"
	"testing"
)

// TestCases runs every regression case under testdata/cases — the same
// cases the compatcheck CLI runs.
func TestCases(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "cases")
	results, err := RunDir(root)
	if err != nil {
		t.Fatalf("run cases: %v", err)
	}
	for _, res := range results {
		t.Run(res.Case.Name, func(t *testing.T) {
			for _, p := range res.Problems {
				t.Error(p)
			}
			if !res.Passed {
				t.Logf("report: %+v", res.Report)
			}
		})
	}
}
