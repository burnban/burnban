package downshift

import (
	"fmt"
	"sync"

	"github.com/burnban/burnban/internal/store"
)

// Runtime refreshes the active durable config while retaining the compiled
// representation between revisions. A malformed or metadata-mismatched active
// record is an error; the proxy fails closed instead of silently disabling an
// operator's requested routing control.
type Runtime struct {
	S *store.Store

	mu       sync.Mutex
	active   *Compiled
	activeID int64
}

func NewRuntime(s *store.Store) *Runtime { return &Runtime{S: s} }

func (r *Runtime) Active() (*Compiled, error) {
	if r == nil || r.S == nil {
		return nil, fmt.Errorf("downshift runtime has no store")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, err := r.S.ActiveDownshiftDocument()
	if err != nil {
		return nil, fmt.Errorf("load active downshift config: %w", err)
	}
	if record == nil {
		r.active, r.activeID = nil, 0
		return nil, nil
	}
	if r.active != nil && r.activeID == record.ID && r.active.Digest == record.Digest {
		return r.active, nil
	}
	compiled, err := Parse([]byte(record.DocumentJSON))
	if err != nil {
		return nil, fmt.Errorf("active downshift config is invalid: %w", err)
	}
	if compiled.Digest != record.Digest || compiled.Config.Revision != record.Revision ||
		string(compiled.Config.Mode) != record.Mode || compiled.Config.APIVersion != record.APIVersion {
		return nil, fmt.Errorf("active downshift config metadata does not match its durable record")
	}
	r.active, r.activeID = compiled, record.ID
	return compiled, nil
}
