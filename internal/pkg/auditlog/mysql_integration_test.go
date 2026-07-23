package auditlog_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"gorm.io/datatypes"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/securevalue"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestStructuredAuditMySQLInvariantAndRawIPGuard(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run structured audit integration test")
	}
	db, err := mysql.Open(context.Background(), config.Load().MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })

	ids := snowflake.New(969)
	auditID := ids.Next()
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM audit_logs WHERE id IN (?, ?, ?)", auditID, auditID+1, auditID+2).Error
	})
	ctx := requestctx.WithHTTPMeta(context.Background(), "203.0.113.21", "audit-integration")
	ctx = requestctx.WithRequestID(ctx, "audit-integration-request")
	ctx = requestctx.WithAccountID(ctx, "7788")
	if err := db.WithContext(ctx).Table("audit_logs").Create(map[string]any{
		"id": auditID, "actor_type": "merchant", "actor_id": uint64(99),
		"action": "store.order.accept", "resource_type": "order", "resource_id": uint64(12345),
		"before_data": datatypes.JSON(`{"Status":"paid","Version":4,"AddressSnapshot":{"ContactPhone":"13800138000"}}`),
		"after_data":  datatypes.JSON(`{"Status":"accepted","Version":5,"ReasonCode":"manual"}`),
		"result":      "success",
	}).Error; err != nil {
		t.Fatal(err)
	}
	var got struct {
		EventID, BeforeStatus, AfterStatus, IPHash string
		AccountID, OrderID, Version                uint64
		BeforeData                                 datatypes.JSON
		IP                                         *string
	}
	if err := db.Table("audit_logs").Where("id=?", auditID).Take(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got.EventID == "" || got.AccountID != 7788 || got.OrderID != 12345 || got.BeforeStatus != "paid" || got.AfterStatus != "accepted" || got.Version != 5 {
		t.Fatalf("structured audit mismatch: %+v", got)
	}
	if got.IP != nil || got.IPHash != securevalue.Digest("203.0.113.21") || strings.Contains(string(got.BeforeData), "13800138000") {
		t.Fatalf("audit privacy invariant mismatch: %+v before=%s", got, got.BeforeData)
	}

	invalidRawID := auditID + 1
	err = db.Exec(`INSERT INTO audit_logs (id,event_id,actor_type,actor_id,action,resource_type,result,ip)
		VALUES (?, ?, 'system', 0, 'privacy.guard', 'test', 'failed', '203.0.113.22')`, invalidRawID, "audit-raw-ip-guard-"+snowflake.String(invalidRawID)).Error
	if err == nil {
		t.Fatal("database accepted a raw audit IP")
	}
	invalidHashID := auditID + 2
	err = db.Exec(`INSERT INTO audit_logs (id,event_id,actor_type,actor_id,action,resource_type,result,ip_hash)
		VALUES (?, ?, 'system', 0, 'privacy.guard', 'test', 'failed', '203.0.113.22')`, invalidHashID, "audit-hash-guard-"+snowflake.String(invalidHashID)).Error
	if err == nil {
		t.Fatal("database accepted a non-digest audit IP hash")
	}
}
