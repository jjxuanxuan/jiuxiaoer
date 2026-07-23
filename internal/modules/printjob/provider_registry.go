package printjob

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"jiuxiaoer-admin/backend-go/internal/config"
)

const FakeProviderName = "fake"

var (
	ErrProviderNotRegistered = errors.New("print provider is not registered")
	providerNamePattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)
)

// ProviderFactory constructs a provider from the validated process
// configuration. Vendor adapters register their factory during application
// composition; the registry deliberately does not contain a placeholder
// production adapter.
type ProviderFactory func(context.Context, config.Config) (Provider, error)

// ProviderRegistry owns the provider factories available to a process. A
// registry may be read while another package registers an adapter, which is
// useful for modular startup and race-tested integration tests.
type ProviderRegistry struct {
	mu        sync.RWMutex
	factories map[string]ProviderFactory
}

func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{factories: make(map[string]ProviderFactory)}
}

// Register adds one named provider factory. Replacing an existing factory is
// rejected so startup order cannot silently change which adapter is used.
func (r *ProviderRegistry) Register(name string, factory ProviderFactory) error {
	name = strings.TrimSpace(name)
	if !providerNamePattern.MatchString(name) {
		return fmt.Errorf("invalid print provider name %q", name)
	}
	if factory == nil {
		return fmt.Errorf("print provider %q has a nil factory", name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.factories == nil {
		r.factories = make(map[string]ProviderFactory)
	}
	if _, exists := r.factories[name]; exists {
		return fmt.Errorf("print provider %q is already registered", name)
	}
	r.factories[name] = factory
	return nil
}

// Build constructs the provider named by cfg. Factories execute outside the
// registry lock so a slow SDK constructor cannot block unrelated lookups.
func (r *ProviderRegistry) Build(ctx context.Context, cfg config.Config) (Provider, error) {
	name := strings.TrimSpace(cfg.CP1.PrintProvider)
	if !providerNamePattern.MatchString(name) {
		return nil, fmt.Errorf("invalid configured print provider name %q", name)
	}
	if name == FakeProviderName && isProductionEnvironment(cfg.App.Env) {
		return nil, fmt.Errorf("print provider %q is not allowed in production", name)
	}

	r.mu.RLock()
	factory, exists := r.factories[name]
	r.mu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("%w: %q", ErrProviderNotRegistered, name)
	}

	provider, err := factory(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("initialize print provider %q: %w", name, err)
	}
	if provider == nil {
		return nil, fmt.Errorf("initialize print provider %q: factory returned nil", name)
	}
	return provider, nil
}

func isProductionEnvironment(environment string) bool {
	switch strings.ToLower(strings.TrimSpace(environment)) {
	case "prod", "production":
		return true
	default:
		return false
	}
}

var defaultProviderRegistry = func() *ProviderRegistry {
	registry := NewProviderRegistry()
	if err := registry.Register(FakeProviderName, func(context.Context, config.Config) (Provider, error) {
		return &FakeProvider{}, nil
	}); err != nil {
		panic(err)
	}
	return registry
}()

// RegisterProvider exposes the process registry to approved adapter packages.
// Registration is concurrency-safe and duplicate names are rejected.
func RegisterProvider(name string, factory ProviderFactory) error {
	return defaultProviderRegistry.Register(name, factory)
}

// BuildProvider constructs the provider configured for this process.
func BuildProvider(ctx context.Context, cfg config.Config) (Provider, error) {
	return defaultProviderRegistry.Build(ctx, cfg)
}
