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

// ProviderFactory 根据进程配置构造一个已获批准的身份合规适配器。
// 选定服务商后，供应商包在应用装配边界注册其工厂。
type ProviderFactory func(context.Context, config.Config) (Provider, error)

// ProviderRegistry 只包含链接到当前进程的适配器。它刻意不提供生产占位适配器：
// 配置未知代码属于启动错误。
type ProviderRegistry struct {
	mu        sync.RWMutex
	factories map[string]ProviderFactory
}

func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{factories: make(map[string]ProviderFactory)}
}

// Register 将不可变的服务商代码绑定到工厂。
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

// Build 解析配置的代码，并验证构造出的适配器使用完全相同的代码标识自身。
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

// ValidateConfiguredProvider 对通过应用 Dependencies 直接提供的适配器，
// 执行相同的代码和环境检查。
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

// RegisterProvider 向选定的供应商适配器公开进程注册表。
func RegisterProvider(code string, factory ProviderFactory) error {
	return defaultProviderRegistry.Register(code, factory)
}

// BuildProvider 构造为当前进程配置的适配器。
func BuildProvider(ctx context.Context, cfg config.Config) (Provider, error) {
	return defaultProviderRegistry.Build(ctx, cfg)
}
