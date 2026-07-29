package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	mysqlerr "github.com/go-sql-driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type packageTestMerchant struct {
	ID           uint64 `gorm:"primaryKey"`
	Status       string
	ReviewStatus string
	DeletedAt    *time.Time
}

func (packageTestMerchant) TableName() string { return "merchants" }

type packageTestShop struct {
	ID             uint64 `gorm:"primaryKey"`
	MerchantID     uint64
	Status         string
	BusinessStatus string
	DeletedAt      *time.Time
}

func (packageTestShop) TableName() string { return "shops" }

type packageTestProduct struct {
	ID            uint64 `gorm:"primaryKey"`
	CategoryID    uint64
	Name          string
	BrandName     *string
	Spec          *string
	ImageURL      *string
	Status        string
	AgeRestricted bool
	DeletedAt     *time.Time
}

func (packageTestProduct) TableName() string { return "products" }

type packageTestCategory struct {
	ID            uint64 `gorm:"primaryKey"`
	Status        string
	AgeRestricted bool
	DeletedAt     *time.Time
}

func (packageTestCategory) TableName() string { return "categories" }

type packageTestShopProduct struct {
	ID         uint64 `gorm:"primaryKey"`
	MerchantID uint64
	ShopID     uint64
	ProductID  uint64
	Status     string
	DeletedAt  *time.Time
}

func (packageTestShopProduct) TableName() string { return "shop_products" }

type packageTestAudit struct {
	ID           uint64 `gorm:"primaryKey"`
	AccountID    *uint64
	ActorType    string
	ActorID      uint64
	Action       string
	ResourceType string
	ResourceID   uint64
	BeforeData   []byte
	AfterData    []byte
	Result       string
	ErrorCode    *string
	BeforeStatus *string
	AfterStatus  *string
	Version      uint64
	RequestID    *string
	IPHash       *string
	UserAgent    *string
	CreatedAt    time.Time
}

func (packageTestAudit) TableName() string { return "audit_logs" }

