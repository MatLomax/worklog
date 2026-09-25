package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeConfig writes content as the config file in a fresh temp dir and
// returns the dir and the file path.
func writeConfig(t *testing.T, content string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return dir, path
}

// loadErr loads content as a config file and returns the error, failing the
// test when loading succeeds.
func loadErr(t *testing.T, content string) (msg, path string) {
	t.Helper()
	dir, path := writeConfig(t, content)
	cfg, err := LoadConfig(dir)
	if err == nil {
		t.Fatalf("LoadConfig(%q) = %+v, want error", content, cfg)
	}
	return err.Error(), path
}

func TestLoadConfigAbsentFileYieldsDefaults(t *testing.T) {
	cfg, err := LoadConfig(t.TempDir())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !reflect.DeepEqual(cfg, DefaultConfig()) {
		t.Fatalf("cfg = %+v, want defaults", cfg)
	}
}

func TestLoadConfigReadErrorReturned(t *testing.T) {
	dir := t.TempDir()
	// A directory where the file should be makes the read fail with something
	// other than not-exist.
	if err := os.Mkdir(filepath.Join(dir, ConfigFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(dir); err == nil {
		t.Fatal("LoadConfig on an unreadable config: want error")
	}
}

func TestLoadConfigWithoutStatusesYieldsDefaults(t *testing.T) {
	for name, content := range map[string]string{
		"empty object": "{}",
		"bom object":   "\xEF\xBB\xBF{}",
	} {
		t.Run(name, func(t *testing.T) {
			dir, _ := writeConfig(t, content)
			cfg, err := LoadConfig(dir)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if !reflect.DeepEqual(cfg, DefaultConfig()) {
				t.Fatalf("cfg = %+v, want defaults", cfg)
			}
		})
	}
}

// noContentMsg is the error text, after the path, for a config file that
// holds no JSON value.
const noContentMsg = `config file has no content (it is empty or holds only whitespace, comments or a byte-order mark); write a JSON object: {"statuses": [...]}, or {} for the default statuses`

// TestLoadConfigNoContentRejected: an existing file with no JSON value is what
// an editor saving in place exposes for a moment, so it is an error naming the
// file and the remedy, never the defaults (only a missing file is).
func TestLoadConfigNoContentRejected(t *testing.T) {
	for name, content := range map[string]string{
		"empty file":    "",
		"whitespace":    "  \n\t\r\n",
		"bom only":      "\xEF\xBB\xBF",
		"bom and space": "\xEF\xBB\xBF \n",
		"comments only": "// nothing here\n/* still\nnothing */\n",
		"line comment":  "// no newline",
		"cr comment":    "// a\r// b\r",
	} {
		t.Run(name, func(t *testing.T) {
			msg, path := loadErr(t, content)
			if want := path + ": " + noContentMsg; msg != want {
				t.Fatalf("error = %q, want %q", msg, want)
			}
			if _, err := ParseConfig([]byte(content)); err == nil || err.Error() != noContentMsg {
				t.Fatalf("ParseConfig error = %v, want %q", err, noContentMsg)
			}
		})
	}
}

func TestLoadConfigCustomList(t *testing.T) {
	dir, _ := writeConfig(t, `{
  // the list replaces the defaults wholesale
  "statuses": [
    {"name": "pending",     "kind": "open", "default": true},
    {"name": "in_progress", "kind": "active"},
    {"name": "review",      "kind": "active"},
    {"name": "blocked",     "kind": "blocked"},
    {"name": "done",        "kind": "closed"},
    {"name": "dropped",     "kind": "closed"},  /* trailing commas allowed */
  ],
}
`)
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := []string{"pending", "in_progress", "review", "blocked", "done", "dropped"}
	if got := cfg.Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Names = %v, want %v", got, want)
	}
	if k, ok := cfg.Kind("review"); !ok || k != KindActive {
		t.Fatalf("Kind(review) = %q, %v; want active, true", k, ok)
	}
	if cfg.DefaultStatus() != "pending" {
		t.Fatalf("DefaultStatus = %q, want pending", cfg.DefaultStatus())
	}
}

// TestConfigValidateRejects pins every status-list rule violation read from a
// file: the exact message, and its line:col. In each case "@" marks where the
// error must point (and is removed from the content): the offending field's
// value for a rule about one status (the entry's opening brace when that field
// is absent), the list's opening bracket for a rule about the whole list.
func TestConfigValidateRejects(t *testing.T) {
	const pattern = "(must match ^[a-z][a-z0-9]*(_[a-z0-9]+)*$)"
	cases := []struct {
		name     string
		statuses string
		want     string // the message after path:line:col:
	}{
		{"empty list", `@[]`, "statuses: the list is empty; at least one status is required"},
		{"bad chars", `[{"name":@"to-do","kind":"open","default":true},{"name":"done","kind":"closed"}]`,
			`statuses[0]: invalid status name "to-do" ` + pattern},
		{"uppercase", `[{"name":@"Pending","kind":"open","default":true},{"name":"done","kind":"closed"}]`,
			`statuses[0]: invalid status name "Pending" ` + pattern},
		{"leading digit", `[{"name":@"1st","kind":"open","default":true},{"name":"done","kind":"closed"}]`,
			`statuses[0]: invalid status name "1st" ` + pattern},
		{"empty name", `[{"name":@"","kind":"open","default":true},{"name":"done","kind":"closed"}]`,
			`statuses[0]: invalid status name "" ` + pattern},
		{"missing name", `[{"name":"todo","kind":"open","default":true}, @{"kind":"closed"}]`,
			`statuses[1]: invalid status name "" ` + pattern},
		{"trailing underscore", `[{"name":"todo","kind":"open","default":true},{"name":@"blocked_","kind":"open"},{"name":"done","kind":"closed"}]`,
			`statuses[1]: invalid status name "blocked_" ` + pattern},
		{"doubled underscore", `[{"name":"todo","kind":"open","default":true},{"name":@"recent__activity","kind":"active"},{"name":"done","kind":"closed"}]`,
			`statuses[1]: invalid status name "recent__activity" ` + pattern},
		{"duplicate", `[{"name":"todo","kind":"open","default":true},{"name":@"todo","kind":"active"},{"name":"done","kind":"closed"}]`,
			`status "todo": duplicate name`},
		{"unknown kind", `[{"name":"todo","kind":@"waiting","default":true},{"name":"done","kind":"closed"}]`,
			`status "todo": unknown kind "waiting" (must be open, active, blocked, or closed)`},
		{"missing kind", `[@{"name":"todo","default":true},{"name":"done","kind":"closed"}]`,
			`status "todo": unknown kind "" (must be open, active, blocked, or closed)`},
		{"reserved heading", `[{"name":"todo","kind":"open","default":true},{"name":@"recent_activity","kind":"active"},{"name":"done","kind":"closed"}]`,
			`status "recent_activity": its briefing section heading "Recent activity" collides with the fixed "Recent activity" section; rename the status`},
		{"zero defaults", `@[{"name":"todo","kind":"open"},{"name":"done","kind":"closed"}]`,
			`statuses: no status is marked "default": true; exactly one is required`},
		{"two defaults", `[{"name":"todo","kind":"open","default":true},{"name":"doing","kind":"active","default":@true},{"name":"done","kind":"closed"}]`,
			`statuses: ["todo" "doing"] are all marked "default": true; exactly one is allowed`},
		{"default closed", `[{"name":"todo","kind":"open"},{"name":"done","kind":"closed","default":@true}]`,
			`status "done": the default status must be of kind open or active, not closed`},
		{"default blocked", `[{"name":"todo","kind":"open"},{"name":"held","kind":"blocked","default":@true},{"name":"done","kind":"closed"}]`,
			`status "held": the default status must be of kind open or active, not blocked`},
		{"no actionable", `@[{"name":"held","kind":"blocked"},{"name":"done","kind":"closed"}]`,
			"statuses: no status of kind open or active; at least one is required"},
		{"no closed", `@[{"name":"todo","kind":"open","default":true},{"name":"held","kind":"blocked"}]`,
			"statuses: no status of kind closed; at least one is required"},
		{"multi-line", "// header\n@[\n  /* first */ {\"name\": \"todo\", \"kind\": \"open\"},\n  {\"name\": \"done\", \"kind\": \"closed\"},\n]",
			`statuses: no status is marked "default": true; exactly one is required`},
		{"multi-line entry", "[\n  {\"name\": \"todo\", \"kind\": \"open\", \"default\": true},\n  /* é */ {\"name\": @\"Todo\",\n   \"kind\": \"closed\"}]",
			`statuses[1]: invalid status name "Todo" ` + pattern},
		{"escaped name as written", `[{"name":"todo","kind":"open","default":true},{"name":@"t\u006fdo","kind":"closed"}]`,
			`status "t\u006fdo": duplicate name`},
		{"invalid UTF-8 name", "[{\"name\":@\"to\xffdo\",\"kind\":\"open\",\"default\":true},{\"name\":\"done\",\"kind\":\"closed\"}]",
			`statuses[0]: invalid status name "to\xffdo" ` + pattern},
		{"invalid UTF-8 kind", "[{\"name\":\"todo\",\"kind\":@\"op\xc3en\",\"default\":true},{\"name\":\"done\",\"kind\":\"closed\"}]",
			`status "todo": unknown kind "op\xc3en" (must be open, active, blocked, or closed)`},
		{"invisible character in name", "[{\"name\":@\"to\u200bdo\",\"kind\":\"open\",\"default\":true},{\"name\":\"done\",\"kind\":\"closed\"}]",
			`statuses[0]: invalid status name "to\u200bdo" ` + pattern},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Count(tc.statuses, "@") != 1 {
				t.Fatalf("case needs exactly one @ marker: %q", tc.statuses)
			}
			content := `{"statuses": ` + tc.statuses + `}`
			at := strings.Index(content, "@")
			content = strings.Replace(content, "@", "", 1)
			line, col := lineCol([]byte(content), at)
			msg, path := loadErr(t, content)
			if want := fmt.Sprintf("%s:%d:%d: %s", path, line, col, tc.want); msg != want {
				t.Fatalf("error = %q\nwant    %q", msg, want)
			}
			// ParseConfig reports the same, without the path.
			if _, err := ParseConfig([]byte(content)); err == nil || err.Error() != fmt.Sprintf("%d:%d: %s", line, col, tc.want) {
				t.Fatalf("ParseConfig error = %v", err)
			}
		})
	}
}

