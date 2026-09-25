package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// customConfig is a non-default configuration exercising every kind, with a
// second active status and a second closed status.
func customConfig() *Config {
	return &Config{Statuses: []StatusDef{
		{Name: "pending", Kind: KindOpen, Default: true},
		{Name: "in_progress", Kind: KindActive},
		{Name: "review", Kind: KindActive},
		{Name: "on_hold", Kind: KindBlocked},
		{Name: "done", Kind: KindClosed},
		{Name: "wontfix", Kind: KindClosed},
	}}
}

func newStoreWith(t *testing.T, cfg *Config) *Store {
	t.Helper()
	s, err := OpenWithConfig(":memory:", cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func slugsOf(views []TaskView) []string {
	out := make([]string, len(views))
	for i, v := range views {
		out[i] = v.Slug
	}
	return out
}

func containsSlug(views []TaskView, slug string) bool {
	for _, v := range views {
		if v.Slug == slug {
			return true
		}
	}
	return false
}

func TestStatusConfigCreateAndUpdateValidation(t *testing.T) {
	s := newStoreWith(t, customConfig())

	a := mustCreate(t, s, CreateTaskInput{Title: "Omitted"})
	if a.Status != "pending" {
		t.Fatalf("default status = %q, want pending", a.Status)
	}
	r := mustCreate(t, s, CreateTaskInput{Title: "In review", Status: "review"})
	if r.Status != "review" {
		t.Fatalf("status = %q, want review", r.Status)
	}

	const listed = "(configured: pending, in_progress, review, on_hold, done, wontfix)"
	_, err := s.CreateTask(CreateTaskInput{Title: "Bad", Status: "blocked"})
	if err == nil || !strings.Contains(err.Error(), `invalid status "blocked"`) || !strings.Contains(err.Error(), listed) {
		t.Fatalf("create with unconfigured status: err = %v, want invalid status listing %s", err, listed)
	}
	_, err = s.UpdateTask(UpdateTaskInput{Slug: a.Slug, Status: ptr("dropped")})
	if err == nil || !strings.Contains(err.Error(), `invalid status "dropped"`) || !strings.Contains(err.Error(), listed) {
		t.Fatalf("update to unconfigured status: err = %v, want invalid status listing %s", err, listed)
	}
	got, _ := s.GetTask(a.Slug)
	if got.Status != "pending" {
		t.Fatalf("rejected update changed status to %q", got.Status)
	}

	// A different default is honoured.
	cfg := customConfig()
	cfg.Statuses[0].Default = false
	cfg.Statuses[2].Default = true // review
	s2 := newStoreWith(t, cfg)
	if d := mustCreate(t, s2, CreateTaskInput{Title: "X"}); d.Status != "review" {
		t.Fatalf("default status = %q, want review", d.Status)
	}
}

func TestStatusConfigCustomClosed(t *testing.T) {
	s := newStoreWith(t, customConfig())
	mustCreate(t, s, CreateTaskInput{Title: "Rejected idea"})
	mustCreate(t, s, CreateTaskInput{Title: "Follow-on", BlockedBy: []string{"rejected-idea"}})

	if v, _ := s.ListTasks(ListOpts{}); !containsSlug(v, "rejected-idea") {
		t.Fatalf("open task missing from default list: %v", slugsOf(v))
	}
	w, err := s.UpdateTask(UpdateTaskInput{Slug: "rejected-idea", Status: ptr("wontfix")})
	if err != nil {
		t.Fatalf("close as wontfix: %v", err)
	}
	if w.ClosedAt == "" {
		t.Fatal("wontfix did not stamp closed_at")
	}
	bl, err := s.ActiveBlockers(mustGet(t, s, "follow-on").ID)
	if err != nil || len(bl) != 0 {
		t.Fatalf("wontfix blocker still active: %v (err %v)", bl, err)
	}
	next, _ := s.NextTask()
	if next == nil || next.Slug != "follow-on" {
		t.Fatalf("next = %v, want follow-on (unblocked by wontfix)", next)
	}
	def, _ := s.ListTasks(ListOpts{})
	if containsSlug(def, "rejected-idea") {
		t.Fatalf("wontfix task shown in default list: %v", slugsOf(def))
	}
	all, _ := s.ListTasks(ListOpts{IncludeClosed: true})
	if !containsSlug(all, "rejected-idea") {
		t.Fatalf("wontfix task missing with IncludeClosed: %v", slugsOf(all))
	}
	// Reopening clears closed_at.
	re, err := s.UpdateTask(UpdateTaskInput{Slug: "rejected-idea", Status: ptr("review")})
	if err != nil || re.ClosedAt != "" {
		t.Fatalf("reopen: closed_at = %q, err %v", re.ClosedAt, err)
	}
}

func mustGet(t *testing.T, s *Store, slug string) *Task {
	t.Helper()
	task, err := s.GetTask(slug)
	if err != nil {
		t.Fatalf("get %q: %v", slug, err)
	}
	return task
}

func TestStatusConfigCustomActive(t *testing.T) {
	s := newStoreWith(t, customConfig())
	mustCreate(t, s, CreateTaskInput{Title: "Waiting", Priority: 1})
	mustCreate(t, s, CreateTaskInput{Title: "Under review", Status: "review", Priority: 1})
	mustCreate(t, s, CreateTaskInput{Title: "Coding", Status: "in_progress", Priority: 2})
	mustCreate(t, s, CreateTaskInput{Title: "Held", Status: "on_hold", Priority: 1})

	views, _ := s.ListTasks(ListOpts{})
	for _, v := range views {
		want := v.Status != "on_hold"
		if v.Actionable != want {
			t.Fatalf("%s (%s) actionable = %v, want %v", v.Slug, v.Status, v.Actionable, want)
		}
	}
	// Only review work left actionable: NextTask returns it.
	if _, err := s.UpdateTask(UpdateTaskInput{Slug: "waiting", Status: ptr("done")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTask(UpdateTaskInput{Slug: "coding", Status: ptr("done")}); err != nil {
		t.Fatal(err)
	}
	next, _ := s.NextTask()
	if next == nil || next.Slug != "under-review" {
		t.Fatalf("next = %v, want under-review", next)
	}

	mustCreate(t, s, CreateTaskInput{Title: "Coding again", Status: "in_progress"})
	mustCreate(t, s, CreateTaskInput{Title: "Todo"})
	out, err := s.WarmContext(1)
	if err != nil {
		t.Fatal(err)
	}
	order := []string{"## In progress\n", "## Review\n", "## Pending\n", "## Blocked\n", "## Recent activity\n"}
	last := -1
	for _, h := range order {
		i := strings.Index(out, h)
		if i < 0 || i <= last {
			t.Fatalf("section %q missing or out of order in:\n%s", h, out)
		}
		last = i
	}
	if strings.Contains(out, "## On hold") {
		t.Fatalf("blocked-kind status got its own section:\n%s", out)
	}
	review := out[strings.Index(out, "## Review\n"):strings.Index(out, "## Pending\n")]
	if !strings.Contains(review, "under-review") {
		t.Fatalf("review section lacks the review task:\n%s", review)
	}
}

func TestStatusConfigCustomBlocked(t *testing.T) {
	s := newStoreWith(t, customConfig())
	mustCreate(t, s, CreateTaskInput{Title: "Held", Status: "on_hold", Priority: 1})
	mustCreate(t, s, CreateTaskInput{Title: "Free", Priority: 5})

	views, _ := s.ListTasks(ListOpts{})
	if got := slugsOf(views); len(got) != 2 || got[0] != "free" || got[1] != "held" {
		t.Fatalf("order = %v, want [free held] (on_hold sorts after unblocked despite priority)", got)
	}
	if views[1].Actionable {
		t.Fatal("on_hold task is actionable")
	}
	if _, err := s.UpdateTask(UpdateTaskInput{Slug: "free", Status: ptr("done")}); err != nil {
		t.Fatal(err)
	}
	if next, _ := s.NextTask(); next != nil {
		t.Fatalf("next = %v, want nil (only an on_hold task is left)", next.Slug)
	}
	out, _ := s.WarmContext(1)
	if !strings.Contains(out, "## Blocked\n\n- **Held** (`held`)\n") {
		t.Fatalf("on_hold task not under Blocked:\n%s", out)
	}
	if !strings.Contains(out, "_No open tasks._") {
		t.Fatalf("expected _No open tasks._ with only held work:\n%s", out)
	}
}

// leftoverStore opens an on-disk database with a config that includes
// legacy_x, creates a task holding it (plus a dependent), then reopens the
// database with customConfig, which no longer lists legacy_x — the path a
// project takes when it drops a status from its config.
func leftoverStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	old := customConfig()
	old.Statuses = append(old.Statuses, StatusDef{Name: "legacy_x", Kind: KindOpen})
	s1, err := OpenWithConfig(path, old)
	if err != nil {
		t.Fatalf("open with old config: %v", err)
	}
	mustCreate(t, s1, CreateTaskInput{Title: "Old work", Status: "legacy_x", Priority: 1})
	mustCreate(t, s1, CreateTaskInput{Title: "Needs old work", BlockedBy: []string{"old-work"}})
	mustCreate(t, s1, CreateTaskInput{Title: "Old parent"})
	mustCreate(t, s1, CreateTaskInput{Title: "Old child", Parent: "old-parent", Status: "legacy_x"})
	s1.Close()

	s, err := OpenWithConfig(path, customConfig())
	if err != nil {
		t.Fatalf("reopen with new config: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStatusConfigLeftoverStatus(t *testing.T) {
	s := leftoverStore(t)

	if got := mustGet(t, s, "old-work"); got.Status != "legacy_x" {
		t.Fatalf("GetTask status = %q, want legacy_x", got.Status)
	}
	d, err := s.Detail("old-parent")
	if err != nil || len(d.Children) != 1 || d.Children[0].Status != "legacy_x" {
		t.Fatalf("Detail children = %+v (err %v), want old-child as legacy_x", d.Children, err)
	}
	if dw, _ := s.Detail("old-work"); dw.Status != "legacy_x" || !strings.Contains(dw.Markdown(), "**status:** legacy_x") {
		t.Fatalf("Detail status = %q", dw.Status)
	}

	views, _ := s.ListTasks(ListOpts{})
	var old *TaskView
	for i := range views {
		if views[i].Slug == "old-work" {
			old = &views[i]
		}
	}
	if old == nil {
		t.Fatalf("leftover task missing from default list: %v", slugsOf(views))
	}
	if old.Status != "legacy_x" || old.Actionable {
		t.Fatalf("leftover view: status %q actionable %v, want legacy_x / false", old.Status, old.Actionable)
	}
	// A leftover status is held for ordering: despite priority 1, old-work
	// sorts after the workable old-parent, among the held tasks.
	if got, want := slugsOf(views), []string{"old-parent", "old-work", "old-child", "needs-old-work"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v (leftovers sort with held work)", got, want)
	}

	tree, _ := s.Tree("old-parent")
	if len(tree) != 1 || len(tree[0].Children) != 1 || tree[0].Children[0].Status != "legacy_x" {
		t.Fatalf("Tree child status wrong: %+v", tree)
	}

	bl, _ := s.ActiveBlockers(mustGet(t, s, "needs-old-work").ID)
	if len(bl) != 1 || bl[0].Slug != "old-work" {
		t.Fatalf("leftover task no longer blocks its dependent: %v", bl)
	}
	next, _ := s.NextTask()
	if next == nil || next.Slug != "old-parent" {
		t.Fatalf("next = %v, want old-parent (leftovers are not actionable)", next)
	}

	f, err := s.ListTasks(ListOpts{Status: "legacy_x"})
	if err != nil || len(f) != 2 {
		t.Fatalf("ListTasks filtered by legacy_x = %v (err %v), want 2", slugsOf(f), err)
	}
	fr, err := s.Find(FindOpts{Status: "legacy_x"})
	if err != nil || len(fr.Tasks) != 2 {
		t.Fatalf("Find filtered by legacy_x = %v (err %v), want 2", fr, err)
	}
	if _, err := s.ListTasks(ListOpts{Status: "Bad-Name"}); err == nil || !strings.Contains(err.Error(), `invalid status "Bad-Name"`) {
		t.Fatalf("malformed list filter: err = %v", err)
	}
	if _, err := s.Find(FindOpts{Status: "x'y"}); err == nil || !strings.Contains(err.Error(), "invalid status") {
		t.Fatalf("malformed find filter: err = %v", err)
	}

	u, err := s.UpdateTask(UpdateTaskInput{Slug: "old-work", Title: ptr("Old work, renamed")})
	if err != nil || u.Title != "Old work, renamed" || u.Status != "legacy_x" || u.ClosedAt != "" {
		t.Fatalf("title edit of leftover task: %+v (err %v)", u, err)
	}
	if _, err := s.UpdateTask(UpdateTaskInput{Slug: "old-work", Status: ptr("legacy_x")}); err == nil || !strings.Contains(err.Error(), `invalid status "legacy_x"`) {
		t.Fatalf("update to leftover status: err = %v, want rejection", err)
	}
	if _, err := s.CreateTask(CreateTaskInput{Title: "New", Status: "legacy_x"}); err == nil {
		t.Fatal("create with leftover status accepted")
	}

	out, _ := s.WarmContext(1)
	want := "## Legacy x (not in config)\n\n- **Old work, renamed** (`old-work`)\n- **Old child** (`old-child`)\n\n## Recent activity"
	if !strings.Contains(out, want) {
		t.Fatalf("leftover section missing or misplaced; want %q in:\n%s", want, out)
	}
	if strings.Index(out, "## Blocked\n") > strings.Index(out, "## Legacy x (not in config)\n") {
		t.Fatalf("leftover section precedes Blocked:\n%s", out)
	}
}

func TestStatusConfigLeftoverSectionsSortedAndCountAsListed(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	old := customConfig()
	old.Statuses = append(old.Statuses,
		StatusDef{Name: "zeta", Kind: KindOpen}, StatusDef{Name: "alpha", Kind: KindOpen})
	s1, err := OpenWithConfig(path, old)
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s1, CreateTaskInput{Title: "Z", Status: "zeta"})
	mustCreate(t, s1, CreateTaskInput{Title: "A", Status: "alpha"})
	s1.Close()
	s, err := OpenWithConfig(path, customConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out, _ := s.WarmContext(1)
	ia, iz := strings.Index(out, "## Alpha (not in config)\n"), strings.Index(out, "## Zeta (not in config)\n")
	if ia < 0 || iz < 0 || ia > iz {
		t.Fatalf("leftover sections not sorted by name:\n%s", out)
	}
	if strings.Contains(out, "_No open tasks._") {
		t.Fatalf("leftover tasks listed but _No open tasks._ printed:\n%s", out)
	}
}

func TestOpenLoadsConfigBesideDB(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{
  // project statuses
  "statuses": [
    {"name": "todo", "kind": "open", "default": true},
    {"name": "shipped", "kind": "closed"},
  ],
}`
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if got := s.Config().Names(); strings.Join(got, ",") != "todo,shipped" {
		t.Fatalf("Config().Names() = %v, want [todo shipped]", got)
	}
	a := mustCreate(t, s, CreateTaskInput{Title: "A"})
	if a.Status != "todo" {
		t.Fatalf("default status = %q, want todo", a.Status)
	}
	if _, err := s.CreateTask(CreateTaskInput{Title: "B", Status: "pending"}); err == nil {
		t.Fatal("status absent from the loaded config was accepted")
	}
	c, err := s.UpdateTask(UpdateTaskInput{Slug: a.Slug, Status: ptr("shipped")})
	if err != nil || c.ClosedAt == "" {
		t.Fatalf("close via config status: %+v (err %v)", c, err)
	}
}

func TestOpenBadConfigFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(cfgPath, []byte(`{"statuses": [{"name": "todo", "kind": "open"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(dir, FileName))
	if err == nil {
		s.Close()
		t.Fatal("open succeeded with an invalid config")
	}
	if !strings.Contains(err.Error(), cfgPath) {
		t.Fatalf("error %q does not name the config file %s", err, cfgPath)
	}
}

func TestOpenWithConfigValidatesAndIsolates(t *testing.T) {
	if _, err := OpenWithConfig(":memory:", &Config{}); err == nil {
		t.Fatal("empty config accepted")
	}
	s, err := OpenWithConfig(":memory:", nil)
	if err != nil {
		t.Fatalf("nil config: %v", err)
	}
	defer s.Close()
	if got := strings.Join(s.Config().Names(), ","); got != "pending,in_progress,blocked,done,dropped" {
		t.Fatalf("nil config names = %s, want the defaults", got)
	}

	cfg := customConfig()
	s2 := newStoreWith(t, cfg)
	cfg.Statuses[4].Kind = KindOpen // mutate the caller's copy: done no longer closed
	s2.Config().Statuses[5].Kind = KindOpen
	mustCreate(t, s2, CreateTaskInput{Title: "T"})
	c, _ := s2.UpdateTask(UpdateTaskInput{Slug: "t", Status: ptr("done")})
	if c.ClosedAt == "" {
		t.Fatal("mutating a config after open changed the store's behaviour")
	}
	if v, _ := s2.ListTasks(ListOpts{}); len(v) != 0 {
		t.Fatalf("done task listed after config mutation: %v", slugsOf(v))
	}
}

func TestSQLStatusList(t *testing.T) {
	got, err := sqlStatusList([]string{"done", "wont_fix2"})
	if err != nil || got != "('done','wont_fix2')" {
		t.Fatalf("sqlStatusList = %q, %v", got, err)
	}
	for _, bad := range []string{"x'); DROP TABLE task; --", "Done", "", "a b", "blocked_", "a__b"} {
		if _, err := sqlStatusList([]string{"done", bad}); err == nil {
			t.Fatalf("sqlStatusList accepted %q", bad)
		}
	}
	empty, err := sqlStatusList(nil)
	if err != nil || empty != "()" {
		t.Fatalf("empty list = %q, %v", empty, err)
	}
	// SQLite semantics for the empty list: NOT IN () is true, IN () is false.
	s := newStore(t)
	var notIn, in int
	if err := s.db.QueryRow(`SELECT 'a' NOT IN `+empty+`, 'a' IN `+empty).Scan(&notIn, &in); err != nil {
		t.Fatalf("empty list query: %v", err)
	}
	if notIn != 1 || in != 0 {
		t.Fatalf("NOT IN () = %d, IN () = %d; want 1, 0", notIn, in)
	}
}

// TestCreateTaskStampsClosedAt covers CreateTask's own closed_at handling,
// independent of UpdateTask: a task created directly with a closed-kind
// status must not be left with a NULL closed_at.
func TestCreateTaskStampsClosedAt(t *testing.T) {
	s := newStore(t)
	d := mustCreate(t, s, CreateTaskInput{Title: "Done on arrival", Status: "done"})
	if d.ClosedAt == "" {
		t.Fatal("create with done did not stamp closed_at")
	}
	if d.ClosedAt != d.CreatedAt {
		t.Fatalf("closed_at = %q, want equal to created_at %q", d.ClosedAt, d.CreatedAt)
	}

	p := mustCreate(t, s, CreateTaskInput{Title: "Still pending"})
	if p.ClosedAt != "" {
		t.Fatalf("create with pending stamped closed_at = %q, want empty", p.ClosedAt)
	}

	cs := newStoreWith(t, customConfig())
	w := mustCreate(t, cs, CreateTaskInput{Title: "Rejected on arrival", Status: "wontfix"})
	if w.ClosedAt == "" {
		t.Fatal("create with custom closed status wontfix did not stamp closed_at")
	}
}

func TestStatusHeading(t *testing.T) {
	for in, want := range map[string]string{"in_progress": "In progress", "pending": "Pending", "legacy_x": "Legacy x", "a": "A"} {
		if got := statusHeading(in); got != want {
			t.Fatalf("statusHeading(%q) = %q, want %q", in, got, want)
		}
	}
}

// warmFixture builds a representative default-config tree for the WarmContext
// output test, then pins every journal timestamp so the output is stable.
func warmFixture(t *testing.T, s *Store, name string) {
	t.Helper()
	switch name {
	case "full":
		epic := mustCreate(t, s, CreateTaskInput{Title: "Epic", Priority: 2})
		mustCreate(t, s, CreateTaskInput{Title: "Child one", Parent: epic.Slug})
		mustCreate(t, s, CreateTaskInput{Title: "Child two", Parent: epic.Slug, Status: "done"})
		mustCreate(t, s, CreateTaskInput{Title: "Underway", Priority: 1})
		if _, err := s.UpdateTask(UpdateTaskInput{Slug: "underway", Status: ptr("in_progress")}); err != nil {
			t.Fatal(err)
		}
		mustCreate(t, s, CreateTaskInput{Title: "Groundwork", Priority: 1})
		mustCreate(t, s, CreateTaskInput{Title: "Needs groundwork", BlockedBy: []string{"groundwork"}})
		mustCreate(t, s, CreateTaskInput{Title: "On hold", Status: "blocked"})
		mustCreate(t, s, CreateTaskInput{Title: "Shipped", Status: "done"})
		mustCreate(t, s, CreateTaskInput{Title: "Abandoned", Status: "dropped"})
		mustCreate(t, s, CreateTaskInput{Title: "Unblocked by close", BlockedBy: []string{"shipped"}})
	case "onlyblocked":
		mustCreate(t, s, CreateTaskInput{Title: "On hold", Status: "blocked"})
		mustCreate(t, s, CreateTaskInput{Title: "Finished", Status: "done"})
	}
	if _, err := s.db.Exec(`UPDATE journal SET ts = '2026-01-02T03:04:05Z'`); err != nil {
		t.Fatal(err)
	}
}

// TestWarmContextDefaultConfigOutput pins WarmContext's exact output under the
// default config: In progress, Pending, Blocked, Recent activity, byte for
// byte.
func TestWarmContextDefaultConfigOutput(t *testing.T) {
	want := map[string]string{
		"full":        "# worklog — open work\n\n**Next up:** Underway (`underway`, priority 1)\n\n## In progress\n\n- **Underway** (`underway`)\n\n## Pending\n\n- **Groundwork** (`groundwork`)\n- **Epic** (`epic`) · 2 subtasks\n- **Child one** (`child-one`)\n- **Unblocked by close** (`unblocked-by-close`)\n- **Needs groundwork** (`needs-groundwork`) — blocked by groundwork\n\n## Blocked\n\n- **Needs groundwork** (`needs-groundwork`) — blocked by groundwork\n- **On hold** (`on-hold`)\n\n## Recent activity\n\n- 2026-01-02T03:04 [created] (`unblocked-by-close`) created task: Unblocked by close\n- 2026-01-02T03:04 [created] (`abandoned`) created task: Abandoned\n- 2026-01-02T03:04 [created] (`shipped`) created task: Shipped\n- 2026-01-02T03:04 [created] (`on-hold`) created task: On hold\n- 2026-01-02T03:04 [created] (`needs-groundwork`) created task: Needs groundwork\n- 2026-01-02T03:04 [created] (`groundwork`) created task: Groundwork\n- 2026-01-02T03:04 [status_change] (`underway`) pending → in_progress\n- 2026-01-02T03:04 [created] (`underway`) created task: Underway\n- 2026-01-02T03:04 [created] (`child-two`) created task: Child two\n- 2026-01-02T03:04 [created] (`child-one`) created task: Child one\n\n",
		"empty":       "# worklog — open work\n\n_No open tasks._\n",
		"onlyblocked": "# worklog — open work\n\n## Blocked\n\n- **On hold** (`on-hold`)\n\n## Recent activity\n\n- 2026-01-02T03:04 [created] (`finished`) created task: Finished\n- 2026-01-02T03:04 [created] (`on-hold`) created task: On hold\n\n_No open tasks._\n",
	}
	for name, w := range want {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			warmFixture(t, s, name)
			got, err := s.WarmContext(10)
			if err != nil {
				t.Fatal(err)
			}
			if got != w {
				t.Fatalf("WarmContext output changed.\n got: %q\nwant: %q", got, w)
			}
		})
	}
}

// TestWarmContextLeftoverHeadingNeverCollides reopens a database whose tasks
// hold statuses named like the fixed sections (blocked, recent_activity) under
// a config that lists neither: each leftover section carries the
// "(not in config)" suffix, so no heading appears twice.
func TestWarmContextLeftoverHeadingNeverCollides(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	old := customConfig()
	old.Statuses = append(old.Statuses,
		StatusDef{Name: "blocked", Kind: KindBlocked}, StatusDef{Name: "recent_activity", Kind: KindBlocked})
	s1, err := OpenWithConfig(path, old)
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s1, CreateTaskInput{Title: "Stale", Status: "blocked"})
	mustCreate(t, s1, CreateTaskInput{Title: "Old log", Status: "recent_activity"})
	mustCreate(t, s1, CreateTaskInput{Title: "Waits", BlockedBy: []string{"stale"}})
	s1.Close()
	s, err := OpenWithConfig(path, customConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	out, err := s.WarmContext(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Blocked\n\n- **Waits** (`waits`) — blocked by stale\n\n",
		"## Blocked (not in config)\n\n- **Stale** (`stale`)\n\n",
		"## Recent activity (not in config)\n\n- **Old log** (`old-log`)\n\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q in:\n%s", want, out)
		}
	}
	for _, h := range []string{"## Blocked\n", "## Recent activity\n"} {
		if n := strings.Count(out, h); n != 1 {
			t.Fatalf("heading %q appears %d times, want 1:\n%s", h, n, out)
		}
	}
}

// TestListTasksLeftoverSortsWithHeld proves a task holding a status absent
// from the config sorts with held (blocked) work rather than among workable
// tasks, while its Actionable flag and its absence from the Blocked section
// are unchanged.
func TestListTasksLeftoverSortsWithHeld(t *testing.T) {
	s := leftoverStore(t)
	views, err := s.ListTasks(ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	// old-work is priority 1 but a leftover; old-parent (priority 3) is the
	// only workable task, so it leads.
	if got, want := strings.Join(slugsOf(views), ","), "old-parent,old-work,old-child,needs-old-work"; got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
	for _, v := range views {
		if want := v.Slug == "old-parent"; v.Actionable != want {
			t.Errorf("%s: Actionable = %v, want %v", v.Slug, v.Actionable, want)
		}
	}
	out, _ := s.WarmContext(1)
	i := strings.Index(out, "## Blocked\n")
	if i < 0 {
		t.Fatalf("no Blocked section:\n%s", out)
	}
	section := out[i:]
	section = section[:strings.Index(section, "\n\n## ")]
	if section != "## Blocked\n\n- **Needs old work** (`needs-old-work`) — blocked by old-work" {
		t.Fatalf("Blocked section = %q, want only the dependency-blocked task", section)
	}
}

// TestStatusFilterRejectsMalformedNames: list and find filters accept any
// well-formed name (configured or not) and reject a malformed one, naming the
// pattern.
func TestStatusFilterRejectsMalformedNames(t *testing.T) {
	s := newStore(t)
	for _, bad := range []string{"blocked_", "recent__activity", "_x", "Done"} {
		for name, run := range map[string]func() error{
			"ListTasks": func() error { _, err := s.ListTasks(ListOpts{Status: bad}); return err },
			"Find":      func() error { _, err := s.Find(FindOpts{Status: bad}); return err },
		} {
			err := run()
			if err == nil || !strings.Contains(err.Error(), "must match "+StatusNamePattern) {
				t.Errorf("%s(status %q) = %v, want an error naming %s", name, bad, err, StatusNamePattern)
			}
		}
	}
	if _, err := s.ListTasks(ListOpts{Status: "not_configured2"}); err != nil {
		t.Errorf("well-formed unconfigured filter rejected: %v", err)
	}
}
