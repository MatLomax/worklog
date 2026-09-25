package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ConfigFileName is the optional per-project config file, kept beside the
// database in DirName. Its format is JSON with comments and trailing commas
// (JSONC).
const ConfigFileName = "config.jsonc"

// StatusKind classifies a configured status by how the rest of the system
// treats tasks holding it.
type StatusKind string

const (
	// KindOpen is actionable work not yet started.
	KindOpen StatusKind = "open"
	// KindActive is actionable work underway.
	KindActive StatusKind = "active"
	// KindBlocked is a manual hold: neither actionable nor closed.
	KindBlocked StatusKind = "blocked"
	// KindClosed is terminal: a closed task no longer blocks its dependents.
	KindClosed StatusKind = "closed"
)

// validStatusKind reports whether k is one of the four status kinds.
func validStatusKind(k StatusKind) bool {
	switch k {
	case KindOpen, KindActive, KindBlocked, KindClosed:
		return true
	}
	return false
}

// StatusDef is one configured task status.
type StatusDef struct {
	Name    string     `json:"name"`
	Kind    StatusKind `json:"kind"`
	Default bool       `json:"default"`
}

// Config is a project's worklog configuration. Statuses is the complete,
// ordered list of statuses a task may be set to.
type Config struct {
	Statuses []StatusDef `json:"statuses"`
}

// DefaultConfig returns a fresh copy of the configuration used when a project
// has no config file: pending (default), in_progress, blocked, done, dropped.
func DefaultConfig() *Config {
	return &Config{Statuses: []StatusDef{
		{Name: "pending", Kind: KindOpen, Default: true},
		{Name: "in_progress", Kind: KindActive},
		{Name: "blocked", Kind: KindBlocked},
		{Name: "done", Kind: KindClosed},
		{Name: "dropped", Kind: KindClosed},
	}}
}

// StatusNamePattern is the regular expression every status name must match:
// lowercase words of letters and digits joined by single underscores, the
// first starting with a letter. No leading, trailing or doubled underscore is
// allowed, so each name maps to one distinct heading (see statusHeading).
const StatusNamePattern = `^[a-z][a-z0-9]*(_[a-z0-9]+)*$`

var statusNameRe = regexp.MustCompile(StatusNamePattern)

// ValidStatusName reports whether s is a well-formed status name (see
// StatusNamePattern).
func ValidStatusName(s string) bool { return statusNameRe.MatchString(s) }

// LoadConfig reads ConfigFileName from dir and parses it with
// ParseConfigFile. An absent file yields DefaultConfig; a file that exists but
// holds no JSON value is an error (see ParseConfig). A read error is
// returned as the os package reports it (e.g. "open <path>: permission
// denied"); parse and validation errors are prefixed with the file path, and
// all but the no-content error carry the path:line:col of the offending input
// (see ParseConfig).
func LoadConfig(dir string) (*Config, error) {
	path := filepath.Join(dir, ConfigFileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return DefaultConfig(), nil
	}
	if err != nil {
		return nil, err
	}
	return ParseConfigFile(path, data)
}

// ParseConfigFile is ParseConfig for data read from the file at path: every
// error is prefixed with path (path:line:col: for positioned errors). It lets
// a caller that has already read the file parse exactly those bytes, rather
// than re-reading a file that may have changed since.
func ParseConfigFile(path string, data []byte) (*Config, error) {
	cfg, _, err := parseConfig(data, path)
	return cfg, err
}

// ParseConfigFileDefaults is ParseConfigFile that also reports whether the
// result is the defaults a document without "statuses" (such as {}) stands
// for (defaults true), rather than a list the file gives, even one that
// matches the defaults. defaults is false whenever err is non-nil.
func ParseConfigFileDefaults(path string, data []byte) (cfg *Config, defaults bool, err error) {
	return parseConfig(data, path)
}

// ParseConfig decodes and validates JSONC config data. An object that omits
// "statuses" (such as {}) yields DefaultConfig. A document with no JSON value
// at all (empty, or only whitespace, a byte-order mark and comments) is an
// error, errNoContent: that is what an editor saving in place exposes for a
// moment, so it must never be read as the defaults. Keys are matched exactly
// (case-sensitive); an unknown or duplicate key, or a null document,
// "statuses" list, entry or status field, is an error. Syntax, type and key
// errors are prefixed with line:col, pointing at the offending character (for
// a type error, the first character of the offending value). So are the
// status-list rule violations Validate reports: a rule about one status points
// at its offending field's value (or at the entry's opening brace when that
// field is absent), and a rule about the list as a whole (it is empty, has no
// actionable or no closed status, or no default) at the list's opening
// bracket. Key errors echo the key, and rule errors a status name or kind, as
// written in the file (see sourceText).
func ParseConfig(data []byte) (*Config, error) {
	cfg, _, err := parseConfig(data, "")
	return cfg, err
}