// TestConfigValidateInCodeUnpositioned: Validate on a Config built in code
// reports the same rules without a position, echoing names and kinds
// Go-quoted, so invalid bytes are visible.
func TestConfigValidateInCodeUnpositioned(t *testing.T) {
	cases := []struct {
		statuses []StatusDef
		want     string
	}{
		{nil, "statuses: the list is empty; at least one status is required"},
		{[]StatusDef{{Name: "to\xffdo", Kind: KindOpen, Default: true}, {Name: "done", Kind: KindClosed}},
			`statuses[0]: invalid status name "to\xffdo" (must match ^[a-z][a-z0-9]*(_[a-z0-9]+)*$)`},
		{[]StatusDef{{Name: "todo", Kind: "wait\xff", Default: true}, {Name: "done", Kind: KindClosed}},
			`status "todo": unknown kind "wait\xff" (must be open, active, blocked, or closed)`},
		{[]StatusDef{{Name: "todo", Kind: KindOpen, Default: true}, {Name: "doing", Kind: KindActive, Default: true}, {Name: "done", Kind: KindClosed}},
			`statuses: ["todo" "doing"] are all marked "default": true; exactly one is allowed`},
		{[]StatusDef{{Name: "todo", Kind: KindOpen}, {Name: "done", Kind: KindClosed}},
			`statuses: no status is marked "default": true; exactly one is required`},
	}
	for _, tc := range cases {
		err := (&Config{Statuses: tc.statuses}).Validate()
		if err == nil || err.Error() != tc.want {
			t.Errorf("Validate(%+v) = %v, want %q", tc.statuses, err, tc.want)
		}
	}
}

func TestConfigValidateAcceptsDefaults(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate: %v", err)
	}
}

