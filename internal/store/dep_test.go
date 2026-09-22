package store

import (
	"errors"
	"strings"
	"testing"
)

// depCount returns how many blocking edges the task (by slug) currently has,
// read straight from the dep table so a test sees edges regardless of whether
// the blocker is still open (ActiveBlockers hides closed ones).
func depCount(t *testing.T, s *Store, slug string) int {
	t.Helper()
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM dep JOIN task ON task.id = dep.task_id WHERE task.slug = ?`, slug).Scan(&n)
	if err != nil {
		t.Fatalf("depCount %q: %v", slug, err)
	}
	return n
}

func TestSelfBlockRejected(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "A"})
	if err := s.AddDeps("a", []string{"a"}); err == nil {
		t.Fatal("a task blocking itself should error")
	}
	if n := depCount(t, s, "a"); n != 0 {
		t.Fatalf("self-block left %d edges, want 0", n)
	}
}

func TestDirectCycleRejected(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "A"})
	mustCreate(t, s, CreateTaskInput{Title: "B", BlockedBy: []string{"a"}}) // b blocked_by a

	// a blocked_by b would close the 2-cycle a<->b.
	err := s.AddDeps("a", []string{"b"})
	if err == nil {
		t.Fatal("closing a 2-cycle should error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error %q should mention a cycle", err)
	}
	if n := depCount(t, s, "a"); n != 0 {
		t.Fatalf("rejected cyclic edge left %d edges on a, want 0", n)
	}
}

func TestTransitiveCycleRejected(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "A"})
	mustCreate(t, s, CreateTaskInput{Title: "B", BlockedBy: []string{"a"}}) // b<-a
	mustCreate(t, s, CreateTaskInput{Title: "C", BlockedBy: []string{"b"}}) // c<-b, so c<-b<-a

	// a blocked_by c would close the 3-cycle a<-c<-b<-a.
	if err := s.AddDeps("a", []string{"c"}); err == nil {
		t.Fatal("closing a 3-cycle should error")
	}
	if n := depCount(t, s, "a"); n != 0 {
		t.Fatalf("rejected transitive-cycle edge left %d edges on a, want 0", n)
	}
}

func TestNonCyclicDiamondAllowed(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Base"})
	mustCreate(t, s, CreateTaskInput{Title: "Left", BlockedBy: []string{"base"}})
	mustCreate(t, s, CreateTaskInput{Title: "Right", BlockedBy: []string{"base"}})
	// top depends on both left and right — a shared ancestor (base) is not a cycle.
	mustCreate(t, s, CreateTaskInput{Title: "Top", BlockedBy: []string{"left"}})
	if err := s.AddDeps("top", []string{"right"}); err != nil {
		t.Fatalf("diamond dependency should be allowed: %v", err)
	}
	if n := depCount(t, s, "top"); n != 2 {
		t.Fatalf("top should have 2 blockers, got %d", n)
	}
}

func TestAddDepsRollsBackOnBadSlug(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "A"})
	mustCreate(t, s, CreateTaskInput{Title: "Good"})

	// A good blocker followed by a nonexistent one must leave NO edge behind.
	err := s.AddDeps("a", []string{"good", "does-not-exist"})
	if err == nil {
		t.Fatal("a batch with a nonexistent blocker should error")
	}
	if n := depCount(t, s, "a"); n != 0 {
		t.Fatalf("failed batch committed %d edges, want 0 (not atomic)", n)
	}
}

func TestRemoveDepsRollsBackOnBadSlug(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "A"})
	mustCreate(t, s, CreateTaskInput{Title: "X"})
	mustCreate(t, s, CreateTaskInput{Title: "Y"})
	if err := s.AddDeps("a", []string{"x", "y"}); err != nil {
		t.Fatalf("setup add: %v", err)
	}

	// Removing an existing edge then hitting a bad slug must roll the removal back.
	if err := s.RemoveDeps("a", []string{"x", "does-not-exist"}); err == nil {
		t.Fatal("a remove batch with a nonexistent slug should error")
	}
	if n := depCount(t, s, "a"); n != 2 {
		t.Fatalf("failed remove batch dropped an edge (have %d, want 2) — not atomic", n)
	}
}

func TestCreateTaskWithBadBlockerLeavesNoOrphan(t *testing.T) {
	s := newStore(t)
	_, err := s.CreateTask(CreateTaskInput{Title: "Orphan", BlockedBy: []string{"nope"}})
	if err == nil {
		t.Fatal("creating a task with a nonexistent blocker should error")
	}
	if _, err := s.GetTask("orphan"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed create left an orphan task behind (GetTask err = %v)", err)
	}
}
