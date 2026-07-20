package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/amap"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
)

func TestCustomerLBSAnonymousContextFlowIntegration(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run customer LBS integration test")
	}
	ctx := context.Background()
	cfg := config.Load()
	cfg.CustomerLBS.Mode = "enforce"
	cfg.CustomerLBS.Provider = "fake"
	cfg.CustomerLBS.RegeocodeEnabled = true
	cfg.CustomerLBS.RouteRefineEnabled = true
	cfg.CustomerLBS.CacheHMACSecret = "integration-customer-lbs-hmac-secret-123456789"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := mysql.Open(ctx, cfg.MySQL, log)
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	tx := db.Begin()
	defer tx.Rollback()

	redisClient := goredis.NewClient(&goredis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: 15})
	if err := redisClient.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping redis: %v", err)
	}
	defer redisClient.Close()
	if err := redisClient.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flush redis: %v", err)
	}
	defer redisClient.FlushDB(ctx)

	router := NewRouter(Dependencies{Config: cfg, Log: log, DB: tx, Redis: redisClient, CustomerLBSProvider: amap.NewFakeProvider()})
	session := "anonymous-lbs-session-0001"
	resolved := lbsHTTP(t, router, http.MethodPost, "/api/v1/location-contexts", map[string]string{"X-Session-ID": session}, map[string]any{
		"source": "device_location", "latitude": 22.541, "longitude": 113.931,
		"coordinate_system": "gcj02", "accuracy_m": 20, "captured_at": time.Now().Format(time.RFC3339Nano),
	})
	if resolved.status != http.StatusOK {
		t.Fatalf("resolve status=%d body=%v", resolved.status, resolved.body)
	}
	data := object(t, resolved.body["data"])
	contextID := stringValue(t, data["location_context_id"])
	shopID := stringValue(t, object(t, data["service_shop"])["id"])
	if contextID == "" || shopID != "4201" {
		t.Fatalf("unexpected resolved context: %v", data)
	}

	home := lbsHTTP(t, router, http.MethodGet, "/api/v1/home", map[string]string{"X-Session-ID": session, "X-Location-Context": contextID}, nil)
	if home.status != http.StatusOK || stringValue(t, object(t, object(t, home.body["data"])["service_shop"])["id"]) != shopID {
		t.Fatalf("context home status=%d body=%v", home.status, home.body)
	}
	crossSession := lbsHTTP(t, router, http.MethodGet, "/api/v1/location-contexts/"+contextID+"/service-shops", map[string]string{"X-Session-ID": "anonymous-lbs-session-0002"}, nil)
	if crossSession.status != http.StatusForbidden || crossSession.body["error_code"] != "LOCATION_CONTEXT_FORBIDDEN" {
		t.Fatalf("cross-session context was not rejected: status=%d body=%v", crossSession.status, crossSession.body)
	}

	headers := map[string]string{"X-Session-ID": session, "Idempotency-Key": "switch-shop-key-0001"}
	switchBody := map[string]any{"shop_id": shopID, "expected_version": 1}
	switched := lbsHTTP(t, router, http.MethodPut, "/api/v1/location-contexts/"+contextID+"/service-shop", headers, switchBody)
	replayed := lbsHTTP(t, router, http.MethodPut, "/api/v1/location-contexts/"+contextID+"/service-shop", headers, switchBody)
	if switched.status != http.StatusOK || replayed.status != http.StatusOK || !reflect.DeepEqual(switched.body["data"], replayed.body["data"]) {
		t.Fatalf("switch replay mismatch: first=%d %v replay=%d %v", switched.status, switched.body, replayed.status, replayed.body)
	}
	if object(t, switched.body["data"])["version"] != float64(2) {
		t.Fatalf("switch did not increment context version: %v", switched.body)
	}
}

type lbsHTTPResult struct {
	status int
	body   map[string]any
}

func lbsHTTP(t *testing.T, handler http.Handler, method, path string, headers map[string]string, body any) lbsHTTPResult {
	t.Helper()
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	result := lbsHTTPResult{status: recorder.Code, body: map[string]any{}}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result.body); err != nil {
		t.Fatalf("decode response %s %s: %v body=%s", method, path, err, recorder.Body.String())
	}
	return result
}
