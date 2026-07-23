package app

import (
	"context"
	"fmt"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/printjob"
)

func printProviderRequired(cfg config.Config) bool {
	return cfg.CP1.PrintEnabled || cfg.MQ.ConsumerPrintEnabled
}

// buildConfiguredPrintProvider is the single API/worker construction path.
// The resulting instance is stored in Dependencies and reused by both the
// HTTP print service and every print worker owned by the Server.
func buildConfiguredPrintProvider(ctx context.Context, cfg config.Config) (printjob.Provider, error) {
	if !printProviderRequired(cfg) {
		return nil, nil
	}
	if !cfg.CP1.PrintEnabled {
		return nil, fmt.Errorf("print consumer requires JXE_CP1_PRINT_ENABLED=true")
	}
	provider, err := printjob.BuildProvider(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("configure print provider %q: %w", cfg.CP1.PrintProvider, err)
	}
	return provider, nil
}

func (s *Server) ensurePrintProvider(ctx context.Context) error {
	if !printProviderRequired(s.cfg) {
		return nil
	}
	s.printProviderMu.Lock()
	defer s.printProviderMu.Unlock()
	if s.deps.PrintProvider != nil {
		return nil
	}
	provider, err := buildConfiguredPrintProvider(ctx, s.cfg)
	if err != nil {
		return err
	}
	s.deps.PrintProvider = provider
	return nil
}

func (s *Server) newPrintWorker(ctx context.Context) (*printjob.Worker, error) {
	if err := s.ensurePrintProvider(ctx); err != nil {
		return nil, err
	}
	if s.deps.PrintProvider == nil {
		return nil, fmt.Errorf("print provider is not configured")
	}
	return printjob.NewWorker(s.cfg.CP1, s.deps.DB, s.deps.IDGen, s.deps.PrintProvider, s.cfg.App.InstanceID, s.log), nil
}

// routerPrintProvider keeps direct test/router construction deterministic.
// Production uses NewServer and therefore always supplies the already-built
// shared instance. An enabled but unresolved provider is a programmer/startup
// error, never a silent UnavailableProvider fallback.
func routerPrintProvider(deps Dependencies) printjob.Provider {
	if deps.PrintProvider != nil {
		return deps.PrintProvider
	}
	if !printProviderRequired(deps.Config) {
		return &printjob.UnavailableProvider{}
	}
	provider, err := buildConfiguredPrintProvider(context.Background(), deps.Config)
	if err != nil {
		panic(err)
	}
	return provider
}
