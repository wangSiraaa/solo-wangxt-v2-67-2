package regress_test

import (
	"path/filepath"
	"testing"

	"protocompat/internal/regress"
)

// TestImpactScenarios runs every cross-package scenario under
// testdata/impact through the in-memory registry service — the same
// cases the compatcheck CLI runs.
func TestImpactScenarios(t *testing.T) {
	results, err := regress.RunImpactDir(filepath.Join("..", "..", "testdata", "impact"))
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range results {
		t.Run(res.Scenario.Name, func(t *testing.T) {
			for _, p := range res.Problems {
				t.Errorf("%s", p)
			}
		})
	}
}
