package store

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrAmbiguousMatch is returned by a body-replace when the target text occurs
// more than once, so no single occurrence can be chosen unambiguously.
var ErrAmbiguousMatch = errors.New("ambiguous match")

// ErrInvalidSectionOp is returned when a section operation is malformed — a
// move with the wrong number of anchors, a move relative to itself or into its
// own descendant, or an unknown insert position.
var ErrInvalidSectionOp = errors.New("invalid section operation")

// Section identifies one markdown heading within a task body. Path is the
// slash-joined chain of ancestor headings ("Design/Storage"), so nested
// sections are addressable without ambiguity.
type Section struct {
	Path  string `json:"path"`
	Level int    `json:"level"`
	Title string `json:"title"`
}

var reHeading = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*$`)

type heading struct {
	level     int
	title     string
	line      int // index of the heading line
	pathParts []string
}

// parseHeadings returns every ATX heading in body, ignoring those inside fenced
// code blocks, each carrying its ancestor path.
func parseHeadings(body string) ([]heading, []string) {
	lines := strings.Split(body, "\n")
	var hs []heading
	var stack []heading // ancestor chain by increasing level
	inFence := false
	for i, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		m := reHeading.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		level := len(m[1])
		title := m[2]
		for len(stack) > 0 && stack[len(stack)-1].level >= level {
			stack = stack[:len(stack)-1]
		}
		parts := make([]string, 0, len(stack)+1)
		for _, a := range stack {
			parts = append(parts, a.title)
		}
		parts = append(parts, title)
		h := heading{level: level, title: title, line: i, pathParts: parts}
		hs = append(hs, h)
		stack = append(stack, h)
	}
	return hs, lines
}

// TOCFromBody lists the sections in a body.
func TOCFromBody(body string) []Section {
	hs, _ := parseHeadings(body)
	out := make([]Section, len(hs))
	for i, h := range hs {
		out[i] = Section{Path: strings.Join(h.pathParts, "/"), Level: h.level, Title: h.title}
	}
	return out
}

// findSection returns the heading with the given path, plus the line range
// [contentStart, end) of its content — everything under it up to the next
// heading of the same or higher level (so a section owns its subsections).
func findSection(hs []heading, lines []string, path string) (idx, contentStart, end int, ok bool) {
	for i, h := range hs {
		if strings.Join(h.pathParts, "/") == path || h.title == path {
			contentStart = h.line + 1
			end = len(lines)
			for j := i + 1; j < len(hs); j++ {
				if hs[j].level <= h.level {
					end = hs[j].line
					break
				}
			}
			return i, contentStart, end, true
		}
	}
	return 0, 0, 0, false
}

// GetSectionFromBody returns the content under a heading path (its subsections
// included), excluding the heading line itself.
func GetSectionFromBody(body, path string) (string, bool) {
	hs, lines := parseHeadings(body)
	_, start, end, ok := findSection(hs, lines, path)
	if !ok {
		return "", false
	}
	return strings.TrimRight(strings.Join(lines[start:end], "\n"), "\n"), true
}

// SetSectionInBody replaces the content under a heading path, preserving the
// heading line, and returns the new body.
func SetSectionInBody(body, path, content string) (string, bool) {
	hs, lines := parseHeadings(body)
	_, start, end, ok := findSection(hs, lines, path)
	if !ok {
		return body, false
	}
	newLines := append([]string{}, lines[:start]...)
	newLines = append(newLines, strings.Split(content, "\n")...)
	newLines = append(newLines, "") // blank line before the next section
	newLines = append(newLines, lines[end:]...)
	return strings.Join(newLines, "\n"), true
}

// --- seam-normalized block editing -----------------------------------------
//
// The ops below share one seam model, expressed over the []string line slices
// parseHeadings works with: block boundaries carry EXACTLY one blank line, the
// body ends in exactly one trailing newline, and a body emptied entirely
// collapses to "" (no newline). isBlank/trimBlankEnds/joinSegments are the pure
// primitives that guarantee those invariants; every op reduces to slicing the
// lines into segments and joining them.

// isBlank reports whether a line is empty or whitespace-only.
func isBlank(s string) bool { return strings.TrimSpace(s) == "" }

// endsInOpenFence reports whether lines finish inside an unterminated fenced
// code block. A fence never crosses a heading boundary (parseHeadings ignores
// fenced `#` lines), so this can only happen with malformed markdown — but when
// it does, the segment's trailing blank lines are fence content, not seam
// whitespace, and must not be trimmed away.
func endsInOpenFence(lines []string) bool {
	inFence := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
		}
	}
	return inFence
}

// trimBlankEnds drops leading and trailing blank lines from a segment. Trailing
// blanks are left intact when the segment ends inside an unterminated fence,
// since there they are code content rather than a block seam.
func trimBlankEnds(lines []string) []string {
	start := 0
	for start < len(lines) && isBlank(lines[start]) {
		start++
	}
	end := len(lines)
	if !endsInOpenFence(lines[start:]) {
		for end > start && isBlank(lines[end-1]) {
			end--
		}
	}
	return lines[start:end]
}

