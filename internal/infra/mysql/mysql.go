package mysql

import (
	"context"
	"fmt"
	stdlog "log"
	"log/slog"
	"os"
	"time"

	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/auditlog"
)

// Open 解密并返回数据库。
func Open(ctx context.Context, cfg config.MySQLConfig, log *slog.Logger) (*gorm.DB, error) {
	if cfg.DSN == "" {
		// 部分 CLI/开发命令可以不依赖 MySQL；API 和 seed 会在必须使用 DB 时设置 Required。
		if cfg.Required {
			return nil, fmt.Errorf("JXE_MYSQL_DSN is required")
		}
		log.Warn("mysql disabled because JXE_MYSQL_DSN is empty")
		return nil, nil
	}

	db, err := gorm.Open(gormmysql.Open(cfg.DSN), &gorm.Config{
		SkipDefaultTransaction: true,
		Logger: gormlogger.New(
			stdlog.New(os.Stdout, "\r\n", stdlog.LstdFlags),
			gormlogger.Config{
				SlowThreshold:             300 * time.Millisecond,
				LogLevel:                  gormlogger.Warn,
				IgnoreRecordNotFoundError: true,
				ParameterizedQueries:      true,
				Colorful:                  false,
			},
		),
	})
	if err != nil {
		if cfg.Required {
			return nil, err
		}
		log.Warn("mysql connection failed; continuing because mysql is optional", slog.Any("error", err))
		return nil, nil
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	// 启动时主动 Ping，确保 MySQL 必需时能够快速暴露连接问题。
	pingCtx, cancel := context.WithTimeout(ctx, cfg.PingTimeout)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		if cfg.Required {
			return nil, err
		}
		log.Warn("mysql ping failed; continuing because mysql is optional", slog.Any("error", err))
		return nil, nil
	}
	if err := verifyTimeZone(pingCtx, db, cfg.RequiredTimeZone); err != nil {
		if cfg.Required {
			return nil, err
		}
		log.Warn("mysql timezone verification failed; continuing because mysql is optional", slog.Any("error", err))
		return nil, nil
	}
	if err := verifySchema(pingCtx, db, cfg.RequireWineTicketSchema, cfg.RequireWineTicketMoneyContract); err != nil {
		if cfg.Required {
			return nil, err
		}
		log.Warn("mysql schema verification failed; continuing because mysql is optional", slog.Any("error", err))
		return nil, nil
	}
	if err := auditlog.Register(db); err != nil {
		return nil, fmt.Errorf("register audit invariant: %w", err)
	}

	return db, nil
}

type schemaColumn struct {
	table  string
	column string
}

var requiredSchemaColumns = []schemaColumn{
	{table: "products", column: "category_id"},
	{table: "product_stocks", column: "shop_product_id"},
	{table: "orders", column: "pay_status"},
	{table: "orders", column: "expires_at"},
	{table: "payments", column: "provider"},
	{table: "outbox_events", column: "locked_by"},
	{table: "outbox_events", column: "event_version"},
	{table: "outbox_events", column: "replay_of_event_id"},
	{table: "mq_consumer_receipts", column: "consumer_name"},
	{table: "mq_dead_letters", column: "payload_hash"},
	{table: "mq_dead_letter_replays", column: "replay_event_id"},
	{table: "customer_identities", column: "provider_subject"},
	{table: "payment_callbacks", column: "provider_event_id"},
	{table: "wechat_bill_reconciliation_runs", column: "bill_date"},
	{table: "wechat_bill_observations", column: "raw_hash"},
	{table: "wechat_bill_discrepancies", column: "dedupe_key"},
	{table: "refunds", column: "provider_accepted_at"},
	{table: "customer_addresses", column: "version"},
	{table: "customer_addresses", column: "coordinate_system"},
	{table: "customer_addresses", column: "poi_id"},
	{table: "customer_addresses", column: "geocode_status"},
	{table: "shops", column: "service_area_version"},
	{table: "shops", column: "coordinate_system"},
	{table: "shops", column: "overtime_policy_version"},
	{table: "service_cities", column: "city_code"},
	{table: "service_city_adcodes", column: "adcode"},
	{table: "delivery_promise_policies", column: "version"},
	{table: "rider_runtime_states", column: "coordinate_system"},
	{table: "shop_business_hours", column: "day_of_week"},
	{table: "home_slots", column: "slot_type"},
	{table: "after_sales", column: "after_sale_no"},
	{table: "refunds", column: "refund_no"},
	{table: "asset_accounts", column: "asset_type"},
	{table: "asset_transactions", column: "source_type"},
	{table: "asset_entries", column: "balance_after"},
	{table: "member_profiles", column: "tier_code"},
	{table: "compensation_ledger", column: "asset_transaction_id"},
	{table: "products", column: "age_restricted"},
	{table: "orders", column: "compliance_snapshot"},
	{table: "delivery_orders", column: "assignment_version"},
	{table: "print_tasks", column: "task_no"},
	{table: "notification_deliveries", column: "delivery_no"},
	{table: "delivery_verifications", column: "code_hash"},
	{table: "admin_override_approvals", column: "expected_version"},
	{table: "provisioning_operations", column: "operation_no"},
	{table: "identity_verification_requests", column: "state_hash"},
	{table: "identity_verification_requests", column: "verification_level"},
	{table: "identity_verification_callbacks", column: "provider_event_id"},
	{table: "customer_realname_verifications", column: "revoked_at"},
	{table: "rider_applications", column: "application_no"},
	{table: "rider_applications", column: "create_request_hash"},
	{table: "rider_application_reviews", column: "application_snapshot"},
	{table: "customer_search_histories", column: "normalized_keyword"},
	{table: "search_keyword_daily_stats", column: "normalized_keyword"},
	{table: "audit_logs", column: "event_id"},
	{table: "audit_logs", column: "account_id"},
	{table: "audit_logs", column: "ip_hash"},
}

