package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/printjob"
)

type wiringPrintProvider struct{}

var wiringProviderSequence atomic.Uint64

func (*wiringPrintProvider) Submit(context.Context, printjob.PrintRequest) (printjob.PrintResult, error) {
	return printjob.PrintResult{Status: "succeeded"}, nil
}

func (*wiringPrintProvider) Query(context.Context, string) (printjob.PrintResult, error) {
	return printjob.PrintResult{Status: "succeeded"}, nil
}

func TestNewServerBuildsOnePrintProviderForHTTPAndWorker(t *testing.T) {
	providerName := fmt.Sprintf("wiring-stub-%d", wiringProviderSequence.Add(1))
	provider := &wiringPrintProvider{}
	var builds atomic.Int64
	if err := printjob.RegisterProvider(providerName, func(context.Context, config.Config) (printjob.Provider, error) {
		builds.Add(1)
		return provider, nil
	}); err != nil {
		t.Fatal(err)
	}

	cfg := localPrintProviderConfig(providerName)
	server, err := NewServer(context.Background(), cfg, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer server.closeInfra()

	if builds.Load() != 1 {
		t.Fatalf("provider factory builds=%d want=1", builds.Load())
	}
	if server.deps.PrintProvider != provider {
		t.Fatal("NewServer did not retain the registered provider instance")
	}
	if got := routerPrintProvider(server.deps); got != provider {
		t.Fatal("HTTP router did not use the Server provider instance")
	}
	worker, err := server.newPrintWorker(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if worker == nil {
		t.Fatal("print worker was not constructed")
	}
	if builds.Load() != 1 {
		t.Fatalf("worker reconstructed provider; factory builds=%d", builds.Load())
	}
}

func TestNewServerRejectsUnknownEnabledPrintProvider(t *testing.T) {
	cfg := localPrintProviderConfig("unknown-api-provider")
	_, err := NewServer(context.Background(), cfg, discardLogger())
	if !errors.Is(err, printjob.ErrProviderNotRegistered) {
		t.Fatalf("startup error=%v want ErrProviderNotRegistered", err)
	}
	if !strings.Contains(err.Error(), "unknown-api-provider") {
		t.Fatalf("startup error does not identify provider: %v", err)
	}
}

func TestPrintMQWorkerRejectsUnknownProviderBeforeInfrastructureStartup(t *testing.T) {
	cfg := localPrintProviderConfig("unknown-worker-provider")
	cfg.MQ.ConsumerPrintEnabled = true
	server := &Server{
		cfg:  cfg,
		log:  discardLogger(),
		deps: Dependencies{Config: cfg},
	}
	err := server.RunMQWorker(context.Background(), "mq-consumer-print")
	if !errors.Is(err, printjob.ErrProviderNotRegistered) {
		t.Fatalf("worker startup error=%v want ErrProviderNotRegistered", err)
	}
}

func TestEnabledRouterNeverSilentlyUsesUnavailablePrintProvider(t *testing.T) {
	cfg := localPrintProviderConfig("unknown-router-provider")
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("enabled router must panic when its configured provider is unavailable")
		}
	}()
	_ = routerPrintProvider(Dependencies{Config: cfg})
}

func localPrintProviderConfig(provider string) config.Config {
	cfg := config.Load()
	cfg.App.Env = "test"
	cfg.MySQL.DSN = ""
	cfg.MySQL.Required = false
	cfg.Redis.Addr = ""
	cfg.Redis.Required = false
	cfg.RabbitMQ.URL = ""
	cfg.RabbitMQ.Required = false
	cfg.Realtime.Enabled = false
	cfg.MapRoute.Enabled = false
	cfg.CustomerLBS.Mode = "off"
	cfg.RiderApplication.Enabled = false
	cfg.WeChat.PayEnabled = false
	cfg.CP1.PrintEnabled = true
	cfg.CP1.PrintProvider = provider
	cfg.MQ.ConsumerPrintEnabled = false
	return cfg
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
