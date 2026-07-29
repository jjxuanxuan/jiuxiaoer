package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckRejectsProductionDialectBranches(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/example/service.go", `package example

type dialect struct{}
func (dialect) Name() string { return "mysql" }
type database struct{ Dialector dialect }

func choose(db database) bool {
	return db.Dialector.Name() == "mysql"
}
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected production dialect branch to fail")
	}
	if !strings.Contains(stderr.String(), "must not branch on a database dialector") {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckRejectsProductionDialectBranchesInInternalPackage(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/pkg/idempotency/store.go", `package idempotency

type dialect struct{}
func (dialect) Name() string { return "mysql" }
type database struct{ Dialector dialect }

func claim(db database) bool {
	return db.Dialector.Name() == "mysql"
}
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected internal package dialect branch to fail")
	}
	if !strings.Contains(stderr.String(), "must not branch on a database dialector") {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckRejectsSQLiteSpecificProductionStrings(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/example/service.go", `package example

const testDialect = "sqlite"
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected SQLite production behavior to fail")
	}
	if !strings.Contains(stderr.String(), "SQLite-specific behavior") {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckRejectsImplicitPeerServiceConstruction(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/example/service.go", `package example

type Service struct{}
type GiftService struct{}

func NewService() *Service { return &Service{} }

func NewGiftService() *GiftService {
	_ = NewService()
	return &GiftService{}
}
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected implicit Service construction to fail")
	}
	if !strings.Contains(stderr.String(), "inject peer services") {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckAllowsWineTicketCoreAssetServiceConstruction(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/example/service.go", `package example

type Service struct{}
type AssetService struct{}

func NewAssetService() *AssetService { return &AssetService{} }

func NewService() *Service {
	_ = NewAssetService()
	return &Service{}
}
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := check(root, 0, &stdout, &stderr); err != nil {
		t.Fatalf(
			"shared asset core construction should be allowed: %v\n%s",
			err,
			stderr.String(),
		)
	}
}

func TestCheckAllowsSQLiteInTestsAndOnlyWarnsForFileSize(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/example/service.go", "package example\n")
	writeTestFile(t, root, "internal/modules/example/service_test.go", "package example\n// sqlite\n// third line\n")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := check(root, 2, &stdout, &stderr); err != nil {
		t.Fatalf("check returned an error: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "WARN 1 test file") {
		t.Fatalf("expected a file-size warning, got: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "architecture check passed") {
		t.Fatalf("expected a passing result, got: %s", stdout.String())
	}
}

func TestCheckRejectsWineTicketServiceDirectGORMPersistence(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/wineticket/payment_service.go", `package wineticket

import (
	"context"
	"gorm.io/gorm"
)

type PaymentService struct{}

func (s *PaymentService) settle(ctx context.Context, tx *gorm.DB) error {
	query := tx.WithContext(ctx).Model(struct{}{})
	return query.Where("id = ?", 1).Updates(map[string]any{"status": "paid"}).Error
}
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected direct Service GORM persistence to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"must delegate GORM persistence to a repository",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckRejectsNestedWineTicketCatalogServiceDirectGORMPersistence(
	t *testing.T,
) {
	root := testRepository(t)
	writeTestFile(
		t,
		root,
		"internal/modules/wineticket/catalog/service.go",
		`package catalog

import "gorm.io/gorm"

type Service struct {
	db *gorm.DB
}

func (s *Service) load() error {
	return s.db.Where("status = ?", "published").Take(&struct{}{}).Error
}
`,
	)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected nested catalog Service GORM persistence to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"must delegate GORM persistence to a repository",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckAllowsNestedWineTicketCatalogRepositoryPersistence(t *testing.T) {
	root := testRepository(t)
	writeTestFile(
		t,
		root,
		"internal/modules/wineticket/catalog/repository.go",
		`package catalog

import "gorm.io/gorm"

type Repository struct {
	db *gorm.DB
}

func (r *Repository) load() error {
	return r.db.Where("status = ?", "published").Take(&struct{}{}).Error
}
`,
	)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := check(root, 0, &stdout, &stderr); err != nil {
		t.Fatalf(
			"nested catalog repository persistence should be allowed: %v\n%s",
			err,
			stderr.String(),
		)
	}
}

func TestCheckAllowsWineTicketServiceTransactionBoundary(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/wineticket/integrity/service.go", `package integrity

import (
	"context"
	"gorm.io/gorm"
)

type ReconciliationService struct {
	db *gorm.DB
}

func (s *ReconciliationService) scan(ctx context.Context) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return nil
	})
}
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := check(root, 0, &stdout, &stderr); err != nil {
		t.Fatalf("transaction boundary should be allowed: %v\n%s", err, stderr.String())
	}
}

func TestCheckRejectsWineTicketServiceGORMInsideTransactionClosure(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/wineticket/refund_service.go", `package wineticket

import (
	"context"
	"gorm.io/gorm"
)

type RefundService struct {
	db *gorm.DB
}

func (s *RefundService) settle(ctx context.Context) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return tx.Create(&struct{}{}).Error
	})
}
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected GORM persistence inside a transaction closure to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"must delegate GORM persistence to a repository",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckRejectsWineTicketServiceRepositoryDBEscape(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/wineticket/gift_service.go", `package wineticket

import "gorm.io/gorm"

type giftRepository struct {
	db *gorm.DB
}
func (r *giftRepository) DB() *gorm.DB { return r.db }

type GiftService struct {
	repo *giftRepository
}

func (s *GiftService) load() error {
	return s.repo.DB().Model(&struct{}{}).Take(&struct{}{}).Error
}
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected repository DB escape inside a Service to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"must delegate GORM persistence to a repository",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckRejectsWineTicketNestedRepositoryDBEscape(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/wineticket/service.go", `package wineticket

import "gorm.io/gorm"

type repository struct {
	db *gorm.DB
}
func (r *repository) dbConn() *gorm.DB { return r.db }

type serviceCore struct {
	repo *repository
}
type Service struct {
	core *serviceCore
}

func (s *Service) load() error {
	return s.core.repo.dbConn().Find(&[]struct{}{}).Error
}
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected a nested repository DB escape inside a Service to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"must delegate GORM persistence to a repository",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckRejectsRefundSettlementInterfaceDirectGORMPersistence(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/wineticket/refund_settlement.go", `package wineticket

import "gorm.io/gorm"

type refundCallback struct{}

func (h *refundCallback) BusinessType() string { return "refund" }
func (h *refundCallback) LockAndApply(tx *gorm.DB) error {
	return tx.Model(&struct{}{}).UpdateColumn("status", "done").Error
}
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected refund settlement interface GORM persistence to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"must delegate GORM persistence to a repository",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckRejectsAnySettlementFileReceiverDirectGORMPersistence(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/wineticket/return_settlement.go", `package wineticket

import "gorm.io/gorm"

type receivePlan struct{}

func (p *receivePlan) apply(tx *gorm.DB) error {
	return tx.Where("id = ?", 1).Delete(&struct{}{}).Error
}
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected settlement receive plan GORM persistence to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"must delegate GORM persistence to a repository",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckRejectsExpiryHelperDirectGORMPersistence(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/wineticket/expiry_helper.go", `package wineticket

import "gorm.io/gorm"

func expire(tx *gorm.DB) error {
	return tx.Model(&struct{}{}).Update("status", "expired").Error
}
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected expiry helper GORM persistence to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"orchestration helpers must delegate GORM persistence",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckRejectsWineTicketServiceCoreDirectGORMPersistence(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/wineticket/core.go", `package wineticket

import "gorm.io/gorm"

type repository struct {
	db *gorm.DB
}
func (r *repository) DB() *gorm.DB { return r.db }

type serviceCore struct {
	repo *repository
}

func (c *serviceCore) persist() error {
	return c.repo.DB().Model(&struct{}{}).
		UpdateColumns(map[string]any{"status": "done"}).Error
}
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected serviceCore GORM persistence to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"must delegate GORM persistence to a repository",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckRejectsWineTicketFreeGORMHelperInServiceFile(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/wineticket/purchase_service.go", `package wineticket

import "gorm.io/gorm"

func loadPurchaseIDs(db *gorm.DB) error {
	var ids []uint64
	return db.Model(&struct{}{}).Pluck("id", &ids).Error
}
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected a free GORM helper in a Service file to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"orchestration helpers must delegate GORM persistence",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckRejectsWineTicketFreeRepositoryDBEscapeInServiceFile(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/wineticket/gift_service.go", `package wineticket

import "gorm.io/gorm"

type giftRepository struct {
	db *gorm.DB
}
func (r *giftRepository) dbConn() *gorm.DB { return r.db }

type GiftService struct {
	repo *giftRepository
}

func escapedRows(s *GiftService) error {
	rows, err := s.repo.dbConn().Rows()
	if rows != nil {
		_ = rows.Close()
	}
	return err
}
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected a free repository DB escape in a Service file to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"orchestration helpers must delegate GORM persistence",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckRejectsWineTicketWorkerDirectGORMPersistence(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/wineticket/reminder_worker.go", `package wineticket

import (
	"context"
	"gorm.io/gorm"
)

type ExpiryReminderWorker struct {
	db *gorm.DB
}

func (w *ExpiryReminderWorker) materialize(ctx context.Context) error {
	return w.db.WithContext(ctx).Find(&[]struct{}{}).Error
}
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected direct Worker GORM persistence to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"must delegate GORM persistence to a repository",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckRejectsNamedWorkerRepositoryDBEscape(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/wineticket/reminder_worker.go", `package wineticket

import "gorm.io/gorm"

type reminderWorkerRepository struct {
	db *gorm.DB
}
func (r *reminderWorkerRepository) DB() *gorm.DB { return r.db }

type ExpiryReminderWorker struct {
	reminderRepo *reminderWorkerRepository
}

func (w *ExpiryReminderWorker) materialize() error {
	return w.reminderRepo.DB().Find(&[]struct{}{}).Error
}
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected a named Worker repository DB escape to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"must delegate GORM persistence to a repository",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckAllowsWineTicketWorkerTransactionBoundary(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/wineticket/reminder/worker.go", `package reminder

import (
	"context"
	"gorm.io/gorm"
)

type ExpiryReminderWorker struct {
	db *gorm.DB
}

func (w *ExpiryReminderWorker) run(ctx context.Context) error {
	return w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return nil
	})
}
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := check(root, 0, &stdout, &stderr); err != nil {
		t.Fatalf("Worker transaction boundary should be allowed: %v\n%s", err, stderr.String())
	}
}

func TestCheckRejectsWineTicketFreeWorkerDBEscape(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/wineticket/expiry_worker.go", `package wineticket

import "gorm.io/gorm"

type ExpiryReminderWorker struct {
	db *gorm.DB
}

func loadDueLots(worker *ExpiryReminderWorker) error {
	return worker.db.Where("status = ?", "active").Take(&struct{}{}).Error
}
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected free Worker GORM persistence to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"orchestration helpers must delegate GORM persistence",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckRejectsWineTicketRootImplementationFile(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/wineticket/service.go", `package wineticket
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected wine-ticket root implementation file to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"root package may contain only module.go and contracts.go",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckAllowsWineTicketRootCompositionFiles(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/modules/wineticket/module.go", `package wineticket
`)
	writeTestFile(t, root, "internal/modules/wineticket/contracts.go", `package wineticket
`)
	writeTestFile(t, root, "internal/modules/wineticket/catalog/service.go", `package catalog
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := check(root, 0, &stdout, &stderr); err != nil {
		t.Fatalf(
			"composition root files and subpackages should be allowed: %v\n%s",
			err,
			stderr.String(),
		)
	}
}

func TestCheckRejectsWineTicketChildImportingParent(t *testing.T) {
	root := testRepository(t)
	writeTestFile(
		t,
		root,
		"internal/modules/wineticket/gift/service.go",
		`package gift

import _ "jiuxiaoer-admin/backend-go/internal/modules/wineticket"
`,
	)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected wine-ticket child-to-parent import to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"subpackages must not import the parent wineticket package",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckAllowsWineTicketChildImportingSibling(t *testing.T) {
	root := testRepository(t)
	writeTestFile(
		t,
		root,
		"internal/modules/wineticket/gift/service.go",
		`package gift

import _ "jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
`,
	)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := check(root, 0, &stdout, &stderr); err != nil {
		t.Fatalf(
			"wine-ticket sibling import should be allowed: %v\n%s",
			err,
			stderr.String(),
		)
	}
}

func TestCheckRejectsWineTicketForwardingTypeAlias(t *testing.T) {
	root := testRepository(t)
	writeTestFile(
		t,
		root,
		"internal/modules/wineticket/gift/model.go",
		`package gift

import "jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"

type Lot = core.Lot
`,
	)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected forwarding type alias to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"must use the owning package type directly",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckRejectsCoreAssetServiceDependingOnGORM(t *testing.T) {
	root := testRepository(t)
	writeTestFile(
		t,
		root,
		"internal/modules/wineticket/core/asset_service.go",
		`package core

import "gorm.io/gorm"

func expire(*gorm.DB) {}
`,
	)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected core AssetService GORM dependency to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"must depend on asset repository ports, not GORM",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckRejectsRouterImportingWineTicketChild(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/app/router.go", `package app

import _ "jiuxiaoer-admin/backend-go/internal/modules/wineticket/catalog"
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected router wine-ticket child import to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"router.go must depend only on the wineticket composition root",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckAllowsRouterImportingWineTicketCompositionRoot(t *testing.T) {
	root := testRepository(t)
	writeTestFile(t, root, "internal/app/router.go", `package app

import _ "jiuxiaoer-admin/backend-go/internal/modules/wineticket"
`)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := check(root, 0, &stdout, &stderr); err != nil {
		t.Fatalf(
			"router wine-ticket composition-root import should be allowed: %v\n%s",
			err,
			stderr.String(),
		)
	}
}

func TestCheckRejectsChildOwnedWineTicketModelInCore(t *testing.T) {
	root := testRepository(t)
	writeTestFile(
		t,
		root,
		"internal/modules/wineticket/core/types.go",
		`package core

type Gift struct{}
`,
	)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := check(root, 0, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected child-owned wine-ticket model in core to fail")
	}
	if !strings.Contains(
		stderr.String(),
		"gift model Gift must be declared in its owning subpackage",
	) {
		t.Fatalf("unexpected diagnostics: %s", stderr.String())
	}
}

func TestCheckAllowsSharedWineTicketAssetModelsInCore(t *testing.T) {
	root := testRepository(t)
	writeTestFile(
		t,
		root,
		"internal/modules/wineticket/core/types.go",
		`package core

type Lot struct{}
type Transaction struct{}
type RefundPolicy struct{}
`,
	)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := check(root, 0, &stdout, &stderr); err != nil {
		t.Fatalf(
			"shared wine-ticket core models should be allowed: %v\n%s",
			err,
			stderr.String(),
		)
	}
}

func TestCheckRejectsWineTicketServiceWritingAssetFactsByHand(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		source string
		want   string
	}{
		{
			name: "ledger",
			source: `package gift

type Transaction struct{}

func settle() {
	_ = Transaction{}
}
`,
			want: "ledger entries must be created through core.AssetService",
		},
		{
			name: "balance",
			source: `package gift

func settle() {
	_ = map[string]any{"available_quantity": 0}
}
`,
			want: "available balance mutations must be delegated to core.AssetService",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := testRepository(t)
			writeTestFile(
				t,
				root,
				"internal/modules/wineticket/gift/service.go",
				testCase.source,
			)
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			err := check(root, 0, &stdout, &stderr)
			if err == nil {
				t.Fatal("expected manual wine-ticket asset mutation to fail")
			}
			if !strings.Contains(stderr.String(), testCase.want) {
				t.Fatalf("unexpected diagnostics: %s", stderr.String())
			}
		})
	}
}

func TestCheckAllowsWineTicketServiceDelegatingAssetMutation(t *testing.T) {
	root := testRepository(t)
	writeTestFile(
		t,
		root,
		"internal/modules/wineticket/gift/service.go",
		`package gift

type AssetService interface {
	Freeze() error
}

func settle(assets AssetService) error {
	return assets.Freeze()
}
`,
	)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := check(root, 0, &stdout, &stderr); err != nil {
		t.Fatalf(
			"delegated wine-ticket asset mutation should be allowed: %v\n%s",
			err,
			stderr.String(),
		)
	}
}

func testRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal", "modules"), 0o755); err != nil {
		t.Fatalf("create module root: %v", err)
	}
	return root
}

func writeTestFile(t *testing.T, root, relativePath, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create test directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
}