func TestLoadConfigUnknownFieldRejected(t *testing.T) {
	for name, content := range map[string]string{
		"top level": `{"statuses": [{"name":"a","kind":"open","default":true},{"name":"z","kind":"closed"}], "colour": "red"}`,
		"in status": `{"statuses": [{"name":"a","kind":"open","default":true,"color":"red"},{"name":"z","kind":"closed"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			msg, path := loadErr(t, content)
			if !strings.HasPrefix(msg, path) || !strings.Contains(msg, "unknown field") {
				t.Fatalf("error = %q, want path-prefixed unknown field", msg)
			}
		})
	}
}

func TestLoadConfigTrailingDataRejected(t *testing.T) {
	for name, tc := range map[string]struct {
		content string
		loc     string
	}{
		"second object": {"{}\n{}", ":2:1: "},
		"stray brace":   {"{}\n  }", ":2:3: "},
		"stray word":    {"{} x", ":1:4: "},
	} {
		t.Run(name, func(t *testing.T) {
			msg, path := loadErr(t, tc.content)
			if !strings.HasPrefix(msg, path+tc.loc) || !strings.Contains(msg, "after the top-level value") {
				t.Fatalf("error = %q, want prefix %q and trailing-data message", msg, path+tc.loc)
			}
		})
	}
}

func TestLoadConfigJSONCFeatures(t *testing.T) {
	dir, _ := writeConfig(t, `// leading line comment
/* leading
   block comment */
{
  "statuses": [ // comment after bracket
    {"name": "todo", /* inline */ "kind": "open", "default": true,},
    {"name": "done", "kind": "closed",
     // comment between a trailing comma and the brace
    },
    /* a block before the closing bracket */
  ], /* trailing comma on the object */
}
// trailing comment with no newline`)
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.Names(); !reflect.DeepEqual(got, []string{"todo", "done"}) {
		t.Fatalf("Names = %v", got)
	}
}

func TestStripJSONCStringsUntouched(t *testing.T) {
	cases := []string{
		`{"a": "http://x"}`,
		`{"a": "/* no */"}`,
		`{"a": "say \"hi\" // still string"}`,
		`{"a": "ends in backslash \\"}`,
		`{"a": "\\", "b": "// x"}`,
		`{"a": "x,]"}`,
		`{"a": "x,}"}`,
	}
	for _, in := range cases {
		out, err := stripJSONC([]byte(in))
		if err != nil {
			t.Fatalf("stripJSONC(%s): %v", in, err)
		}
		if string(out) != in {
			t.Errorf("stripJSONC(%s) = %s, want unchanged", in, out)
		}
	}
}

func TestStripJSONCBackslashAtStringEndThenComment(t *testing.T) {
	in := `{"a": "c:\\" // comment "b"` + "\n}"
	want := `{"a": "c:\\"` + strings.Repeat(" ", len(` // comment "b"`)) + "\n}"
	out, err := stripJSONC([]byte(in))
	if err != nil {
		t.Fatalf("stripJSONC: %v", err)
	}
	if string(out) != want {
		t.Fatalf("stripJSONC = %q, want %q", out, want)
	}
}

func TestStripJSONCEscapedQuoteThenSlashesInString(t *testing.T) {
	in := `{"a": "q\" // not a comment"} // a comment`
	want := `{"a": "q\" // not a comment"}             `
	out, err := stripJSONC([]byte(in))
	if err != nil {
		t.Fatalf("stripJSONC: %v", err)
	}
	if string(out) != want {
		t.Fatalf("stripJSONC = %q, want %q", out, want)
	}
}

func TestStripJSONCBlanksCommentsAndTrailingCommas(t *testing.T) {
	in := "{\"a\": [1, 2,], /* x\ny */ \"b\": 3, // z\n}"
	want := "{\"a\": [1, 2 ],     \n     \"b\": 3      \n}"
	out, err := stripJSONC([]byte(in))
	if err != nil {
		t.Fatalf("stripJSONC: %v", err)
	}
	if string(out) != want {
		t.Fatalf("stripJSONC =\n%q\nwant\n%q", out, want)
	}
}

func TestStripJSONCPreservesLength(t *testing.T) {
	for _, in := range []string{
		"",
		"{}",
		"\xEF\xBB\xBF{}",
		"// c\n{\"a\": 1,}\n",
		"/* a\r\nb */ [1,2,/* c */]",
		`{"s": "\"//\\", }`,
		"{\"x\": \"unterminated",
	} {
		out, err := stripJSONC([]byte(in))
		if err != nil {
			t.Fatalf("stripJSONC(%q): %v", in, err)
		}
		if len(out) != len(in) {
			t.Errorf("stripJSONC(%q) length = %d, want %d", in, len(out), len(in))
		}
		if strings.Count(string(out), "\n") != strings.Count(in, "\n") {
			t.Errorf("stripJSONC(%q) changed the newline count", in)
		}
	}
}

func TestStripJSONCDoesNotModifyInput(t *testing.T) {
	in := []byte("{\"a\": 1, // c\n}")
	orig := string(in)
	if _, err := stripJSONC(in); err != nil {
		t.Fatal(err)
	}
	if string(in) != orig {
		t.Fatalf("input modified: %q", in)
	}
}

func TestLoadConfigUnterminatedBlockComment(t *testing.T) {
	msg, path := loadErr(t, "{\n  \"statuses\": [] /* open\n  never closed\n")
	want := path + ":2:18: unterminated block comment"
	if msg != want {
		t.Fatalf("error = %q, want %q", msg, want)
	}
}

func TestParseConfigUnterminatedBlockComment(t *testing.T) {
	_, err := ParseConfig([]byte("{} /* x"))
	if err == nil || err.Error() != "1:4: unterminated block comment" {
		t.Fatalf("err = %v, want 1:4: unterminated block comment", err)
	}
}

func TestLoadConfigSyntaxErrorLineCol(t *testing.T) {
	content := "/* a comment\n   spanning lines */\n{\n  \"statuses\": [\n    {\"name\": \"a\" \"kind\": \"open\"}\n  ]\n}\n"
	msg, path := loadErr(t, content)
	// The missing comma: the decoder stops at the second key's opening quote,
	// line 5, column 18.
	want := path + ":5:18: "
	if !strings.HasPrefix(msg, want) {
		t.Fatalf("error = %q, want prefix %q", msg, want)
	}
}

func TestLoadConfigTruncatedInput(t *testing.T) {
	msg, path := loadErr(t, "{\n  \"statuses\": [\n")
	if !strings.HasPrefix(msg, path+":") || !strings.Contains(msg, "unexpected end") {
		t.Fatalf("error = %q, want path-prefixed unexpected end of input", msg)
	}
}

func TestLoadConfigTypeErrorLineCol(t *testing.T) {
	content := "{\n  // statuses\n  \"statuses\": [\n    {\"name\": \"a\", \"kind\": \"open\", \"default\": \"yes\"}\n  ]\n}\n"
	msg, path := loadErr(t, content)
	if !strings.HasPrefix(msg, path+":4:") {
		t.Fatalf("error = %q, want prefix %q", msg, path+":4:")
	}
	if !strings.Contains(msg, ": statuses[0].default: expected boolean, got string") {
		t.Fatalf("error = %q, want a type-error message", msg)
	}
	line := strings.Split(content, "\n")[3]
	colStr := strings.SplitN(strings.TrimPrefix(msg, path+":4:"), ":", 2)[0]
	start := strings.Index(line, `"yes"`) + 1
	end := start + len(`"yes"`) - 1
	var col int
	for _, r := range colStr {
		col = col*10 + int(r-'0')
	}
	if col < start || col > end {
		t.Fatalf("column %d outside the offending value (columns %d-%d) in %q", col, start, end, msg)
	}
}

func TestConfigStatusHelpers(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		name                          string
		has, closed, actionable, held bool
		kind                          StatusKind
	}{
		{"pending", true, false, true, false, KindOpen},
		{"in_progress", true, false, true, false, KindActive},
		{"blocked", true, false, false, true, KindBlocked},
		{"done", true, true, false, false, KindClosed},
		{"dropped", true, true, false, false, KindClosed},
		{"archived", false, false, false, false, ""},
	}
	for _, tc := range cases {
		if got := cfg.Has(tc.name); got != tc.has {
			t.Errorf("Has(%q) = %v", tc.name, got)
		}
		if got := cfg.IsClosed(tc.name); got != tc.closed {
			t.Errorf("IsClosed(%q) = %v", tc.name, got)
		}
		if got := cfg.IsActionable(tc.name); got != tc.actionable {
			t.Errorf("IsActionable(%q) = %v", tc.name, got)
		}
		if got := cfg.IsBlocked(tc.name); got != tc.held {
			t.Errorf("IsBlocked(%q) = %v", tc.name, got)
		}
		k, ok := cfg.Kind(tc.name)
		if k != tc.kind || ok != tc.has {
			t.Errorf("Kind(%q) = %q, %v; want %q, %v", tc.name, k, ok, tc.kind, tc.has)
		}
	}
	if got := cfg.DefaultStatus(); got != "pending" {
		t.Errorf("DefaultStatus = %q", got)
	}
	if got, want := cfg.Names(), []string{"pending", "in_progress", "blocked", "done", "dropped"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Names = %v, want %v", got, want)
	}
	if got, want := cfg.NamesOfKind(KindClosed), []string{"done", "dropped"}; !reflect.DeepEqual(got, want) {
		t.Errorf("NamesOfKind(closed) = %v, want %v", got, want)
	}
	if got, want := cfg.NamesOfKind(KindActive, KindOpen), []string{"pending", "in_progress"}; !reflect.DeepEqual(got, want) {
		t.Errorf("NamesOfKind(active, open) = %v, want %v (config order)", got, want)
	}
	if got := cfg.NamesOfKind(); len(got) != 0 {
		t.Errorf("NamesOfKind() = %v, want empty", got)
	}
}

func TestDefaultConfigIndependentCopies(t *testing.T) {
	a := DefaultConfig()
	a.Statuses[0].Name = "mutated"
	a.Statuses = append(a.Statuses, StatusDef{Name: "extra", Kind: KindOpen})
	b := DefaultConfig()
	if b.Statuses[0].Name != "pending" || len(b.Statuses) != 5 {
		t.Fatalf("DefaultConfig shares state: %+v", b.Statuses)
	}
	want := []StatusDef{
		{Name: "pending", Kind: KindOpen, Default: true},
		{Name: "in_progress", Kind: KindActive},
		{Name: "blocked", Kind: KindBlocked},
		{Name: "done", Kind: KindClosed},
		{Name: "dropped", Kind: KindClosed},
	}
	if !reflect.DeepEqual(b.Statuses, want) {
		t.Fatalf("DefaultConfig = %+v, want %+v", b.Statuses, want)
	}
}

