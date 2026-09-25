package deps

import (
	"testing"
)

func TestAddCycle(t *testing.T) {
	// Existing: billing -> common, top -> billing, top -> common.
	g := Graph{
		"acme.billing": {"acme.common"},
		"acme.top":     {"acme.billing", "acme.common"},
	}
	if cycle := g.AddCycle("acme.new", []Lock{{Package: "acme.common", Version: "v1"}}); cycle != nil {
		t.Fatalf("acyclic edge reported as cycle: %v", cycle)
	}
	// common -> billing closes common -> billing -> common.
	cycle := g.AddCycle("acme.common", []Lock{{Package: "acme.billing", Version: "v1"}})
	if cycle == nil {
		t.Fatal("expected cycle, got none")
	}
	if got := FormatCycle(cycle); got != "acme.common -> acme.billing -> acme.common" {
		t.Fatalf("cycle = %q", got)
	}

	// Self dependency.
	self := Graph{}
	if c := self.AddCycle("acme.a", []Lock{{Package: "acme.a", Version: "v1"}}); len(c) != 2 || c[0] != "acme.a" || c[1] != "acme.a" {
		t.Fatalf("self cycle = %v", c)
	}

	// Longer existing path: a -> b -> c, adding c -> a closes it.
	g2 := Graph{"acme.a": {"acme.b"}, "acme.b": {"acme.c"}}
	c2 := g2.AddCycle("acme.c", []Lock{{Package: "acme.a", Version: "v1"}})
	if c2 == nil {
		t.Fatal("expected long cycle")
	}
	if got := FormatCycle(c2); got != "acme.c -> acme.a -> acme.b -> acme.c" {
		t.Fatalf("long cycle = %q", got)
	}
}

func TestCycleError(t *testing.T) {
	e := &CycleError{Cycle: []string{"a", "b", "a"}}
	if e.Error() != "dependency cycle rejected: a -> b -> a" {
		t.Fatalf("error = %q", e.Error())
	}
}
