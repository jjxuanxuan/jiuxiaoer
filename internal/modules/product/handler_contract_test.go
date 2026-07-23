package product

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestProductListRejectsUndocumentedQueryParameters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, target := range []string{
		"/api/v1/products?order_by=id%20desc",
		"/api/v1/products?filter=status:on_sale",
		"/api/v1/products?unknown=value",
	} {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodGet, target, nil)
		if err := validateProductListQuery(ctx); problem.FromError(err).ErrorCode != "VALIDATION_INVALID_QUERY" {
			t.Fatalf("undocumented product query accepted for %s: %v", target, err)
		}
	}
}

func TestCategoryListUsesNonPaginatedItemsEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dsn := fmt.Sprintf("file:category_handler_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Category{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Category{ID: 1, Name: "酒类", Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/categories", nil)
	NewHandler(NewService(db, nil)).ListCategories(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("category status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body.Data["items"]; !ok {
		t.Fatalf("category response omitted items: %s", recorder.Body.String())
	}
	if _, ok := body.Data["next_page_token"]; ok {
		t.Fatalf("category response unexpectedly exposed pagination: %s", recorder.Body.String())
	}
}