func TestValidStatusName(t *testing.T) {
	for s, want := range map[string]bool{
		"pending": true, "in_progress": true, "a": true, "r2d2": true,
		"a_1": true, "a1_b2_c3": true,
		"": false, "Pending": false, "1st": false, "_x": false, "to-do": false, "a b": false, "é": false,
		"blocked_": false, "recent__activity": false, "a_": false, "a__b": false, "a_b_": false, "_": false,
	} {
		if got := ValidStatusName(s); got != want {
			t.Errorf("ValidStatusName(%q) = %v, want %v", s, got, want)
		}
	}
}

// validEntries is a minimal valid status list body, for embedding in documents
// that probe key and syntax handling.
const validEntries = `{"name":"a","kind":"open","default":true},{"name":"z","kind":"closed"}`

// wantLoadErrAt loads content and requires an error whose text is exactly
// path:loc: followed by a message containing every one of subs.
func wantLoadErrAt(t *testing.T, content, loc string, subs ...string) {
	t.Helper()
	msg, path := loadErr(t, content)
	if !strings.HasPrefix(msg, path+":"+loc+": ") {
		t.Fatalf("error = %q, want prefix %q", msg, path+":"+loc+": ")
	}
	for _, s := range subs {
		if !strings.Contains(msg, s) {
			t.Fatalf("error = %q, want it to mention %q", msg, s)
		}
	}
}

func TestLoadConfigCommaNotAfterValueRejected(t *testing.T) {
	cases := map[string]struct{ content, loc string }{
		"lone comma in object":  {`{,}`, "1:2"},
		"lone comma in array":   {`{"statuses": [,]}`, "1:15"},
		"double comma in array": {`{"statuses": [` + validEntries + ` ,, ]}`, fmt.Sprintf("1:%d", len(`{"statuses": [`+validEntries+` ,`)+1)},
		"double comma, object":  {`{"statuses": [` + validEntries + `],, }`, fmt.Sprintf("1:%d", len(`{"statuses": [`+validEntries+`],`)+1)},
		"comma after comment":   {"{\"statuses\": [" + validEntries + ", /* c */ , ]}", fmt.Sprintf("1:%d", len(`{"statuses": [`+validEntries+`, /* c */ `)+1)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			wantLoadErrAt(t, tc.content, tc.loc, "invalid character ','")
		})
	}
}

// TestLoadConfigCommaAfterKeyOrTopLevelRejected: a comma after an object key,
// or after the top-level value, is not a trailing comma even when a } or ]
// follows it; it is reported where it stands, as a strict parser would.
func TestLoadConfigCommaAfterKeyOrTopLevelRejected(t *testing.T) {
	cases := map[string]struct{ content, want string }{
		"after key":             {`{"statuses"  ,  }`, `1:14: invalid character ',' after object key`},
		"after key, comment":    {"{\"statuses\" /* c */ , // d\n}", `1:21: invalid character ',' after object key`},
		"after key in entry":    {`{"statuses": [{"name": "a", "kind" , }]}`, `1:36: invalid character ',' after object key`},
		"after top-level value": {`{"statuses":[]},}`, `1:16: unexpected data after the top-level value`},
		"top level, then ]":     {"{} /* c */ ,\n]", `1:12: unexpected data after the top-level value`},
		"after top-level array": {`[],]`, `1:1: document: expected object, got array`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			msg, path := loadErr(t, tc.content)
			if want := path + ":" + tc.want; msg != want {
				t.Fatalf("error = %q, want %q", msg, want)
			}
		})
	}
}

// TestStripJSONCTrailingCommaContext: stripJSONC blanks a comma only when it
// follows a complete value inside an array or object and the next significant
// character closes that container; a comma after a key or after the
// top-level value is kept for checkConfig to report.
func TestStripJSONCTrailingCommaContext(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"nested trailing": {`{"a": {"b": [1, {"c": true,}, "x",], "d": null,}, "e": -2,}`,
			`{"a": {"b": [1, {"c": true }, "x" ], "d": null }, "e": -2 }`},
		"after string value": {`{"a": "k",}`, `{"a": "k" }`},
		"after key":          {`{"a" ,}`, `{"a" ,}`},
		"after nested key":   {`[{"a",}]`, `[{"a",}]`},
		"after top level":    {`{},}`, `{},}`},
		"top-level array":    {`[1,],]`, `[1 ],]`},
		"after colon":        {`{"a":,}`, `{"a":,}`},
		"after bare key":     {`{a,}`, `{a,}`},
		"after decimal":      {`[1.,]`, `[1.,]`},
		"string with braces": {`{"k": "{[", "v": 1,}`, `{"k": "{[", "v": 1 }`},
		"key after comma":    {`{"a": 1, "b",}`, `{"a": 1, "b",}`},
		"array string elem":  {`["a", "b",]`, `["a", "b" ]`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := stripJSONC([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.want {
				t.Fatalf("stripJSONC(%s) = %s, want %s", tc.in, out, tc.want)
			}
		})
	}
}

// TestStripJSONCStrictJSONUnchanged: a comma that follows a value is only a
// trailing-comma candidate until the next significant character; a string,
// {, [ or other value starting after it makes it a separator, so strict JSON
// passes through unchanged, whatever the element types.
func TestStripJSONCStrictJSONUnchanged(t *testing.T) {
	for _, in := range []string{
		`["a","b"]`,
		`{"a":1,"b":2}`,
		`{"a":"x","b":"y"}`,
		`[1,"x"]`,
		`["x",1]`,
		`[true,"x"]`,
		`[{},{}]`,
		`[[],[]]`,
		`[{},[]]`,
		`[[],{}]`,
		`["a",{}]`,
		`[1,[2]]`,
		`{"a":[],"b":{}}`,
		`{"a":{},"b":[1,{"c":[]}]}`,
		`{"statuses":[` + validEntries + `]}`,
	} {
		out, err := stripJSONC([]byte(in))
		if err != nil {
			t.Fatalf("stripJSONC(%s): %v", in, err)
		}
		if string(out) != in {
			t.Errorf("stripJSONC(%s) = %s, want unchanged", in, out)
		}
	}
}

// TestLoadConfigEmptySecondEntryIsRuleError: an empty object as the second
// status entry parses (its separating comma is kept) and fails the name rule
// at its opening brace, not as a syntax error.
func TestLoadConfigEmptySecondEntryIsRuleError(t *testing.T) {
	content := `{"statuses": [{"name": "a", "kind": "open", "default": true}, {}]}`
	msg, path := loadErr(t, content)
	col := strings.Index(content, "{}") + 1
	if want := fmt.Sprintf(`%s:1:%d: statuses[1]: invalid status name "" (must match %s)`, path, col, StatusNamePattern); msg != want {
		t.Fatalf("error = %q, want %q", msg, want)
	}
}