// parseConfig is ParseConfig with src (a file path, or "" for none) prefixed
// to every error; defaults reports that the document omits "statuses", so cfg
// is DefaultConfig.
func parseConfig(data []byte, src string) (cfg *Config, defaults bool, err error) {
	fail := func(err error) error {
		if src == "" {
			return err
		}
		return fmt.Errorf("%s: %w", src, err)
	}
	failAt := func(off int, msg string) error {
		line, col := lineCol(data, off)
		if src == "" {
			return fmt.Errorf("%d:%d: %s", line, col, msg)
		}
		return fmt.Errorf("%s:%d:%d: %s", src, line, col, msg)
	}

	clean, serr := stripJSONC(data)
	if serr != nil {
		var ce *jsoncError
		if errors.As(serr, &ce) {
			return nil, false, failAt(ce.Offset, ce.Msg)
		}
		return nil, false, fail(serr)
	}

	if bytes.IndexFunc(clean, func(r rune) bool { return !isJSONSpace(r) }) < 0 {
		return nil, false, fail(errNoContent)
	}

	layout, ce := checkConfig(data, clean)
	if ce != nil {
		return nil, false, failAt(ce.Offset, ce.Msg)
	}
	var raw rawConfig
	if err := json.Unmarshal(clean, &raw); err != nil {
		// checkConfig accepts only a document that matches rawConfig's shape
		// exactly, so the decoder has nothing left to reject. Should it reject
		// something anyway, its error is passed on without a line:col, since
		// its offsets and field paths depend on which encoding/json engine the
		// binary was built with.
		return nil, false, fail(fmt.Errorf("decoding the config: %w", err))
	}

	if raw.Statuses == nil {
		return DefaultConfig(), true, nil
	}
	cfg = &Config{Statuses: *raw.Statuses}
	if v := cfg.check(func(i int, field string) (string, bool) { return layout.text(clean, i, field) }); v != nil {
		if off := layout.offset(v); off >= 0 {
			return nil, false, failAt(off, v.msg)
		}
		return nil, false, fail(errors.New(v.msg))
	}
	return cfg, false, nil
}

// rawConfig is the config document as decoded, before validation; Statuses
// is nil when the document omits it.
type rawConfig struct {
	Statuses *[]StatusDef `json:"statuses"`
}

// errNoContent reports a config file with no JSON value in it.
var errNoContent = errors.New("config file has no content (it is empty or holds only whitespace, comments or a byte-order mark); write a JSON object: {\"statuses\": [...]}, or {} for the default statuses")

// isJSONSpace reports whether r is JSON insignificant whitespace, which in
// stripJSONC output includes blanked comments and byte-order mark.
func isJSONSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

// maxConfigDepth is how deep arrays and objects may nest in the config (the
// top-level object is level 1); errTooDeep reports input nested deeper.
const (
	maxConfigDepth = 10000
	errTooDeep     = "nesting too deep: arrays and objects may nest at most 10000 levels"
)

// configShape names the position of a value in the config document, which
// decides the JSON type it must have, the keys an object there may carry,
// and whether null is allowed.
type configShape int

const (
	shapeAny      configShape = iota // no rules: inside a value already reported as the wrong type
	shapeDocument                    // the top-level value: an object
	shapeStatuses                    // the "statuses" list: an array
	shapeEntry                       // one element of the "statuses" list: an object
	shapeString                      // a string field of a status entry (name, kind)
	shapeBoolean                     // a boolean field of a status entry (default)
)

// want returns the JSON type a value at shape must have, or "" for any.
func (s configShape) want() string {
	switch s {
	case shapeDocument, shapeEntry:
		return "object"
	case shapeStatuses:
		return "array"
	case shapeString:
		return "string"
	case shapeBoolean:
		return "boolean"
	}
	return ""
}

