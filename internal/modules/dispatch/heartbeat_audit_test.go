package dispatch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/auditlog"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/securevalue"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type heartbeatAuditFixture struct {
	ID           uint64 `gorm:"primaryKey"`
	EventID      string
	ActorType    string
	ActorID      uint64
	AccountID    *uint64
	Action       string
	ResourceType string
	ResourceID   *uint64
	BeforeData   datatypes.JSON
	AfterData    datatypes.JSON
	Result       string
	ErrorCode    *string
	ReasonCode   *string
	RequestID    *string
	IP           *string
	IPHash       *string
	UserAgent    *string
	CreatedAt    time.Time
}

func (heartbeatAuditFixture) TableName() string { return "audit_logs" }

func TestHeartbeatLocationAnomalyAuditRedactsSensitivePayload(t *testing.T) {
	service, db := newHeartbeatAuditTestService(t, nil)
	ctx := heartbeatAuditContext("203.0.113.31")
	claims := heartbeatClaims()
	latitude, longitude, accuracy := 91.1234567, 113.9876543, 8.0
	req := HeartbeatReq{
		DeviceID: "device-secret-12345", Sequence: 1, CapturedAt: time.Now().Format(time.RFC3339Nano),
		Latitude: &latitude, Longitude: &longitude, CoordinateSystem: "gcj02", AccuracyM: &accuracy,
	}

	_, err := service.Heartbeat(ctx, claims, req, "203.0.113.31")
	if details := problem.FromError(err); details.ErrorCode != "VALIDATION_FAILED" {
		t.Fatalf("error_code=%q want VALIDATION_FAILED", details.ErrorCode)
	}
	row := firstHeartbeatAudit(t, db)
	if row.Action != heartbeatLocationAnomalyAction || row.Result != "failed" || stringValue(row.ErrorCode) != "VALIDATION_FAILED" || stringValue(row.ReasonCode) != "latitude_out_of_range" {
		t.Fatalf("unexpected audit: %+v", row)
	}
	if row.AccountID == nil || *row.AccountID != 101 || row.ResourceID == nil || *row.ResourceID != 42 {
		t.Fatalf("missing structured identities: %+v", row)
	}
	if row.IP != nil || row.IPHash == nil || *row.IPHash != securevalue.Digest("203.0.113.31") {
		t.Fatalf("IP privacy contract violated: ip=%v ip_hash=%v", row.IP, row.IPHash)
	}
	assertHeartbeatAuditHasNoLocationSecrets(t, row, req, "203.0.113.31")
}

func TestHeartbeatRateLimitAuditsOnlyControlledDimension(t *testing.T) {
	tests := []struct {
		name       string
		dimension  string
		reasonCode string
		seedKey    func(riderID uint64, deviceID, clientIP string) string
		seedCount  string
	}{
		{name: "rider", dimension: "rider", reasonCode: "rider_burst_limit", seedKey: func(riderID uint64, _, _ string) string {
			return fmt.Sprintf("dispatch:rider:heartbeat-limit:%d", riderID)
		}, seedCount: "5"},
		{name: "device", dimension: "device", reasonCode: "device_burst_limit", seedKey: func(_ uint64, deviceID, _ string) string {
			return fmt.Sprintf("dispatch:device:heartbeat-limit:%x", sha256.Sum256([]byte(deviceID)))
		}, seedCount: "5"},
		{name: "ip", dimension: "ip", reasonCode: "ip_source_limit", seedKey: func(_ uint64, _, clientIP string) string {
			return fmt.Sprintf("dispatch:ip:heartbeat-limit:%x", sha256.Sum256([]byte(clientIP)))
		}, seedCount: "200"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mini := miniredis.RunT(t)
			redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
			t.Cleanup(func() { _ = redisClient.Close() })
			service, db := newHeartbeatAuditTestService(t, redisClient)
			ctx := heartbeatAuditContext("203.0.113.32")
			claims := heartbeatClaims()
			req := validHeartbeatRequest()
			mini.Set(test.seedKey(42, req.DeviceID, "203.0.113.32"), test.seedCount)

			_, err := service.Heartbeat(ctx, claims, req, "203.0.113.32")
			if details := problem.FromError(err); details.ErrorCode != "RATE_LIMITED" {
				t.Fatalf("error_code=%q want RATE_LIMITED", details.ErrorCode)
			}
			row := firstHeartbeatAudit(t, db)
			if row.Action != heartbeatRateLimitedAction || stringValue(row.ReasonCode) != test.reasonCode {
				t.Fatalf("unexpected audit: %+v", row)
			}
			if !strings.Contains(string(row.AfterData), `"rate_limit_dimension":"`+test.dimension+`"`) {
				t.Fatalf("missing controlled rate-limit dimension: %s", row.AfterData)
			}
			assertHeartbeatAuditHasNoLocationSecrets(t, row, req, "203.0.113.32")
		})
	}
}