// TestJSONCErrorPositionsInInputAsWritten: every jsoncError formats its own
// line:col from the input as written, including checkConfig's, whose scan
// runs over stripJSONC's output. A leading byte-order mark is not a column
// and a multi-byte character in a comment is one, both of which the stripped
// text would miscount.
func TestJSONCErrorPositionsInInputAsWritten(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"unknown key":      {"\xEF\xBB\xBF{\n  /* é */ \"x\": 1}", `2:11: unknown field "x"`},
		"type error":       {"{\"statuses\": /* é */ 1}", `1:22: statuses: expected array, got number`},
		"after value":      {"\xEF\xBB\xBF{} x", `1:4: unexpected data after the top-level value`},
		"syntax":           {"{\n\n  \"statuses\" 1}", `3:14: invalid character '1' after object key`},
		"unterminated end": {"{\"statuses\": [", `1:15: unexpected end of input`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			data := []byte(tc.in)
			clean, err := stripJSONC(data)
			if err != nil {
				t.Fatal(err)
			}
			_, ce := checkConfig(data, clean)
			if ce == nil {
				t.Fatal("checkConfig accepted the input")
			}
			if got := ce.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
			if _, err := ParseConfig(data); err == nil || err.Error() != tc.want {
				t.Fatalf("ParseConfig error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadConfigDuplicateKeyRejected(t *testing.T) {
	cases := map[string]struct{ content, loc, key string }{
		"top level": {"{\n  \"statuses\": [" + validEntries + "],\n  \"statuses\": [" + validEntries + "]\n}", "3:3", "statuses"},
		"in entry":  {"{\"statuses\": [\n  {\"name\":\"a\",\"kind\":\"open\",\"kind\":\"active\",\"default\":true},{\"name\":\"z\",\"kind\":\"closed\"}]}", "2:29", "kind"},
		"nested":    {`{"statuses": [{"name":{"x":1,"x":2},"kind":"open","default":true}]}`, "1:30", "x"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			wantLoadErrAt(t, tc.content, tc.loc, fmt.Sprintf("duplicate key %q", tc.key))
		})
	}
}

func TestLoadConfigKeyCaseMismatchRejected(t *testing.T) {
	cases := map[string]struct{ content, loc, key, known string }{
		"top level": {`{"Statuses": [` + validEntries + `]}`, "1:2", "Statuses", "statuses"},
		"name":      {"{\"statuses\": [\n  {\"NAME\":\"a\",\"kind\":\"open\",\"default\":true},{\"name\":\"z\",\"kind\":\"closed\"}]}", "2:4", "NAME", "name"},
		"kind":      {`{"statuses": [{"name":"a","Kind":"open","default":true},{"name":"z","kind":"closed"}]}`, "1:27", "Kind", "kind"},
		"default":   {`{"statuses": [{"name":"a","kind":"open","Default":true},{"name":"z","kind":"closed"}]}`, "1:41", "Default", "default"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			wantLoadErrAt(t, tc.content, tc.loc,
				fmt.Sprintf("unknown field %q", tc.key), fmt.Sprintf("did you mean %q", tc.known))
		})
	}
}

func TestLoadConfigUnknownFieldHasPosition(t *testing.T) {
	wantLoadErrAt(t, "{\n  \"statuses\": ["+validEntries+"],\n  \"colour\": \"red\"\n}", "3:3", `unknown field "colour"`)
}

func TestLoadConfigNullRejected(t *testing.T) {
	cases := map[string]struct{ content, loc, sub string }{
		"document":      {"// c\nnull", "2:1", "not null"},
		"statuses":      {`{"statuses": null}`, "1:14", `"statuses" must not be null`},
		"entry":         {`{"statuses": [` + validEntries + `, null]}`, fmt.Sprintf("1:%d", len(`{"statuses": [`+validEntries+`, `)+1), "statuses[2] must not be null"},
		"name":          {`{"statuses": [{"name":null,"kind":"open","default":true},{"name":"z","kind":"closed"}]}`, "1:23", "statuses[0].name must not be null"},
		"kind":          {`{"statuses": [{"name":"a","kind":"open","default":true},{"name":"z","kind":null}]}`, "1:76", "statuses[1].kind must not be null"},
		"default":       {`{"statuses": [{"name":"a","kind":"open","default":null},{"name":"z","kind":"closed"}]}`, "1:51", "statuses[0].default must not be null"},
		"default false": {`{"statuses": [{"name":"a","kind":"open","default":true},{"name":"z","kind":"closed","default":null}]}`, "1:95", "statuses[1].default must not be null"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			wantLoadErrAt(t, tc.content, tc.loc, tc.sub)
		})
	}
}

func TestLoadConfigCommentedEmptyObjectYieldsDefaults(t *testing.T) {
	dir, _ := writeConfig(t, "// only comments around an empty object\n{ /* nothing */ }\n")
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !reflect.DeepEqual(cfg, DefaultConfig()) {
		t.Fatalf("cfg = %+v, want defaults", cfg)
	}
}

// TestLoadConfigColumnsCountRunes puts multi-byte characters before the error
// on its line: the column must count them as one each.
func TestLoadConfigColumnsCountRunes(t *testing.T) {
	// "é" is 2 bytes, "→" 3: a byte column would be 1+3 and 2+6 too far.
	t.Run("syntax error", func(t *testing.T) {
		wantLoadErrAt(t, `{"statuses" /* ééé */ "x"}`, "1:23", "invalid character")
	})
	t.Run("key error", func(t *testing.T) {
		wantLoadErrAt(t, "{\n  /* → → */ \"Statuses\": []}", "2:13", `unknown field "Statuses"`)
	})
	t.Run("bom not counted", func(t *testing.T) {
		wantLoadErrAt(t, "\xEF\xBB\xBF{,}", "1:2", "invalid character ','")
	})
}

func TestConfigValidateRejectsFixedHeadingCollision(t *testing.T) {
	for _, tc := range []struct{ name, kind, heading, fixed string }{
		{"blocked", "open", "Blocked", "Blocked"},
		{"blocked", "active", "Blocked", "Blocked"},
		{"recent_activity", "open", "Recent activity", "Recent activity"},
		{"recent_activity", "active", "Recent activity", "Recent activity"},
	} {
		t.Run(tc.name+"/"+tc.kind, func(t *testing.T) {
			cfg := &Config{Statuses: []StatusDef{
				{Name: "todo", Kind: KindOpen, Default: true},
				{Name: "done", Kind: KindClosed},
				{Name: tc.name, Kind: StatusKind(tc.kind)},
			}}
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted status %q of kind %s", tc.name, tc.kind)
			}
			for _, w := range []string{fmt.Sprintf("status %q", tc.name), fmt.Sprintf("%q", tc.heading), fmt.Sprintf("fixed %q", tc.fixed)} {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %s", err, w)
				}
			}
		})
	}
}

// TestConfigValidateAcceptsSectionlessKindWithFixedHeading: blocked- and
// closed-kind statuses get no briefing section of their own, so a name whose
// heading equals a fixed one is allowed for them.
func TestConfigValidateAcceptsSectionlessKindWithFixedHeading(t *testing.T) {
	for _, kind := range []StatusKind{KindBlocked, KindClosed} {
		for _, name := range []string{"blocked", "recent_activity"} {
			cfg := &Config{Statuses: []StatusDef{
				{Name: "todo", Kind: KindOpen, Default: true},
				{Name: "done", Kind: KindClosed},
				{Name: name, Kind: kind},
			}}
			if err := cfg.Validate(); err != nil {
				t.Errorf("%s-kind %q rejected: %v", kind, name, err)
			}
		}
	}
}

