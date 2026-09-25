package store

import (
	"fmt"
	"sort"
	"strings"
)

// statusSet is a validated status configuration together with the SQL
// fragments derived from it. It is built whole and never mutated — a store
// changes configuration (SetConfig) by swapping in a new set — so the Go-side
// checks (cfg) and the SQL-side lists always agree.
type statusSet struct {
	cfg *Config
	// closedList is the parenthesised SQL literal list of closed-kind names,
	// e.g. ('done','dropped'), for use after IN / NOT IN.
	closedList string
	// blockedList is the same for blocked-kind names.
	blockedList string
	// configuredList is the same for every configured name.
	configuredList string
}

// newStatusSet validates cfg and builds a statusSet from a private copy of it.
func newStatusSet(cfg *Config) (*statusSet, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	c := cfg.clone()
	closedList, err := sqlStatusList(c.NamesOfKind(KindClosed))
	if err != nil {
		return nil, err
	}
	blockedList, err := sqlStatusList(c.NamesOfKind(KindBlocked))
	if err != nil {
		return nil, err
	}
	configuredList, err := sqlStatusList(c.Names())
	if err != nil {
		return nil, err
	}
	return &statusSet{cfg: c, closedList: closedList, blockedList: blockedList, configuredList: configuredList}, nil
}

// defaultStatusSet backs a Store built without a configuration.
var defaultStatusSet = func() *statusSet {
	st, err := newStatusSet(DefaultConfig())
	if err != nil {
		panic("store: invalid default status config: " + err.Error())
	}
	return st
}()

// statuses returns the store's status set, or the default one for a Store
// constructed without a configuration.
func (s *Store) statuses() *statusSet {
	if s.status == nil {
		return defaultStatusSet
	}
	return s.status
}

// sqlStatusList renders names as a parenthesised list of single-quoted SQL
// string literals, e.g. ('done','dropped'), for embedding after IN or NOT IN.
// Embedding is safe only because every name is checked against
// ValidStatusName, whose alphabet has no quote or other SQL metacharacter; a
// name that fails the check is an error, never embedded. An empty list renders
// as (), which SQLite accepts: x IN () is false and x NOT IN () is true.
func sqlStatusList(names []string) (string, error) {
	quoted := make([]string, len(names))
	for i, n := range names {
		if !ValidStatusName(n) {
			return "", fmt.Errorf("status %q cannot be embedded in SQL: not a valid status name", n)
		}
		quoted[i] = "'" + n + "'"
	}
	return "(" + strings.Join(quoted, ",") + ")", nil
}

// clone returns a deep copy of c.
func (c *Config) clone() *Config {
	return &Config{Statuses: append([]StatusDef(nil), c.Statuses...)}
}

// checkSettable returns an error unless status is a configured status — the
// rule for any status a task is created with or changed to.
func (st *statusSet) checkSettable(status string) error {
	if st.cfg.Has(status) {
		return nil
	}
	return fmt.Errorf("invalid status %q (configured: %s)", status, strings.Join(st.cfg.Names(), ", "))
}

// checkFilter returns an error unless status is a well-formed status name. A
// filter accepts names absent from the config so tasks still holding a status
// the config no longer lists remain queryable.
func checkFilter(status string) error {
	if ValidStatusName(status) {
		return nil
	}
	return fmt.Errorf("invalid status %q (must match %s)", status, StatusNamePattern)
}

// statusHeading renders a status name as a section heading: underscores become
// spaces and the first letter is upper-cased (in_progress → "In progress").
func statusHeading(name string) string {
	h := strings.ReplaceAll(name, "_", " ")
	if h == "" {
		return h
	}
	return strings.ToUpper(h[:1]) + h[1:]
}

// sortedKeys returns the keys of m in ascending order.
func sortedKeys(m map[string][]TaskView) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
