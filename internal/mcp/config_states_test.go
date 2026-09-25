package mcp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/MatLomax/worklog/internal/store"
)

// reloadEvent is what one read of the config file finds: a missing file, a
// read error, or content (valid or not). force names the configuration a
// valid content puts in force: "defaults" for a document without
// "statuses", else the content's own name.
type reloadEvent struct {
	name    string
	missing bool
	err     error
	data    string
	invalid string // the problem text, for content that can't be used
	force   string // the configuration valid content puts in force
}

// reloadModel is the reload rule as the README states it, over the server's
// state: the configuration in force (force: "" for none, "defaults", or the
// name of a loaded file config), whether a read has found the file present
// (seen), the file's current problem ("", "invalid", "unreadable" or
// "removed") with its text (msg), and the event the last read found (last;
// "" before the first read), since a read that finds the same does nothing.
type reloadModel struct {
	force, problem, msg, last string
	seen                      bool
}

// label names the model's state without the last read: in force, seen, and
// the problem, e.g. "file-seen-removed".
func (m reloadModel) label() string {
	force := m.force
	switch force {
	case "":
		force = "none"
	case "defaults":
	default:
		force = "file"
	}
	seen := "unseen"
	if m.seen {
		seen = "seen"
	}
	problem := m.problem
	if problem == "" {
		problem = "none"
	}
	return force + "-" + seen + "-" + problem
}

// step returns the model after a read finds e, and the lines it logs.
func (m reloadModel) step(e reloadEvent, cfgFile string) (reloadModel, []string) {
	if m.last == e.name {
		return m, nil
	}
	m.last = e.name
	switch {
	case e.err != nil:
		m.problem, m.msg = "unreadable", e.err.Error()
	case e.missing:
		switch {
		case !m.seen || m.force == "defaults":
			m.force, m.problem, m.msg = "defaults", "", ""
			return m, nil
		case m.force != "":
			m.problem, m.msg = "removed", cfgFile+" is missing; still using the last valid statuses (deleting the file takes effect when the server restarts)"
		default:
			m.problem, m.msg = "removed", cfgFile+": the file was removed; recreate it, or restart the server to use the default statuses"
		}
	case e.invalid != "":
		m.seen, m.problem, m.msg = true, "invalid", e.invalid
	default:
		m.seen, m.force, m.problem, m.msg = true, e.force, "", ""
		return m, nil
	}
	return m, []string{"worklog: config: " + m.msg}
}

// reloadEvents lists the reads TestConfigReloadStateTable applies, for the
// config file at cfgFile.
func reloadEvents(cfgFile string) []reloadEvent {
	explicitDefaults := `{"statuses": [{"name": "pending", "kind": "open", "default": true}, {"name": "in_progress", "kind": "active"}, {"name": "blocked", "kind": "blocked"}, {"name": "done", "kind": "closed"}, {"name": "dropped", "kind": "closed"}]}`
	noClosed := cfgFile + ":1:14: statuses: no status of kind closed; at least one is required"
	return []reloadEvent{
		{name: "missing", missing: true},
		{name: "denied", err: permissionDenied(cfgFile)},
		{name: "io error", err: &fs.PathError{Op: "read", Path: cfgFile, Err: errors.New("input/output error")}},
		{name: "invalid", data: noClosedConfig, invalid: noClosed},
		{name: "invalid again", data: noClosedConfig + "\n// another edit, the same mistake\n", invalid: noClosed},
		{name: "empty", data: "", invalid: cfgFile + ": config file has no content (it is empty or holds only whitespace, comments or a byte-order mark); write a JSON object: {\"statuses\": [...]}, or {} for the default statuses"},
		{name: "todo", data: todoFinConfig, force: "todo"},
		{name: "queued", data: queuedConfig, force: "queued"},
		{name: "explicit defaults", data: explicitDefaults, force: "explicit defaults"},
		{name: "{}", data: "{}", force: "defaults"},
	}
}

