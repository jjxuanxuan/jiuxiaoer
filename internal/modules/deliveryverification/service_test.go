package deliveryverification

import (
	"context"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/securevalue"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestGeneratedCodesAndDomainSeparatedHashes 验证Generated Codes And Domain Separated Hashes的预期行为。
func TestGeneratedCodesAndDomainSeparatedHashes(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		code, err := newCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != 6 {
			t.Fatalf("code %q is not six digits", code)
		}
		for _, r := range code {
			if r < '0' || r > '9' {
				t.Fatalf("code %q is not numeric", code)
			}
		}
		seen[code] = true
	}
	if len(seen) < 95 {
		t.Fatalf("unexpectedly low code diversity: %d", len(seen))
	}
	cfg := config.CP1Config{VerificationPepper: "unit-test-pepper"}
	pickup := hashCode(cfg, 1, "pickup", "123456")
	delivery := hashCode(cfg, 1, "delivery", "123456")
	otherOrder := hashCode(cfg, 2, "pickup", "123456")
	if pickup == delivery || pickup == otherOrder {
		t.Fatal("verification hashes are not domain separated")
	}
}

// TestVerificationDTOAttemptBudget 验证核验DTO尝试 Budget的预期行为。
func TestVerificationDTOAttemptBudget(t *testing.T) {
	got := dto(Verification{MaxAttempts: 5, FailedAttempts: 2}, "")
	if got.RemainingAttempts != 3 {
		t.Fatalf("remaining attempts = %d", got.RemainingAttempts)
	}
}

