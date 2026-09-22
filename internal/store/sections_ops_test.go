package store

import (
	"errors"
	"strings"
	"testing"
)

// bodyOf returns a task's current body after a section op.
func bodyOf(t *testing.T, s *Store, slug string) string {
	t.Helper()
	task, err := s.GetTask(slug)
	if err != nil {
		t.Fatalf("get task %q: %v", slug, err)
	}
	return task.Body
}

func TestAppendSectionLandsAfterSubsections(t *testing.T) {
	s := newStore(t)
	body := "## Design\ntext1\n### Storage\ntext2\n## Other\ntext3\n"
	mustCreate(t, s, CreateTaskInput{Title: "Doc", Body: body})

	if _, err := s.AppendSection("doc", "Design", "appended"); err != nil {
		t.Fatalf("append: %v", err)
	}
	got := bodyOf(t, s, "doc")
	want := "## Design\ntext1\n### Storage\ntext2\n\nappended\n\n## Other\ntext3\n"
	if got != want {
		t.Fatalf("append body\n got %q\nwant %q", got, want)
	}
	// Other section intact, exactly one trailing newline, no doubled blanks.
	assertSeams(t, got)
	if !strings.Contains(got, "## Other\ntext3") {
		t.Fatalf("Other section clobbered: %q", got)
	}

	// Unresolved path is ErrNotFound.
	if _, err := s.AppendSection("doc", "Nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("append to missing section: got %v, want ErrNotFound", err)
	}
}

func TestAppendPreservesUnclosedFenceTrailingBlanks(t *testing.T) {
	s := newStore(t)
	// Malformed input: the fence is never closed, so its trailing blank lines
	// are code content, not a section seam, and must survive the append rather
	// than being trimmed away (the seam normalizer only trims real boundaries).
	mustCreate(t, s, CreateTaskInput{Title: "Doc", Body: "## A\n```\ncode\n\n\n"})
	if _, err := s.AppendSection("doc", "A", "new"); err != nil {
		t.Fatalf("append: %v", err)
	}
	got := bodyOf(t, s, "doc")
	if !strings.Contains(got, "code\n\n\n") {
		t.Fatalf("in-fence blank lines dropped: %q", got)
	}
	if !strings.Contains(got, "new") {
		t.Fatalf("appended content missing: %q", got)
	}
}

func TestDeleteSectionCases(t *testing.T) {
	// Middle section.
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Doc", Body: "## A\na\n## B\nb\n## C\nc\n"})
	if _, err := s.DeleteSection("doc", "B"); err != nil {
		t.Fatalf("delete middle: %v", err)
	}
	got := bodyOf(t, s, "doc")
	if got != "## A\na\n\n## C\nc\n" {
		t.Fatalf("delete middle = %q", got)
	}
	assertSeams(t, got)

	// First section.
	s2 := newStore(t)
	mustCreate(t, s2, CreateTaskInput{Title: "Doc", Body: "## A\na\n## B\nb\n## C\nc\n"})
	if _, err := s2.DeleteSection("doc", "A"); err != nil {
		t.Fatalf("delete first: %v", err)
	}
	got = bodyOf(t, s2, "doc")
	// Nothing precedes A, so only the trailing seam matters; B and C were
	// contiguous and stay contiguous (no blank injected between preserved lines).
	if got != "## B\nb\n## C\nc\n" {
		t.Fatalf("delete first = %q", got)
	}
	assertSeams(t, got)

	// Last section.
	s3 := newStore(t)
	mustCreate(t, s3, CreateTaskInput{Title: "Doc", Body: "## A\na\n## B\nb\n## C\nc\n"})
	if _, err := s3.DeleteSection("doc", "C"); err != nil {
		t.Fatalf("delete last: %v", err)
	}
	got = bodyOf(t, s3, "doc")
	// Nothing follows C, so A and B stay contiguous as in the original.
	if got != "## A\na\n## B\nb\n" {
		t.Fatalf("delete last = %q", got)
	}
	assertSeams(t, got)

	// Only section -> empty body "".
	s4 := newStore(t)
	mustCreate(t, s4, CreateTaskInput{Title: "Doc", Body: "## A\na\nb\n"})
	if _, err := s4.DeleteSection("doc", "A"); err != nil {
		t.Fatalf("delete only: %v", err)
	}
	if got = bodyOf(t, s4, "doc"); got != "" {
		t.Fatalf("delete only should empty the body, got %q", got)
	}

	// Unresolved path -> ErrNotFound.
	if _, err := s4.DeleteSection("doc", "Missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: got %v, want ErrNotFound", err)
	}
}

