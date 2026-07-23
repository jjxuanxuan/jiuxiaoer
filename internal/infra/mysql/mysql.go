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
	if err := verifySchema(pingCtx, db); err != nil {
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

// verifySchema 核验Schema是否有效。
func verifySchema(ctx context.Context, db *gorm.DB) error {
	type schemaColumnRow struct {
		TableName  string `gorm:"column:table_name"`
		ColumnName string `gorm:"column:column_name"`
	}
	var rows []schemaColumnRow
	err := db.WithContext(ctx).Raw(`
			SELECT table_name, column_name
			FROM information_schema.columns
			WHERE table_schema = DATABASE()
			  AND table_name IN ('products', 'product_stocks', 'orders', 'payments', 'outbox_events', 'mq_consumer_receipts', 'mq_dead_letters', 'mq_dead_letter_replays', 'customer_identities', 'payment_callbacks', 'wechat_bill_reconciliation_runs', 'wechat_bill_observations', 'wechat_bill_discrepancies', 'customer_addresses', 'shops', 'service_cities', 'service_city_adcodes', 'delivery_promise_policies', 'rider_runtime_states', 'shop_business_hours', 'home_slots', 'after_sales', 'refunds', 'asset_accounts', 'asset_transactions', 'asset_entries', 'member_profiles', 'compensation_ledger', 'delivery_orders', 'print_tasks', 'notification_deliveries', 'delivery_verifications', 'admin_override_approvals', 'provisioning_operations', 'identity_verification_requests', 'identity_verification_callbacks', 'customer_realname_verifications', 'rider_applications', 'rider_application_reviews', 'customer_search_histories', 'search_keyword_daily_stats', 'audit_logs')`).Scan(&rows).Error
	if err != nil {
		return fmt.Errorf("verify database schema: %w", err)
	}
	found := make(map[schemaColumn]bool, len(rows))
	for _, row := range rows {
		found[schemaColumn{table: row.TableName, column: row.ColumnName}] = true
	}
	return validateRequiredSchemaColumns(found)
}

// validateRequiredSchemaColumns 校验Required Schema Columns是否合法。
func validateRequiredSchemaColumns(found map[schemaColumn]bool) error {
	for _, required := range requiredSchemaColumns {
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