var requiredWineTicketSchemaColumns = []schemaColumn{
	{table: "payments", column: "biz_type"},
	{table: "refunds", column: "biz_type"},
	{table: "orders", column: "order_type"},
	{table: "delivery_orders", column: "scheduled_start_at"},
	{table: "delivery_returns", column: "settlement_type"},
	{table: "wine_ticket_packages", column: "package_no"},
	{table: "wine_ticket_purchase_quotas", column: "package_code"},
	{table: "wine_ticket_purchases", column: "purchase_no"},
	{table: "wine_ticket_lots", column: "lot_no"},
	{table: "wine_ticket_transactions", column: "transaction_no"},
	{table: "delivery_time_slots", column: "service_date"},
	{table: "wine_ticket_redemptions", column: "redemption_no"},
	{table: "wine_ticket_redemption_allocations", column: "redemption_id"},
	{table: "wine_ticket_gifts", column: "gift_no"},
	{table: "wine_ticket_gift_allocations", column: "gift_id"},
	{table: "wine_ticket_gift_claim_tokens", column: "token_digest"},
	{table: "wine_ticket_renewals", column: "renewal_no"},
	{table: "wine_ticket_refunds", column: "wine_ticket_refund_no"},
	{table: "wine_ticket_refund_allocations", column: "wine_ticket_refund_id"},
	{table: "wine_ticket_reminders", column: "scheduled_at"},
	{table: "wine_ticket_reminders", column: "locked_by"},
	{table: "wine_ticket_reminders", column: "locked_until"},
	{table: "notification_subscription_consents", column: "provider_receipt"},
	{table: "wine_ticket_exceptions", column: "exception_no"},
	{table: "wine_ticket_reconciliation_checkpoints", column: "high_watermarks"},
	{table: "wine_ticket_reconciliation_checkpoints", column: "lease_until"},
	{table: "admin_user_shops", column: "admin_user_id"},
}

func verifyTimeZone(ctx context.Context, db *gorm.DB, required string) error {
	if required == "" {
		return nil
	}
	type timeZoneRow struct {
		SessionTimeZone string `gorm:"column:session_time_zone"`
		GlobalTimeZone  string `gorm:"column:global_time_zone"`
	}
	var row timeZoneRow
	if err := db.WithContext(ctx).Raw(`
		SELECT
			@@SESSION.time_zone AS session_time_zone,
			@@GLOBAL.time_zone AS global_time_zone`).Scan(&row).Error; err != nil {
		return fmt.Errorf("verify mysql timezone: %w", err)
	}
	return validateMySQLTimeZone(row.SessionTimeZone, row.GlobalTimeZone, required)
}

func validateMySQLTimeZone(session, global, required string) error {
	if session != required || global != required {
		return fmt.Errorf(
			"mysql timezone is incompatible: session=%q global=%q, want %q",
			session,
			global,
			required,
		)
	}
	return nil
}

// verifySchema 核验Schema是否有效。
func verifySchema(ctx context.Context, db *gorm.DB, requireWineTicket bool, requireWineTicketMoneyContract bool) error {
	type schemaColumnRow struct {
		TableName  string `gorm:"column:table_name"`
		ColumnName string `gorm:"column:column_name"`
	}
	var rows []schemaColumnRow
	err := db.WithContext(ctx).Raw(`
			SELECT table_name, column_name
			FROM information_schema.columns
			WHERE table_schema = DATABASE()`).Scan(&rows).Error
	if err != nil {
		return fmt.Errorf("verify database schema: %w", err)
	}
	found := make(map[schemaColumn]bool, len(rows))
	for _, row := range rows {
		found[schemaColumn{table: row.TableName, column: row.ColumnName}] = true
	}
	if err := validateRequiredSchemaColumnsForProfile(found, requireWineTicket); err != nil {
		return err
	}
	if requireWineTicketMoneyContract {
		return verifyWineTicketMoneyContract(ctx, db)
	}
	return nil
}

