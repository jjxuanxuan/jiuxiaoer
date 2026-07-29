package ops

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	mysqlinfra "jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/integrity"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type mysqlCountingExceptionResolutionExecutor struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (e *mysqlCountingExceptionResolutionExecutor) ExecuteWineTicketExceptionResolution(
	ctx context.Context,
	_ *gorm.DB,
	command ExceptionResolutionCommand,
) (datatypes.JSON, error) {
	e.calls.Add(1)
	select {
	case e.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.release:
	}
	return jsonData(map[string]any{
		"action":        command.Action,
		"asset_changed": false,
		"closure":       "executed_once",
	}), nil
}

func TestMySQLExceptionResolutionConcurrentDifferentKeysExecutesOnce(
	t *testing.T,
) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip(
			"set JXE_RUN_INTEGRATION=1 to run wine-ticket exception MySQL acceptance",
		)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg := config.Load()
	cfg.MySQL.Required = true
	cfg.MySQL.RequiredTimeZone = "+08:00"
	cfg.MySQL.RequireWineTicketSchema = true
	cfg.MySQL.RequireWineTicketMoneyContract = false
	if cfg.MySQL.MaxOpenConns < 4 {
		cfg.MySQL.MaxOpenConns = 4
	}
	if cfg.MySQL.MaxIdleConns < 2 {
		cfg.MySQL.MaxIdleConns = 2
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := mysqlinfra.Open(ctx, cfg.MySQL, log)
	if err != nil || db == nil {
		t.Fatalf("open schema-verified mysql: %v", err)
	}
	db = db.Session(&gorm.Session{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get mysql connection pool: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	ids := snowflake.New(1007)
	exceptionID := ids.Next()
	bizID := ids.Next()
	adminID := ids.Next()
	adminAccountID := ids.Next()
	adminRoleID := ids.Next()
	rolePermissionID := ids.Next()
	exceptionNo := "WTEXSOA14-" + strconv.FormatUint(exceptionID, 10)
	bizNo := "WTPUSOA14-" + strconv.FormatUint(bizID, 10)
	correlationID := "soa14-" + strconv.FormatUint(exceptionID, 10)
	path := "/api/v1/admin/wine-tickets/exceptions/:exception_no/resolution"
	idempotencyKeys := [2]string{
		"soa014-resolution-a-" + strconv.FormatUint(exceptionID, 10),
		"soa014-resolution-b-" + strconv.FormatUint(exceptionID, 10),
	}
	now := time.Now().In(shanghaiLocation).Truncate(time.Millisecond)
	row := integrity.Exception{
		ID:               exceptionID,
		ExceptionNo:      exceptionNo,
		ExceptionType:    "single_operator_concurrency",
		BizType:          "wine_ticket_purchase",
		BizID:            bizID,
		BizNo:            &bizNo,
		SourceType:       "acceptance_test",
		CorrelationID:    &correlationID,
		Severity:         "P1",
		Status:           ExceptionStatusInvestigating,
		ExpectedSnapshot: datatypes.JSON(`{"available_quantity":1}`),
		ActualSnapshot:   datatypes.JSON(`{"available_quantity":1}`),
		OccurrenceCount:  1,
		FirstDetectedAt:  now,
		LastDetectedAt:   now,
		Version:          1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	t.Cleanup(func() {
		for _, cleanup := range []struct {
			name string
			err  error
		}{
			{
				name: "idempotency keys",
				err: db.Exec(
					"DELETE FROM idempotency_keys WHERE actor_type = 'admin' AND actor_id = ? AND path = ?",
					adminID,
					path,
				).Error,
			},
			{
				name: "audit logs",
				err: db.Exec(
					"DELETE FROM audit_logs WHERE resource_type = 'wine_ticket_exception' AND resource_id = ?",
					exceptionID,
				).Error,
			},
			{
				name: "exception",
				err: db.Where("id = ?", exceptionID).
					Delete(&integrity.Exception{}).Error,
			},
			{
				name: "role permission",
				err: db.Exec(
					"DELETE FROM role_permissions WHERE id = ?",
					rolePermissionID,
				).Error,
			},
			{
				name: "admin user",
				err: db.Exec(
					"DELETE FROM admin_users WHERE id = ?",
					adminID,
				).Error,
			},
			{
				name: "admin role",
				err: db.Exec(
					"DELETE FROM roles WHERE id = ?",
					adminRoleID,
				).Error,
			},
			{
				name: "admin account",
				err: db.Exec(
					"DELETE FROM accounts WHERE id = ?",
					adminAccountID,
				).Error,
			},
		} {
			if cleanup.err != nil {
				t.Errorf("cleanup %s: %v", cleanup.name, cleanup.err)
			}
		}
	})
	if err := db.Table("accounts").Create(map[string]any{
		"id":           adminAccountID,
		"account_type": "admin",
		"username":     "soa014_admin_" + strconv.FormatUint(adminID, 10),
		"status":       "active",
	}).Error; err != nil {
		t.Fatalf("insert exception admin account: %v", err)
	}
	if err := db.Table("roles").Create(map[string]any{
		"id":     adminRoleID,
		"code":   "soa014_role_" + strconv.FormatUint(adminRoleID, 10),
		"name":   "SOA014 验收角色",
		"scope":  "all",
		"status": "active",
	}).Error; err != nil {
		t.Fatalf("insert exception admin role: %v", err)
	}
	if err := db.Table("admin_users").Create(map[string]any{
		"id":             adminID,
		"account_id":     adminAccountID,
		"role_id":        adminRoleID,
		"admin_sub_role": "operation",
		"name":           "SOA014 验收管理员",
		"status":         "active",
	}).Error; err != nil {
		t.Fatalf("insert exception admin user: %v", err)
	}
	permissionResult := db.Exec(
		`INSERT INTO role_permissions (id,role_id,permission_id)
		 SELECT ?,?,p.id
		   FROM permissions p
		  WHERE p.code='wine_ticket_exception:resolve'
		    AND p.status='active'
		    AND p.deleted_at IS NULL`,
		rolePermissionID,
		adminRoleID,
	)
	if permissionResult.Error != nil || permissionResult.RowsAffected != 1 {
		t.Fatalf(
			"insert exception live permission rows=%d err=%v",
			permissionResult.RowsAffected,
			permissionResult.Error,
		)
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("insert investigating exception: %v", err)
	}

	executor := &mysqlCountingExceptionResolutionExecutor{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	service := NewService(db, ids, nil).
		WithExceptionResolutionExecutor(executor)
	claims := &auth.Claims{
		AccountType: "admin",
		AdminUserID: strconv.FormatUint(adminID, 10),
		Permissions: []string{"wine_ticket_exception:resolve"},
	}
	request := ExceptionResolutionRequest{
		ResolutionAction: ExceptionActionCloseWithoutAssetChange,
		Reason:           "ACC-SOA-014 concurrent direct resolution",
		ReviewTicketNo:   "OPS-SOA-014",
		ExpectedVersion:  1,
	}
	type callResult struct {
		response ExceptionAdminDTO
		err      error
	}
	start := make(chan struct{})
	ready := make(chan struct{}, len(idempotencyKeys))
	results := make(chan callResult, len(idempotencyKeys))
	for _, key := range idempotencyKeys {
		key := key
		go func() {
			ready <- struct{}{}
			<-start
			response, callErr := service.ResolveException(
				ctx,
				claims,
				http.MethodPost,
				path,
				key,
				exceptionNo,
				request,
			)
			results <- callResult{response: response, err: callErr}
		}()
	}
	for range idempotencyKeys {
		<-ready
	}
	close(start)

	select {
	case <-executor.entered:
		close(executor.release)
	case <-ctx.Done():
		close(executor.release)
		t.Fatalf("resolution executor was not reached: %v", ctx.Err())
	}

	successes := 0
	conflicts := 0
	for range idempotencyKeys {
		result := <-results
		if result.err == nil {
			successes++
			if result.response.Status != ExceptionStatusResolved ||
				result.response.Version != 2 {
				t.Fatalf(
					"successful resolution response=%+v",
					result.response,
				)
			}
			continue
		}
		if problem.FromError(result.err).ErrorCode ==
			"WT_CONCURRENT_MODIFICATION" {
			conflicts++
			continue
		}
		t.Fatalf("unexpected concurrent resolution error: %v", result.err)
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf(
			"concurrent resolutions successes=%d conflicts=%d, want 1/1",
			successes,
			conflicts,
		)
	}
	if calls := executor.calls.Load(); calls != 1 {
		t.Fatalf("resolution executor calls=%d, want 1", calls)
	}

	var stored integrity.Exception
	if err := db.Where("id = ?", exceptionID).Take(&stored).Error; err != nil {
		t.Fatalf("load resolved exception: %v", err)
	}
	if stored.Status != ExceptionStatusResolved ||
		stored.Version != 2 ||
		stored.ResolvedAt == nil ||
		string(stored.ResolutionResult) == "" {
		t.Fatalf("resolved exception=%+v", stored)
	}
	var resolvedCount int64
	if err := db.Model(&integrity.Exception{}).
		Where("id = ? AND status = ?", exceptionID, ExceptionStatusResolved).
		Count(&resolvedCount).Error; err != nil {
		t.Fatalf("count resolved exceptions: %v", err)
	}
	if resolvedCount != 1 {
		t.Fatalf("resolved exception rows=%d, want 1", resolvedCount)
	}

	var auditCount int64
	if err := db.Table("audit_logs").
		Where(
			"resource_type = 'wine_ticket_exception' AND resource_id = ?",
			exceptionID,
		).
		Count(&auditCount).Error; err != nil {
		t.Fatalf("count exception audits: %v", err)
	}
	var successAuditCount int64
	if err := db.Table("audit_logs").
		Where(
			"resource_type = 'wine_ticket_exception' AND resource_id = ? AND action = ? AND result = 'success'",
			exceptionID,
			"wine_ticket_exception.resolution_executed",
		).
		Count(&successAuditCount).Error; err != nil {
		t.Fatalf("count successful resolution audits: %v", err)
	}
	if auditCount != 1 || successAuditCount != 1 {
		t.Fatalf(
			"exception audits total=%d successful_resolution=%d, want 1/1",
			auditCount,
			successAuditCount,
		)
	}
}
