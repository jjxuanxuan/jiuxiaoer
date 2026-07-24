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

// ProviderFactory 根据已校验的进程配置构造服务商。供应商适配器在应用装配时
// 注册其工厂；注册表刻意不包含生产占位适配器。
type ProviderFactory func(context.Context, config.Config) (Provider, error)

// ProviderRegistry 管理进程可用的服务商工厂。其他包注册适配器时仍可读取注册表，
// 这有利于模块化启动以及通过竞争检测的集成测试。
type ProviderRegistry struct {
	mu        sync.RWMutex
	factories map[string]ProviderFactory
}

func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{factories: make(map[string]ProviderFactory)}
}

// Register 添加一个具名服务商工厂。替换现有工厂会被拒绝，
// 防止启动顺序悄然改变所使用的适配器。
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

// Build 构造 cfg 指定的服务商。工厂在注册表锁之外执行，
// 避免缓慢的 SDK 构造器阻塞无关查询。
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

// RegisterProvider 向获批准的适配器包公开进程注册表。
// 注册过程并发安全，并拒绝重复名称。
func RegisterProvider(name string, factory ProviderFactory) error {
	return defaultProviderRegistry.Register(name, factory)
}

// BuildProvider 构造为当前进程配置的服务商。
func BuildProvider(ctx context.Context, cfg config.Config) (Provider, error) {
	return defaultProviderRegistry.Build(ctx, cfg)
}
