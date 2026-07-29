package ops

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/integrity"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type exceptionTestAudit struct {
	ID           uint64 `gorm:"primaryKey"`
	EventID      *string
	AccountID    *uint64
	ActorType    string
	ActorID      uint64
	Action       string
	ResourceType string
	ResourceID   uint64
	BeforeData   datatypes.JSON
	AfterData    datatypes.JSON
	Result       string
	BeforeStatus *string
	AfterStatus  *string
	Version      uint64
	RequestID    *string
	IPHash       *string
	UserAgent    *string
	CreatedAt    time.Time
}

func (exceptionTestAudit) TableName() string { return "audit_logs" }

func TestAdminExceptionRoutesDoNotExposeReviewEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterAdminRoutes(
		engine.Group("/api/v1/admin/wine-tickets"),
		NewHandler(nil),
	)
	foundResolution := false
	for _, route := range engine.Routes() {
		switch route.Path {
		case "/api/v1/admin/wine-tickets/exceptions/:exception_no/resolution":
			foundResolution = route.Method == "POST"
		case "/api/v1/admin/wine-tickets/exceptions/:exception_no/resolution/review":
			t.Fatalf("legacy review route is still registered: %+v", route)
		}
	}
	if !foundResolution {
		t.Fatal("direct resolution route is not registered")
	}
}