// joinSegments trims each segment's blank ends, drops any that become empty,
// joins the rest with exactly one blank line between them, and returns a body
// ending in exactly one trailing newline — or "" when every segment is empty.
func joinSegments(segments ...[]string) string {
	var out []string
	for _, seg := range segments {
		t := trimBlankEnds(seg)
		if len(t) == 0 {
			continue
		}
		if len(out) > 0 {
			out = append(out, "") // one blank line between blocks
		}
		out = append(out, t...)
	}
	if len(out) == 0 {
		return ""
	}
	out = append(out, "") // single trailing newline
	return strings.Join(out, "\n")
}

// AppendToSectionInBody inserts content as a block at the section's end line —
// after the section's own content and all its subsections, before the next
// section — with a single blank-line seam on each side.
func AppendToSectionInBody(body, path, content string) (string, error) {
	hs, lines := parseHeadings(body)
	_, _, end, ok := findSection(hs, lines, path)
	if !ok {
		return "", fmt.Errorf("section %q: %w", path, ErrNotFound)
	}
	return joinSegments(lines[:end], strings.Split(content, "\n"), lines[end:]), nil
}

// InsertSectionInBody inserts a new heading (and optional body) relative to an
// existing heading path, or — when path is empty — at document scope. position
// is one of before, after, firstChild, lastChild. The new heading's depth is
// the target's level for before/after, the target's level+1 for first/lastChild,
// and the document's shallowest level (or 2) at document scope. A whitespace-only
// sectionBody yields a heading-only block.
func InsertSectionInBody(body, path, heading, sectionBody, position string) (string, error) {
	hs, lines := parseHeadings(body)
	var depth, insertAt int
	if strings.TrimSpace(path) == "" {
		depth = 2
		if len(hs) > 0 {
			depth = hs[0].level
			for _, h := range hs {
				if h.level < depth {
					depth = h.level
				}
			}
		}
		switch position {
		case "before", "firstChild":
			insertAt = 0
		case "after", "lastChild":
			insertAt = len(lines)
		default:
			return "", fmt.Errorf("insert position %q: %w", position, ErrInvalidSectionOp)
		}
	} else {
		idx, _, end, ok := findSection(hs, lines, path)
		if !ok {
			return "", fmt.Errorf("section %q: %w", path, ErrNotFound)
		}
		h := hs[idx]
		switch position {
		case "before":
			depth, insertAt = h.level, h.line
		case "after":
			depth, insertAt = h.level, end
		case "firstChild":
			depth, insertAt = h.level+1, end
			for j := idx + 1; j < len(hs) && hs[j].line < end; j++ {
				if hs[j].level == h.level+1 {
					insertAt = hs[j].line
					break
				}
			}
		case "lastChild":
			depth, insertAt = h.level+1, end
		default:
			return "", fmt.Errorf("insert position %q: %w", position, ErrInvalidSectionOp)
		}
	}
	headline := strings.Repeat("#", depth) + " " + strings.TrimSpace(heading)
	block := []string{headline}
	if bt := strings.TrimSpace(sectionBody); bt != "" {
		block = append(block, "")
		block = append(block, strings.Split(bt, "\n")...)
	}
	return joinSegments(lines[:insertAt], block, lines[insertAt:]), nil
}

// DeleteSectionInBody removes a section's entire span — its heading, body, and
// all subsections — and re-seams the surrounding blocks.
func DeleteSectionInBody(body, path string) (string, error) {
	hs, lines := parseHeadings(body)
	idx, _, end, ok := findSection(hs, lines, path)
	if !ok {
		return "", fmt.Errorf("section %q: %w", path, ErrNotFound)
	}
	return joinSegments(lines[:hs[idx].line], lines[end:]), nil
}

// MoveSectionInBody moves a section (with its subsections) before or after
// another heading path. Exactly one of before/after must be set. The moved
// block keeps its original heading levels verbatim — no depth rewriting.
func MoveSectionInBody(body, path, before, after string) (string, error) {
	beforeSet := strings.TrimSpace(before) != ""
	afterSet := strings.TrimSpace(after) != ""
	if beforeSet == afterSet {
		return "", fmt.Errorf("move needs exactly one of before/after: %w", ErrInvalidSectionOp)
	}
	hs, lines := parseHeadings(body)
	sIdx, _, sEnd, ok := findSection(hs, lines, path)
	if !ok {
		return "", fmt.Errorf("section %q: %w", path, ErrNotFound)
	}
	src := hs[sIdx]
	targetPath := before
	if afterSet {
		targetPath = after
	}
	tIdx, _, tEnd, ok := findSection(hs, lines, targetPath)
	if !ok {
		return "", fmt.Errorf("section %q: %w", targetPath, ErrNotFound)
	}
	tgt := hs[tIdx]
	if tgt.line == src.line {
		return "", fmt.Errorf("cannot move a section relative to itself: %w", ErrInvalidSectionOp)
	}
	if tgt.line > src.line && tgt.line < sEnd {
		return "", fmt.Errorf("cannot move a section into its own descendant: %w", ErrInvalidSectionOp)
	}
	block := lines[src.line:sEnd]
	insertAt := tgt.line
	if afterSet {
		insertAt = tEnd
	}
	// The guards guarantee insertAt is never strictly inside the source span.
	var segments [][]string
	if insertAt <= src.line {
		segments = [][]string{lines[:insertAt], block, lines[insertAt:src.line], lines[sEnd:]}
	} else {
		segments = [][]string{lines[:src.line], lines[sEnd:insertAt], block, lines[insertAt:]}
	}
	return joinSegments(segments...), nil
}

