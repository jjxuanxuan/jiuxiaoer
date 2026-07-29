package catalog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	mysqlinfra "jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type mysqlPackagePublishFixture struct {
	merchantID       uint64
	shopID           uint64
	categoryID       uint64
	productID        uint64
	shopProductID    uint64
	actorID          uint64
	adminAccountID   uint64
	adminRoleID      uint64
	rolePermissionID uint64
	packageCode      string
}

func openPackageMySQLAcceptance(
	t *testing.T,
) (context.Context, *gorm.DB) {
	t.Helper()
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run wine-ticket catalog MySQL acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cfg := config.Load()
	cfg.MySQL.Required = true
	cfg.MySQL.RequiredTimeZone = "+08:00"
	cfg.MySQL.RequireWineTicketSchema = true
	cfg.MySQL.RequireWineTicketMoneyContract = false
	if cfg.MySQL.MaxOpenConns < 8 {
		cfg.MySQL.MaxOpenConns = 8
	}
	if cfg.MySQL.MaxIdleConns < 4 {
		cfg.MySQL.MaxIdleConns = 4
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := mysqlinfra.Open(ctx, cfg.MySQL, log)
	if err != nil || db == nil {
		t.Fatalf("open schema- and timezone-verified mysql: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get mysql pool: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return ctx, db
}

func newMySQLPackagePublishFixture(
	t *testing.T,
	db *gorm.DB,
	ids *snowflake.Generator,
) mysqlPackagePublishFixture {
	t.Helper()
	fixture := mysqlPackagePublishFixture{
		merchantID:       ids.Next(),
		shopID:           ids.Next(),
		categoryID:       ids.Next(),
		productID:        ids.Next(),
		shopProductID:    ids.Next(),
		actorID:          ids.Next(),
		adminAccountID:   ids.Next(),
		adminRoleID:      ids.Next(),
		rolePermissionID: ids.Next(),
	}
	fixture.packageCode = fmt.Sprintf("SOA019_%d", ids.Next())
	t.Cleanup(func() {
		cleanupMySQLPackagePublishFixture(t, db, fixture)
	})

	steps := []struct {
		name  string
		table string
		row   map[string]any
	}{
		{
			name:  "merchant",
			table: "merchants",
			row: map[string]any{
				"id": fixture.merchantID, "code": fmt.Sprintf("soa019-%d", fixture.merchantID),
				"name": "SOA019 验收商户", "status": "active", "review_status": "approved",
			},
		},
		{
			name:  "shop",
			table: "shops",
			row: map[string]any{
				"id": fixture.shopID, "merchant_id": fixture.merchantID,
				"name": "SOA019 验收门店", "city": "上海市", "district": "浦东新区",
				"address": "SOA019 验收地址", "status": "active", "business_status": "open",
			},
		},
		{
			name:  "category",
			table: "categories",
			row: map[string]any{
				"id": fixture.categoryID, "name": "SOA019 酒类",
				"status": "active", "age_restricted": true,
			},
		},
		{
			name:  "product",
			table: "products",
			row: map[string]any{
				"id": fixture.productID, "category_id": fixture.categoryID,
				"name": "SOA019 验收葡萄酒", "status": "on_sale", "age_restricted": true,
				"sale_price_amount": int64(59900), "original_price_amount": int64(69900),
			},
		},
		{
			name:  "shop product",
			table: "shop_products",
			row: map[string]any{
				"id": fixture.shopProductID, "merchant_id": fixture.merchantID,
				"shop_id": fixture.shopID, "product_id": fixture.productID,
				"sale_price_amount": int64(59900), "status": "on_sale",
			},
		},
	}
	for _, step := range steps {
		if err := db.Table(step.table).Create(step.row).Error; err != nil {
			t.Fatalf("seed mysql package %s: %v", step.name, err)
		}
	}
	if err := db.Table("accounts").Create(map[string]any{
		"id":           fixture.adminAccountID,
		"account_type": "admin",
		"username":     fmt.Sprintf("soa019_admin_%d", fixture.actorID),
		"status":       "active",
	}).Error; err != nil {
		t.Fatalf("seed mysql package admin account: %v", err)
	}
	if err := db.Table("roles").Create(map[string]any{
		"id":     fixture.adminRoleID,
		"code":   fmt.Sprintf("soa019_role_%d", fixture.adminRoleID),
		"name":   "SOA019 验收角色",
		"scope":  "all",
		"status": "active",
	}).Error; err != nil {
		t.Fatalf("seed mysql package admin role: %v", err)
	}
	if err := db.Table("admin_users").Create(map[string]any{
		"id":             fixture.actorID,
		"account_id":     fixture.adminAccountID,
		"role_id":        fixture.adminRoleID,
		"admin_sub_role": "operation",
		"name":           "SOA019 验收管理员",
		"status":         "active",
	}).Error; err != nil {
		t.Fatalf("seed mysql package admin user: %v", err)
	}
	permissionResult := db.Exec(
		`INSERT INTO role_permissions (id,role_id,permission_id)
		 SELECT ?,?,p.id
		   FROM permissions p
		  WHERE p.code='wine_ticket_package:publish'
		    AND p.status='active'
		    AND p.deleted_at IS NULL`,
		fixture.rolePermissionID,
		fixture.adminRoleID,
	)
	if permissionResult.Error != nil || permissionResult.RowsAffected != 1 {
		t.Fatalf(
			"seed mysql package live permission rows=%d err=%v",
			permissionResult.RowsAffected,
			permissionResult.Error,
		)
	}
	return fixture
}

func cleanupMySQLPackagePublishFixture(
	t *testing.T,
	db *gorm.DB,
	fixture mysqlPackagePublishFixture,
) {
	t.Helper()
	steps := []struct {
		name  string
		query *gorm.DB
	}{
		{
			name: "idempotency",
			query: db.Exec(
				"DELETE FROM idempotency_keys WHERE actor_type = 'admin' AND actor_id = ?",
				fixture.actorID,
			),
		},
		{
			name: "audits",
			query: db.Exec(
				`DELETE FROM audit_logs
				  WHERE resource_type = 'wine_ticket_package'
				    AND resource_id IN (
				      SELECT id FROM wine_ticket_packages WHERE package_code = ?
				    )`,
				fixture.packageCode,
			),
		},
		{
			name:  "packages",
			query: db.Exec("DELETE FROM wine_ticket_packages WHERE package_code = ?", fixture.packageCode),
		},
		{
			name:  "shop product",
			query: db.Exec("DELETE FROM shop_products WHERE id = ?", fixture.shopProductID),
		},
		{
			name:  "product",
			query: db.Exec("DELETE FROM products WHERE id = ?", fixture.productID),
		},
		{
			name:  "category",
			query: db.Exec("DELETE FROM categories WHERE id = ?", fixture.categoryID),
		},
		{
			name:  "shop",
			query: db.Exec("DELETE FROM shops WHERE id = ?", fixture.shopID),
		},
		{
			name:  "merchant",
			query: db.Exec("DELETE FROM merchants WHERE id = ?", fixture.merchantID),
		},
		{
			name:  "role permission",
			query: db.Exec("DELETE FROM role_permissions WHERE id = ?", fixture.rolePermissionID),
		},
		{
			name:  "admin user",
			query: db.Exec("DELETE FROM admin_users WHERE id = ?", fixture.actorID),
		},
		{
			name:  "admin role",
			query: db.Exec("DELETE FROM roles WHERE id = ?", fixture.adminRoleID),
		},
		{
			name:  "admin account",
			query: db.Exec("DELETE FROM accounts WHERE id = ?", fixture.adminAccountID),
		},
	}
	for _, step := range steps {
		if step.query.Error != nil {
			t.Errorf("cleanup mysql package %s: %v", step.name, step.query.Error)
		}
	}
}

// ACC-SOA-019：同一 package_code 的两个草稿并发发布时，真实 MySQL
// 唯一约束和服务事务共同保证最多一个 published 版本。
func TestMySQLPackageSameCodeConcurrentPublishHasSingleWinner(t *testing.T) {
	ctx, db := openPackageMySQLAcceptance(t)
	ids := snowflake.New(949)
	fixture := newMySQLPackagePublishFixture(t, db, ids)
	service := NewService(db, ids)
	claims := packageAdminClaimsFor(
		idString(fixture.actorID),
		"wine_ticket_package:create",
		"wine_ticket_package:publish",
	)

	request := validPackageWriteRequest()
	request.PackageCode = fixture.packageCode
	request.IssuerMerchantID = idString(fixture.merchantID)
	request.SettlementShopID = idString(fixture.shopID)
	request.SettlementShopProductID = idString(fixture.shopProductID)
	request.ProductID = idString(fixture.productID)

	drafts := make([]AdminPackageDTO, 2)
	for index := range drafts {
		created, err := service.CreateAdminPackage(
			ctx,
			claims,
			http.MethodPost,
			"/admin/wine-tickets/packages",
			fmt.Sprintf("soa019-create-%d-%d", fixture.actorID, index),
			request,
		)
		if err != nil {
			t.Fatalf("create mysql package draft %d: %v", index, err)
		}
		if created.Status != PackageStatusDraft {
			t.Fatalf("created package %d status=%q, want draft", index, created.Status)
		}
		drafts[index] = created
	}

	start := make(chan struct{})
	results := make([]error, len(drafts))
	var wait sync.WaitGroup
	wait.Add(len(drafts))
	for index := range drafts {
		go func(index int) {
			defer wait.Done()
			<-start
			_, results[index] = service.PublishAdminPackage(
				ctx,
				claims,
				http.MethodPost,
				"/admin/wine-tickets/packages/:package_no/publish",
				fmt.Sprintf("soa019-publish-%d-%d", fixture.actorID, index),
				drafts[index].PackageNo,
				ExpectedVersionRequest{ExpectedVersion: drafts[index].Version},
			)
		}(index)
	}
	close(start)
	wait.Wait()

	successes, conflicts := 0, 0
	for _, resultErr := range results {
		if resultErr == nil {
			successes++
			continue
		}
		details := problem.FromError(resultErr)
		if details.Status != http.StatusConflict ||
			(details.ErrorCode != "WT_CONCURRENT_MODIFICATION" &&
				details.ErrorCode != "WT_PACKAGE_NOT_AVAILABLE") {
			t.Fatalf("concurrent publish returned unstable error: %v", resultErr)
		}
		conflicts++
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("publish results successes=%d conflicts=%d, want 1/1", successes, conflicts)
	}

	var rows []Package
	if err := db.
		Where("package_code = ? AND deleted_at IS NULL", fixture.packageCode).
		Order("package_version ASC").
		Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("package rows=%d, want 2", len(rows))
	}
	published := 0
	for _, row := range rows {
		if row.Status == PackageStatusPublished {
			published++
		}
	}
	if published != 1 {
		t.Fatalf("published package rows=%d, want 1", published)
	}

	var publishAudits []packageTestAudit
	if err := db.
		Where(
			"resource_type = ? AND action = ? AND resource_id IN ?",
			"wine_ticket_package",
			"wine_ticket.package.publish",
			[]uint64{rows[0].ID, rows[1].ID},
		).
		Find(&publishAudits).Error; err != nil {
		t.Fatal(err)
	}
	if len(publishAudits) != 1 ||
		publishAudits[0].Result != "success" ||
		publishAudits[0].AfterStatus == nil ||
		*publishAudits[0].AfterStatus != PackageStatusPublished {
		t.Fatalf("publish audits=%+v, want one successful published audit", publishAudits)
	}
}
