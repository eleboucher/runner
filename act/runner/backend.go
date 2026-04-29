package runner

import (
	"fmt"
	"sync"

	"code.forgejo.org/forgejo/runner/v12/act/common"
)

// ID identifies a backend; matches the label scheme 1:1.
type ID string

type LabelConfiguration struct {
	Backend ID
	Options map[string]any
}

type Backend interface {
	GetID() ID
	ValidateLabelConfiguration(label string, lc *LabelConfiguration) error
	ValidateLabelString(label, str string) error
	CreateExecutionEnvironment(rc *RunContext, lc *LabelConfiguration) common.Executor
}

type Factory interface {
	GetID() ID
	CreateBackend(config map[string]any) (Backend, error)
}

// baseBackend supplies no-op defaults for Validate* so concrete backends
// only declare the methods they actually need.
type baseBackend struct{ id ID }

func (b baseBackend) GetID() ID                                                       { return b.id }
func (baseBackend) ValidateLabelConfiguration(_ string, _ *LabelConfiguration) error  { return nil }
func (baseBackend) ValidateLabelString(_, _ string) error                             { return nil }

var (
	registryMu sync.Mutex
	factories  = map[ID]Factory{}
	instances  = map[ID]Backend{}
)

func RegisterFactory(f Factory) error {
	registryMu.Lock()
	defer registryMu.Unlock()
	id := f.GetID()
	if _, has := factories[id]; has {
		return fmt.Errorf("backend factory %q already registered", id)
	}
	factories[id] = f
	return nil
}

func LookupFactory(id ID) (Factory, error) {
	registryMu.Lock()
	defer registryMu.Unlock()
	f, ok := factories[id]
	if !ok {
		return nil, fmt.Errorf("no backend factory registered for %q", id)
	}
	return f, nil
}

func RegisterBackend(b Backend) {
	registryMu.Lock()
	defer registryMu.Unlock()
	instances[b.GetID()] = b
}

// LookupBackend returns the cached instance, or constructs one from the
// registered factory on first access.
func LookupBackend(id ID) (Backend, error) {
	registryMu.Lock()
	if b, ok := instances[id]; ok {
		registryMu.Unlock()
		return b, nil
	}
	f, ok := factories[id]
	registryMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no backend registered for %q", id)
	}
	b, err := f.CreateBackend(nil)
	if err != nil {
		return nil, fmt.Errorf("create backend %q: %w", id, err)
	}
	RegisterBackend(b)
	return b, nil
}