func TestInsertSectionPositions(t *testing.T) {
	base := "## A\na\n## B\nb\n"

	// before: sibling at level L immediately before target.
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Doc", Body: base})
	if _, err := s.InsertSection("doc", "B", "New", "nb", "before"); err != nil {
		t.Fatalf("insert before: %v", err)
	}
	got := bodyOf(t, s, "doc")
	if got != "## A\na\n\n## New\n\nnb\n\n## B\nb\n" {
		t.Fatalf("insert before = %q", got)
	}
	assertSeams(t, got)
	assertLevel(t, s, "doc", "New", 2)

	// after: sibling at level L after the target's entire span.
	s = newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Doc", Body: "## A\na\n### Sub\nsub\n## B\nb\n"})
	if _, err := s.InsertSection("doc", "A", "New", "", "after"); err != nil {
		t.Fatalf("insert after: %v", err)
	}
	got = bodyOf(t, s, "doc")
	// After A's whole span (incl. ### Sub), before B; heading-only.
	if got != "## A\na\n### Sub\nsub\n\n## New\n\n## B\nb\n" {
		t.Fatalf("insert after = %q", got)
	}
	assertSeams(t, got)
	assertLevel(t, s, "doc", "New", 2)

	// firstChild with no existing direct child -> becomes sole child at end.
	s = newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Doc", Body: "## A\na\n## B\nb\n"})
	if _, err := s.InsertSection("doc", "A", "Kid", "kb", "firstChild"); err != nil {
		t.Fatalf("insert firstChild none: %v", err)
	}
	got = bodyOf(t, s, "doc")
	if got != "## A\na\n\n### Kid\n\nkb\n\n## B\nb\n" {
		t.Fatalf("insert firstChild (none) = %q", got)
	}
	assertLevel(t, s, "doc", "A/Kid", 3)

	// firstChild with an existing direct child -> before that child.
	s = newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Doc", Body: "## A\na\n### Old\nold\n## B\nb\n"})
	if _, err := s.InsertSection("doc", "A", "Kid", "", "firstChild"); err != nil {
		t.Fatalf("insert firstChild existing: %v", err)
	}
	got = bodyOf(t, s, "doc")
	// The seam is only around the inserted Kid; Old and B were contiguous and stay so.
	if got != "## A\na\n\n### Kid\n\n### Old\nold\n## B\nb\n" {
		t.Fatalf("insert firstChild (existing) = %q", got)
	}
	assertLevel(t, s, "doc", "A/Kid", 3)

	// lastChild -> at target's end, depth L+1.
	s = newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Doc", Body: "## A\na\n### Old\nold\n## B\nb\n"})
	if _, err := s.InsertSection("doc", "A", "Kid", "", "lastChild"); err != nil {
		t.Fatalf("insert lastChild: %v", err)
	}
	got = bodyOf(t, s, "doc")
	if got != "## A\na\n### Old\nold\n\n### Kid\n\n## B\nb\n" {
		t.Fatalf("insert lastChild = %q", got)
	}
	assertLevel(t, s, "doc", "A/Kid", 3)

	// Document-level, before/firstChild -> start; shallowest existing level (2).
	s = newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Doc", Body: base})
	if _, err := s.InsertSection("doc", "", "Top", "tb", "before"); err != nil {
		t.Fatalf("insert doc start: %v", err)
	}
	got = bodyOf(t, s, "doc")
	if got != "## Top\n\ntb\n\n## A\na\n## B\nb\n" {
		t.Fatalf("insert doc start = %q", got)
	}
	assertSeams(t, got)
	assertLevel(t, s, "doc", "Top", 2)

	// Document-level, after/lastChild -> end.
	s = newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Doc", Body: base})
	if _, err := s.InsertSection("doc", "", "End", "", "lastChild"); err != nil {
		t.Fatalf("insert doc end: %v", err)
	}
	got = bodyOf(t, s, "doc")
	if got != "## A\na\n## B\nb\n\n## End\n" {
		t.Fatalf("insert doc end = %q", got)
	}
	assertSeams(t, got)

	// Document-level into an empty body -> depth 2.
	s = newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Empty", Body: ""})
	if _, err := s.InsertSection("empty", "", "Fresh", "fb", "after"); err != nil {
		t.Fatalf("insert into empty: %v", err)
	}
	got = bodyOf(t, s, "empty")
	if got != "## Fresh\n\nfb\n" {
		t.Fatalf("insert into empty = %q", got)
	}
	assertLevel(t, s, "empty", "Fresh", 2)

	// Whitespace-only body is dropped -> heading only.
	s = newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Doc", Body: base})
	if _, err := s.InsertSection("doc", "B", "Bare", "   \n  ", "after"); err != nil {
		t.Fatalf("insert heading-only: %v", err)
	}
	got = bodyOf(t, s, "doc")
	if got != "## A\na\n## B\nb\n\n## Bare\n" {
		t.Fatalf("insert heading-only = %q", got)
	}

	// Unresolved non-empty path -> ErrNotFound.
	if _, err := s.InsertSection("doc", "Nope", "X", "", "after"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("insert missing path: got %v, want ErrNotFound", err)
	}
}