func TestPackageLifecycleVersioningVisibilityAndIdempotency(t *testing.T) {
	service, db := newPackageTestService(t)
	seedValidSettlement(t, db)
	claims := packageAdminClaims(
		"wine_ticket_package:create",
		"wine_ticket_package:update",
		"wine_ticket_package:list",
		"wine_ticket_package:view",
	)
	publisher := packageAdminClaimsFor(
		"9002",
		"wine_ticket_package:publish",
		"wine_ticket_package:unpublish",
	)
	ctx := context.Background()
	req := validPackageWriteRequest()

	created, err := service.CreateAdminPackage(ctx, claims, "POST", "/admin/wine-tickets/packages", "create-key-0001", req)
	if err != nil {
		t.Fatalf("create package: %v", err)
	}
	if created.Status != PackageStatusDraft || created.PackageVersion != 1 || created.Version != 1 {
		t.Fatalf("unexpected created package: %+v", created)
	}
	if created.PackageCode != req.PackageCode || created.IssuerMerchantID != "101" || created.RefundPolicy.SchemaVersion != 1 {
		t.Fatalf("admin DTO is incomplete: %+v", created)
	}
	if !strings.HasSuffix(created.CreatedAt, "+08:00") || !strings.HasSuffix(created.UpdatedAt, "+08:00") {
		t.Fatalf("timestamps must be explicit Shanghai offsets: created=%s updated=%s", created.CreatedAt, created.UpdatedAt)
	}

	replayed, err := service.CreateAdminPackage(ctx, claims, "POST", "/admin/wine-tickets/packages", "create-key-0001", req)
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if replayed.PackageNo != created.PackageNo || replayed.PackageVersion != created.PackageVersion {
		t.Fatalf("replay created another resource: first=%+v replay=%+v", created, replayed)
	}

	second, err := service.CreateAdminPackage(ctx, claims, "POST", "/admin/wine-tickets/packages", "create-key-0002", req)
	if err != nil {
		t.Fatalf("create second business version: %v", err)
	}
	if second.PackageVersion != 2 || second.PackageNo == created.PackageNo {
		t.Fatalf("same code must allocate next business version: %+v", second)
	}

	published, err := service.PublishAdminPackage(ctx, publisher, "POST", "/admin/wine-tickets/packages/:package_no/publish", "publish-key-01", created.PackageNo, ExpectedVersionRequest{ExpectedVersion: created.Version})
	if err != nil {
		t.Fatalf("publish package: %v", err)
	}
	if published.Status != PackageStatusPublished || published.Version != 2 || published.PublishedAt == nil || published.PublishedBy == nil {
		t.Fatalf("unexpected published package: %+v", published)
	}
	replayedPublish, err := service.PublishAdminPackage(
		ctx,
		publisher,
		"POST",
		"/admin/wine-tickets/packages/:package_no/publish",
		"publish-key-01",
		created.PackageNo,
		ExpectedVersionRequest{ExpectedVersion: created.Version},
	)
	if err != nil ||
		replayedPublish.PackageNo != published.PackageNo ||
		replayedPublish.Version != published.Version ||
		replayedPublish.Status != published.Status {
		t.Fatalf(
			"idempotent publish replay=%+v err=%v, want %+v",
			replayedPublish,
			err,
			published,
		)
	}

	_, err = service.PublishAdminPackage(ctx, publisher, "POST", "/admin/wine-tickets/packages/:package_no/publish", "publish-key-02", second.PackageNo, ExpectedVersionRequest{ExpectedVersion: second.Version})
	assertProblemCode(t, err, "WT_PACKAGE_NOT_AVAILABLE")

	public, err := service.PublicPackage(ctx, created.PackageNo)
	if err != nil {
		t.Fatalf("public detail: %v", err)
	}
	payload, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	for _, forbidden := range []string{"package_code", "issuer_merchant_id", "settlement_shop_id", "settlement_shop_product_id", "per_customer_limit", `"refund_policy":`, `"renewal_policy":`, `"delivery_policy":`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("public DTO leaked %q: %s", forbidden, body)
		}
	}
	if public.Product.ProductID != "401" || public.Product.Name != "测试葡萄酒" {
		t.Fatalf("public product projection mismatch: %+v", public.Product)
	}

	updateReq := req
	updateReq.ExpectedVersion = uintPtr(published.Version)
	_, err = service.UpdateAdminPackage(ctx, claims, "PUT", "/admin/wine-tickets/packages/:package_no", "update-key-001", created.PackageNo, updateReq)
	assertProblemCode(t, err, "WT_PACKAGE_NOT_AVAILABLE")

	unpublished, err := service.UnpublishAdminPackage(ctx, publisher, "POST", "/admin/wine-tickets/packages/:package_no/unpublish", "unpublish-001", created.PackageNo, ExpectedVersionRequest{ExpectedVersion: published.Version})
	if err != nil {
		t.Fatalf("unpublish package: %v", err)
	}
	if unpublished.Status != PackageStatusUnpublished || unpublished.Version != 3 {
		t.Fatalf("unexpected unpublished package: %+v", unpublished)
	}
	_, err = service.PublicPackage(ctx, created.PackageNo)
	assertProblemCode(t, err, "WT_PACKAGE_NOT_FOUND")
	_, err = service.PublishAdminPackage(ctx, publisher, "POST", "/admin/wine-tickets/packages/:package_no/publish", "republish-001", created.PackageNo, ExpectedVersionRequest{ExpectedVersion: unpublished.Version})
	assertProblemCode(t, err, "WT_PACKAGE_NOT_AVAILABLE")

	var auditCount int64
	if err := db.Model(&packageTestAudit{}).Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 4 {
		t.Fatalf("audit row count=%d, want create/create/publish/unpublish = 4", auditCount)
	}
}

