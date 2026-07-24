package address

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestL2AddressMissingAcceptanceScenarios 验证 L2 地址能力缺失项的验收场景。
func TestL2AddressMissingAcceptanceScenarios(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run L2 address acceptance tests")
	}
	ctx := context.Background()
	cfg := config.Load()
	db, err := mysql.Open(ctx, cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("begin transaction: %v", tx.Error)
	}
	defer tx.Rollback()

	idGen := snowflake.New(989)
	account1, customer1 := idGen.Next(), idGen.Next()
	account2, customer2 := idGen.Next(), idGen.Next()
	if err := tx.Exec(`INSERT INTO accounts (id, account_type, phone, status) VALUES
		(?, 'customer', '13600001001', 'active'), (?, 'customer', '13600001002', 'active')`, account1, account2).Error; err != nil {
		t.Fatalf("insert accounts: %v", err)
	}
	if err := tx.Exec(`INSERT INTO customers (id, account_id, phone, status) VALUES
		(?, ?, '13600001001', 'active'), (?, ?, '13600001002', 'active')`, customer1, account1, customer2, account2).Error; err != nil {
		t.Fatalf("insert customers: %v", err)
	}
	claims1 := &auth.Claims{AccountType: "customer", CustomerID: fmt.Sprint(customer1)}
	claims2 := &auth.Claims{AccountType: "customer", CustomerID: fmt.Sprint(customer2)}
	service := NewService(tx, idGen)
	newAddress := func(customerID uint64, isDefault bool) CustomerAddress {
		lat, lng := 22.54, 113.93
		cityCode := "440300"
		return CustomerAddress{ID: idGen.Next(), CustomerID: customerID, ContactName: "acceptance", ContactPhone: "13600001001", Province: "广东省", City: "深圳市", CityCode: &cityCode, District: "南山区", AddressDetail: "验收地址", Latitude: &lat, Longitude: &lng, CoordinateSystem: "gcj02", LocationSource: "legacy", GeocodeStatus: "unverified", IsDefault: isDefault, Version: 1}
	}
	updateReq := func(version uint32) AddressUpdateReq {
		lat, lng := 22.54, 113.93
		return AddressUpdateReq{ContactName: "updated", ContactPhone: "13600001001", Province: "广东省", City: "深圳市", CityCode: "440300", District: "南山区", AddressDetail: "更新地址", Latitude: &lat, Longitude: &lng, CoordinateSystem: "gcj02", Version: version}
	}

	t.Run("ACC-L2-ADDR-002-owner-isolation", func(t *testing.T) {
		row := newAddress(customer1, false)
		if err := tx.Create(&row).Error; err != nil {
			t.Fatalf("create address: %v", err)
		}
		_, err := service.Update(ctx, claims2, "PUT", "/api/v1/addresses/:id", "owner-update", fmt.Sprint(row.ID), updateReq(1))
		assertAddressProblem(t, err, "ADDRESS_NOT_FOUND")
		err = service.Delete(ctx, claims2, "DELETE", "/api/v1/addresses/:id", "owner-delete", fmt.Sprint(row.ID))
		assertAddressProblem(t, err, "ADDRESS_NOT_FOUND")
		_, err = service.SetDefault(ctx, claims2, "POST", "/api/v1/addresses/:id/set-default", "owner-default", fmt.Sprint(row.ID))
		assertAddressProblem(t, err, "ADDRESS_NOT_FOUND")
		var count int64
		if err := tx.Model(&CustomerAddress{}).Where("id = ? AND customer_id = ? AND deleted_at IS NULL", row.ID, customer1).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("owner row changed: count=%d err=%v", count, err)
		}
	})

	t.Run("ACC-L2-ADDR-004-delete-default-keeps-order-snapshot", func(t *testing.T) {
		row := newAddress(customer1, true)
		if err := tx.Create(&row).Error; err != nil {
			t.Fatalf("create address: %v", err)
		}
		orderID := idGen.Next()
		snapshot := `{"contact_name":"acceptance","address_detail":"acceptance address"}`
		if err := tx.Exec(`INSERT INTO orders
			(id, order_no, customer_id, merchant_id, shop_id, status, pay_status, delivery_status, goods_amount, discount_amount, delivery_fee_amount, payable_amount, paid_amount, address_snapshot)
			VALUES (?, ?, ?, 4001, 4201, 'pending_payment', 'pending', 'pending', 100, 0, 0, 100, 0, ?)`, orderID, fmt.Sprintf("ACC%d", orderID), customer1, snapshot).Error; err != nil {
			t.Fatalf("insert order: %v", err)
		}
		var storedBefore string
		if err := tx.Table("orders").Select("address_snapshot").Where("id = ?", orderID).Scan(&storedBefore).Error; err != nil {
			t.Fatalf("read order snapshot before delete: %v", err)
		}
		if err := service.Delete(ctx, claims1, "DELETE", "/api/v1/addresses/:id", "delete-default", fmt.Sprint(row.ID)); err != nil {
			t.Fatalf("delete default: %v", err)
		}
		var activeDefaults int64
		if err := tx.Model(&CustomerAddress{}).Where("customer_id = ? AND is_default = 1 AND deleted_at IS NULL", customer1).Count(&activeDefaults).Error; err != nil || activeDefaults != 0 {
			t.Fatalf("expected zero active defaults: count=%d err=%v", activeDefaults, err)
		}
		var stored string
		if err := tx.Table("orders").Select("address_snapshot").Where("id = ?", orderID).Scan(&stored).Error; err != nil || stored != storedBefore {
			t.Fatalf("order snapshot changed: got=%s err=%v", stored, err)
		}
	})

	t.Run("ACC-L2-ADDR-005-stale-version", func(t *testing.T) {
		row := newAddress(customer1, false)
		row.Version = 2
		if err := tx.Create(&row).Error; err != nil {
			t.Fatalf("create address: %v", err)
		}
		_, err := service.Update(ctx, claims1, "PUT", "/api/v1/addresses/:id", "stale-version", fmt.Sprint(row.ID), updateReq(1))
		assertAddressProblem(t, err, "ADDRESS_VERSION_CONFLICT")
		var stored CustomerAddress
		if err := tx.First(&stored, row.ID).Error; err != nil || stored.ContactName != "acceptance" || stored.Version != 2 {
			t.Fatalf("stale update changed row: %+v err=%v", stored, err)
		}
	})
}

// assertAddressProblem 断言地址问题详情符合预期。
func assertAddressProblem(t *testing.T, err error, want string) {
	t.Helper()
	var details *problem.Details
	if err == nil || !errors.As(err, &details) || details.ErrorCode != want {
		t.Fatalf("expected %s, got %v", want, err)
	}
}
