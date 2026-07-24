package address

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestConcurrentSetDefaultKeepsExactlyOneAddress 验证并发设置默认地址时只保留一个默认项。
func TestConcurrentSetDefaultKeepsExactlyOneAddress(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run MySQL concurrency test")
	}
	ctx := context.Background()
	cfg := config.Load()
	db, err := mysql.Open(ctx, cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	idGen := snowflake.New(996)
	accountID, customerID := idGen.Next(), idGen.Next()
	addressIDs := []uint64{idGen.Next(), idGen.Next()}
	phone := fmt.Sprintf("135%08d", time.Now().UnixNano()%100000000)
	defer func() {
		db.Exec("DELETE FROM idempotency_keys WHERE actor_type = 'customer' AND actor_id = ?", customerID)
		db.Exec("DELETE FROM customer_addresses WHERE customer_id = ?", customerID)
		db.Exec("DELETE FROM customers WHERE id = ?", customerID)
		db.Exec("DELETE FROM accounts WHERE id = ?", accountID)
	}()
	if err := db.Exec("INSERT INTO accounts (id, account_type, phone, status) VALUES (?, 'customer', ?, 'active')", accountID, phone).Error; err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.Exec("INSERT INTO customers (id, account_id, phone, status) VALUES (?, ?, ?, 'active')", customerID, accountID, phone).Error; err != nil {
		t.Fatalf("insert customer: %v", err)
	}
	for index, addressID := range addressIDs {
		if err := db.Exec(`INSERT INTO customer_addresses
			(id, customer_id, contact_name, contact_phone, province, city, city_code, district, address_detail, latitude, longitude, is_default, version)
			VALUES (?, ?, 'concurrency', ?, '广东省', '深圳市', '440300', '南山区', ?, 22.54, 113.93, ?, 1)`,
			addressID, customerID, phone, fmt.Sprintf("address-%d", index), index == 0).Error; err != nil {
			t.Fatalf("insert address: %v", err)
		}
	}

	service := NewService(db, idGen)
	claims := &auth.Claims{AccountType: "customer", CustomerID: strconv.FormatUint(customerID, 10)}
	errorsCh := make(chan error, 1000)
	var wg sync.WaitGroup
	for index := 0; index < 1000; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			addressID := addressIDs[index%2]
			_, err := service.SetDefault(ctx, claims, "POST", "/api/v1/addresses/:id/set-default", fmt.Sprintf("default-race-%04d", index), strconv.FormatUint(addressID, 10))
			if err != nil {
				errorsCh <- err
			}
		}(index)
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Fatalf("concurrent set-default failed: %v", err)
	}
	var count int64
	if err := db.Table("customer_addresses").Where("customer_id = ? AND is_default = 1 AND deleted_at IS NULL", customerID).Count(&count).Error; err != nil {
		t.Fatalf("count defaults: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one default address, got %d", count)
	}
}