func TestExpiredLockRequiresAuthorizedUnlockAndPersistsHashedDimensions(t *testing.T) {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Verification{}, &Attempt{}, &verificationAuditFixture{}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	lockedUntil := now.Add(-time.Minute)
	verification := Verification{
		ID: 1, DeliveryOrderID: 11, Stage: "pickup", ModeSnapshot: "enforce",
		CodeHash: hashCode(config.CP1Config{VerificationPepper: "test-pepper"}, 11, "pickup", "123456"),
		Status:   "locked", FailedAttempts: 5, MaxAttempts: 5,
		ExpiresAt: now.Add(time.Hour), LockedUntil: &lockedUntil, Version: 3,
	}
	if err := db.Create(&verification).Error; err != nil {
		t.Fatal(err)
	}

	ctx := requestctx.WithHTTPMeta(context.Background(), "203.0.113.9", "test-agent")
	detail, err := VerifyLocked(ctx, db, config.CP1Config{
		PickupVerificationMode: "enforce", VerificationPepper: "test-pepper",
	}, snowflake.New(1), 11, "pickup", "123456", 7, 8, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if detail == nil || detail.ErrorCode != "VERIFICATION_LOCKED" {
		t.Fatalf("detail=%+v, want VERIFICATION_LOCKED", detail)
	}

	var got Verification
	if err := db.First(&got, verification.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != "locked" || got.FailedAttempts != 5 || got.Version != 3 {
		t.Fatalf("expired lock was silently reset: %+v", got)
	}

	var attempt Attempt
	if err := db.First(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	wantDeviceHash := securevalue.Digest("verification-device-v1\x00session-1")
	wantIPHash := securevalue.Digest("203.0.113.9")
	if attempt.AccountID == nil || *attempt.AccountID != 8 || attempt.DeviceIDHash == nil || *attempt.DeviceIDHash != wantDeviceHash || attempt.IPHash == nil || *attempt.IPHash != wantIPHash {
		t.Fatalf("attempt dimensions were not persisted as hashes: %+v", attempt)
	}
	if attempt.DeviceIDHash != nil && *attempt.DeviceIDHash == "session-1" || attempt.IPHash != nil && *attempt.IPHash == "203.0.113.9" {
		t.Fatalf("raw device/IP data leaked into attempt: %+v", attempt)
	}
	var failedAudits int64
	if err := db.Table("audit_logs").Where("action='delivery_verification.failed' AND resource_id=? AND result='failed'", verification.DeliveryOrderID).Count(&failedAudits).Error; err != nil {
		t.Fatal(err)
	}
	if failedAudits != 1 {
		t.Fatalf("failed verification audits=%d want=1", failedAudits)
	}
}

func TestVerificationRateLimitIncludesAccountAndDevice(t *testing.T) {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Attempt{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	accountID := uint64(88)
	for index := 0; index < 20; index++ {
		failure := "VERIFICATION_CODE_INVALID"
		if err := db.Create(&Attempt{
			ID: uint64(index + 1), VerificationID: 1, DeliveryOrderID: 11,
			Stage: "pickup", ActorType: "rider", ActorID: uint64(index + 100),
			AccountID: &accountID, Result: "failed", FailureCode: &failure,
			AttemptNo: uint(index + 1), CreatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	limited, err := rateLimited(context.Background(), db, 999, accountID, "other-device", now)
	if err != nil {
		t.Fatal(err)
	}
	if !limited {
		t.Fatal("account failure budget must be enforced across rider actors")
	}

	if err := db.Exec("DELETE FROM delivery_verification_attempts").Error; err != nil {
		t.Fatal(err)
	}
	deviceHash := securevalue.Digest("verification-device-v1\x00shared-session")
	for index := 0; index < 20; index++ {
		failure := "VERIFICATION_CODE_INVALID"
		otherAccount := uint64(index + 1000)
		if err := db.Create(&Attempt{
			ID: uint64(index + 101), VerificationID: 1, DeliveryOrderID: 11,
			Stage: "pickup", ActorType: "rider", ActorID: uint64(index + 200),
			AccountID: &otherAccount, DeviceIDHash: &deviceHash,
			Result: "failed", FailureCode: &failure, AttemptNo: uint(index + 1), CreatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	limited, err = rateLimited(context.Background(), db, 998, 9999, deviceHash, now)
	if err != nil {
		t.Fatal(err)
	}
	if !limited {
		t.Fatal("device/session failure budget must be enforced across accounts and riders")
	}
}

func TestValidateUnlockStateRejectsTerminalAndConsumedCredentials(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name              string
		deliveryStatus    string
		pickupReadyStatus string
		stage             string
		verification      string
		expiresAt         time.Time
		wantCode          string
	}{
		{name: "locked pickup", deliveryStatus: "accepted", pickupReadyStatus: "ready", stage: "pickup", verification: "locked", expiresAt: now.Add(time.Minute)},
		{name: "expired delivery", deliveryStatus: "delivering", pickupReadyStatus: "ready", stage: "delivery", verification: "expired", expiresAt: now.Add(-time.Minute)},
		{name: "active but expired", deliveryStatus: "delivering", pickupReadyStatus: "ready", stage: "delivery", verification: "active", expiresAt: now.Add(-time.Minute)},
		{name: "already active", deliveryStatus: "accepted", pickupReadyStatus: "ready", stage: "pickup", verification: "active", expiresAt: now.Add(time.Minute), wantCode: "VERIFICATION_INVALID_STATUS"},
		{name: "verified", deliveryStatus: "delivering", pickupReadyStatus: "ready", stage: "delivery", verification: "verified", expiresAt: now.Add(time.Minute), wantCode: "VERIFICATION_INVALID_STATUS"},
		{name: "invalidated", deliveryStatus: "accepted", pickupReadyStatus: "ready", stage: "pickup", verification: "invalidated", expiresAt: now.Add(time.Minute), wantCode: "VERIFICATION_INVALID_STATUS"},
		{name: "terminal delivery", deliveryStatus: "completed", pickupReadyStatus: "ready", stage: "delivery", verification: "locked", expiresAt: now.Add(time.Minute), wantCode: "DELIVERY_TERMINAL"},
		{name: "wrong phase", deliveryStatus: "delivering", pickupReadyStatus: "ready", stage: "pickup", verification: "locked", expiresAt: now.Add(time.Minute), wantCode: "VERIFICATION_STAGE_INACTIVE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateUnlockState(tt.deliveryStatus, tt.pickupReadyStatus, tt.stage, tt.verification, tt.expiresAt, now)
			if got := problem.FromError(err); tt.wantCode == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else if got.ErrorCode != tt.wantCode {
				t.Fatalf("error code = %q, want %q", got.ErrorCode, tt.wantCode)
			}
		})
	}
}

func TestDeliveryVerificationTTLUsesETAWithFloorAndCap(t *testing.T) {
	cfg := config.CP1Config{DeliveryVerificationTTL: 2 * time.Hour, DeliveryVerificationMaxTTL: 6 * time.Hour}
	tests := []struct {
		name     string
		snapshot string
		want     time.Duration
	}{
		{name: "short ETA uses two hour floor", snapshot: `{"route":{"duration_seconds":900}}`, want: 2 * time.Hour},
		{name: "longer ETA gets one hour buffer", snapshot: `{"route":{"duration_seconds":5400}}`, want: 150 * time.Minute},
		{name: "oversized ETA is capped", snapshot: `{"route":{"duration_seconds":86400}}`, want: 6 * time.Hour},
		{name: "missing snapshot falls back", snapshot: ``, want: 2 * time.Hour},
		{name: "malformed duration falls back", snapshot: `{"route":{"duration_seconds":"bad"}}`, want: 2 * time.Hour},
		{name: "negative duration falls back", snapshot: `{"route":{"duration_seconds":-1}}`, want: 2 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deliveryVerificationTTLFromSnapshot(cfg, []byte(tt.snapshot)); got != tt.want {
				t.Fatalf("ttl=%s want=%s", got, tt.want)
			}
		})
	}
}

func TestGenerateAndRotateVerificationWriteIndependentAuditsWithoutSecrets(t *testing.T) {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Verification{}, &verificationAuditFixture{}); err != nil {
		t.Fatal(err)
	}
	cfg := config.CP1Config{
		PickupVerificationMode: "enforce", VerificationPepper: "test-pepper",
		DataEncryptionKey: "test-encryption-key", VerificationTTL: 30 * time.Minute,
		VerificationMaxAttempts: 5,
	}
	ids := snowflake.New(3)
	if err := GeneratePickup(context.Background(), db, cfg, ids, 77); err != nil {
		t.Fatal(err)
	}
	if err := generateStage(context.Background(), db, cfg, ids, 77, "pickup", true, "assignment_changed"); err != nil {
		t.Fatal(err)
	}
	var audits []verificationAuditFixture
	if err := db.Where("resource_id=?", 77).Order("created_at, id").Find(&audits).Error; err != nil {
		t.Fatal(err)
	}
	if len(audits) != 2 || audits[0].Action != "delivery_verification.generated" || audits[1].Action != "delivery_verification.regenerated" {
		t.Fatalf("unexpected generation audits: %+v", audits)
	}
	for _, item := range audits {
		payload := strings.ToLower(string(item.AfterData))
		if strings.Contains(payload, "code_hash") || strings.Contains(payload, "cipher") || strings.Contains(payload, "code_mask") || strings.Contains(payload, "123456") {
			t.Fatalf("verification secret leaked into audit: %s", payload)
		}
	}
}

type verificationTestDelivery struct {
	ID        uint64 `gorm:"primaryKey"`
	OrderID   uint64
	DeletedAt gorm.DeletedAt
}

func (verificationTestDelivery) TableName() string { return "delivery_orders" }

type verificationTestOrder struct {
	ID                      uint64 `gorm:"primaryKey"`
	DeliveryPromiseSnapshot datatypes.JSON
	DeletedAt               gorm.DeletedAt
}

func (verificationTestOrder) TableName() string { return "orders" }

func TestDeliveryVerificationTTLLoadsOrderPromiseSnapshot(t *testing.T) {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&verificationTestDelivery{}, &verificationTestOrder{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&verificationTestOrder{ID: 101, DeliveryPromiseSnapshot: datatypes.JSON(`{"route":{"duration_seconds":5400}}`)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&verificationTestDelivery{ID: 11, OrderID: 101}).Error; err != nil {
		t.Fatal(err)
	}
	cfg := config.CP1Config{DeliveryVerificationTTL: 2 * time.Hour, DeliveryVerificationMaxTTL: 6 * time.Hour}
	got, err := deliveryVerificationTTL(context.Background(), db, cfg, 11)
	if err != nil {
		t.Fatal(err)
	}
	if got != 150*time.Minute {
		t.Fatalf("ttl=%s want=%s", got, 150*time.Minute)
	}
}

func TestInvalidateManyAndByOrderAreIdempotent(t *testing.T) {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&verificationTestDelivery{}, &Verification{}, &verificationAuditFixture{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&[]verificationTestDelivery{{ID: 11, OrderID: 101}, {ID: 12, OrderID: 101}, {ID: 13, OrderID: 102}}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Hour)
	rows := []Verification{
		{ID: 1, DeliveryOrderID: 11, Stage: "pickup", Status: "active", ExpiresAt: now, Version: 1},
		{ID: 2, DeliveryOrderID: 11, Stage: "delivery", Status: "verified", ExpiresAt: now, Version: 3},
		{ID: 3, DeliveryOrderID: 12, Stage: "pickup", Status: "locked", ExpiresAt: now, LockedUntil: &now, Version: 2},
		{ID: 4, DeliveryOrderID: 13, Stage: "pickup", Status: "active", ExpiresAt: now, Version: 1},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	ids := snowflake.New(2)
	if err := InvalidateMany(ctx, db, ids, []uint64{11}, "order_cancelled"); err != nil {
		t.Fatal(err)
	}
	var active, verified Verification
	if err := db.First(&active, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&verified, 2).Error; err != nil {
		t.Fatal(err)
	}
	if active.Status != "invalidated" || active.InvalidatedAt == nil || active.InvalidationReasonCode == nil || *active.InvalidationReasonCode != "order_cancelled" || active.Version != 2 {
		t.Fatalf("active credential not invalidated correctly: %+v", active)
	}
	if verified.Status != "verified" || verified.InvalidatedAt != nil || verified.Version != 3 {
		t.Fatalf("terminal credential was mutated: %+v", verified)
	}

	// Repeating a terminal action must not rewrite the original invalidation
	// fact or bump its optimistic-lock version again.
	firstInvalidatedAt := *active.InvalidatedAt
	if err := InvalidateMany(ctx, db, ids, []uint64{11}, "different_reason"); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&active, 1).Error; err != nil {
		t.Fatal(err)
	}
	if active.Version != 2 || active.InvalidationReasonCode == nil || *active.InvalidationReasonCode != "order_cancelled" || !active.InvalidatedAt.Equal(firstInvalidatedAt) {
		t.Fatalf("repeat invalidation was not idempotent: %+v", active)
	}

	if err := InvalidateByOrder(ctx, db, ids, 101, "order_fully_refunded"); err != nil {
		t.Fatal(err)
	}
	var locked, other Verification
	if err := db.First(&locked, 3).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&other, 4).Error; err != nil {
		t.Fatal(err)
	}
	if locked.Status != "invalidated" || locked.LockedUntil != nil || locked.InvalidationReasonCode == nil || *locked.InvalidationReasonCode != "order_fully_refunded" || locked.Version != 3 {
		t.Fatalf("order credential not invalidated correctly: %+v", locked)
	}
	if other.Status != "active" || other.Version != 1 {
		t.Fatalf("credential from another order was mutated: %+v", other)
	}
	var invalidationAudits int64
	if err := db.Table("audit_logs").Where("action='delivery_verification.invalidated'").Count(&invalidationAudits).Error; err != nil {
		t.Fatal(err)
	}
	if invalidationAudits != 2 {
		t.Fatalf("invalidation audits=%d want=2", invalidationAudits)
	}
}

type verificationAuditFixture struct {
	ID           uint64 `gorm:"primaryKey"`
	ActorType    string
	ActorID      uint64
	Action       string
	ResourceType string
	ResourceID   uint64
	AfterData    datatypes.JSON
	Result       string
	RequestID    *string
	CreatedAt    time.Time
}

func (verificationAuditFixture) TableName() string { return "audit_logs" }

func TestSensitiveVerificationViewRateLimitCountsActorAndDelivery(t *testing.T) {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&verificationAuditFixture{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	for index := 0; index < 10; index++ {
		if err := sensitiveAuditResult(ctx, db, uint64(index+1), "customer", 88, 99, "delivery", "success", ""); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{db: db}
	limited, err := service.sensitiveViewRateLimited(ctx, "customer", 88, 99, now)
	if err != nil {
		t.Fatal(err)
	}
	if !limited {
		t.Fatal("ten views of one verification in a minute must be limited")
	}
	limited, err = service.sensitiveViewRateLimited(ctx, "customer", 88, 100, now)
	if err != nil {
		t.Fatal(err)
	}
	if limited {
		t.Fatal("per-delivery budget must not block a different verification below the account budget")
	}
}