// copyDir copies the files in src (not its subdirectories) into dst.
func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestConfigReloadStateTable drives every reachable (state, event) cell of
// the reload rule through a real server, reading through the readFile hook
// and logging to a buffer, and checks each call against reloadModel: the
// status a task created without one is saved with (or the exact refusal), the
// exact note, the exact log lines (a steady state logs nothing), the server's
// state, and that the attached store holds the configuration in force.
// Every reachable state is found by a breadth-first walk of the model from a
// fresh server; each cell replays a shortest path to its state on a fresh
// server over its own copy of a new database, then applies its event.
func TestConfigReloadStateTable(t *testing.T) {
	template := filepath.Join(t.TempDir(), store.DirName)
	initDB(t, filepath.Join(template, store.FileName))
	events := reloadEvents(cfgFileOf(filepath.Join(template, store.FileName)))
	// configs maps each configuration the model can put in force to its
	// statuses and default status.
	configs := map[string]*store.Config{"defaults": store.DefaultConfig()}
	for _, e := range events {
		if e.force != "" && e.force != "defaults" {
			cfg, err := store.ParseConfig([]byte(e.data))
			if err != nil {
				t.Fatal(err)
			}
			configs[e.force] = cfg
		}
	}

	// run replays the events at path on a fresh server over a copy of the
	// template database, checking every call, and makes a second call after
	// the last event to check that the state is steady.
	run := func(t *testing.T, path []int) {
		t.Helper()
		dir := filepath.Join(t.TempDir(), store.DirName)
		copyDir(t, template, dir)
		cfgFile := cfgFileOf(filepath.Join(dir, store.FileName))
		events := reloadEvents(cfgFile)
		titles := 0
		var cur reloadEvent
		var log bytes.Buffer
		s := &Server{dbPath: filepath.Join(dir, store.FileName), out: bufio.NewWriter(io.Discard), stderr: &log,
			readFile: func(string) ([]byte, error) {
				switch {
				case cur.err != nil:
					return nil, cur.err
				case cur.missing:
					return nil, &fs.PathError{Op: "open", Path: cfgFile, Err: fs.ErrNotExist}
				}
				return []byte(cur.data), nil
			}}
		defer s.shutdown()
		var m reloadModel
		var trail []string
		check := func(logged []string) {
			t.Helper()
			where := strings.Join(trail, " -> ")
			titles++
			blocks, isErr := callBlocks(t, s, "task-create", map[string]any{"title": fmt.Sprintf("Task %d", titles)})
			if m.force == "" {
				want := refusedFragment + m.msg + " " + retryHintMessage
				if m.problem == "removed" {
					want = removedRefusal(cfgFile)
				}
				if !isErr || len(blocks) != 1 || blocks[0] != want {
					t.Fatalf("%s: isErr=%v out=%q, want the refusal %q", where, isErr, blocks, want)
				}
				if s.st != nil {
					t.Fatalf("%s: store attached with no configuration in force", where)
				}
			} else {
				wantNote := ""
				switch m.problem {
				case "removed":
					wantNote = removedNote(cfgFile)
				case "invalid", "unreadable":
					prefix := notePrefix
					if m.force == "defaults" {
						prefix = defaultsNotePrefix
					}
					wantNote = prefix + m.msg + " (fix the file to apply your changes)"
				}
				wantBlocks := 1
				if wantNote != "" {
					wantBlocks = 2
				}
				if isErr || len(blocks) != wantBlocks || (wantNote != "" && blocks[1] != wantNote) {
					t.Fatalf("%s: isErr=%v out=%q, want a task and note %q", where, isErr, blocks, wantNote)
				}
				want := configs[m.force]
				if !strings.Contains(blocks[0], `"status": "`+want.DefaultStatus()+`"`) {
					t.Fatalf("%s: created %s, want status %q", where, blocks[0], want.DefaultStatus())
				}
				if !reflect.DeepEqual(s.cfg, want) {
					t.Fatalf("%s: server config = %v, want %v", where, s.cfg.Names(), want.Names())
				}
				if s.st == nil || !reflect.DeepEqual(s.st.Config(), s.cfg) {
					t.Fatalf("%s: store config differs from the server's %v", where, s.cfg.Names())
				}
				if s.cfgDefaults != (m.force == "defaults") {
					t.Fatalf("%s: cfgDefaults = %v with %q in force", where, s.cfgDefaults, m.force)
				}
			}
			if s.seenFile != m.seen || (s.cfgErr != nil) != (m.problem != "") ||
				(s.cfgErr != nil && (errText(s.cfgErr) != m.msg || s.cfgRemoved != (m.problem == "removed"))) {
				t.Fatalf("%s: server seenFile=%v cfgErr=%v cfgRemoved=%v, want model %+v", where, s.seenFile, s.cfgErr, s.cfgRemoved, m)
			}
			if got := strings.Split(strings.TrimSuffix(log.String(), "\n"), "\n"); log.Len() == 0 && len(logged) != 0 || log.Len() != 0 && !reflect.DeepEqual(got, logged) {
				t.Fatalf("%s: logged %q, want %q", where, log.String(), logged)
			}
			log.Reset()
		}
		for _, i := range path {
			cur = events[i]
			trail = append(trail, cur.name)
			var logged []string
			m, logged = m.step(cur, cfgFile)
			check(logged)
		}
		trail = append(trail, "(steady)")
		check(nil)
	}

	// Walk the model breadth-first from a fresh server, running every cell.
	type node struct {
		m    reloadModel
		path []int
	}
	cfgFile := cfgFileOf(filepath.Join(template, store.FileName))
	seen := map[reloadModel]bool{{}: true}
	queue := []node{{}}
	labels := map[string]bool{}
	cells := map[string]bool{}
	var runs [][]int
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		labels[n.m.label()] = true
		for i, e := range events {
			path := append(append([]int(nil), n.path...), i)
			cells[n.m.label()+" / "+e.name] = true
			runs = append(runs, path)
			next, _ := n.m.step(e, cfgFile)
			if !seen[next] {
				seen[next] = true
				queue = append(queue, node{next, path})
			}
		}
	}

	// The reachable states, by label ("none-unseen-none" is a fresh server).
	wantLabels := []string{
		"none-unseen-none", "none-unseen-unreadable", "none-seen-invalid", "none-seen-unreadable", "none-seen-removed",
		"defaults-unseen-none", "defaults-unseen-unreadable", "defaults-seen-none", "defaults-seen-invalid", "defaults-seen-unreadable",
		"file-seen-none", "file-seen-invalid", "file-seen-unreadable", "file-seen-removed",
	}
	var gotLabels []string
	for l := range labels {
		gotLabels = append(gotLabels, l)
	}
	sort.Strings(gotLabels)
	sort.Strings(wantLabels)
	if !reflect.DeepEqual(gotLabels, wantLabels) {
		t.Fatalf("reachable states = %q, want %q", gotLabels, wantLabels)
	}
	for _, cell := range []string{
		"none-unseen-unreadable / io error", "none-unseen-unreadable / invalid", "none-unseen-unreadable / todo",
		"none-seen-invalid / denied", "none-seen-invalid / invalid again",
		"none-seen-unreadable / io error", "none-seen-unreadable / invalid again",
		"none-seen-removed / denied", "none-seen-removed / todo", "none-seen-removed / {}",
		"defaults-unseen-none / todo", "defaults-unseen-unreadable / io error", "defaults-unseen-unreadable / invalid",
		"defaults-unseen-unreadable / todo", "defaults-seen-none / invalid", "defaults-seen-none / denied",
		"defaults-seen-invalid / denied", "defaults-seen-invalid / invalid again", "defaults-seen-unreadable / invalid", "defaults-seen-unreadable / io error",
		"file-seen-none / denied", "file-seen-removed / denied", "file-seen-none / missing", "defaults-seen-none / missing",
	} {
		if !cells[cell] {
			t.Errorf("cell %s was not run", cell)
		}
	}
	t.Logf("%d states, %d distinct cells by label, %d runs", len(seen), len(cells), len(runs))

	for _, path := range runs {
		names := make([]string, len(path))
		for j, k := range path {
			names[j] = events[k].name
		}
		t.Run(strings.Join(names, " -> "), func(t *testing.T) {
			t.Parallel()
			run(t, path)
		})
	}
}
