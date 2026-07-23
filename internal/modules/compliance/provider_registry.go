package compliance

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"jiuxiaoer-admin/backend-go/internal/config"
)

const FakeProviderCode = "fake"

var (
	ErrProviderNotRegistered = errors.New("identity compliance provider is not registered")
	ErrProviderCodeMismatch  = errors.New("identity compliance provider code does not match configuration")
	providerCodePattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// ProviderFactory constructs one approved identity compliance adapter from
// process configuration. Vendor packages register their factory at the
// application composition boundary after a provider has been selected.
type ProviderFactory func(context.Context, config.Config) (Provider, error)

// ProviderRegistry contains only adapters linked into the current process. It
// deliberately has no placeholder production adapter: an unknown configured
// code is a startup error.
type ProviderRegistry struct {
	mu        sync.RWMutex
	factories map[string]ProviderFactory
}

func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{factories: make(map[string]ProviderFactory)}
}

// Register binds an immutable provider code to a factory.
func (r *ProviderRegistry) Register(code string, factory ProviderFactory) error {
	code = strings.TrimSpace(code)
	if !providerCodePattern.MatchString(code) {
		return fmt.Errorf("invalid identity compliance provider code %q", code)
	}
	if factory == nil {
		return fmt.Errorf("identity compliance provider %q has a nil factory", code)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.factories == nil {
		r.factories = make(map[string]ProviderFactory)
	}
	if _, exists := r.factories[code]; exists {
		return fmt.Errorf("identity compliance provider %q is already registered", code)
	}
	r.factories[code] = factory
	return nil
}

// Build resolves the configured code and verifies that the constructed
// adapter identifies itself with that exact code.
func (r *ProviderRegistry) Build(ctx context.Context, cfg config.Config) (Provider, error) {
	code := strings.TrimSpace(cfg.CP1.IdentityProvider)
	if !providerCodePattern.MatchString(code) {
		return nil, fmt.Errorf("invalid configured identity compliance provider code %q", code)
	}
	if code == FakeProviderCode && productionEnvironment(cfg.App.Env) {
		return nil, fmt.Errorf("identity compliance provider %q is not allowed in production", code)
	}

	r.mu.RLock()
	factory, exists := r.factories[code]
	r.mu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("%w: %q", ErrProviderNotRegistered, code)
	}

	provider, err := factory(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("initialize identity compliance provider %q: %w", code, err)
	}
	if err := ValidateConfiguredProvider(cfg, provider); err != nil {
		return nil, err
	}
	return provider, nil
}

// ValidateConfiguredProvider applies the same code and environment checks to
// an adapter supplied directly through application Dependencies.
func ValidateConfiguredProvider(cfg config.Config, provider Provider) error {
	configured := strings.TrimSpace(cfg.CP1.IdentityProvider)
	if !providerCodePattern.MatchString(configured) {
		return fmt.Errorf("invalid configured identity compliance provider code %q", configured)
	}
	if provider == nil {
		return fmt.Errorf("initialize identity compliance provider %q: factory returned nil", configured)
	}
	actual := strings.TrimSpace(provider.Code())
	if actual != configured {
		return fmt.Errorf("%w: configured=%q adapter=%q", ErrProviderCodeMismatch, configured, actual)
	}
	if actual == FakeProviderCode && productionEnvironment(cfg.App.Env) {
		return fmt.Errorf("identity compliance provider %q is not allowed in production", actual)
	}
	return nil
}

func productionEnvironment(environment string) bool {
	switch strings.ToLower(strings.TrimSpace(environment)) {
	case "prod", "production":
		return true
	default:
		return false
	}
}

var defaultProviderRegistry = func() *ProviderRegistry {
	registry := NewProviderRegistry()
	if err := registry.Register(FakeProviderCode, func(_ context.Context, cfg config.Config) (Provider, error) {
		return NewFakeProvider(cfg.CP1.IdentityCallbackSecret), nil
	}); err != nil {
		panic(err)
	}
	return registry
}()

// RegisterProvider exposes the process registry to a selected vendor adapter.
func RegisterProvider(code string, factory ProviderFactory) error {
	return defaultProviderRegistry.Register(code, factory)
}

// BuildProvider constructs the adapter configured for this process.
func BuildProvider(ctx context.Context, cfg config.Config) (Provider, error) {
	return defaultProviderRegistry.Build(ctx, cfg)
}
