package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	goredis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/amap"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type l2AcceptanceFixture struct {
	ctx           context.Context
	cfg           config.Config
	db            *gorm.DB
	redis         *goredis.Client
	router        *gin.Engine
	customerToken string
	otherToken    string
	adminToken    string
	merchantToken string
	customerID    uint64
	addressID     string
	lbsProvider   amap.Provider
	idGen         *snowflake.Generator
	seq           int
	close         func()
}

// newL2AcceptanceFixture 创建并初始化L 2 验收测试夹具。
func newL2AcceptanceFixture(t *testing.T) *l2AcceptanceFixture {
	t.Helper()
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run L2 acceptance tests")
	}
	ctx := context.Background()
	cfg := config.Load()
	cfg.Service.EnforcementMode = "enforce"
	db, err := mysql.Open(ctx, cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("begin transaction: %v", tx.Error)
	}
	redisClient := goredis.NewClient(&goredis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: 9})
	if err := redisClient.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flush redis: %v", err)
	}
	lbsProvider := amap.NewFakeProvider()
	router := NewRouter(Dependencies{Config: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: tx, Redis: redisClient, CustomerLBSProvider: lbsProvider})
	f := &l2AcceptanceFixture{ctx: ctx, cfg: cfg, db: tx, redis: redisClient, router: router, lbsProvider: lbsProvider, idGen: snowflake.New(988)}
	f.close = func() {
		tx.Rollback()
		_ = redisClient.FlushDB(ctx).Err()
		_ = redisClient.Close()
		_ = sqlDB.Close()
	}

	f.customerToken, f.customerID = f.loginCustomer(t, "136")
	f.otherToken, _ = f.loginCustomer(t, "137")
	f.adminToken = tokenFromLogin(t, router, "/api/v1/auth/admin/login", map[string]any{"username": "admin", "password": cfg.Security.AdminBootstrapPassword})
	f.merchantToken = tokenFromLogin(t, router, "/api/v1/auth/merchant/login", map[string]any{"username": "merchant_demo", "password": "merchant123"})
	address := performOK(t, router, http.MethodPost, "/api/v1/addresses", f.customerToken, f.key("fixture-address"), map[string]any{
		"contact_name": "L2 acceptance", "contact_phone": "13600000001", "province": "广东省", "city": "深圳市", "city_code": "440300",
		"district": "南山区", "district_code": "440305", "address_detail": "科技园", "location_source": "map_pin",
		"latitude": 22.54, "longitude": 113.93, "coordinate_system": "gcj02",
	})
	f.addressID = stringValue(t, object(t, address["data"])["id"])
	return f
}

func (f *l2AcceptanceFixture) routerWithConfig(cfg config.Config) *gin.Engine {
	return NewRouter(Dependencies{
		Config: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: f.db, Redis: f.redis,
		CustomerLBSProvider: f.lbsProvider,
	})
}

// loginCustomer 登录测试客户。
func (f *l2AcceptanceFixture) loginCustomer(t *testing.T, prefix string) (string, uint64) {
	t.Helper()
	phone := fmt.Sprintf("%s%08d", prefix, time.Now().UnixNano()%100000000)
	_, customerID := seedCustomerReadyForSMSLogin(t, f.db, f.cfg, phone)
	performOK(t, f.router, http.MethodPost, "/api/v1/auth/customer/send-code", "", "", map[string]any{"phone": phone})
	login := performOK(t, f.router, http.MethodPost, "/api/v1/auth/customer/sms-login", "", "", map[string]any{"phone": phone, "code": "123456"})
	return stringValue(t, object(t, login["data"])["access_token"]), customerID
}

// tokenFromLogin 从登录响应中提取令牌。
func tokenFromLogin(t *testing.T, router http.Handler, path string, body any) string {
	t.Helper()
	login := performOK(t, router, http.MethodPost, path, "", "", body)
	return stringValue(t, object(t, login["data"])["access_token"])
}

// key 返回密钥。
func (f *l2AcceptanceFixture) key(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s-%d", prefix, f.seq)
}

// subtest 运行子测试。
func (f *l2AcceptanceFixture) subtest(t *testing.T, fn func(*testing.T)) {
	t.Helper()
	f.seq++
	name := fmt.Sprintf("acc_%d", f.seq)
	if err := f.db.SavePoint(name).Error; err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	defer func() {
		_ = f.db.RollbackTo(name).Error
	}()
	fn(t)
}

// expectProblem 断言问题详情响应。
func expectProblem(t *testing.T, status int, response map[string]any, wantStatus int, wantCode string) {
	t.Helper()
	if status != wantStatus {
		t.Fatalf("expected status %d, got %d: %#v", wantStatus, status, response)
	}
	code, _ := response["error_code"].(string)
	if code != wantCode {
		t.Fatalf("expected error_code %s, got %q: %#v", wantCode, code, response)
	}
}

// performStatusOnly 执行请求并只返回状态码。
func performStatusOnly(t *testing.T, handler http.Handler, method, path, token, idempotencyKey string) int {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder.Code
}

// containsID 判断集合是否包含指定 ID。
func containsID(items []any, field, id string) bool {
	for _, item := range items {
		row, _ := item.(map[string]any)
		if value, _ := row[field].(string); value == id {
			return true
		}
	}
	return false
}

// uniqueName 返回唯一值 Name。
func uniqueName(prefix string) string {
	return strings.ReplaceAll(fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()), " ", "-")
}