// TestParseConfigFileParsesGivenBytes pins that ParseConfigFile works on the
// bytes it is handed, never the file at path (which here does not exist), and
// formats errors exactly as LoadConfig does.
func TestParseConfigFileParsesGivenBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", ConfigFileName)
	cfg, err := ParseConfigFile(path, []byte(`{"statuses": [`+validEntries+`]}`))
	if err != nil {
		t.Fatalf("ParseConfigFile: %v", err)
	}
	if got := cfg.Names(); !reflect.DeepEqual(got, []string{"a", "z"}) {
		t.Fatalf("names = %v, want [a z]", got)
	}
	for _, empty := range []string{"", "  \n", "// only a comment\n/* and a block */"} {
		cfg, err := ParseConfigFile(path, []byte(empty))
		if err == nil || err.Error() != path+": "+noContentMsg {
			t.Fatalf("ParseConfigFile(%q) = %+v, %v; want the no-content error", empty, cfg, err)
		}
	}
	if cfg, err := ParseConfigFile(path, []byte("{}")); err != nil || !reflect.DeepEqual(cfg, DefaultConfig()) {
		t.Fatalf("ParseConfigFile({}) = %+v, %v; want defaults", cfg, err)
	}
	if _, err := ParseConfigFile(path, []byte("{\n  ,}")); err == nil || !strings.HasPrefix(err.Error(), path+":2:3: ") {
		t.Fatalf("syntax error = %v, want prefix %q", err, path+":2:3: ")
	}
	if _, err := ParseConfigFile(path, []byte(`{"statuses": []}`)); err == nil || !strings.HasPrefix(err.Error(), path+":1:14: statuses: the list is empty") {
		t.Fatalf("validation error = %v, want path:line:col-prefixed", err)
	}
}

// TestLoadConfigTypeErrorsUseJSONTypes: a value of the wrong JSON type is
// reported in JSON terms at its position, never as a Go type.
func TestLoadConfigTypeErrorsUseJSONTypes(t *testing.T) {
	cases := map[string]struct{ content, loc, want string }{
		"array document":  {`[]`, "1:1", "document: expected object, got array"},
		"statuses object": {`{"statuses": {}}`, "1:14", "statuses: expected array, got object"},
		"default string":  {`{"statuses": [{"name":"a","kind":"open","default":"yes"},{"name":"z","kind":"closed"}]}`, "1:51", "statuses[0].default: expected boolean, got string"},
		"name number":     {`{"statuses": [{"name":"a","kind":"open","default":true},{"name":3,"kind":"closed"}]}`, "1:65", "statuses[1].name: expected string, got number"},
		"kind boolean":    {`{"statuses": [{"name":"a","kind":true,"default":true},{"name":"z","kind":"closed"}]}`, "1:34", "statuses[0].kind: expected string, got boolean"},
		"entry array":     {`{"statuses": [[]]}`, "1:15", "statuses[0]: expected object, got array"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			msg, path := loadErr(t, tc.content)
			if want := path + ":" + tc.loc + ": " + tc.want; msg != want {
				t.Fatalf("error = %q, want %q", msg, want)
			}
		})
	}
}

// TestLoadConfigCROnlyLineEndings: a lone \r ends a line comment and counts as
// a line break, as \r\n does (once), for error positions.
func TestLoadConfigCROnlyLineEndings(t *testing.T) {
	t.Run("line comment ends at CR", func(t *testing.T) {
		dir, _ := writeConfig(t, "// hi\r{\"statuses\": ["+validEntries+"]}\r")
		cfg, err := LoadConfig(dir)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if got := cfg.Names(); !reflect.DeepEqual(got, []string{"a", "z"}) {
			t.Fatalf("names = %v, want the configured [a z], not the defaults", got)
		}
	})
	for name, nl := range map[string]string{"CR": "\r", "CRLF": "\r\n", "LF": "\n"} {
		t.Run(name+" line numbers", func(t *testing.T) {
			// The key missing its preceding comma is on line 4, column 3.
			content := "// c" + nl + "{" + nl + "  \"statuses\": [" + validEntries + "]" + nl + "  \"x\": 1}" + nl
			wantLoadErrAt(t, content, "4:3", "invalid character '\"'")
		})
	}
}

// TestLoadConfigTypeErrorPositions: a type error points at the first
// character of the offending value (the opening bracket or quote, the first
// digit or letter), counted from the start of the input, whatever precedes
// the top-level value: comments, whitespace, a byte-order mark.
func TestLoadConfigTypeErrorPositions(t *testing.T) {
	cases := map[string]struct{ content, loc, want string }{
		"header comment":        {"// header\n// comment\n{ \"statuses\": 5}", "3:15", "statuses: expected array, got number"},
		"leading space":         {"   {\"statuses\": 5}", "1:17", "statuses: expected array, got number"},
		"leading block comment": {"/* x */ {\"statuses\": 5}", "1:22", "statuses: expected array, got number"},
		"bom":                   {"\xEF\xBB\xBF{\"statuses\": 5}", "1:14", "statuses: expected array, got number"},
		"bom and space":         {"\xEF\xBB\xBF  {\"statuses\": 5}", "1:16", "statuses: expected array, got number"},
		"cr only with comment":  {"// c\r\r  {\"statuses\": 55}", "3:16", "statuses: expected array, got number"},
		"negative number":       {"\n{\"statuses\": -1.5e3}", "2:14", "statuses: expected array, got number"},
		"object":                {"// c\n  {\"statuses\": {}}", "2:16", "statuses: expected array, got object"},
		"statuses string":       {"\n{\"statuses\": \"a\\\"b\\\\\"}", "2:14", "statuses: expected array, got string"},
		"default string":        {"// c\n{\"statuses\": [\n  {\"name\": \"a\", \"kind\": \"open\", \"default\": \"yes\"}]}", "3:44", "statuses[0].default: expected boolean, got string"},
		"default escaped":       {`{"statuses": [{"name":"a","kind":"open","default":"y\"es\\"}]}`, "1:51", "statuses[0].default: expected boolean, got string"},
		"name number":           {"  {\"statuses\": [{\"name\": 3, \"kind\": \"open\", \"default\": true}]}", "1:26", "statuses[0].name: expected string, got number"},
		"kind boolean":          {"\n {\"statuses\": [{\"name\": \"a\", \"kind\": false}]}", "2:38", "statuses[0].kind: expected string, got boolean"},
		"array document":        {"// c\n  []", "2:3", "document: expected object, got array"},
		"string document":       {"  \"x\"", "1:3", "document: expected object, got string"},
		"number document":       {" 42", "1:2", "document: expected object, got number"},
		"boolean document":      {"\ntrue", "2:1", "document: expected object, got boolean"},
		"emoji document":        {`"😀"`, "1:1", "document: expected object, got string"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			msg, path := loadErr(t, tc.content)
			if want := path + ":" + tc.loc + ": " + tc.want; msg != want {
				t.Fatalf("error = %q, want %q", msg, want)
			}
		})
	}
}

// TestLoadConfigSyntaxErrorInMultiByteRune: the decoder reports an unexpected
// multi-byte character at one of its later bytes; the column is that of the
// character itself.
func TestLoadConfigSyntaxErrorInMultiByteRune(t *testing.T) {
	cases := map[string]struct{ content, loc string }{
		"emoji document":    {"😀", "1:1"},
		"emoji value":       {`{"statuses": 😀}`, "1:14"},
		"accented value":    {`{"statuses": é}`, "1:14"},
		"after multi-byte":  {"// é\n→ {}", "2:1"},
		"second line emoji": {"{\n  \"statuses\": [😀]}", "2:16"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			wantLoadErrAt(t, tc.content, tc.loc, "invalid character")
		})
	}
}