func TestHeartbeatBusinessRollbackStillPersistsRejectionAudit(t *testing.T) {
	service, db := newHeartbeatAuditTestService(t, nil)
	req := validHeartbeatRequest()
	_, err := service.Heartbeat(heartbeatAuditContext("203.0.113.33"), heartbeatClaims(), req, "203.0.113.33")
	if details := problem.FromError(err); details.ErrorCode != "RIDER_UNAVAILABLE" {
		t.Fatalf("error_code=%q want RIDER_UNAVAILABLE", details.ErrorCode)
	}
	row := firstHeartbeatAudit(t, db)
	if row.Action != heartbeatRejectedAction || stringValue(row.ErrorCode) != "RIDER_UNAVAILABLE" || stringValue(row.ReasonCode) != "rider_unavailable" {
		t.Fatalf("unexpected rejection audit: %+v", row)
	}
	var runtimeCount int64
	if err := db.Model(&RiderRuntimeState{}).Count(&runtimeCount).Error; err != nil {
		t.Fatal(err)
	}
	if runtimeCount != 0 {
		t.Fatalf("business transaction unexpectedly persisted %d runtime rows", runtimeCount)
	}
}

func TestHeartbeatHandlerAuditsLocationBindingRejection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service, db := newHeartbeatAuditTestService(t, nil)
	handler := NewHandler(service)
	router := gin.New()
	router.POST("/heartbeat", func(c *gin.Context) {
		c.Set("auth_claims", heartbeatClaims())
		ctx := heartbeatAuditContext("203.0.113.34")
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, handler.Heartbeat)
	body := []byte(`{"device_id":"device-secret-12345","sequence":1,"captured_at":"2026-07-22T10:00:00Z","latitude":91,"longitude":113.9,"coordinate_system":"gcj02","accuracy_m":8}`)
	request := httptest.NewRequest(http.MethodPost, "/heartbeat", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	row := firstHeartbeatAudit(t, db)
	if row.Action != heartbeatLocationAnomalyAction || stringValue(row.ReasonCode) != "location_contract_invalid" {
		t.Fatalf("unexpected binding audit: %+v", row)
	}
	if strings.Contains(string(row.AfterData), "91") || strings.Contains(string(row.AfterData), "device-secret") {
		t.Fatalf("binding audit leaked request payload: %s", row.AfterData)
	}
}

func newHeartbeatAuditTestService(t *testing.T, redisClient *redis.Client) (*Service, *gorm.DB) {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := auditlog.Register(db); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&heartbeatAuditFixture{}, &riderRow{}, &RiderRuntimeState{}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewService(config.Config{}, db, redisClient, snowflake.New(990), nil, log), db
}

func heartbeatAuditContext(ip string) context.Context {
	ctx := requestctx.WithHTTPMeta(context.Background(), ip, "heartbeat-audit-test")
	ctx = requestctx.WithRequestID(ctx, "request-heartbeat-audit")
	return requestctx.WithAccountID(ctx, "101")
}

func heartbeatClaims() *auth.Claims {
	return &auth.Claims{AccountType: "rider", AccountID: "101", RiderID: "42", Permissions: []string{"rider_location:update"}}
}

func validHeartbeatRequest() HeartbeatReq {
	latitude, longitude, accuracy := 22.543096, 114.057865, 8.0
	return HeartbeatReq{
		DeviceID: "device-secret-12345", Sequence: 1, CapturedAt: time.Now().Format(time.RFC3339Nano),
		Latitude: &latitude, Longitude: &longitude, CoordinateSystem: "gcj02", AccuracyM: &accuracy,
	}
}

func firstHeartbeatAudit(t *testing.T, db *gorm.DB) heartbeatAuditFixture {
	t.Helper()
	var row heartbeatAuditFixture
	if err := db.Order("created_at,id").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.EventID == "" || row.RequestID == nil || *row.RequestID != "request-heartbeat-audit" {
		t.Fatalf("missing audit correlation fields: %+v", row)
	}
	return row
}

func assertHeartbeatAuditHasNoLocationSecrets(t *testing.T, row heartbeatAuditFixture, req HeartbeatReq, rawIP string) {
	t.Helper()
	payload := strings.ToLower(string(row.AfterData))
	for _, forbidden := range []string{
		`"latitude":`, `"longitude":`, `"accuracy_m":`, `"device_id":`, strings.ToLower(req.DeviceID),
		fmt.Sprintf("%x", sha256.Sum256([]byte(req.DeviceID))), strings.ToLower(rawIP),
	} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("audit payload contains forbidden value %q: %s", forbidden, row.AfterData)
		}
	}
}