// configField is one allowed key of an object in the config and the shape of
// its value.
type configField struct {
	key   string
	shape configShape
}

// configFields lists, per object shape, the exact keys allowed.
var configFields = map[configShape][]configField{
	shapeDocument: {{"statuses", shapeStatuses}},
	shapeEntry:    {{"name", shapeString}, {"kind", shapeString}, {"default", shapeBoolean}},
}

// checkConfig checks that clean (stripJSONC's output for data, holding at
// least one non-space byte) is one JSON value of the config's shape, and
// reports the first problem at the offset of the offending input; the error
// carries data, so its Error() gives the line:col in the input as written. Its own scan, not
// encoding/json, finds every positioned error, so positions, field paths and
// messages are the same whichever encoding/json engine the binary is built
// with (the GOEXPERIMENT=jsonv2 and nojsonv2 engines differ in all three).
//
// In order of precedence:
//   - in input order, the first syntax error (at the offending character),
//     unexpected end of input, nesting deeper than maxConfigDepth (at the
//     bracket that opens the level too many), duplicate key in any object,
//     key that is not an exact-case match for a known field, or null where the
//     document, the "statuses" list, a status entry or a status field is
//     expected (encoding/json would match keys case-insensitively, keep the
//     last of duplicate keys and decode null as a zero value);
//   - then the first value of the wrong JSON type, at its first character, as
//     "<field path>: expected <type>, got <type>" (field paths such as
//     statuses[1].default, "document" for the top-level value);
//   - then any data after the top-level value.
//
// A document checkConfig accepts decodes into rawConfig without error, and
// the returned layout records where its "statuses" list, entries and entry
// fields sit, for positioning the rule violations Validate finds after
// decoding.
func checkConfig(data, clean []byte) (*configLayout, *jsoncError) {
	w := &configWalker{src: data, data: clean, layout: configLayout{statuses: -1}}
	if err := w.value(shapeDocument, ""); err != nil {
		return nil, err
	}
	if w.typeErr != nil {
		return nil, w.typeErr
	}
	w.skipSpace()
	if w.pos < len(w.data) {
		return nil, w.errAt(w.pos, "unexpected data after the top-level value")
	}
	return &w.layout, nil
}

// configLayout records where the "statuses" list and its parts sit in the
// config input, as byte offsets.
type configLayout struct {
	statuses int           // the list's opening bracket; -1 when the document has no list
	entries  []entryLayout // one per list element, in order
}

// entryLayout records where one status entry and its fields sit.
type entryLayout struct {
	start  int               // the entry's opening brace
	fields map[string][2]int // per present field (name, kind, default), its value's [start, end)
}

// offset returns where v is reported: the offending field's value, the
// entry's opening brace when that field is absent (or v names no field), or
// the list's opening bracket for a rule about the whole list; -1 when the
// layout does not cover v.
func (l *configLayout) offset(v *ruleViolation) int {
	if v.entry < 0 {
		return l.statuses
	}
	if v.entry >= len(l.entries) {
		return -1
	}
	e := l.entries[v.entry]
	if at, ok := e.fields[v.field]; ok {
		return at[0]
	}
	return e.start
}

// text returns field of entry i as written in data (a string value with its
// quotes and escapes; see sourceText); ok is false when the entry lacks it.
func (l *configLayout) text(data []byte, i int, field string) (string, bool) {
	if i < 0 || i >= len(l.entries) {
		return "", false
	}
	at, ok := l.entries[i].fields[field]
	if !ok {
		return "", false
	}
	return sourceText(data[at[0]:at[1]]), true
}

// sourceText renders raw config text for an error message as written, except
// that a byte that is not valid UTF-8 is shown as \xNN and a character that
// does not print (other than a space) as \uNNNN or \UNNNNNNNN, so neither
// disappears from, nor is silently replaced in, the message.
func sourceText(raw []byte) string {
	var b strings.Builder
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			fmt.Fprintf(&b, `\x%02x`, raw[i])
		case r != ' ' && !unicode.IsPrint(r):
			if r > 0xFFFF {
				fmt.Fprintf(&b, `\U%08x`, r)
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		default:
			b.Write(raw[i : i+size])
		}
		i += size
	}
	return b.String()
}