func TestRuneStart(t *testing.T) {
	data := []byte("a😀é\x80b")
	for off, want := range map[int]int{0: 0, 1: 1, 2: 1, 3: 1, 4: 1, 5: 5, 6: 5, 7: 7, 8: 8, 9: 9, 42: 42} {
		if got := runeStart(data, off); got != want {
			t.Errorf("runeStart(%d) = %d, want %d", off, got, want)
		}
	}
}

// TestLoadConfigTypeErrorAtValueStart: a type error points at the first
// character of the offending value (the opening bracket or quote, the first
// digit, sign or letter), never at the whitespace, blanked comment or
// trailing comma after it. Each value here is followed by such filler.
func TestLoadConfigTypeErrorAtValueStart(t *testing.T) {
	cases := []struct{ content, value string }{
		{`{"statuses": 5   }`, `5`},
		{"{\"statuses\": 5 /* c */ }", `5`},
		{"  {\"statuses\": -1.5e3 // c\n}", `-1.5e3`},
		{`{"statuses": "a\"b"  ,}`, `"a\"b"`},
		{"{\"statuses\": true\n\n}", `true`},
		{"{\"statuses\": [{\"name\": 3  , \"kind\": \"open\"}]}", `3`},
		{"{\"statuses\": [{\"name\": \"a\", \"kind\": false /* x */}]}", `false`},
		{"{\"statuses\": [{\"name\": \"a\", \"kind\": \"open\", \"default\": \"yes\"\t\r\n,}]}", `"yes"`},
		{"/* h */ {\"statuses\": [  [1] , ]}", `[1] `},
		{`{"statuses": {  } }`, `{  }`},
		{"  \"x\"   \n", `"x"`},
		{" 42 // c\n", `42`},
		{"true   ", `true`},
		{"[ ]  ", `[ ]`},
	}
	for _, tc := range cases {
		t.Run(tc.content, func(t *testing.T) {
			at := strings.Index(tc.content, tc.value)
			if at < 0 || strings.Count(tc.content, tc.value) != 1 {
				t.Fatalf("value %q is not unique in the case", tc.value)
			}
			line, col := lineCol([]byte(tc.content), at)
			msg, path := loadErr(t, tc.content)
			if want := fmt.Sprintf("%s:%d:%d: ", path, line, col); !strings.HasPrefix(msg, want) {
				t.Fatalf("error = %q, want prefix %q", msg, want)
			}
			if !strings.Contains(msg, ": expected ") {
				t.Fatalf("error = %q, want a type error", msg)
			}
		})
	}
}

// TestLoadConfigSyntaxErrorMessages pins the exact text and position of
// syntax errors, including the inputs on which encoding/json's jsonv2 and
// nojsonv2 engines disagree (a multi-byte character, a bad escape, a control
// character in a string, a malformed number): the config's own scan reports
// them, so both builds give the same message.
func TestLoadConfigSyntaxErrorMessages(t *testing.T) {
	cases := map[string]struct{ content, want string }{
		"key comma":        {`{,}`, `1:2: invalid character ',' looking for beginning of object key string`},
		"value comma":      {`{"statuses": [,]}`, `1:15: invalid character ',' looking for beginning of value`},
		"missing colon":    {`{"statuses" "x"}`, `1:13: invalid character '"' after object key`},
		"missing comma":    {`{"statuses": [] "x": 1}`, `1:17: invalid character '"' after object key:value pair`},
		"array element":    {`{"statuses": [1 2]}`, `1:17: invalid character '2' after array element`},
		"emoji value":      {`{"statuses": 😀}`, `1:14: invalid character '😀' looking for beginning of value`},
		"emoji document":   {`😀`, `1:1: invalid character '😀' looking for beginning of value`},
		"invalid byte":     {"{\"statuses\": \xff}", `1:14: invalid character '\xff' looking for beginning of value`},
		"bad escape":       {`{"statuses": "\x"}`, `1:16: invalid character 'x' in string escape code`},
		"bad unicode":      {`{"statuses": "\u12g4"}`, `1:19: invalid character 'g' in \u hexadecimal character escape`},
		"tab in string":    {"{\"statuses\": \"a\tb\"}", `1:16: invalid character '\t' in string literal`},
		"leading zero":     {`{"statuses": 01}`, `1:15: invalid character '1' after object key:value pair`},
		"bare minus":       {`{"statuses": -}`, `1:15: invalid character '}' in numeric literal`},
		"empty fraction":   {`{"statuses": 1.}`, `1:16: invalid character '}' after decimal point in numeric literal`},
		"empty exponent":   {`{"statuses": 1e+}`, `1:17: invalid character '}' in exponent of numeric literal`},
		"short true":       {`{"statuses": tru}`, `1:17: invalid character '}' in literal true (expecting 'e')`},
		"short null":       {`{"statuses": nul}`, `1:17: invalid character '}' in literal null (expecting 'l')`},
		"mismatched close": {`{"statuses": {]}`, `1:15: invalid character ']' looking for beginning of object key string`},
		"unclosed string":  {`{"statuses": "a`, `1:16: unexpected end of input`},
		"unclosed array":   {"{\"statuses\": [\n", `2:1: unexpected end of input`},
		"trailing data":    {`{} x`, `1:4: unexpected data after the top-level value`},
		"extra close":      {`{}}`, `1:3: unexpected data after the top-level value`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			msg, path := loadErr(t, tc.content)
			if want := path + ":" + tc.want; msg != want {
				t.Fatalf("error = %q, want %q", msg, want)
			}
		})
	}
}

// TestLoadConfigErrorPrecedence: a syntax, key or null error anywhere in the
// document outranks a type error before it, and a type error outranks data
// after the top-level value.
func TestLoadConfigErrorPrecedence(t *testing.T) {
	cases := map[string]struct{ content, want string }{
		"syntax after type": {`{"statuses": [5, 6 7]}`, `1:20: invalid character '7' after array element`},
		"key after type":    {`{"statuses": [{"name": 1, "Kind": "open"}]}`, `1:27: unknown field "Kind" (field names are case-sensitive; did you mean "kind"?)`},
		"null after type":   {`{"statuses": [{"name": 1, "kind": null}]}`, `1:35: statuses[0].kind must not be null`},
		"dup inside type":   {`{"statuses": [{"name": {"x": 1, "x": 2}}]}`, `1:33: duplicate key "x"`},
		"first type wins":   {`{"statuses": [{"name": 1, "kind": 2}]}`, `1:24: statuses[0].name: expected string, got number`},
		"type then trailer": {`[] x`, `1:1: document: expected object, got array`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			msg, path := loadErr(t, tc.content)
			if want := path + ":" + tc.want; msg != want {
				t.Fatalf("error = %q, want %q", msg, want)
			}
		})
	}
}