func TestMoveSection(t *testing.T) {
	// Move before a target.
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Doc", Body: "## A\na\n## B\nb\n## C\nc\n"})
	if _, err := s.MoveSection("doc", "C", "A", ""); err != nil {
		t.Fatalf("move before: %v", err)
	}
	got := bodyOf(t, s, "doc")
	// C is seamed against A at the move point; A and B were contiguous and stay so.
	if got != "## C\nc\n\n## A\na\n## B\nb\n" {
		t.Fatalf("move before = %q", got)
	}
	assertSeams(t, got)

	// Move after a target, preserving the moved section's subsections and not
	// rewriting depths.
	s = newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Doc", Body: "## A\na\n### Sub\nsub\n## B\nb\n## C\nc\n"})
	if _, err := s.MoveSection("doc", "A", "", "C"); err != nil {
		t.Fatalf("move after: %v", err)
	}
	got = bodyOf(t, s, "doc")
	// B and C were contiguous and stay so; A (with its ### Sub) is seamed in after C.
	if got != "## B\nb\n## C\nc\n\n## A\na\n### Sub\nsub\n" {
		t.Fatalf("move after = %q", got)
	}
	assertSeams(t, got)
	// Sub kept its level (### under A) — no depth rewriting.
	assertLevel(t, s, "doc", "A/Sub", 3)

	// Self guard: moving relative to itself.
	if _, err := s.MoveSection("doc", "A", "A", ""); !errors.Is(err, ErrInvalidSectionOp) {
		t.Fatalf("self move: got %v, want ErrInvalidSectionOp", err)
	}

	// Into-own-descendant guard.
	s = newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Doc", Body: "## A\na\n### Sub\nsub\n## B\nb\n"})
	if _, err := s.MoveSection("doc", "A", "A/Sub", ""); !errors.Is(err, ErrInvalidSectionOp) {
		t.Fatalf("descendant move: got %v, want ErrInvalidSectionOp", err)
	}

	// Both set / neither set -> ErrInvalidSectionOp.
	if _, err := s.MoveSection("doc", "A", "B", "B"); !errors.Is(err, ErrInvalidSectionOp) {
		t.Fatalf("both anchors: got %v, want ErrInvalidSectionOp", err)
	}
	if _, err := s.MoveSection("doc", "A", "", ""); !errors.Is(err, ErrInvalidSectionOp) {
		t.Fatalf("no anchor: got %v, want ErrInvalidSectionOp", err)
	}

	// Unresolved source / target -> ErrNotFound.
	if _, err := s.MoveSection("doc", "Nope", "A", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing source: got %v, want ErrNotFound", err)
	}
	if _, err := s.MoveSection("doc", "A", "Nope", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing target: got %v, want ErrNotFound", err)
	}
}