func TestPackageDraftUpdateUsesCASAndPreservesBusinessVersion(t *testing.T) {
	service, _ := newPackageTestService(t)
	claims := packageAdminClaims("wine_ticket_package:create", "wine_ticket_package:update")
	ctx := context.Background()
	req := validPackageWriteRequest()
	created, err := service.CreateAdminPackage(ctx, claims, "POST", "/admin/wine-tickets/packages", "draft-create-01", req)
	if err != nil {
		t.Fatal(err)
	}

	changed := req
	changed.Name = "更新后的囤酒套餐"
	changed.ExpectedVersion = uintPtr(created.Version)
	updated, err := service.UpdateAdminPackage(ctx, claims, "PUT", "/admin/wine-tickets/packages/:package_no", "draft-update-01", created.PackageNo, changed)
	if err != nil {
		t.Fatalf("update draft: %v", err)
	}
	if updated.Name != changed.Name || updated.PackageVersion != created.PackageVersion || updated.Version != created.Version+1 {
		t.Fatalf("draft update broke version semantics: created=%+v updated=%+v", created, updated)
	}

	changed.ExpectedVersion = uintPtr(created.Version)
	_, err = service.UpdateAdminPackage(ctx, claims, "PUT", "/admin/wine-tickets/packages/:package_no", "draft-update-02", created.PackageNo, changed)
	assertProblemCode(t, err, "WT_CONCURRENT_MODIFICATION")

	changed.ExpectedVersion = uintPtr(updated.Version)
	changed.PackageCode = "ANOTHER_CODE"
	_, err = service.UpdateAdminPackage(ctx, claims, "PUT", "/admin/wine-tickets/packages/:package_no", "draft-update-03", created.PackageNo, changed)
	assertProblemCode(t, err, "VALIDATION_FAILED")
}

func TestPackageCreatorAndLastEditorCanPublishDirectly(t *testing.T) {
	service, db := newPackageTestService(t)
	seedValidSettlement(t, db)
	ctx := requestctx.WithRequestID(
		context.Background(),
		"package-direct-publish-request",
	)
	creator := packageAdminClaimsFor(
		"9001",
		"wine_ticket_package:create",
		"wine_ticket_package:publish",
	)
	editor := packageAdminClaimsFor(
		"9002",
		"wine_ticket_package:update",
		"wine_ticket_package:publish",
	)

	creatorRequest := validPackageWriteRequest()
	creatorPackage, err := service.CreateAdminPackage(
		ctx,
		creator,
		"POST",
		"/admin/wine-tickets/packages",
		"direct-creator-create-01",
		creatorRequest,
	)
	if err != nil {
		t.Fatal(err)
	}
	publishedByCreator, err := service.PublishAdminPackage(
		ctx,
		creator,
		"POST",
		"/admin/wine-tickets/packages/:package_no/publish",
		"direct-creator-publish-01",
		creatorPackage.PackageNo,
		ExpectedVersionRequest{ExpectedVersion: creatorPackage.Version},
	)
	if err != nil {
		t.Fatalf("creator could not publish own package: %v", err)
	}
	if publishedByCreator.PublishedBy == nil ||
		*publishedByCreator.PublishedBy != "9001" {
		t.Fatalf(
			"published_by=%v, want creator 9001",
			publishedByCreator.PublishedBy,
		)
	}

	editorRequest := validPackageWriteRequest()
	editorRequest.PackageCode = "DIRECT_EDITOR_PACKAGE"
	editorPackage, err := service.CreateAdminPackage(
		ctx,
		creator,
		"POST",
		"/admin/wine-tickets/packages",
		"direct-editor-create-01",
		editorRequest,
	)
	if err != nil {
		t.Fatal(err)
	}
	changed := editorRequest
	changed.Name = "经第二位运营编辑的囤酒套餐"
	changed.ExpectedVersion = uintPtr(editorPackage.Version)
	updated, err := service.UpdateAdminPackage(
		ctx,
		editor,
		"PUT",
		"/admin/wine-tickets/packages/:package_no",
		"direct-editor-update-01",
		editorPackage.PackageNo,
		changed,
	)
	if err != nil {
		t.Fatal(err)
	}
	publishedByEditor, err := service.PublishAdminPackage(
		ctx,
		editor,
		"POST",
		"/admin/wine-tickets/packages/:package_no/publish",
		"direct-editor-publish-01",
		editorPackage.PackageNo,
		ExpectedVersionRequest{ExpectedVersion: updated.Version},
	)
	if err != nil {
		t.Fatalf("last editor could not publish package: %v", err)
	}
	if publishedByEditor.PublishedBy == nil ||
		*publishedByEditor.PublishedBy != "9002" {
		t.Fatalf(
			"published_by=%v, want last editor 9002",
			publishedByEditor.PublishedBy,
		)
	}

	var publishAudits []packageTestAudit
	if err := db.
		Where(
			"resource_type = ? AND action = ?",
			"wine_ticket_package",
			"wine_ticket.package.publish",
		).
		Order("id ASC").
		Find(&publishAudits).Error; err != nil {
		t.Fatal(err)
	}
	if len(publishAudits) != 2 {
		t.Fatalf("publish audit count=%d, want 2", len(publishAudits))
	}
	publishKeys := []string{
		"direct-creator-publish-01",
		"direct-editor-publish-01",
	}
	for index, audit := range publishAudits {
		if audit.Result != "success" || audit.ErrorCode != nil {
			t.Fatalf("unexpected publish audit result: %+v", audit)
		}
		var before, after map[string]any
		if err := json.Unmarshal(audit.BeforeData, &before); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(audit.AfterData, &after); err != nil {
			t.Fatal(err)
		}
		beforeVersion, beforeVersionOK := before["version"].(float64)
		afterVersion, afterVersionOK := after["version"].(float64)
		if audit.BeforeStatus == nil || *audit.BeforeStatus != PackageStatusDraft ||
			audit.AfterStatus == nil || *audit.AfterStatus != PackageStatusPublished ||
			audit.RequestID == nil || *audit.RequestID != requestctx.RequestID(ctx) ||
			!beforeVersionOK || !afterVersionOK ||
			afterVersion != beforeVersion+1 ||
			audit.Version != uint64(afterVersion) ||
			before["status"] != PackageStatusDraft ||
			after["status"] != PackageStatusPublished ||
			after["permission"] != "wine_ticket_package:publish" ||
			after["request_id"] != requestctx.RequestID(ctx) ||
			after["correlation_id"] != requestctx.RequestID(ctx) ||
			after["idempotency_key_hash"] != idempotency.KeyHash(publishKeys[index]) ||
			after["service_instance"] != "catalog-audit-test" {
			t.Fatalf(
				"incomplete package publish audit=%+v before=%v after=%v",
				audit,
				before,
				after,
			)
		}
		if strings.Contains(string(audit.AfterData), "approval_evidence") {
			t.Fatalf("publish audit retained approval evidence: %s", audit.AfterData)
		}
	}
}