// configWalker is the scan behind checkConfig. data is the stripped input it
// scans and src the input as written, which errors carry for their line:col;
// pos is the offset of the next unread byte; depth counts the arrays and
// objects open around it.
type configWalker struct {
	src     []byte
	data    []byte
	pos     int
	depth   int
	typeErr *jsoncError // the first value of the wrong type, reported once the scan completes
	layout  configLayout
}

// errAt returns the error msg at offset off of the input.
func (w *configWalker) errAt(off int, msg string) *jsoncError {
	return &jsoncError{Offset: off, Msg: msg, data: w.src}
}

// skipSpace advances past whitespace (which includes blanked comments).
func (w *configWalker) skipSpace() {
	for w.pos < len(w.data) && isJSONSpace(rune(w.data[w.pos])) {
		w.pos++
	}
}

// eof reports input that ends inside a value.
func (w *configWalker) eof() *jsoncError {
	return w.errAt(len(w.data), "unexpected end of input")
}

// badChar reports the unexpected character starting at off; context says
// what the scan was looking at, e.g. "looking for beginning of value".
func (w *configWalker) badChar(off int, context string) *jsoncError {
	var ch string
	if r, size := utf8.DecodeRune(w.data[off:]); r == utf8.RuneError && size <= 1 {
		ch = fmt.Sprintf(`'\x%02x'`, w.data[off])
	} else {
		ch = strconv.QuoteRune(r)
	}
	return w.errAt(off, "invalid character "+ch+" "+context)
}

// value scans one value found at shape; path names it for error messages
// (e.g. statuses[2], statuses[2].kind; "" for the document).
func (w *configWalker) value(shape configShape, path string) *jsoncError {
	w.skipSpace()
	if w.pos >= len(w.data) {
		return w.eof()
	}
	start := w.pos
	var got string
	switch c := w.data[start]; {
	case c == '{':
		got = "object"
	case c == '[':
		got = "array"
	case c == '"':
		got = "string"
	case c == 't' || c == 'f':
		got = "boolean"
	case c == 'n':
		got = "null"
	case c == '-' || ('0' <= c && c <= '9'):
		got = "number"
	default:
		return w.badChar(start, "looking for beginning of value")
	}
	if got == "null" {
		if err := w.literal("null"); err != nil {
			return err
		}
		switch shape {
		case shapeDocument:
			return w.errAt(start, "the config must be an object, not null")
		case shapeStatuses:
			return w.errAt(start, `"statuses" must not be null (omit it to use the default statuses)`)
		case shapeEntry, shapeString, shapeBoolean:
			return w.errAt(start, path+" must not be null")
		}
		return nil
	}
	if want := shape.want(); want != "" && want != got {
		if w.typeErr == nil {
			where := path
			if where == "" {
				where = "document"
			}
			w.typeErr = w.errAt(start, fmt.Sprintf("%s: expected %s, got %s", where, want, got))
		}
		shape = shapeAny
	}
	switch got {
	case "object":
		return w.object(shape, path)
	case "array":
		return w.array(shape, path)
	case "string":
		_, err := w.str()
		return err
	case "boolean":
		if w.data[start] == 't' {
			return w.literal("true")
		}
		return w.literal("false")
	}
	return w.number()
}

// open consumes the { or [ at pos, entering one level deeper.
func (w *configWalker) open() *jsoncError {
	w.depth++
	if w.depth > maxConfigDepth {
		return w.errAt(w.pos, errTooDeep)
	}
	w.pos++
	return nil
}

// close consumes the } or ] at pos, leaving one level.
func (w *configWalker) close() {
	w.pos++
	w.depth--
}

