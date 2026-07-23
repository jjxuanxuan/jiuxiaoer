package product

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestCategoryCatalogRevisionTracksMySQLMutationsTransactionally(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run category revision integration test")
	}
	ctx := context.Background()
	db, err := mysql.Open(ctx, config.Load().MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })

	before, err := NewRepository(db).CategoryCatalogRevision(ctx)
	if err != nil {
		t.Fatalf("read initial revision: %v", err)
	}
	categoryID := snowflake.New(972).Next()
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		repository := NewRepository(tx)
		previous := before
		mutations := []struct {
			name string
			run  func() error
		}{
			{name: "create", run: func() error {
				return tx.Exec(`INSERT INTO categories (id, name, sort_order, status, age_restricted) VALUES (?, 'revision integration', 99, 'active', 0)`, categoryID).Error
			}},
			{name: "update", run: func() error {
				return tx.Exec(`UPDATE categories SET name = 'revision integration updated' WHERE id = ?`, categoryID).Error
			}},
			{name: "status", run: func() error {
				return tx.Exec(`UPDATE categories SET status = 'inactive' WHERE id = ?`, categoryID).Error
			}},
			{name: "soft delete", run: func() error {
				return tx.Exec(`UPDATE categories SET deleted_at = CURRENT_TIMESTAMP(3) WHERE id = ?`, categoryID).Error
			}},
			{name: "hard delete", run: func() error {
				return tx.Exec(`DELETE FROM categories WHERE id = ?`, categoryID).Error
			}},
		}
		for _, mutation := range mutations {
			if err := mutation.run(); err != nil {
				return err
			}
			revision, err := repository.CategoryCatalogRevision(ctx)
			if err != nil {
				return err
			}
			if revision == previous {
				return fmt.Errorf("%s did not switch category revision %s", mutation.name, previous)
			}
			previous = revision
		}
		return context.Canceled
	})
	if err != context.Canceled {
		t.Fatalf("expected rollback sentinel, got %v", err)
	}

	after, err := NewRepository(db).CategoryCatalogRevision(ctx)
	if err != nil {
		t.Fatalf("read revision after rollback: %v", err)
	}
	if after != before {
		t.Fatalf("rolled-back mutation changed revision from %s to %s", before, after)
	}
}
