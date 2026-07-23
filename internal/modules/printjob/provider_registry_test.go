package printjob

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"jiuxiaoer-admin/backend-go/internal/config"
)

type registryTestProvider struct{}

func (*registryTestProvider) Submit(context.Context, PrintRequest) (PrintResult, error) {
	return PrintResult{Status: "succeeded"}, nil
}

func (*registryTestProvider) Query(context.Context, string) (PrintResult, error) {
	return PrintResult{Status: "succeeded"}, nil
}

func TestProviderRegistryBuildsRegisteredProviderConcurrently(t *testing.T) {
	registry := NewProviderRegistry()
	provider := &registryTestProvider{}
	var builds atomic.Int64
	if err := registry.Register("contract-stub", func(context.Context, config.Config) (Provider, error) {
		builds.Add(1)
		return provider, nil
	}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{CP1: config.CP1Config{PrintProvider: "contract-stub"}}
	const goroutines = 32
	var waitGroup sync.WaitGroup
	errorsFound := make(chan error, goroutines*2)
	for index := range goroutines {
		waitGroup.Add(2)
		go func() {
			defer waitGroup.Done()
			built, err := registry.Build(context.Background(), cfg)
			if err != nil {
				errorsFound <- err
				return
			}
			if built != provider {
				errorsFound <- errors.New("registry returned a different provider")
			}
		}()
		go func() {
			defer waitGroup.Done()
			name := fmt.Sprintf("side-provider-%d", index)
			if err := registry.Register(name, func(context.Context, config.Config) (Provider, error) {
				return &registryTestProvider{}, nil
			}); err != nil {
				errorsFound <- err
			}
		}()
	}
	waitGroup.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	if builds.Load() != goroutines {
		t.Fatalf("factory builds=%d want=%d", builds.Load(), goroutines)
	}
}

func TestProviderRegistryRejectsUnknownDuplicateAndNilProviders(t *testing.T) {
	registry := NewProviderRegistry()
	cfg := config.Config{CP1: config.CP1Config{PrintProvider: "missing-provider"}}
	if _, err := registry.Build(context.Background(), cfg); !errors.Is(err, ErrProviderNotRegistered) {
		t.Fatalf("unknown provider error=%v", err)
	}
	if err := registry.Register("contract-stub", func(context.Context, config.Config) (Provider, error) {
		return &registryTestProvider{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("contract-stub", func(context.Context, config.Config) (Provider, error) {
		return &registryTestProvider{}, nil
	}); err == nil {
		t.Fatal("duplicate provider registration must fail")
	}
	if err := registry.Register("nil-factory", nil); err == nil {
		t.Fatal("nil factory registration must fail")
	}
	if err := registry.Register("nil-provider", func(context.Context, config.Config) (Provider, error) {
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg.CP1.PrintProvider = "nil-provider"
	if _, err := registry.Build(context.Background(), cfg); err == nil {
		t.Fatal("nil provider result must fail")
	}
}

func TestProviderRegistryRejectsFakeInProduction(t *testing.T) {
	registry := NewProviderRegistry()
	if err := registry.Register(FakeProviderName, func(context.Context, config.Config) (Provider, error) {
		return &FakeProvider{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		App: config.AppConfig{Env: "production"},
		CP1: config.CP1Config{PrintProvider: FakeProviderName},
	}
	if _, err := registry.Build(context.Background(), cfg); err == nil {
		t.Fatal("fake print provider must not build in production")
	}
}
