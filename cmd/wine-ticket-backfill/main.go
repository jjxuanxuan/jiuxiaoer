package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/cp1data"
	"jiuxiaoer-admin/backend-go/internal/pkg/logger"
)

func main() {
	var (
		job            = flag.String("job", "", "wine-ticket-payments, wine-ticket-refunds, or wine-ticket-returns")
		execute        = flag.Bool("execute", false, "apply updates; the default is a read-only dry run")
		confirmation   = flag.String("confirm", "", "required confirmation phrase in execute mode")
		checkpointFile = flag.String("checkpoint", "", "private checkpoint JSON path")
		reportFile     = flag.String("report", "", "optional private report JSON path")
		resume         = flag.Bool("resume", false, "resume from the supplied checkpoint")
		batchSize      = flag.Int("batch-size", 500, "rows per batch (500-2000)")
		rowsPerSecond  = flag.Int("rows-per-second", 500, "maximum row scan/update rate")
		minID          = flag.Uint64("min-id", 0, "inclusive lower ID bound")
		maxID          = flag.Uint64("max-id", 0, "inclusive upper ID bound; zero means maximum")
	)
	flag.Parse()

	cfg := config.Load()
	cfg.MySQL.Required = true
	cfg.MySQL.RequireWineTicketSchema = true
	log := logger.New(cfg.App.Env)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := mysql.Open(ctx, cfg.MySQL, log)
	if err != nil {
		exit(log, "connect to MySQL", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		exit(log, "open SQL database handle", err)
	}
	defer sqlDB.Close()

	options := cp1data.BackfillOptions{
		Job:            strings.TrimSpace(*job),
		Execute:        *execute,
		AllowWrite:     strings.EqualFold(strings.TrimSpace(os.Getenv("JXE_WINE_TICKET_BACKFILL_ALLOW_WRITE")), "true"),
		Confirmation:   strings.TrimSpace(*confirmation),
		BatchSize:      *batchSize,
		RowsPerSecond:  *rowsPerSecond,
		Range:          cp1data.IDRange{Min: *minID, Max: *maxID},
		SampleLimit:    100,
		CheckpointFile: strings.TrimSpace(*checkpointFile),
		Resume:         *resume,
		MaxRetries:     5,
	}
	runner, err := cp1data.NewBackfiller(db, options)
	if err != nil {
		exit(log, "validate backfill options", err)
	}
	report, err := runner.Run(ctx)
	if err != nil {
		exit(log, "run backfill", err)
	}
	if strings.TrimSpace(*reportFile) != "" {
		if err := cp1data.SaveReport(*reportFile, report); err != nil {
			exit(log, "save report", err)
		}
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		exit(log, "encode report", err)
	}
	fmt.Println(string(raw))
}

func exit(log *slog.Logger, action string, err error) {
	log.Error(action+" failed", slog.Any("error", err))
	os.Exit(1)
}