func TestExceptionAdminDirectResolutionIdempotencyAndSnapshotSafety(t *testing.T) {
	service, db, now := newExceptionAdminTestService(t)
	seedException(t, db, now, 101, "WTEX101", ExceptionActionCloseWithoutAssetChange)
	ctx := requestctx.WithRequestID(
		context.Background(),
		"exception-direct-resolution-request",
	)

	operator := exceptionAdminClaims(
		"7001",
		"wine_ticket_exception:list",
		"wine_ticket_exception:view",
		"wine_ticket_exception:resolve",
	)
	items, next, err := service.ListAdminExceptions(
		ctx,
		operator,
		pagination.Query{PageSize: 20},
		ExceptionAdminFilter{
			Status:   ExceptionStatusInvestigating,
			Severity: "P1",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if next != "" || len(items) != 1 {
		t.Fatalf("exceptions=%+v next=%q", items, next)
	}
	serialized, err := json.Marshal(items[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{
		"openid-secret",
		"13800000000",
		"share-token-secret",
		"完整地址",
	} {
		if strings.Contains(string(serialized), leaked) {
			t.Fatalf("exception projection leaked %q: %s", leaked, serialized)
		}
	}

	resolution := ExceptionResolutionRequest{
		ResolutionAction: ExceptionActionCloseWithoutAssetChange,
		Reason:           "provider and asset facts agree; close alert only",
		ReviewTicketNo:   "OPS-20260727-001",
		ExpectedVersion:  1,
	}
	resolved, err := service.ResolveException(
		ctx,
		operator,
		"POST",
		"/api/v1/admin/wine-tickets/exceptions/:exception_no/resolution",
		"resolution-key-001",
		"WTEX101",
		resolution,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != ExceptionStatusResolved ||
		resolved.Version != 2 ||
		resolved.ResolvedAt == nil ||
		resolved.ProposedBy == nil ||
		*resolved.ProposedBy != "7001" ||
		resolved.ReviewedBy != nil ||
		resolved.ReviewDecision != nil ||
		!strings.Contains(string(resolved.ResolutionResult), `"asset_changed":false`) {
		t.Fatalf("resolved=%+v", resolved)
	}
	replayed, err := service.ResolveException(
		ctx,
		operator,
		"POST",
		"/api/v1/admin/wine-tickets/exceptions/:exception_no/resolution",
		"resolution-key-001",
		"WTEX101",
		resolution,
	)
	if err != nil || replayed.Version != resolved.Version {
		t.Fatalf("resolution replay=%+v err=%v", replayed, err)
	}
	_, err = service.ResolveException(
		ctx,
		operator,
		"POST",
		"/api/v1/admin/wine-tickets/exceptions/:exception_no/resolution",
		"resolution-key-002",
		"WTEX101",
		resolution,
	)
	if problem.FromError(err).ErrorCode != "WT_CONCURRENT_MODIFICATION" {
		t.Fatalf("stale concurrent resolution error=%v", err)
	}

	var auditRows []exceptionTestAudit
	if err := db.Model(&exceptionTestAudit{}).
		Where("resource_type = ?", "wine_ticket_exception").
		Find(&auditRows).Error; err != nil {
		t.Fatal(err)
	}
	if len(auditRows) != 1 ||
		auditRows[0].Action != "wine_ticket_exception.resolution_executed" {
		t.Fatalf("audits=%+v, want one direct-resolution audit", auditRows)
	}
	var before, after map[string]any
	if err := json.Unmarshal(auditRows[0].BeforeData, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(auditRows[0].AfterData, &after); err != nil {
		t.Fatal(err)
	}
	if auditRows[0].BeforeStatus == nil ||
		*auditRows[0].BeforeStatus != ExceptionStatusInvestigating ||
		auditRows[0].AfterStatus == nil ||
		*auditRows[0].AfterStatus != ExceptionStatusResolved ||
		auditRows[0].Version != 2 ||
		auditRows[0].RequestID == nil ||
		*auditRows[0].RequestID != requestctx.RequestID(ctx) ||
		before["status"] != ExceptionStatusInvestigating ||
		before["version"] != float64(1) ||
		after["status"] != ExceptionStatusResolved ||
		after["version"] != float64(2) ||
		after["permission"] != "wine_ticket_exception:resolve" ||
		after["request_id"] != requestctx.RequestID(ctx) ||
		after["request_correlation_id"] != requestctx.RequestID(ctx) ||
		after["correlation_id"] != "corr-WTEX101" ||
		after["idempotency_key_hash"] != idempotency.KeyHash("resolution-key-001") ||
		after["service_instance"] != "exception-audit-test" ||
		after["review_ticket_no"] != resolution.ReviewTicketNo {
		t.Fatalf(
			"incomplete resolution audit=%+v before=%v after=%v",
			auditRows[0],
			before,
			after,
		)
	}
}

func TestExceptionResolutionRechecksLivePermissionBeforeIdempotency(t *testing.T) {
	service, db, now := newExceptionAdminTestService(t)
	seedException(
		t,
		db,
		now,
		151,
		"WTEX151",
		ExceptionActionCloseWithoutAssetChange,
	)
	if err := db.Exec(`
		UPDATE role_permissions
		   SET deleted_at = CURRENT_TIMESTAMP
		 WHERE role_id = 9800
		   AND permission_id = 2148
	`).Error; err != nil {
		t.Fatal(err)
	}
	operator := exceptionAdminClaims(
		"7001",
		"wine_ticket_exception:resolve",
	)
	key := "resolution-revoked-live-001"
	_, err := service.ResolveException(
		context.Background(),
		operator,
		"POST",
		"/api/v1/admin/wine-tickets/exceptions/:exception_no/resolution",
		key,
		"WTEX151",
		ExceptionResolutionRequest{
			ResolutionAction: ExceptionActionCloseWithoutAssetChange,
			Reason:           "revoked live permission must not execute",
			ReviewTicketNo:   "OPS-WTEX151",
			ExpectedVersion:  1,
		},
	)
	if problem.FromError(err).ErrorCode != "PERM_FORBIDDEN" {
		t.Fatalf("revoked live resolution permission error=%v", err)
	}
	var stored integrity.Exception
	if err := db.Where("id = ?", 151).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != ExceptionStatusInvestigating || stored.Version != 1 {
		t.Fatalf("revoked resolution changed exception=%+v", stored)
	}
	var idempotencyCount, auditCount int64
	if err := db.Model(&idempotency.Record{}).
		Where("key_hash = ?", idempotency.KeyHash(key)).
		Count(&idempotencyCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&exceptionTestAudit{}).
		Where(
			"resource_type = ? AND resource_id = ?",
			"wine_ticket_exception",
			151,
		).
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if idempotencyCount != 0 || auditCount != 0 {
		t.Fatalf(
			"revoked resolution created side effects idempotency=%d audit=%d",
			idempotencyCount,
			auditCount,
		)
	}
}

func TestExceptionAdminRejectsUnsafeClosureAndResolvesLegacyPendingProposal(
	t *testing.T,
) {
	service, db, now := newExceptionAdminTestService(t)
	seedException(t, db, now, 201, "WTEX201", ExceptionActionRestoreQuantity)
	seedException(t, db, now, 202, "WTEX202", ExceptionActionCloseWithoutAssetChange)
	operator := exceptionAdminClaims("8001", "wine_ticket_exception:resolve")

	_, err := service.ResolveException(
		context.Background(),
		operator,
		"POST",
		"/api/v1/admin/wine-tickets/exceptions/:exception_no/resolution",
		"resolution-unsafe-001",
		"WTEX201",
		ExceptionResolutionRequest{
			ResolutionAction: ExceptionActionRestoreQuantity,
			Reason:           "attempt unsupported adjustment",
			ReviewTicketNo:   "OPS-WTEX201",
			ExpectedVersion:  1,
		},
	)
	if problem.FromError(err).ErrorCode != "WT_EXCEPTION_ACTION_UNAVAILABLE" {
		t.Fatalf("unsafe closure error=%v", err)
	}
	var unsafeRow integrity.Exception
	if err := db.Where("exception_no = ?", "WTEX201").Take(&unsafeRow).Error; err != nil {
		t.Fatal(err)
	}
	if unsafeRow.Status != ExceptionStatusInvestigating ||
		unsafeRow.Version != 1 ||
		unsafeRow.ProposedBy != nil ||
		unsafeRow.ResolutionResult != nil {
		t.Fatalf("unsafe closure changed exception: %+v", unsafeRow)
	}

	legacyProposedAt := now.Add(-time.Hour)
	if err := db.Model(&integrity.Exception{}).
		Where("exception_no = ?", "WTEX202").
		Updates(map[string]any{
			"status":           ExceptionStatusPendingReview,
			"proposed_action":  ExceptionActionCloseWithoutAssetChange,
			"proposed_reason":  "legacy proposal must not execute implicitly",
			"review_ticket_no": "OPS-LEGACY-202",
			"proposed_by":      uint64(8001),
			"proposed_at":      legacyProposedAt,
			"version":          2,
			"updated_at":       legacyProposedAt,
		}).Error; err != nil {
		t.Fatal(err)
	}

	resolved, err := service.ResolveException(
		context.Background(),
		operator,
		"POST",
		"/api/v1/admin/wine-tickets/exceptions/:exception_no/resolution",
		"resolution-legacy-pending-001",
		"WTEX202",
		ExceptionResolutionRequest{
			ResolutionAction: ExceptionActionCloseWithoutAssetChange,
			Reason:           "freshly verified facts allow direct closure",
			ReviewTicketNo:   "OPS-RESUBMIT-202",
			ExpectedVersion:  2,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != ExceptionStatusResolved ||
		resolved.Version != 3 ||
		resolved.ProposedReason == nil ||
		*resolved.ProposedReason != "freshly verified facts allow direct closure" ||
		resolved.ReviewTicketNo == nil ||
		*resolved.ReviewTicketNo != "OPS-RESUBMIT-202" ||
		resolved.ReviewedBy != nil ||
		resolved.ReviewDecision != nil ||
		!strings.Contains(
			string(resolved.ResolutionResult),
			`"review_ticket_no":"OPS-RESUBMIT-202"`,
		) {
		t.Fatalf("resolved legacy pending exception=%+v", resolved)
	}
}

func newExceptionAdminTestService(
	t *testing.T,
) (*Service, *gorm.DB, time.Time) {
	t.Helper()
	dsn := uniqueSQLiteMemoryDSN(t, "_busy_timeout=5000")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&integrity.Exception{},
		&idempotency.Record{},
		&exceptionTestAudit{},
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(
		"CREATE UNIQUE INDEX uk_exception_test_idempotency " +
			"ON idempotency_keys(actor_type,actor_id,path,key_hash)",
	).Error; err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE accounts (
			id INTEGER PRIMARY KEY,
			account_type TEXT NOT NULL,
			status TEXT NOT NULL,
			deleted_at DATETIME
		)`,
		`CREATE TABLE roles (
			id INTEGER PRIMARY KEY,
			code TEXT NOT NULL,
			status TEXT NOT NULL,
			deleted_at DATETIME
		)`,
		`CREATE TABLE admin_users (
			id INTEGER PRIMARY KEY,
			account_id INTEGER NOT NULL,
			role_id INTEGER NOT NULL,
			status TEXT NOT NULL,
			deleted_at DATETIME
		)`,
		`CREATE TABLE permissions (
			id INTEGER PRIMARY KEY,
			code TEXT NOT NULL,
			status TEXT NOT NULL,
			deleted_at DATETIME
		)`,
		`CREATE TABLE role_permissions (
			id INTEGER PRIMARY KEY,
			role_id INTEGER NOT NULL,
			permission_id INTEGER NOT NULL,
			deleted_at DATETIME
		)`,
		`INSERT INTO accounts (id,account_type,status) VALUES
			(17001,'admin','active'),
			(18001,'admin','active')`,
		`INSERT INTO roles (id,code,status) VALUES
			(9800,'exception_test_operator','active')`,
		`INSERT INTO admin_users (id,account_id,role_id,status) VALUES
			(7001,17001,9800,'active'),
			(8001,18001,9800,'active')`,
		`INSERT INTO permissions (id,code,status) VALUES
			(2148,'wine_ticket_exception:resolve','active')`,
		`INSERT INTO role_permissions (id,role_id,permission_id) VALUES
			(9811,9800,2148)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("seed exception live authorization: %v", err)
		}
	}
	service := NewService(db, snowflake.New(89), nil).
		WithInstance("exception-audit-test")
	now := time.Date(
		2026,
		time.July,
		27,
		15,
		30,
		0,
		123000000,
		shanghaiLocation,
	)
	service.now = func() time.Time { return now }
	return service, db, now
}

func seedException(
	t *testing.T,
	db *gorm.DB,
	now time.Time,
	id uint64,
	exceptionNo string,
	_ string,
) {
	t.Helper()
	bizNo := "WTPU-" + exceptionNo
	correlationID := "corr-" + exceptionNo
	row := integrity.Exception{
		ID:               id,
		ExceptionNo:      exceptionNo,
		ExceptionType:    "lot_replay_mismatch",
		BizType:          "wine_ticket_purchase",
		BizID:            id + 1000,
		BizNo:            &bizNo,
		SourceType:       "daily_reconciliation",
		CorrelationID:    &correlationID,
		Severity:         "P1",
		Status:           ExceptionStatusInvestigating,
		ExpectedSnapshot: datatypes.JSON(`{"available_quantity":6,"phone":"13800000000","nested":{"share_token":"share-token-secret"}}`),
		ActualSnapshot:   datatypes.JSON(`{"available_quantity":5,"openid":"openid-secret","address_snapshot":"完整地址"}`),
		OccurrenceCount:  1,
		FirstDetectedAt:  now,
		LastDetectedAt:   now,
		Version:          1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
}

func exceptionAdminClaims(
	adminID string,
	permissions ...string,
) *auth.Claims {
	return &auth.Claims{
		AccountType: "admin",
		AdminUserID: adminID,
		Permissions: permissions,
	}
}
