package app

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestWineTicketMaintenanceWorkerDoesNotRequireRabbitMQ(t *testing.T) {
	db, err := gorm.Open(
		sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Load()
	cfg.WineTicket.Enabled = true
	cfg.WineTicket.MaintenanceOwner = config.WineTicketMaintenanceOwnerWorker
	cfg.WineTicket.ReminderEnabled = false
	cfg.WineTicket.WeChatReminderEnabled = false
	cfg.WineTicket.ReconciliationEnabled = false
	cfg.Order.ExpiryWorkerEnabled = false
	cfg.AfterSale.RefundExecutionEnabled = false

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	server := &Server{
		cfg: cfg,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		deps: Dependencies{
			Config: cfg,
			DB:     db,
			IDGen:  snowflake.New(217),
		},
	}
	if err := server.RunMQWorker(ctx, "wine-ticket-maintenance"); err != nil {
		t.Fatalf("standalone wine-ticket worker required unrelated MQ: %v", err)
	}
}

func TestWineTicketMaintenanceStartsOrderExpiryWithoutPaymentProvider(
	t *testing.T,
) {
	db, err := gorm.Open(
		sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Load()
	cfg.Order.ExpiryWorkerEnabled = true
	idGen := snowflake.New(218)
	server := &Server{
		cfg: cfg,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		deps: Dependencies{
			Config:  cfg,
			DB:      db,
			IDGen:   idGen,
			Metrics: metrics.New("wine-ticket-worker-test", ""),
		},
	}
	module := wineticket.NewModule(
		db,
		idGen,
		wineticket.ModuleOptions{},
	)
	spawned := 0

	server.startWineTicketOrderExpiryWorker(
		context.Background(),
		module,
		func(run func()) {
			spawned++
			if run == nil {
				t.Fatal("spawned order expiry run function is nil")
			}
		},
	)

	if spawned != 1 {
		t.Fatalf("spawned=%d, want 1", spawned)
	}
}

func TestWineTicketMaintenanceOwnershipMatrix(t *testing.T) {
	tests := []struct {
		name       string
		enabled    bool
		owner      string
		apiShared  bool
		apiWine    bool
		workerWine bool
	}{
		{
			name:  "disabled feature preserves retail workers in API",
			owner: config.WineTicketMaintenanceOwnerAPI, apiShared: true,
		},
		{
			name:  "preconfigured worker owner cannot move retail while feature is disabled",
			owner: config.WineTicketMaintenanceOwnerWorker, apiShared: true,
		},
		{
			name:    "API owner preserves existing combined workers",
			enabled: true, owner: config.WineTicketMaintenanceOwnerAPI,
			apiShared: true, apiWine: true,
		},
		{
			name:    "worker owner atomically moves combined workers",
			enabled: true, owner: config.WineTicketMaintenanceOwnerWorker,
			workerWine: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Load()
			cfg.WineTicket.Enabled = test.enabled
			cfg.WineTicket.MaintenanceOwner = test.owner
			if got := apiOwnsSharedMaintenance(cfg); got != test.apiShared {
				t.Fatalf("api shared maintenance=%v, want %v", got, test.apiShared)
			}
			if got := apiOwnsWineTicketMaintenance(cfg); got != test.apiWine {
				t.Fatalf("api wine-ticket maintenance=%v, want %v", got, test.apiWine)
			}
			if got := workerOwnsWineTicketMaintenance(cfg); got != test.workerWine {
				t.Fatalf("worker wine-ticket maintenance=%v, want %v", got, test.workerWine)
			}
		})
	}
}

func TestWineTicketMaintenanceWorkerRejectsWrongOwner(t *testing.T) {
	cfg := config.Load()
	cfg.WineTicket.Enabled = true
	cfg.WineTicket.MaintenanceOwner = config.WineTicketMaintenanceOwnerAPI
	server := &Server{cfg: cfg}

	err := server.RunMQWorker(context.Background(), "wine-ticket-maintenance")
	if err == nil || !strings.Contains(err.Error(), "JXE_WINE_TICKET_MAINTENANCE_OWNER=worker") {
		t.Fatalf("expected clear owner error, got %v", err)
	}
}

func TestWineTicketMaintenanceWorkerRejectsDisabledMaster(t *testing.T) {
	cfg := config.Load()
	cfg.WineTicket.Enabled = false
	cfg.WineTicket.MaintenanceOwner = config.WineTicketMaintenanceOwnerWorker
	server := &Server{cfg: cfg}

	err := server.RunMQWorker(context.Background(), "wine-ticket-maintenance")
	if err == nil || !strings.Contains(err.Error(), "JXE_WINE_TICKET_ENABLED=true") {
		t.Fatalf("expected clear master-switch error, got %v", err)
	}
}