type moneyContractColumn struct {
	table  string
	column string
}

var requiredWineTicketMoneyContractColumns = map[moneyContractColumn]string{
	{table: "payments", column: "biz_type"}:                  "NO",
	{table: "payments", column: "biz_id"}:                    "NO",
	{table: "payments", column: "order_id"}:                  "YES",
	{table: "refunds", column: "biz_type"}:                   "NO",
	{table: "refunds", column: "biz_id"}:                     "NO",
	{table: "refunds", column: "order_id"}:                   "YES",
	{table: "refunds", column: "after_sale_id"}:              "YES",
	{table: "delivery_returns", column: "settlement_type"}:   "NO",
	{table: "delivery_returns", column: "settlement_status"}: "NO",
}

var requiredWineTicketMoneyContractConstraints = []string{
	"chk_payment_business_link",
	"chk_refund_business_link",
	"chk_delivery_return_settlement_type",
	"chk_delivery_return_settlement_state",
}

func verifyWineTicketMoneyContract(ctx context.Context, db *gorm.DB) error {
	type columnRow struct {
		TableName  string `gorm:"column:table_name"`
		ColumnName string `gorm:"column:column_name"`
		IsNullable string `gorm:"column:is_nullable"`
	}
	var columnRows []columnRow
	if err := db.WithContext(ctx).Raw(`
		SELECT table_name, column_name, is_nullable
		FROM information_schema.columns
		WHERE table_schema = DATABASE()
		  AND table_name IN ('payments','refunds','delivery_returns')
	`).Scan(&columnRows).Error; err != nil {
		return fmt.Errorf("verify wine-ticket money contract columns: %w", err)
	}
	foundColumns := make(map[moneyContractColumn]string, len(columnRows))
	for _, row := range columnRows {
		foundColumns[moneyContractColumn{table: row.TableName, column: row.ColumnName}] = row.IsNullable
	}

	type constraintRow struct {
		ConstraintName string `gorm:"column:constraint_name"`
	}
	var constraintRows []constraintRow
	if err := db.WithContext(ctx).Raw(`
		SELECT constraint_name
		FROM information_schema.table_constraints
		WHERE table_schema = DATABASE()
		  AND constraint_type = 'CHECK'
		  AND constraint_name IN (
		    'chk_payment_business_link',
		    'chk_refund_business_link',
		    'chk_delivery_return_settlement_type',
		    'chk_delivery_return_settlement_state'
		  )
	`).Scan(&constraintRows).Error; err != nil {
		return fmt.Errorf("verify wine-ticket money contract constraints: %w", err)
	}
	foundConstraints := make(map[string]bool, len(constraintRows))
	for _, row := range constraintRows {
		foundConstraints[row.ConstraintName] = true
	}
	return validateWineTicketMoneyContract(foundColumns, foundConstraints)
}

func validateWineTicketMoneyContract(foundColumns map[moneyContractColumn]string, foundConstraints map[string]bool) error {
	for column, expectedNullable := range requiredWineTicketMoneyContractColumns {
		if actual, ok := foundColumns[column]; !ok || actual != expectedNullable {
			return fmt.Errorf(
				"database schema is incompatible: wine-ticket money CONTRACT requires %s.%s IS_NULLABLE=%s; apply the manual schema-only CONTRACT after backfill gates pass",
				column.table, column.column, expectedNullable,
			)
		}
	}
	for _, constraint := range requiredWineTicketMoneyContractConstraints {
		if !foundConstraints[constraint] {
			return fmt.Errorf(
				"database schema is incompatible: wine-ticket money CONTRACT constraint %s is missing; apply the manual schema-only CONTRACT after backfill gates pass",
				constraint,
			)
		}
	}
	return nil
}

// validateRequiredSchemaColumns 校验Required Schema Columns是否合法。
func validateRequiredSchemaColumns(found map[schemaColumn]bool) error {
	return validateRequiredSchemaColumnsForProfile(found, false)
}

func validateRequiredSchemaColumnsForProfile(found map[schemaColumn]bool, requireWineTicket bool) error {
	requiredColumns := requiredSchemaColumns
	if requireWineTicket {
		requiredColumns = append(append([]schemaColumn{}, requiredSchemaColumns...), requiredWineTicketSchemaColumns...)
	}
	for _, required := range requiredColumns {
		if !found[required] {
			return fmt.Errorf(
				"database schema is incompatible: missing %s.%s; run Goose migrations and do not share this database with the legacy Sequelize backend",
				required.table,
				required.column,
			)
		}
	}
	return nil
}