func TestPackagePublishRechecksLivePermissionBeforeIdempotency(t *testing.T) {
	service, db := newPackageTestService(t)
	seedValidSettlement(t, db)
	creator := packageAdminClaimsFor(
		"9001",
		"wine_ticket_package:create",
		"wine_ticket_package:publish",
	)
	created, err := service.CreateAdminPackage(
		context.Background(),
		creator,
		"POST",
		"/admin/wine-tickets/packages",
		"live-permission-create-01",
		validPackageWriteRequest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		UPDATE role_permissions
		   SET deleted_at = CURRENT_TIMESTAMP
		 WHERE role_id = 9900
		   AND permission_id = 2164
	`).Error; err != nil {
		t.Fatal(err)
	}

	publishKey := "live-permission-publish-01"
	_, err = service.PublishAdminPackage(
		context.Background(),
		creator,
		"POST",
		"/admin/wine-tickets/packages/:package_no/publish",
		publishKey,
		created.PackageNo,
		ExpectedVersionRequest{ExpectedVersion: created.Version},
	)
	if problem.FromError(err).ErrorCode != "PERM_FORBIDDEN" {
		t.Fatalf("revoked live publish permission error=%v", err)
	}
	var stored Package
	if err := db.Where("package_no = ?", created.PackageNo).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != PackageStatusDraft || stored.Version != created.Version {
		t.Fatalf("revoked publish changed package=%+v", stored)
	}
	var idempotencyCount, auditCount int64
	if err := db.Model(&idempotency.Record{}).
		Where("key_hash = ?", idempotency.KeyHash(publishKey)).
		Count(&idempotencyCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&packageTestAudit{}).
		Where(
			"resource_type = ? AND resource_id = ? AND action = ?",
			"wine_ticket_package",
			stored.ID,
			"wine_ticket.package.publish",
		).
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if idempotencyCount != 0 || auditCount != 0 {
		t.Fatalf(
			"revoked publish created side effects idempotency=%d audit=%d",
			idempotencyCount,
			auditCount,
		)
	}
}

func TestPackageUnpublishRequiresDedicatedPermission(t *testing.T) {
	service, db := newPackageTestService(t)
	seedValidSettlement(t, db)
	ctx := context.Background()
	creator := packageAdminClaimsFor("9001", "wine_ticket_package:create")
	publisher := packageAdminClaimsFor("9002", "wine_ticket_package:publish")
	unpublisher := packageAdminClaimsFor("9003", "wine_ticket_package:unpublish")

	created, err := service.CreateAdminPackage(
		ctx,
		creator,
		"POST",
		"/admin/wine-tickets/packages",
		"permission-create-01",
		validPackageWriteRequest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	published, err := service.PublishAdminPackage(
		ctx,
		publisher,
		"POST",
		"/admin/wine-tickets/packages/:package_no/publish",
		"permission-publish-01",
		created.PackageNo,
		ExpectedVersionRequest{ExpectedVersion: created.Version},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.UnpublishAdminPackage(
		ctx,
		publisher,
		"POST",
		"/admin/wine-tickets/packages/:package_no/unpublish",
		"permission-unpublish-denied-01",
		created.PackageNo,
		ExpectedVersionRequest{ExpectedVersion: published.Version},
	)
	assertProblemCode(t, err, "PERM_FORBIDDEN")

	unpublished, err := service.UnpublishAdminPackage(
		ctx,
		unpublisher,
		"POST",
		"/admin/wine-tickets/packages/:package_no/unpublish",
		"permission-unpublish-allowed-01",
		created.PackageNo,
		ExpectedVersionRequest{ExpectedVersion: published.Version},
	)
	if err != nil {
		t.Fatalf("dedicated unpublish permission rejected: %v", err)
	}
	if unpublished.Status != PackageStatusUnpublished {
		t.Fatalf("unpublish status=%s", unpublished.Status)
	}
}

func TestPublishRejectsBrokenSettlementRelationshipAndStatus(t *testing.T) {
	service, db := newPackageTestService(t)
	seedValidSettlement(t, db)
	claims := packageAdminClaims("wine_ticket_package:create")
	publisher := packageAdminClaimsFor("9002", "wine_ticket_package:publish")
	req := validPackageWriteRequest()
	created, err := service.CreateAdminPackage(context.Background(), claims, "POST", "/admin/wine-tickets/packages", "broken-create-1", req)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.Model(&packageTestShopProduct{}).Where("id = ?", 301).Update("product_id", 999).Error; err != nil {
		t.Fatal(err)
	}
	_, err = service.PublishAdminPackage(context.Background(), publisher, "POST", "/admin/wine-tickets/packages/:package_no/publish", "broken-publish-1", created.PackageNo, ExpectedVersionRequest{ExpectedVersion: created.Version})
	assertProblemCode(t, err, "WT_PACKAGE_NOT_AVAILABLE")

	if err := db.Model(&packageTestShopProduct{}).Where("id = ?", 301).Updates(map[string]any{"product_id": 401, "status": "off_sale"}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = service.PublishAdminPackage(context.Background(), publisher, "POST", "/admin/wine-tickets/packages/:package_no/publish", "broken-publish-2", created.PackageNo, ExpectedVersionRequest{ExpectedVersion: created.Version})
	assertProblemCode(t, err, "WT_PACKAGE_NOT_AVAILABLE")
}

func TestPackageWriteValidationMatchesV1PolicyContract(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*PackageWriteRequest)
	}{
		{"min greater than max", func(req *PackageWriteRequest) { req.MinPurchaseQuantity = 3; req.MaxPurchaseQuantity = 2 }},
		{"customer limit below min", func(req *PackageWriteRequest) { value := uint(0); req.PerCustomerLimit = &value }},
		{"refund never-used false", func(req *PackageWriteRequest) { req.RefundPolicy.RequireNeverUsed = boolPtr(false) }},
		{"missing required false-capable field", func(req *PackageWriteRequest) { req.RenewalPolicy.MaxCount = nil }},
		{"delivery fee not included", func(req *PackageWriteRequest) { req.DeliveryPolicy.DeliveryFeeIncluded = boolPtr(false) }},
		{"bad external id", func(req *PackageWriteRequest) { req.ProductID = "0401" }},
		{"bad sale window", func(req *PackageWriteRequest) {
			req.SaleStartAt, req.SaleEndAt = stringPtr("2026-08-02T00:00:00Z"), stringPtr("2026-08-01T00:00:00Z")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := validPackageWriteRequest()
			test.mutate(&req)
			_, err := normalizePackageWrite(req, false)
			assertProblemCode(t, err, "VALIDATION_FAILED")
		})
	}

	req := validPackageWriteRequest()
	req.SaleStartAt = stringPtr("2026-08-01T00:00:00Z")
	req.SaleEndAt = stringPtr("2026-08-03T00:00:00+08:00")
	normalized, err := normalizePackageWrite(req, false)
	if err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	if normalized.SaleStartAt == nil || normalized.SaleStartAt.Format(time.RFC3339) != "2026-08-01T08:00:00+08:00" {
		t.Fatalf("input time was not normalized to Shanghai: %v", normalized.SaleStartAt)
	}
}

func TestPackageWriteDBErrorMapsMySQLConcurrencyFailures(t *testing.T) {
	for _, number := range []uint16{1062, 1205, 1213} {
		err := packageWriteDBError(&mysqlerr.MySQLError{
			Number:  number,
			Message: "injected package concurrency failure",
		})
		assertProblemCode(t, err, "WT_CONCURRENT_MODIFICATION")
	}
	infrastructureErr := errors.New("connection reset")
	if got := packageWriteDBError(infrastructureErr); got != infrastructureErr {
		t.Fatalf("unrelated infrastructure error was remapped: %v", got)
	}
}

func TestPublicListUsesSaleWindowAndStableIDCursor(t *testing.T) {
	service, db := newPackageTestService(t)
	seedValidSettlement(t, db)
	now := service.nowShanghai()
	req := validPackageWriteRequest()
	claims := packageAdminClaims("wine_ticket_package:create")
	publisher := packageAdminClaimsFor("9002", "wine_ticket_package:publish")

	first, err := service.CreateAdminPackage(context.Background(), claims, "POST", "/admin/wine-tickets/packages", "list-create-01", req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PublishAdminPackage(context.Background(), publisher, "POST", "/admin/wine-tickets/packages/:package_no/publish", "list-publish-01", first.PackageNo, ExpectedVersionRequest{ExpectedVersion: first.Version}); err != nil {
		t.Fatal(err)
	}

	// 不同套餐编码可以同时发布，并提供足够记录来验证 ID 游标。
	req.PackageCode = "GIFT_2026"
	req.PackageType = PackageTypeGift
	second, err := service.CreateAdminPackage(context.Background(), claims, "POST", "/admin/wine-tickets/packages", "list-create-02", req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PublishAdminPackage(context.Background(), publisher, "POST", "/admin/wine-tickets/packages/:package_no/publish", "list-publish-02", second.PackageNo, ExpectedVersionRequest{ExpectedVersion: second.Version}); err != nil {
		t.Fatal(err)
	}

	query := pagination.Query{PageSize: 1, TokenHash: "test"}
	items, next, err := service.ListPublicPackages(context.Background(), query, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || next == "" || items[0].PackageNo != second.PackageNo {
		t.Fatalf("unexpected first page: items=%+v next=%q", items, next)
	}

	var expired Package
	if err := db.Where("package_no = ?", first.PackageNo).Take(&expired).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&Package{}).Where("id = ?", expired.ID).Updates(map[string]any{
		"sale_start_at": now.Add(-2 * time.Hour),
		"sale_end_at":   now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = service.PublicPackage(context.Background(), first.PackageNo)
	assertProblemCode(t, err, "WT_PACKAGE_NOT_FOUND")
}

func newPackageTestService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	dsn := uniqueSQLiteMemoryDSN(t)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&Package{},
		&packageTestMerchant{},
		&packageTestShop{},
		&packageTestProduct{},
		&packageTestCategory{},
		&packageTestShopProduct{},
		&idempotency.Record{},
		&packageTestAudit{},
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE UNIQUE INDEX uk_test_idempotency ON idempotency_keys(actor_type, actor_id, path, key_hash)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE UNIQUE INDEX uk_test_package_code_version ON wine_ticket_packages(package_code, package_version)").Error; err != nil {
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
			(19001,'admin','active'),
			(19002,'admin','active'),
			(19003,'admin','active')`,
		`INSERT INTO roles (id,code,status) VALUES
			(9900,'catalog_test_operator','active')`,
		`INSERT INTO admin_users (id,account_id,role_id,status) VALUES
			(9001,19001,9900,'active'),
			(9002,19002,9900,'active'),
			(9003,19003,9900,'active')`,
		`INSERT INTO permissions (id,code,status) VALUES
			(2164,'wine_ticket_package:publish','active'),
			(2165,'wine_ticket_package:unpublish','active')`,
		`INSERT INTO role_permissions (id,role_id,permission_id) VALUES
			(9911,9900,2164),
			(9912,9900,2165)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("seed package live authorization: %v", err)
		}
	}
	service := NewService(db, snowflake.New(77)).
		WithInstance("catalog-audit-test")
	fixedNow := time.Date(2026, 7, 27, 15, 30, 0, 123000000, shanghaiLocation)
	service.now = func() time.Time { return fixedNow }
	return service, db
}

func seedValidSettlement(t *testing.T, db *gorm.DB) {
	t.Helper()
	brand, spec, image := "测试酒庄", "750ml", "https://example.com/wine.png"
	rows := []any{
		&packageTestMerchant{ID: 101, Status: "active", ReviewStatus: "approved"},
		&packageTestShop{ID: 201, MerchantID: 101, Status: "active", BusinessStatus: "open"},
		&packageTestCategory{ID: 501, Status: "active", AgeRestricted: true},
		&packageTestProduct{ID: 401, CategoryID: 501, Name: "测试葡萄酒", BrandName: &brand, Spec: &spec, ImageURL: &image, Status: "on_sale", AgeRestricted: false},
		&packageTestShopProduct{ID: 301, MerchantID: 101, ShopID: 201, ProductID: 401, Status: "on_sale"},
	}
	for _, row := range rows {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func validPackageWriteRequest() PackageWriteRequest {
	return PackageWriteRequest{
		PackageCode:             "STOCKPILE_2026",
		IssuerMerchantID:        "101",
		SettlementShopID:        "201",
		SettlementShopProductID: "301",
		ProductID:               "401",
		RedeemCityCode:          "310100",
		Name:                    "年度囤酒套餐",
		Subtitle:                stringPtr("一次购买，分次提取"),
		CoverImageURL:           stringPtr("https://example.com/package.png"),
		PackageType:             PackageTypeStockpile,
		BottleQuantity:          6,
		SalePriceAmount:         59900,
		MinPurchaseQuantity:     1,
		MaxPurchaseQuantity:     10000,
		ValidityDays:            365,
		PerCustomerLimit:        uintPtr(10000),
		RefundPolicy: RefundPolicyInput{
			SchemaVersion: intPtr(1), Enabled: boolPtr(true), WindowHours: intPtr(168),
			RequireNeverUsed: boolPtr(true), FeeAmount: int64Ptr(0),
		},
		RenewalPolicy: RenewalPolicyInput{
			SchemaVersion: intPtr(1), Enabled: boolPtr(true), ExtensionDays: intPtr(30),
			MaxCount: intPtr(2), GraceDays: intPtr(0), FeeAmount: int64Ptr(990),
		},
		DeliveryPolicy: DeliveryPolicyInput{
			SchemaVersion: intPtr(1), DeliveryFeeIncluded: boolPtr(true), DispatchLeadMinutes: intPtr(120),
		},
	}
}

func packageAdminClaims(permissions ...string) *auth.Claims {
	return packageAdminClaimsFor("9001", permissions...)
}

func packageAdminClaimsFor(adminUserID string, permissions ...string) *auth.Claims {
	return &auth.Claims{AccountType: "admin", AdminUserID: adminUserID, Permissions: permissions}
}

func assertProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s", code)
	}
	var details *problem.Details
	if !errors.As(err, &details) || details.ErrorCode != code {
		t.Fatalf("error=%v, want problem code %s", err, code)
	}
}

func uintPtr(value uint) *uint       { return &value }
func stringPtr(value string) *string { return &value }
