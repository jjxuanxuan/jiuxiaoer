package config

import (
	"strings"
	"testing"
	"time"
)

func TestWineTicketReconciliationFlagLoadsFailClosed(t *testing.T) {
	t.Setenv("JXE_WINE_TICKET_RECONCILIATION_ENABLED", "true")
	t.Setenv("JXE_WINE_TICKET_RECONCILIATION_BATCH_SIZE", "321")
	t.Setenv("JXE_WINE_TICKET_RECONCILIATION_BATCH_INTERVAL", "250ms")
	t.Setenv("JXE_WINE_TICKET_RECONCILIATION_SWEEP_INTERVAL", "20m")
	t.Setenv("JXE_WINE_TICKET_RECONCILIATION_DAILY_START", "10m")
	t.Setenv("JXE_WINE_TICKET_RECONCILIATION_LEASE_DURATION", "3m")
	cfg := Load()
	if !cfg.WineTicket.ReconciliationEnabled ||
		cfg.WineTicket.ReconciliationBatchSize != 321 ||
		cfg.WineTicket.ReconciliationBatchInterval != 250*time.Millisecond ||
		cfg.WineTicket.ReconciliationSweepInterval != 20*time.Minute ||
		cfg.WineTicket.ReconciliationDailyStart != 10*time.Minute ||
		cfg.WineTicket.ReconciliationLeaseDuration != 3*time.Minute {
		t.Fatalf("reconciliation settings not loaded: %+v", cfg.WineTicket)
	}
	problems := strings.Join(cfg.wineTicketRuntimeProblems(), "; ")
	if !strings.Contains(problems, "JXE_WINE_TICKET_ENABLED=true") {
		t.Fatalf("reconciliation bypassed master gate: %q", problems)
	}
}

func TestWineTicketReconciliationBoundsAreValidated(t *testing.T) {
	cfg := Load()
	cfg.WineTicket.Enabled = true
	cfg.WineTicket.ReconciliationEnabled = true
	cfg.WineTicket.ReconciliationBatchSize = 2001
	cfg.WineTicket.ReconciliationBatchInterval = 0
	cfg.WineTicket.ReconciliationSweepInterval = 0
	cfg.WineTicket.ReconciliationDailyStart = 6 * time.Hour
	cfg.WineTicket.ReconciliationLeaseDuration = 0
	problems := strings.Join(cfg.wineTicketRuntimeProblems(), "; ")
	for _, expected := range []string{
		"RECONCILIATION_BATCH_SIZE",
		"RECONCILIATION_BATCH_INTERVAL",
		"RECONCILIATION_SWEEP_INTERVAL",
		"RECONCILIATION_DAILY_START",
		"RECONCILIATION_LEASE_DURATION",
	} {
		if !strings.Contains(problems, expected) {
			t.Fatalf("missing %s validation in %q", expected, problems)
		}
	}
}
