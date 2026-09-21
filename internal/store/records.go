package store

import (
	"fmt"
	"sort"
	"strings"
)

// recordColumns maps a record kind to its editable markdown fields
// (external field name -> physical column). Values are literals, so building
// SQL from them is injection-safe; user input only ever selects a key.
var recordColumns = map[string]map[string]string{
	"decision": {"decision": "decision_md", "rationale": "rationale_md"},
	"journal":  {"text": "text_md"},
}

// recordTables maps a record kind to its backing table.
var recordTables = map[string]string{
	"decision": "decision",
	"journal":  "journal",
}

// EditRecord replaces one markdown field of an existing decision or journal
// row, in place. It is deliberately silent — no journal entry is appended and
// the row's ts is untouched — because an in-place correction of a formatting
// slip is not itself an event worth logging.
func (s *Store) EditRecord(kind string, id int64, field, content string) error {
	fields, ok := recordColumns[kind]
	if !ok {
		return fmt.Errorf("unknown record kind %q", kind)
	}
	col, ok := fields[field]
	if !ok {
		valid := make([]string, 0, len(fields))
		for f := range fields {
			valid = append(valid, f)
		}
		sort.Strings(valid)
		return fmt.Errorf("unknown field %q for record kind %q; valid fields: %s", field, kind, strings.Join(valid, ", "))
	}
	table := recordTables[kind]
	res, err := s.db.Exec(
		fmt.Sprintf(`UPDATE %s SET %s = ? WHERE id = ?`, table, col),
		content, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	// A decision's journal mirror carries only the decision text, not the
	// rationale, so only editing the decision field needs to cascade.
	if kind == "decision" && field == "decision" {
		if _, err := s.db.Exec(`UPDATE journal SET text_md = ? WHERE decision_id = ?`, content, id); err != nil {
			return err
		}
	}
	return nil
}

// DeleteRecord removes a decision or journal row by id. Like EditRecord it is
// silent — no journal entry is appended for the deletion.
func (s *Store) DeleteRecord(kind string, id int64) error {
	table, ok := recordTables[kind]
	if !ok {
		return fmt.Errorf("unknown record kind %q", kind)
	}
	// Delete the linked journal mirror explicitly, before the decision row, so
	// the mirror is always removed regardless of whether FK cascade is enabled
	// on this connection.
	if kind == "decision" {
		if _, err := s.db.Exec(`DELETE FROM journal WHERE decision_id = ?`, id); err != nil {
			return err
		}
	}
	res, err := s.db.Exec(fmt.Sprintf(`DELETE FROM %s WHERE id = ?`, table), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