func TestReplaceInBody(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Doc", Body: "## A\nalpha here\n## B\nbeta\n"})

	// Unique match replaced.
	if _, err := s.ReplaceInBody("doc", "alpha here", "gamma there"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got := bodyOf(t, s, "doc"); got != "## A\ngamma there\n## B\nbeta\n" {
		t.Fatalf("replace = %q", got)
	}

	// Zero matches -> ErrNotFound.
	if _, err := s.ReplaceInBody("doc", "nowhere", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("zero match: got %v, want ErrNotFound", err)
	}

	// Empty old -> ErrNotFound.
	if _, err := s.ReplaceInBody("doc", "", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty old: got %v, want ErrNotFound", err)
	}

	// Multiple matches -> ErrAmbiguousMatch.
	s2 := newStore(t)
	mustCreate(t, s2, CreateTaskInput{Title: "Doc", Body: "## A\ndup\n## B\ndup\n"})
	if _, err := s2.ReplaceInBody("doc", "dup", "x"); !errors.Is(err, ErrAmbiguousMatch) {
		t.Fatalf("multi match: got %v, want ErrAmbiguousMatch", err)
	}

	// Overlapping occurrences are ambiguous too: "aa" occurs twice in "aaa", so
	// it must error rather than silently replace the first.
	sOverlap := newStore(t)
	mustCreate(t, sOverlap, CreateTaskInput{Title: "Doc", Body: "## A\naaa\n"})
	if _, err := sOverlap.ReplaceInBody("doc", "aa", "b"); !errors.Is(err, ErrAmbiguousMatch) {
		t.Fatalf("overlapping match: got %v, want ErrAmbiguousMatch", err)
	}

	// new:"" removes the single occurrence (verbatim, no normalization).
	s3 := newStore(t)
	mustCreate(t, s3, CreateTaskInput{Title: "Doc", Body: "## A\nkeep REMOVE me\n"})
	if _, err := s3.ReplaceInBody("doc", "REMOVE ", ""); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := bodyOf(t, s3, "doc"); got != "## A\nkeep me\n" {
		t.Fatalf("remove = %q", got)
	}
}

// assertSeams checks the one-blank-line + single-trailing-newline invariants.
func assertSeams(t *testing.T, body string) {
	t.Helper()
	if body == "" {
		return
	}
	if strings.Contains(body, "\n\n\n") {
		t.Fatalf("doubled blank line in body: %q", body)
	}
	if !strings.HasSuffix(body, "\n") || strings.HasSuffix(body, "\n\n") {
		t.Fatalf("body must end in exactly one trailing newline: %q", body)
	}
}

// assertLevel checks a section resolves at the expected heading level.
func assertLevel(t *testing.T, s *Store, slug, path string, level int) {
	t.Helper()
	toc, err := s.TOC(slug)
	if err != nil {
		t.Fatalf("toc: %v", err)
	}
	for _, sec := range toc {
		if sec.Path == path {
			if sec.Level != level {
				t.Fatalf("section %q level = %d, want %d", path, sec.Level, level)
			}
			return
		}
	}
	t.Fatalf("section %q not found in toc %v", path, toc)
}
