package servicearea

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestL2ServiceAreaMissingAcceptanceScenarios 验证 L2 服务区能力缺失项的验收场景。
func TestL2ServiceAreaMissingAcceptanceScenarios(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run L2 service-area acceptance tests")
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
		t.Fatalf("begin acceptance transaction: %v", tx.Error)
	}
	defer tx.Rollback()

	service := NewService(cfg.Service, tx, nil, nil)
	run := func(t *testing.T, fn func(*testing.T)) {
		t.Helper()
		name := "sp_" + snowflakeID()
		if err := tx.SavePoint(name).Error; err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		defer tx.RollbackTo(name)
		fn(t)
	}

	t.Run("ACC-L2-LBS-002-cross-city", func(t *testing.T) {
		run(t, func(t *testing.T) {
			_, err := service.ResolveWithDB(ctx, tx, ResolveInput{CityCode: "999999", Latitude: 22.54, Longitude: 113.93})
			assertProblemCode(t, err, "CITY_UNSUPPORTED")
		})
	})

	t.Run("ACC-L2-LBS-003-outside-radius", func(t *testing.T) {
		run(t, func(t *testing.T) {
			_, err := service.ResolveWithDB(ctx, tx, ResolveInput{CityCode: "440300", Latitude: 0, Longitude: 0})
			assertProblemCode(t, err, "OUT_OF_SERVICE_AREA")
		})
	})

	t.Run("ACC-L2-LBS-004-all-shops-closed", func(t *testing.T) {
		run(t, func(t *testing.T) {
			if err := tx.Exec("UPDATE shops SET business_status = 'resting' WHERE city_code = '440300'").Error; err != nil {
				t.Fatalf("close shops: %v", err)
			}
			_, err := service.ResolveWithDB(ctx, tx, ResolveInput{CityCode: "440300", Latitude: 22.54, Longitude: 113.93})
			assertProblemCode(t, err, "NO_OPEN_SHOP")
		})
	})

	t.Run("ACC-L2-LBS-005-invalid-location", func(t *testing.T) {
		run(t, func(t *testing.T) {
			for _, input := range []ResolveInput{
				{CityCode: "", Latitude: 22.54, Longitude: 113.93},
				{CityCode: "440300", Latitude: 91, Longitude: 113.93},
				{CityCode: "440300", Latitude: 22.54, Longitude: 181},
			} {
				_, err := service.ResolveWithDB(ctx, tx, input)
				assertProblemCode(t, err, "LOCATION_REQUIRED")
			}
		})
	})

	t.Run("ACC-L2-LBS-008-business-hour-boundaries", func(t *testing.T) {
		run(t, func(t *testing.T) {
			fixed := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
			service.now = func() time.Time { return fixed }
			if err := tx.Exec("DELETE FROM shop_business_hours WHERE shop_id = 4201").Error; err != nil {
				t.Fatalf("clear hours: %v", err)
			}
			id := snowflake.New(990).Next()
			if err := tx.Exec(`INSERT INTO shop_business_hours (id, shop_id, day_of_week, open_time, close_time, status)
				VALUES (?, 4201, 1, '10:00:00', '11:00:00', 'active')`, id).Error; err != nil {
				t.Fatalf("insert hours: %v", err)
			}
			if _, err := service.ResolveWithDB(ctx, tx, ResolveInput{CityCode: "440300", Latitude: 22.54, Longitude: 113.93}); err != nil {
				t.Fatalf("opening boundary must be inclusive: %v", err)
			}
			service.now = func() time.Time { return fixed.Add(time.Hour) }
			_, err := service.ResolveWithDB(ctx, tx, ResolveInput{CityCode: "440300", Latitude: 22.54, Longitude: 113.93})
			assertProblemCode(t, err, "NO_OPEN_SHOP")

			service.now = func() time.Time { return fixed.Add(30 * time.Minute) }
			if err := tx.Exec("UPDATE shops SET business_status = 'resting' WHERE id = 4201").Error; err != nil {
				t.Fatalf("rest shop: %v", err)
			}
			_, err = service.ResolveWithDB(ctx, tx, ResolveInput{CityCode: "440300", Latitude: 22.54, Longitude: 113.93})
			assertProblemCode(t, err, "NO_OPEN_SHOP")
		})
	})
}

// assertProblemCode 断言问题详情代码符合预期。
func assertProblemCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", want)
	}
	var details *problem.Details
	if !errors.As(err, &details) || details.ErrorCode != want {
		t.Fatalf("expected %s, got %v", want, err)
	}
}

// snowflakeID 返回雪花 IDID。
func snowflakeID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