// object scans the object at pos, whose value has shape (shapeDocument,
// shapeEntry, or shapeAny for an object that has no key rules).
func (w *configWalker) object(shape configShape, path string) *jsoncError {
	if err := w.open(); err != nil {
		return err
	}
	fields, checked := configFields[shape]
	seen := map[string]bool{}
	w.skipSpace()
	if w.pos < len(w.data) && w.data[w.pos] == '}' {
		w.close()
		return nil
	}
	for {
		w.skipSpace()
		if w.pos >= len(w.data) {
			return w.eof()
		}
		if w.data[w.pos] != '"' {
			return w.badChar(w.pos, "looking for beginning of object key string")
		}
		start := w.pos
		key, err := w.str()
		if err != nil {
			return err
		}
		// Messages show the key as written (see sourceText), like status
		// names, so a byte that is not valid UTF-8 is not shown as the U+FFFD
		// it decodes to. Keys are compared decoded, as JSON defines them:
		// "a" and "\u0061" are the same key.
		written := sourceText(w.data[start:w.pos])
		if seen[key] {
			return w.errAt(start, "duplicate key "+written)
		}
		seen[key] = true
		child := shapeAny
		if checked {
			found := false
			for _, f := range fields {
				if f.key == key {
					child, found = f.shape, true
					break
				}
			}
			if !found {
				msg := "unknown field " + written
				for _, f := range fields {
					if strings.EqualFold(f.key, key) {
						msg += fmt.Sprintf(" (field names are case-sensitive; did you mean %q?)", f.key)
						break
					}
				}
				return w.errAt(start, msg)
			}
		}
		w.skipSpace()
		if w.pos >= len(w.data) {
			return w.eof()
		}
		if w.data[w.pos] != ':' {
			return w.badChar(w.pos, "after object key")
		}
		w.pos++
		childPath := key
		if path != "" {
			childPath = path + "." + key
		}
		w.skipSpace()
		valStart := w.pos
		if err := w.value(child, childPath); err != nil {
			return err
		}
		if shape == shapeEntry {
			e := &w.layout.entries[len(w.layout.entries)-1]
			if e.fields == nil {
				e.fields = map[string][2]int{}
			}
			e.fields[key] = [2]int{valStart, w.pos}
		}
		w.skipSpace()
		if w.pos >= len(w.data) {
			return w.eof()
		}
		switch w.data[w.pos] {
		case ',':
			w.pos++
		case '}':
			w.close()
			return nil
		default:
			return w.badChar(w.pos, "after object key:value pair")
		}
	}
}

// array scans the array at pos, whose value has shape (shapeStatuses, whose
// elements are status entries, or shapeAny).
func (w *configWalker) array(shape configShape, path string) *jsoncError {
	if shape == shapeStatuses {
		w.layout.statuses = w.pos
	}
	if err := w.open(); err != nil {
		return err
	}
	elem := shapeAny
	if shape == shapeStatuses {
		elem = shapeEntry
	}
	w.skipSpace()
	if w.pos < len(w.data) && w.data[w.pos] == ']' {
		w.close()
		return nil
	}
	for i := 0; ; i++ {
		if shape == shapeStatuses {
			w.skipSpace()
			w.layout.entries = append(w.layout.entries, entryLayout{start: w.pos})
		}
		if err := w.value(elem, fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
		w.skipSpace()
		if w.pos >= len(w.data) {
			return w.eof()
		}
		switch w.data[w.pos] {
		case ',':
			w.pos++
		case ']':
			w.close()
			return nil
		default:
			return w.badChar(w.pos, "after array element")
		}
	}
}

// str scans the string literal at pos and returns its decoded value.
func (w *configWalker) str() (string, *jsoncError) {
	start := w.pos
	for i := start + 1; i < len(w.data); i++ {
		switch c := w.data[i]; {
		case c == '"':
			w.pos = i + 1
			var s string
			if err := json.Unmarshal(w.data[start:w.pos], &s); err != nil {
				// Unreachable: the literal was checked above.
				return "", w.errAt(start, "invalid string literal")
			}
			return s, nil
		case c < 0x20:
			return "", w.badChar(i, "in string literal")
		case c == '\\':
			i++
			if i >= len(w.data) {
				return "", w.eof()
			}
			switch w.data[i] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				for j := 0; j < 4; j++ {
					i++
					if i >= len(w.data) {
						return "", w.eof()
					}
					if !isHexDigit(w.data[i]) {
						return "", w.badChar(i, `in \u hexadecimal character escape`)
					}
				}
			default:
				return "", w.badChar(i, "in string escape code")
			}
		}
	}
	return "", w.eof()
}

