package store

import "fmt"

// SetConfig replaces the store's status configuration, so a long-running
// process (the MCP server) can follow edits to ConfigFileName without
// reopening the database. cfg is validated and copied exactly as
// OpenWithConfig does; nil means DefaultConfig. On a validation error the
// store keeps its current configuration.
//
// SetConfig is not safe to call concurrently with any other Store method: the
// status set is read without synchronisation by every query and write. The MCP
// server calls it from its single request loop, between tool calls.
func (s *Store) SetConfig(cfg *Config) error {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	st, err := newStatusSet(cfg)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	s.status = st
	return nil
}
