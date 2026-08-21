package store

import (
	"fmt"
	"regexp"
	"strings"
)

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