// FuzzCheckConfig cross-checks the config's own scan against encoding/json:
// a document checkConfig accepts is valid JSON that decodes without error, and
// one it rejects for its syntax is not valid JSON. It also requires that
// ParseConfig never falls back to an unpositioned decoder error, that
// stripJSONC leaves strict JSON unchanged, and that every rule violation in an
// accepted document has a position.
func FuzzCheckConfig(f *testing.F) {
	for _, seed := range []string{
		`{}`, `{"statuses": [` + validEntries + `]}`, `{"statuses": [{"name":"a\u0062","kind":"open","default":true}]}`,
		`{,}`, `{"statuses": 😀}`, `{"statuses": "\x"}`, `{"statuses": 1.}`, `{"statuses": -0.5e-3}`,
		`[[{"a": 1}]]`, `{"statuses": [[]]}`, `{"statuses": null}`, `{} x`, `"\ud800"`, "\"\xff\"", `{"a":1}`,
		`{"statuses"  ,  }`, `{"statuses":[]},}`, `{"a": [1, {"b": 2,},],}`,
		`["a","b"]`, `{"a":1,"b":2}`, `[1,"x"]`, `[{},{}]`, `[[],[]]`, `{"statuses": [` + validEntries + `, {}]}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		clean, err := stripJSONC(data)
		// Strict JSON has no comments and no trailing commas, so stripJSONC
		// must leave it as it is.
		if json.Valid(data) && !bytes.HasPrefix(data, utf8BOM) && (err != nil || !bytes.Equal(clean, data)) {
			t.Fatalf("stripJSONC changed strict JSON %q to %q (err %v)", data, clean, err)
		}
		if err != nil || bytes.IndexFunc(clean, func(r rune) bool { return !isJSONSpace(r) }) < 0 {
			return
		}
		layout, ce := checkConfig(data, clean)
		valid := json.Valid(clean)
		switch {
		case ce == nil:
			if !valid {
				t.Fatalf("checkConfig accepted %q, which encoding/json rejects", clean)
			}
			var raw rawConfig
			if err := json.Unmarshal(clean, &raw); err != nil {
				t.Fatalf("checkConfig accepted %q, which does not decode: %v", clean, err)
			}
			// Every rule violation in an accepted document has a position.
			if raw.Statuses != nil {
				if v := (&Config{Statuses: *raw.Statuses}).check(nil); v != nil && layout.offset(v) < 0 {
					t.Fatalf("rule violation %q in %q has no position", v.msg, clean)
				}
			}
		case strings.HasPrefix(ce.Msg, "invalid character "), strings.HasPrefix(ce.Msg, "unexpected "):
			if valid {
				t.Fatalf("checkConfig reported %q for %q, which encoding/json accepts", ce.Msg, clean)
			}
		}
		if _, err := ParseConfig(data); err != nil && strings.Contains(err.Error(), "decoding the config") {
			t.Fatalf("ParseConfig(%q) fell back to the decoder: %v", data, err)
		}
	})
}

// TestLoadConfigNestingTooDeep: input nested past the decoder's depth limit
// is reported in the config's own words, at the bracket that went too deep.
func TestLoadConfigNestingTooDeep(t *testing.T) {
	// The document's { is level 1, so the reported bracket is the one that
	// opens level 10001.
	cases := map[string]struct {
		content string
		col     int
	}{
		// 13 bytes of prefix, then the 10000th [.
		"arrays": {`{"statuses": ` + strings.Repeat("[", 10001) + strings.Repeat("]", 10001) + `}`, 13 + 10000},
		// 15 bytes of prefix (its brackets at levels 2 and 3), then the 9998th
		// {"a": unit. Objects under an array element are not key-checked, so
		// the depth limit is what rejects them.
		"objects": {`{"statuses": [[` + strings.Repeat(`{"a": `, 10000) + `1` + strings.Repeat("}", 10000) + `]]}`, 15 + 6*9997 + 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			msg, path := loadErr(t, tc.content)
			if want := fmt.Sprintf("%s:1:%d: nesting too deep: arrays and objects may nest at most 10000 levels", path, tc.col); msg != want {
				t.Fatalf("error = %q, want %q", msg, want)
			}
			if strings.Contains(msg, "exceeded max depth") {
				t.Fatalf("raw decoder text leaked: %q", msg)
			}
		})
	}
}

// TestParseConfigFileDefaultsReportsSource pins which parses report the
// defaults: a document without "statuses" does, however it is written; a
// "statuses" list does not, even one that matches the defaults; an error
// never does.
func TestParseConfigFileDefaultsReportsSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), ConfigFileName)
	explicit, err := json.Marshal(map[string]any{"statuses": DefaultConfig().Statuses})
	if err != nil {
		t.Fatal(err)
	}
	for content, wantDefaults := range map[string]bool{
		"{}":                                   true,
		"\xEF\xBB\xBF{}":                       true,
		"// no statuses\n{ /* none */ }\n":     true,
		string(explicit):                       false,
		`{"statuses": [` + validEntries + `]}`: false,
	} {
		cfg, defaults, err := ParseConfigFileDefaults(path, []byte(content))
		if err != nil {
			t.Fatalf("ParseConfigFileDefaults(%q): %v", content, err)
		}
		if defaults != wantDefaults {
			t.Fatalf("ParseConfigFileDefaults(%q) defaults = %v, want %v", content, defaults, wantDefaults)
		}
		want, _ := ParseConfigFile(path, []byte(content))
		if !reflect.DeepEqual(cfg, want) {
			t.Fatalf("ParseConfigFileDefaults(%q) = %+v, want ParseConfigFile's %+v", content, cfg, want)
		}
		if !reflect.DeepEqual(cfg, DefaultConfig()) && (wantDefaults || content == string(explicit)) {
			t.Fatalf("ParseConfigFileDefaults(%q) = %+v, want the default statuses", content, cfg)
		}
	}
	for _, bad := range []string{"", "// only a comment", `{"statuses": []}`, `{"statuses": null}`, "{\n  ,}", "{} x"} {
		cfg, defaults, err := ParseConfigFileDefaults(path, []byte(bad))
		if err == nil || defaults || cfg != nil {
			t.Fatalf("ParseConfigFileDefaults(%q) = %+v, %v, %v; want an error and no defaults", bad, cfg, defaults, err)
		}
		if _, want := ParseConfigFile(path, []byte(bad)); err.Error() != want.Error() {
			t.Fatalf("ParseConfigFileDefaults(%q) error = %q, want ParseConfigFile's %q", bad, err, want)
		}
	}
}

// TestConfigKeyErrorsShowKeyAsWritten pins that unknown, wrong-case and
// duplicate key errors show the key as written in the file, as name errors
// do: a byte that is not valid UTF-8 as \xNN (not the U+FFFD it decodes to),
// an escape as its escape, and a character that does not print as \uNNNN.
// Duplicates are still found on the decoded key.
func TestConfigKeyErrorsShowKeyAsWritten(t *testing.T) {
	cases := map[string]struct{ content, want string }{
		"unknown, invalid UTF-8": {"{\"sta\xfftuses\": []}", `1:2: unknown field "sta\xfftuses"`},
		"unknown, escape":        {`{"\u0001": 1}`, `1:2: unknown field "\u0001"`},
		"unknown, non-printing":  {"{\"a\u200b\": 1}", `1:2: unknown field "a\u200b"`},
		"wrong case, escape": {`{"St\u0061tuses": []}`,
			`1:2: unknown field "St\u0061tuses" (field names are case-sensitive; did you mean "statuses"?)`},
		"wrong case, invalid UTF-8": {"{\"statuses\": [{\"NAME\xff\": \"a\"}]}",
			`1:16: unknown field "NAME\xff"`},
		"duplicate, escape": {`{"statuses": [` + validEntries + `], "st\u0061tuses": []}`,
			fmt.Sprintf(`1:%d: duplicate key "st\u0061tuses"`, len(`{"statuses": [`+validEntries+`], `)+1)},
		// Both keys decode to U+FFFD, so they are the same key.
		"duplicate, invalid UTF-8": {"{\"statuses\": [{\"name\": {\"\xff\": 1, \"\xfe\": 2}}]}",
			`1:33: duplicate key "\xfe"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(tc.content)); err == nil || err.Error() != tc.want {
				t.Fatalf("ParseConfig(%q) error = %v, want %q", tc.content, err, tc.want)
			}
		})
	}
}
