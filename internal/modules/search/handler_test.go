package search

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	goredis "github.com/redis/go-redis/v9"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestDiscoveryHandlerGuestAndCustomerIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := searchTestDB(t)
	if err := db.AutoMigrate(&searchConfigRow{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&[]searchConfigRow{
		{ID: 1, ConfigKey: blocklistConfig, ConfigValue: `[]`, Status: "active"},
		{ID: 2, ConfigKey: defaultConfig, ConfigValue: `["默认词"]`, Status: "active"},
	}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	if err := db.Create(&[]History{
		{ID: 1, CustomerID: 42, Keyword: "本人历史", NormalizedKeyword: "本人历史", SearchCount: 1, LastSearchedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: 2, CustomerID: 43, Keyword: "他人历史", NormalizedKeyword: "他人历史", SearchCount: 1, LastSearchedAt: now, CreatedAt: now, UpdatedAt: now},
	}).Error; err != nil {
		t.Fatal(err)
	}
	redisServer := miniredis.RunT(t)
	redisClient := goredis.NewClient(&goredis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	service := NewService(config.SearchConfig{HistoryMax: 20, HistoryRetention: 180 * 24 * time.Hour, HotWindowDays: 7, HotCacheTTL: 5 * time.Minute}, db, redisClient, snowflake.New(906), nil, nil)
	service.now = func() time.Time { return now }
	handler := NewHandler(service)

	guestRouter := gin.New()
	RegisterPublicRoutes(guestRouter.Group("/api/v1"), handler)
	guestResponse := performSearchRequest(guestRouter, http.MethodGet, "/api/v1/search/discovery", "", nil)
	if guestResponse.Code != http.StatusOK {
		t.Fatalf("guest discovery status=%d body=%s", guestResponse.Code, guestResponse.Body.String())
	}
	var guestBody struct {
		Data DiscoveryResponse `json:"data"`
	}
	if err := json.Unmarshal(guestResponse.Body.Bytes(), &guestBody); err != nil {
		t.Fatal(err)
	}
	if len(guestBody.Data.History) != 0 || len(guestBody.Data.HotKeywords) != 1 || guestBody.Data.HotKeywords[0].Keyword != "默认词" {
		t.Fatalf("unexpected guest discovery: %#v", guestBody.Data)
	}

	customerRouter := gin.New()
	customerRouter.Use(func(c *gin.Context) {
		c.Set("auth_claims", &auth.Claims{AccountType: "customer", CustomerID: "42"})
		c.Next()
	})
	RegisterPublicRoutes(customerRouter.Group("/api/v1"), handler)
	customerResponse := performSearchRequest(customerRouter, http.MethodGet, "/api/v1/search/discovery?history_limit=20", "", nil)
	if customerResponse.Code != http.StatusOK {
		t.Fatalf("customer discovery status=%d body=%s", customerResponse.Code, customerResponse.Body.String())
	}
	var customerBody struct {
		Data DiscoveryResponse `json:"data"`
	}
	if err := json.Unmarshal(customerResponse.Body.Bytes(), &customerBody); err != nil {
		t.Fatal(err)
	}
	if len(customerBody.Data.History) != 1 || customerBody.Data.History[0].Keyword != "本人历史" {
		t.Fatalf("customer history leaked or missing: %#v", customerBody.Data.History)
	}

	invalidLimit := performSearchRequest(guestRouter, http.MethodGet, "/api/v1/search/discovery?hot_limit=21", "", nil)
	if invalidLimit.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid limit 400, got %d body=%s", invalidLimit.Code, invalidLimit.Body.String())
	}
}

func TestWriteHandlersRequireCustomerClaims(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := NewService(config.SearchConfig{HistoryMax: 20, EventRatePerMinute: 30}, nil, nil, snowflake.New(907), nil, nil)
	handler := NewHandler(service)

	guestRouter := gin.New()
	RegisterCustomerRoutes(guestRouter.Group("/api/v1"), handler)
	guest := performSearchRequest(guestRouter, http.MethodPost, "/api/v1/search/events", `{"keyword":"白酒","source":"manual"}`, map[string]string{"Idempotency-Key": "event-key-0001"})
	if guest.Code != http.StatusUnauthorized {
		t.Fatalf("expected guest write 401, got %d body=%s", guest.Code, guest.Body.String())
	}

	adminRouter := gin.New()
	adminRouter.Use(func(c *gin.Context) {
		c.Set("auth_claims", &auth.Claims{AccountType: "admin", AdminUserID: "1"})
		c.Next()
	})
	RegisterCustomerRoutes(adminRouter.Group("/api/v1"), handler)
	admin := performSearchRequest(adminRouter, http.MethodPost, "/api/v1/search/events", `{"keyword":"白酒","source":"manual"}`, map[string]string{"Idempotency-Key": "event-key-0002"})
	if admin.Code != http.StatusForbidden {
		t.Fatalf("expected admin write 403, got %d body=%s", admin.Code, admin.Body.String())
	}
	clear := performSearchRequest(adminRouter, http.MethodDelete, "/api/v1/search/history", "", map[string]string{"Idempotency-Key": "clear-key-0002"})
	if clear.Code != http.StatusForbidden {
		t.Fatalf("expected admin clear 403, got %d body=%s", clear.Code, clear.Body.String())
	}
}

func performSearchRequest(handler http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	handler.ServeHTTP(recorder, request)
	return recorder
}
