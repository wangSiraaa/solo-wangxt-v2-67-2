// Command compatcheck is the CLI front-end for the compatibility checker.
//
//	compatcheck run <cases-dir>                      run regression cases
//	compatcheck check -old <dir> -new <dir>          ad-hoc comparison, JSON report
//
// Regression cases live in directories with old/ and new/ proto trees and
// a case.json expectation file; see testdata/cases.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"protocompat/internal/compat"
	"protocompat/internal/regress"
	"protocompat/internal/schema"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		os.Exit(runCases(os.Args[2:]))
	case "check":
		os.Exit(checkTrees(os.Args[2:]))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  compatcheck run <cases-dir>                 run all regression cases under a directory
  compatcheck check -old DIR -new DIR [-samples FILE.json] [-format json|text]
                                              compare two proto trees directly
`)
}

func runCases(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	verbose := fs.Bool("v", false, "print full reports for failing cases")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
		return 2
	}
	results, err := regress.RunDir(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	passed, failed := 0, 0
	for _, res := range results {
		if res.Passed {
			passed++
			fmt.Printf("PASS %s (verdict=%s)\n", res.Case.Name, res.Report.Verdict)
			continue
		}
		failed++
		fmt.Printf("FAIL %s\n", res.Case.Name)
		for _, p := range res.Problems {
			fmt.Printf("     - %s\n", p)
		}
		if *verbose {
			out, _ := json.MarshalIndent(res.Report, "     ", "  ")
			fmt.Printf("     report: %s\n", out)
		}
	}
	fmt.Printf("%d passed, %d failed\n", passed, failed)
	if failed > 0 {
		return 1
	}
	return 0
}

func checkTrees(args []string) int {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	oldDir := fs.String("old", "", "directory with the old .proto tree")
	newDir := fs.String("new", "", "directory with the new .proto tree")
	samplesFile := fs.String("samples", "", "optional JSON file with sample payloads")
	format := fs.String("format", "json", "output format: json or text")
	_ = fs.Parse(args)
	if *oldDir == "" || *newDir == "" {
		usage()
		return 2
	}

	oldFiles, err := loadTreeForCLI(*oldDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "old tree: %v\n", err)
		return 2
	}
	newFiles, err := loadTreeForCLI(*newDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "new tree: %v\n", err)
		return 2
	}
	var samples []compat.Sample
	if *samplesFile != "" {
		raw, err := os.ReadFile(*samplesFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "samples: %v\n", err)
			return 2
		}
		if err := json.Unmarshal(raw, &samples); err != nil {
			fmt.Fprintf(os.Stderr, "samples: %v\n", err)
			return 2
		}
	}

	ctx := context.Background()
	oldC, err := schema.Compile(ctx, oldFiles)
	if err != nil {
		fmt.Fprintf(os.Stderr, "old schema: %v\n", err)
		return 2
	}
	newC, err := schema.Compile(ctx, newFiles)
	if err != nil {
		fmt.Fprintf(os.Stderr, "new schema: %v\n", err)
		return 2
	}
	owned := append(append([]string{}, oldC.OwnedPaths...), newC.OwnedPaths...)
	report := compat.Check(compat.Input{
		Old: oldC.Files, New: newC.Files, OwnedPaths: owned, Samples: samples,
	})

	switch *format {
	case "json":
		out, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(out))
	case "text":
		fmt.Printf("verdict: %s\n", report.Verdict)
		for _, f := range report.Findings {
			loc := f.Message
			if f.Path != "" {
				if loc != "" {
					loc += "."
				}
				loc += f.Path
			}
			fmt.Printf("  [%s/%s] %-32s %s\n", f.Severity, f.Dimension, f.Code, loc)
			fmt.Printf("      %s\n", f.Detail)
		}
		for _, s := range report.Samples {
			fmt.Printf("  sample %s (%s, %s): %s\n", s.Name, s.Message, s.Encoding, s.Status)
			for _, d := range s.Diffs {
				fmt.Printf("      %s: %s -> %s\n", d.Path, d.Old, d.New)
			}
		}
	}
	if report.Verdict == compat.VerdictIncompatible {
		return 1
	}
	return 0
}

func loadTreeForCLI(root string) ([]schema.SourceFile, error) {
	return regress.LoadTree(root)
}