// isHexDigit reports whether c is a hexadecimal digit.
func isHexDigit(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

// isDigit reports whether c is a decimal digit.
func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// literal scans the literal word (true, false or null) at pos.
func (w *configWalker) literal(word string) *jsoncError {
	for k := 0; k < len(word); k++ {
		i := w.pos + k
		if i >= len(w.data) {
			return w.eof()
		}
		if w.data[i] != word[k] {
			return w.badChar(i, fmt.Sprintf("in literal %s (expecting %s)", word, strconv.QuoteRune(rune(word[k]))))
		}
	}
	w.pos += len(word)
	return nil
}

// number scans the number at pos: an optional minus, an integer part with no
// leading zero, then an optional fraction and exponent.
func (w *configWalker) number() *jsoncError {
	d, i := w.data, w.pos
	if d[i] == '-' {
		i++
	}
	switch {
	case i >= len(d):
		return w.eof()
	case d[i] == '0':
		i++
	case isDigit(d[i]):
		for i < len(d) && isDigit(d[i]) {
			i++
		}
	default:
		return w.badChar(i, "in numeric literal")
	}
	if i < len(d) && d[i] == '.' {
		i++
		if i >= len(d) {
			return w.eof()
		}
		if !isDigit(d[i]) {
			return w.badChar(i, "after decimal point in numeric literal")
		}
		for i < len(d) && isDigit(d[i]) {
			i++
		}
	}
	if i < len(d) && (d[i] == 'e' || d[i] == 'E') {
		i++
		if i < len(d) && (d[i] == '+' || d[i] == '-') {
			i++
		}
		if i >= len(d) {
			return w.eof()
		}
		if !isDigit(d[i]) {
			return w.badChar(i, "in exponent of numeric literal")
		}
		for i < len(d) && isDigit(d[i]) {
			i++
		}
	}
	w.pos = i
	return nil
}

// Validate checks the status list: at least one status; every name
// well-formed and unique; every kind known; no open- or active-kind status
// (the kinds that get their own WarmContext briefing section) whose heading
// would equal a fixed section heading of the briefing; exactly one default, of
// kind open or active; at least one actionable (open or active) status; and at
// least one closed status. Blocked- and closed-kind statuses get no section of
// their own, so any name is allowed for them. Names and kinds are echoed
// Go-quoted; its errors carry no position (ParseConfig adds one for a config
// read from input).
func (c *Config) Validate() error {
	if v := c.check(nil); v != nil {
		return errors.New(v.msg)
	}
	return nil
}

// ruleViolation is a status-list rule the config breaks, and the part of the
// config it concerns.
type ruleViolation struct {
	msg   string
	entry int    // the offending status's index, or -1 for a rule about the whole list
	field string // the offending field of that status ("name", "kind", "default")
}

// check applies Validate's rules and returns the first violation, or nil.
// source, when non-nil, returns a status field's value as written in the
// input, for echoing in the message; ok false (or a nil source) echoes the
// decoded value Go-quoted instead.
func (c *Config) check(source func(i int, field string) (text string, ok bool)) *ruleViolation {
	echo := func(i int, field, decoded string) string {
		if source != nil {
			if t, ok := source(i, field); ok {
				return t
			}
		}
		return strconv.Quote(decoded)
	}
	list := func(msg string) *ruleViolation { return &ruleViolation{msg: msg, entry: -1} }
	at := func(i int, field, format string, args ...any) *ruleViolation {
		return &ruleViolation{msg: fmt.Sprintf(format, args...), entry: i, field: field}
	}
	if len(c.Statuses) == 0 {
		return list("statuses: the list is empty; at least one status is required")
	}
	seen := make(map[string]bool, len(c.Statuses))
	var defaults []int
	actionable, closedN := 0, 0
	for i, s := range c.Statuses {
		name := echo(i, "name", s.Name)
		if !ValidStatusName(s.Name) {
			return at(i, "name", "statuses[%d]: invalid status name %s (must match %s)", i, name, StatusNamePattern)
		}
		if seen[s.Name] {
			return at(i, "name", "status %s: duplicate name", name)
		}
		seen[s.Name] = true
		if !validStatusKind(s.Kind) {
			return at(i, "kind", "status %s: unknown kind %s (must be open, active, blocked, or closed)", name, echo(i, "kind", string(s.Kind)))
		}
		// Only open- and active-kind statuses get a briefing section of their
		// own (see WarmContext), so only they can collide with a fixed one.
		if s.Kind == KindOpen || s.Kind == KindActive {
			h := statusHeading(s.Name)
			for _, fixed := range fixedSectionHeadings {
				if strings.EqualFold(h, fixed) {
					return at(i, "name", "status %s: its briefing section heading %q collides with the fixed %q section; rename the status", name, h, fixed)
				}
			}
		}
		if s.Default {
			if s.Kind != KindOpen && s.Kind != KindActive {
				return at(i, "default", "status %s: the default status must be of kind open or active, not %s", name, s.Kind)
			}
			defaults = append(defaults, i)
		}
		switch s.Kind {
		case KindOpen, KindActive:
			actionable++
		case KindClosed:
			closedN++
		}
	}
	switch {
	case actionable == 0:
		return list("statuses: no status of kind open or active; at least one is required")
	case closedN == 0:
		return list("statuses: no status of kind closed; at least one is required")
	case len(defaults) == 0:
		return list(`statuses: no status is marked "default": true; exactly one is required`)
	case len(defaults) > 1:
		names := make([]string, len(defaults))
		for k, i := range defaults {
			names[k] = echo(i, "name", c.Statuses[i].Name)
		}
		// Reported at the second default, the first one too many.
		return at(defaults[1], "default", `statuses: [%s] are all marked "default": true; exactly one is allowed`, strings.Join(names, " "))
	}
	return nil
}

// lookup returns the definition of name, or nil when it is not configured.
func (c *Config) lookup(name string) *StatusDef {
	for i := range c.Statuses {
		if c.Statuses[i].Name == name {
			return &c.Statuses[i]
		}
	}
	return nil
}

// Has reports whether name is a configured status.
func (c *Config) Has(name string) bool { return c.lookup(name) != nil }

// Kind returns the kind of name; ok is false when name is not configured.
func (c *Config) Kind(name string) (kind StatusKind, ok bool) {
	if d := c.lookup(name); d != nil {
		return d.Kind, true
	}
	return "", false
}

// DefaultStatus returns the status new tasks receive.
func (c *Config) DefaultStatus() string {
	for _, s := range c.Statuses {
		if s.Default {
			return s.Name
		}
	}
	return ""
}

// IsClosed reports whether name is a configured status of kind closed. A
// status absent from the config is not closed.
func (c *Config) IsClosed(name string) bool {
	k, _ := c.Kind(name)
	return k == KindClosed
}

// IsActionable reports whether name is a configured status of kind open or
// active. A status absent from the config is not actionable.
func (c *Config) IsActionable(name string) bool {
	k, _ := c.Kind(name)
	return k == KindOpen || k == KindActive
}

// IsBlocked reports whether name is a configured status of kind blocked. A
// status absent from the config is not blocked.
func (c *Config) IsBlocked(name string) bool {
	k, _ := c.Kind(name)
	return k == KindBlocked
}

// Names returns every configured status name in config order.
func (c *Config) Names() []string {
	out := make([]string, 0, len(c.Statuses))
	for _, s := range c.Statuses {
		out = append(out, s.Name)
	}
	return out
}

// NamesOfKind returns, in config order, the names of statuses whose kind is
// any of kinds.
func (c *Config) NamesOfKind(kinds ...StatusKind) []string {
	var out []string
	for _, s := range c.Statuses {
		for _, k := range kinds {
			if s.Kind == k {
				out = append(out, s.Name)
				break
			}
		}
	}
	return out
}

// utf8BOM is the byte-order mark some editors write at the start of a file.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// stripJSONC returns a same-length copy of data with JSONC extensions blanked
// out so a strict JSON parser accepts it: // and /* */ comments become spaces
// (newlines inside block comments are kept), and a trailing comma becomes a
// space. A trailing comma is one inside an array or object that directly
// follows a complete value (not an object key) and whose next significant
// character is the ] or } closing it. Any other comma (as in {,}, the second
// comma of ,,, a comma after a key as in {"a",}, or a comma after the
// top-level value) is left for checkConfig to reject there. String literals
// are copied untouched, and a leading UTF-8 byte-order mark is blanked.
// Because every byte keeps its offset, error offsets in the output map straight
// back to the original input.
func stripJSONC(data []byte) ([]byte, error) {
	out := make([]byte, len(data))
	copy(out, data)
	begin := 0
	if bytes.HasPrefix(out, utf8BOM) {
		out[0], out[1], out[2] = ' ', ' ', ' '
		begin = len(utf8BOM)
	}
	var open []byte     // the { and [ enclosing the current position, innermost last
	keyNext := false    // inside an object, where the next string is a key
	afterValue := false // the last significant token completed a value, not a key
	pendingComma := -1  // index of a trailing-comma candidate awaiting its next significant char
	inObject := func() bool { return len(open) > 0 && open[len(open)-1] == '{' }
	for i := begin; i < len(out); i++ {
		c := out[i]
		switch {
		case c == '"':
			pendingComma = -1
			isKey := inObject() && keyNext
			keyNext = false
			afterValue = !isKey
			for i++; i < len(out); i++ {
				if out[i] == '\\' {
					i++
				} else if out[i] == '"' {
					break
				}
			}
		case c == '/' && i+1 < len(out) && out[i+1] == '/':
			// A line comment ends at any line break: \n, \r\n, or a lone \r.
			for ; i < len(out) && out[i] != '\n' && out[i] != '\r'; i++ {
				out[i] = ' '
			}
		case c == '/' && i+1 < len(out) && out[i+1] == '*':
			start := i
			out[i], out[i+1] = ' ', ' '
			closed := false
			for i += 2; i < len(out); i++ {
				if out[i] == '*' && i+1 < len(out) && out[i+1] == '/' {
					out[i], out[i+1] = ' ', ' '
					i++
					closed = true
					break
				}
				if out[i] != '\n' && out[i] != '\r' {
					out[i] = ' '
				}
			}
			if !closed {
				return nil, &jsoncError{Offset: start, Msg: "unterminated block comment", data: data}
			}
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
		case c == ',':
			if afterValue && len(open) > 0 {
				pendingComma = i
			} else {
				pendingComma = -1
			}
			afterValue = false
			keyNext = inObject()
		case c == '{' || c == '[':
			pendingComma = -1
			open = append(open, c)
			keyNext = c == '{'
			afterValue = false
		case c == '}' || c == ']':
			if pendingComma >= 0 {
				out[pendingComma] = ' '
			}
			pendingComma = -1
			if len(open) > 0 {
				open = open[:len(open)-1]
			}
			keyNext = false
			afterValue = true
		default:
			// A digit or letter can end a number or true, false or null;
			// anything else (a colon, a sign, a decimal point, or a stray
			// character checkConfig will reject) does not. A bare word where an
			// object key belongs is no value either.
			pendingComma = -1
			afterValue = endsValue(c) && !(inObject() && keyNext)
			if c == ':' {
				keyNext = false
			}
		}
	}
	return out, nil
}

// endsValue reports whether c can be the last character of a number or of
// the literal true, false or null: a digit or a letter.
func endsValue(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// jsoncError is a config error at a byte offset of the input. data is that
// input as written (before stripJSONC), from which Error() computes the
// line:col; every jsoncError is made with it.
type jsoncError struct {
	Offset int
	Msg    string
	data   []byte
}

// Error returns the error as line:col: msg, positioned in data.
func (e *jsoncError) Error() string {
	line, col := lineCol(e.data, e.Offset)
	return fmt.Sprintf("%d:%d: %s", line, col, e.Msg)
}

// lineCol converts a byte offset in data to a 1-based line and column. A line
// break is \n, \r\n (one break), or a lone \r. The column is counted in
// Unicode code points, not bytes (a byte that is not valid UTF-8 counts as
// one); a leading byte-order mark is not counted. An offset past the end is clamped to the end, and one inside a
// multi-byte character is moved back to that character's first byte.
func lineCol(data []byte, off int) (line, col int) {
	if off > len(data) {
		off = len(data)
	}
	if off < 0 {
		off = 0
	}
	off = runeStart(data, off)
	line, lineStart := 1, 0
	if bytes.HasPrefix(data, utf8BOM) && off >= len(utf8BOM) {
		lineStart = len(utf8BOM)
	}
	for i := 0; i < off; i++ {
		// \n breaks; \r breaks unless a \n follows, which then breaks for it.
		if data[i] == '\n' || (data[i] == '\r' && (i+1 == len(data) || data[i+1] != '\n')) {
			line++
			lineStart = i + 1
		}
	}
	col = 1 + utf8.RuneCount(data[lineStart:off])
	return line, col
}

// runeStart returns the offset of the first byte of the UTF-8 character that
// contains data[off], or off itself when off is a character's first byte, is
// the end of data, or lies in no well-formed character.
func runeStart(data []byte, off int) int {
	if off >= len(data) || utf8.RuneStart(data[off]) {
		return off
	}
	for i := off - 1; i >= 0 && i > off-utf8.UTFMax; i-- {
		if !utf8.RuneStart(data[i]) {
			continue
		}
		if _, size := utf8.DecodeRune(data[i:]); size > 1 && i+size > off {
			return i
		}
		break
	}
	return off
}