// countMatches counts the occurrences of sub in s, including overlapping ones
// (advancing by one past each match's start), so the uniqueness check below is
// honest even for self-overlapping search text like "aa" in "aaa" — which a
// non-overlapping count would report as one, wrongly claiming it unambiguous.
func countMatches(s, sub string) int {
	if sub == "" {
		return 0
	}
	n, from := 0, 0
	for {
		i := strings.Index(s[from:], sub)
		if i < 0 {
			return n
		}
		n++
		from += i + 1
	}
}

// ReplaceInBodyText replaces one exact, unique substring anywhere in the body
// with new (which may be "" to remove it). It is a verbatim splice — no seam
// normalization, so it may touch heading lines or any other text. An empty or
// absent old is ErrNotFound; more than one occurrence is ErrAmbiguousMatch.
func ReplaceInBodyText(body, old, new string) (string, error) {
	if old == "" {
		return "", fmt.Errorf("empty match text: %w", ErrNotFound)
	}
	switch n := countMatches(body, old); {
	case n == 0:
		return "", fmt.Errorf("text %q not found: %w", old, ErrNotFound)
	case n > 1:
		return "", fmt.Errorf("text %q appears %d times: %w", old, n, ErrAmbiguousMatch)
	}
	return strings.Replace(body, old, new, 1), nil
}

// AppendSection appends a block to the end of a task-body section.
func (s *Store) AppendSection(slug, path, content string) (*Task, error) {
	t, err := s.taskBySlug(slug)
	if err != nil {
		return nil, err
	}
	body, err := AppendToSectionInBody(t.Body, path, content)
	if err != nil {
		return nil, err
	}
	return s.UpdateTask(UpdateTaskInput{Slug: slug, Body: &body})
}

// InsertSection inserts a new section into a task body relative to a heading
// path (or at document scope when path is empty).
func (s *Store) InsertSection(slug, path, heading, sectionBody, position string) (*Task, error) {
	t, err := s.taskBySlug(slug)
	if err != nil {
		return nil, err
	}
	body, err := InsertSectionInBody(t.Body, path, heading, sectionBody, position)
	if err != nil {
		return nil, err
	}
	return s.UpdateTask(UpdateTaskInput{Slug: slug, Body: &body})
}

// DeleteSection removes a task-body section and its subsections.
func (s *Store) DeleteSection(slug, path string) (*Task, error) {
	t, err := s.taskBySlug(slug)
	if err != nil {
		return nil, err
	}
	body, err := DeleteSectionInBody(t.Body, path)
	if err != nil {
		return nil, err
	}
	return s.UpdateTask(UpdateTaskInput{Slug: slug, Body: &body})
}

// MoveSection moves a task-body section before or after another heading path.
func (s *Store) MoveSection(slug, path, before, after string) (*Task, error) {
	t, err := s.taskBySlug(slug)
	if err != nil {
		return nil, err
	}
	body, err := MoveSectionInBody(t.Body, path, before, after)
	if err != nil {
		return nil, err
	}
	return s.UpdateTask(UpdateTaskInput{Slug: slug, Body: &body})
}

// ReplaceInBody replaces one exact, unique substring in a task body.
func (s *Store) ReplaceInBody(slug, old, new string) (*Task, error) {
	t, err := s.taskBySlug(slug)
	if err != nil {
		return nil, err
	}
	body, err := ReplaceInBodyText(t.Body, old, new)
	if err != nil {
		return nil, err
	}
	return s.UpdateTask(UpdateTaskInput{Slug: slug, Body: &body})
}

// TOC lists a task's sections.
func (s *Store) TOC(slug string) ([]Section, error) {
	t, err := s.taskBySlug(slug)
	if err != nil {
		return nil, err
	}
	return TOCFromBody(t.Body), nil
}

// GetSection returns one section of a task's body.
func (s *Store) GetSection(slug, path string) (string, error) {
	t, err := s.taskBySlug(slug)
	if err != nil {
		return "", err
	}
	content, ok := GetSectionFromBody(t.Body, path)
	if !ok {
		return "", fmt.Errorf("section %q: %w", path, ErrNotFound)
	}
	return content, nil
}

// SetSection replaces one section of a task's body, leaving the rest untouched.
func (s *Store) SetSection(slug, path, content string) (*Task, error) {
	t, err := s.taskBySlug(slug)
	if err != nil {
		return nil, err
	}
	body, ok := SetSectionInBody(t.Body, path, content)
	if !ok {
		return nil, fmt.Errorf("section %q: %w", path, ErrNotFound)
	}
	return s.UpdateTask(UpdateTaskInput{Slug: slug, Body: &body})
}
