package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/cp1data"
	"jiuxiaoer-admin/backend-go/internal/pkg/logger"
)

type flags struct {
	operation             string
	job                   string
	checks                string
	execute               bool
	confirmation          string
	batchSize             int
	rowsPerSecond         int
	minID                 uint64
	maxID                 uint64
	templateMap           string
	fallbackTemplateID    uint64
	cutoverAt             string
	mappingReason         string
	checkpointFile        string
	resume                bool
	reportFile            string
	verificationAuditFile string
	sampleLimit           int
}

func main() {
	var values flags
	flag.StringVar(&values.operation, "operation", "dq", "dq or backfill")
	flag.StringVar(&values.job, "job", "", "backfill job: print-tasks, print-settings, or verification-history")
	flag.StringVar(&values.checks, "checks", "", "comma-separated DQ IDs; defaults to DQ-001 through DQ-010")
	flag.BoolVar(&values.execute, "execute", false, "apply a backfill; omitted means dry-run")
	flag.StringVar(&values.confirmation, "confirm", "", "required write confirmation phrase")
	flag.IntVar(&values.batchSize, "batch-size", 500, "primary-key batch size (500-2000 for backfills)")
	flag.IntVar(&values.rowsPerSecond, "rows-per-second", 500, "maximum scanned primary rows per second")
	flag.Uint64Var(&values.minID, "min-id", 0, "inclusive minimum primary key")
	flag.Uint64Var(&values.maxID, "max-id", 0, "inclusive maximum primary key; zero means no upper bound")
	flag.StringVar(&values.templateMap, "template-map", "", "legacy-to-published template mapping, for example 12:9001,13:9002")
	flag.Uint64Var(&values.fallbackTemplateID, "fallback-template-id", 0, "explicit published receipt.v1 template used when no exact mapping exists")
	flag.StringVar(&values.cutoverAt, "verification-cutover-at", "", "explicit verification enforce cutover time in RFC3339")
	flag.StringVar(&values.mappingReason, "mapping-reason", "", "auditable reason for historical verification mode mapping")
	flag.StringVar(&values.checkpointFile, "checkpoint-file", "", "0600 JSON checkpoint path; required for writes and resume")
	flag.BoolVar(&values.resume, "resume", false, "resume from an option-bound checkpoint")
	flag.StringVar(&values.reportFile, "report-file", "", "optional 0600 JSON report path")
	flag.StringVar(&values.verificationAuditFile, "verification-audit-file", "", "verification migration audit JSON path")
	flag.IntVar(&values.sampleLimit, "sample-limit", 20, "maximum samples retained per DQ check; backfill manual lists are never truncated")
	flag.Parse()

	cfg := config.Load()
	log := logger.New(cfg.App.Env)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log, cfg, values); err != nil {
		log.Error("cp1 data command failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, cfg config.Config, values flags) error {
	cutover, err := parseOptionalTime(values.cutoverAt)
	if err != nil {
		return err
	}
	dsn := strings.TrimSpace(os.Getenv("JXE_CP1_DATA_DSN"))
	if values.execute && dsn == "" {
		return fmt.Errorf("write mode requires the dedicated JXE_CP1_DATA_DSN; the runtime DSN is never used for backfill writes")
	}
	if dsn == "" {
		dsn = cfg.MySQL.DSN
	}
	if dsn == "" {
		return fmt.Errorf("JXE_CP1_DATA_DSN or JXE_MYSQL_DSN is required")
	}
	cfg.MySQL.DSN = dsn
	cfg.MySQL.Required = true
	if cfg.MySQL.MaxOpenConns > 5 {
		cfg.MySQL.MaxOpenConns = 5
	}
	if cfg.MySQL.MaxIdleConns > cfg.MySQL.MaxOpenConns {
		cfg.MySQL.MaxIdleConns = cfg.MySQL.MaxOpenConns
	}
	db, err := mysql.Open(ctx, cfg.MySQL, log)
	if err != nil {
		return fmt.Errorf("open cp1 data database: %w", err)
	}
	if db == nil {
		return fmt.Errorf("cp1 data database is unavailable")
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	switch values.operation {
	case "dq":
		return runDQ(ctx, db, values, cutover)
	case "backfill":
		return runBackfill(ctx, db, values, cutover)
	default:
		return fmt.Errorf("operation must be dq or backfill")
	}
}

func runDQ(ctx context.Context, db *gorm.DB, values flags, cutover *time.Time) error {
	audit, err := loadVerificationAudit(values.verificationAuditFile)
	if err != nil {
		return err
	}
	checker, err := cp1data.NewChecker(db, cp1data.DQOptions{
		CheckIDs:              csv(values.checks),
		SampleLimit:           values.sampleLimit,
		BatchSize:             values.batchSize,
		VerificationCutoverAt: cutover,
		VerificationAudit:     audit,
	})
	if err != nil {
		return err
	}
	report, err := checker.Run(ctx)
	if err != nil {
		return err
	}
	if err := writeOutput(values.reportFile, report); err != nil {
		return err
	}
	if !report.Passed {
		return fmt.Errorf("one or more DQ checks failed or are blocked")
	}
	return nil
}

func runBackfill(ctx context.Context, db *gorm.DB, values flags, cutover *time.Time) error {
	templateMap, err := parseTemplateMap(values.templateMap)
	if err != nil {
		return err
	}
	if values.job == "verification-history" && strings.TrimSpace(values.verificationAuditFile) == "" {
		return fmt.Errorf("verification-history requires --verification-audit-file so the cutover mapping is independently retained")
	}
	backfiller, err := cp1data.NewBackfiller(db, cp1data.BackfillOptions{
		Job:                       values.job,
		Execute:                   values.execute,
		AllowWrite:                strings.EqualFold(strings.TrimSpace(os.Getenv("JXE_CP1_DATA_ALLOW_WRITE")), "true"),
		Confirmation:              values.confirmation,
		BatchSize:                 values.batchSize,
		RowsPerSecond:             values.rowsPerSecond,
		Range:                     cp1data.IDRange{Min: values.minID, Max: values.maxID},
		TemplateMap:               templateMap,
		FallbackTemplateID:        values.fallbackTemplateID,
		VerificationCutoverAt:     cutover,
		VerificationMappingReason: values.mappingReason,
		SampleLimit:               values.sampleLimit,
		CheckpointFile:            values.checkpointFile,
		Resume:                    values.resume,
		MaxRetries:                5,
	})
	if err != nil {
		return err
	}
	report, err := backfiller.Run(ctx)
	if err != nil {
		if values.job == "verification-history" && cutover != nil {
			audit := cp1data.BuildVerificationAudit(report, *cutover, values.mappingReason)
			_ = cp1data.SaveReport(values.verificationAuditFile, audit)
		}
		_ = writeOutput(values.reportFile, report)
		return err
	}
	if values.job == "verification-history" {
		audit := cp1data.BuildVerificationAudit(report, *cutover, values.mappingReason)
		if err := cp1data.SaveReport(values.verificationAuditFile, audit); err != nil {
			return err
		}
	}
	return writeOutput(values.reportFile, report)
}

// The helpers below use concrete GORM values. Keeping them separate from flag
// parsing makes the command's safety validation unit-testable.

func parseOptionalTime(raw string) (*time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	value, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("verification-cutover-at must use RFC3339: %w", err)
	}
	value = value.UTC()
	return &value, nil
}

func parseTemplateMap(raw string) (map[uint64]uint64, error) {
	result := map[uint64]uint64{}
	if strings.TrimSpace(raw) == "" {
		return result, nil
	}
	for _, pair := range strings.Split(raw, ",") {
		parts := strings.Split(strings.TrimSpace(pair), ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid template mapping %q", pair)
		}
		from, err := strconv.ParseUint(strings.TrimSpace(parts[0]), 10, 64)
		if err != nil || from == 0 {
			return nil, fmt.Errorf("invalid legacy template id in %q", pair)
		}
		to, err := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || to == 0 {
			return nil, fmt.Errorf("invalid target template id in %q", pair)
		}
		if existing := result[from]; existing != 0 && existing != to {
			return nil, fmt.Errorf("legacy template %d has conflicting mappings", from)
		}
		result[from] = to
	}
	return result, nil
}

func csv(raw string) []string {
	var values []string
	for _, value := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			values = append(values, trimmed)
		}
	}
	return values
}

func loadVerificationAudit(path string) (*cp1data.VerificationAudit, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var audit cp1data.VerificationAudit
	if err := json.Unmarshal(raw, &audit); err != nil {
		return nil, fmt.Errorf("decode verification audit: %w", err)
	}
	if audit.SchemaVersion != "cp1.verification-migration-audit.v1" {
		return nil, fmt.Errorf("unsupported verification audit schema %q", audit.SchemaVersion)
	}
	if audit.DryRun || !audit.Completed {
		return nil, fmt.Errorf("verification audit is not an applied, completed migration report")
	}
	return &audit, nil
}

func writeOutput(reportFile string, value any) error {
	if err := cp1data.SaveReport(reportFile, value); err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
