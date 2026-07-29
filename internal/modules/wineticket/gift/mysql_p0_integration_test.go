package gift

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type mysqlGiftConcurrencyFixture struct {
	giftID     uint64
	sourceLot  uint64
	productID  uint64
	giverID    uint64
	receiverID uint64
	rawTokens  []string
}

func seedMySQLGiftConcurrencyFixture(
	t *testing.T,
	db *gorm.DB,
	ids *snowflake.Generator,
	now time.Time,
	service *GiftService,
) mysqlGiftConcurrencyFixture {
	t.Helper()
	fixture := mysqlGiftConcurrencyFixture{
		giftID: ids.Next(), sourceLot: ids.Next(), productID: ids.Next(),
		giverID: ids.Next(), receiverID: ids.Next(),
		rawTokens: make([]string, 0, giftActiveTokenMax),
	}
	for _, customer := range []struct {
		id       uint64
		nickname string
	}{
		{id: fixture.giverID, nickname: "MySQL 赠送人"},
		{id: fixture.receiverID, nickname: "MySQL 领取人"},
	} {
		err := db.Table("customers").Create(map[string]any{
			"id": customer.id, "account_id": ids.Next(),
			"nickname": customer.nickname,
			"phone":    "13" + strconv.FormatUint(customer.id%1_000_000_000, 10),
			"status":   "active",
		}).Error
		if err != nil {
			t.Fatalf("seed gift customer: %v", err)
		}
	}
	if err := db.Table("customer_realname_verifications").Create(map[string]any{
		"customer_id": fixture.receiverID, "request_id": ids.Next(),
		"status": "verified", "provider": "mysql_acceptance",
		"masked_name": "M**", "masked_document_no": "3***************1",
		"adult_result": "adult", "verified_at": now,
		"expires_at": now.AddDate(1, 0, 0), "version": 1,
	}).Error; err != nil {
		t.Fatalf("seed gift receiver realname: %v", err)
	}
	if err := db.Table("products").Create(map[string]any{
		"id": fixture.productID, "category_id": ids.Next(),
		"name": "MySQL Gift P0 验收酒", "status": "on_sale",
		"age_restricted": true,
	}).Error; err != nil {
		t.Fatalf("seed gift product: %v", err)
	}
	expiresAt := now.AddDate(1, 0, 0)
	source := core.Lot{
		ID: fixture.sourceLot, LotNo: "MYSQL-GIFT-SOURCE-" + idString(fixture.sourceLot),
		OwnerCustomerID: fixture.giverID, PurchaseID: ids.Next(),
		SourceType: LotSourcePurchase, IssuerMerchantID: ids.Next(),
		ProductID: fixture.productID, RedeemCityCode: "310100",
		TotalQuantity: 1, AvailableQuantity: 0,
		OriginalExpiresAt: expiresAt, ExpiresAt: expiresAt,
		ExpiryChangedAt: now, EverUsed: true, Status: LotStatusDepleted,
		Version: 2, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&source).Error; err != nil {
		t.Fatalf("seed gift source lot: %v", err)
	}
	gift := Gift{
		ID: fixture.giftID, GiftNo: "MYSQL-GIFT-" + idString(fixture.giftID),
		GiverCustomerID:  fixture.giverID,
		IssuerMerchantID: source.IssuerMerchantID,
		ProductID:        source.ProductID, RedeemCityCode: source.RedeemCityCode,
		Quantity: 1, Status: GiftStatusPending,
		ClaimDeadline: now.Add(48 * time.Hour), EarliestExpiresAt: expiresAt,
		Version: uint(giftActiveTokenMax + 1), CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&gift).Error; err != nil {
		t.Fatalf("seed gift: %v", err)
	}
	allocation := GiftAllocation{
		ID: ids.Next(), GiftID: gift.ID, SourceLotID: source.ID, Quantity: 1,
		SourceExpiresAt: source.ExpiresAt, Status: GiftAllocationStatusHeld,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&allocation).Error; err != nil {
		t.Fatalf("seed gift allocation: %v", err)
	}
	holdID := ids.Next()
	hold := core.Transaction{
		ID:                      holdID,
		TransactionNo:           "MYSQL-GIFT-HOLD-" + idString(holdID),
		LotID:                   source.ID,
		OwnerCustomerID:         source.OwnerCustomerID,
		TransactionType:         TransactionTypeGiftHold,
		QuantityDelta:           -int(allocation.Quantity),
		BeforeAvailableQuantity: allocation.Quantity,
		AfterAvailableQuantity:  0,
		BizType:                 "gift",
		BizID:                   gift.ID,
		ActionKey: fmt.Sprintf(
			"gift_hold:%d:%d",
			gift.ID,
			source.ID,
		),
		CreatedAt: now,
	}
	if err := db.Create(&hold).Error; err != nil {
		t.Fatalf("seed gift hold transaction: %v", err)
	}
	for index := 0; index < giftActiveTokenMax; index++ {
		raw := base64.RawURLEncoding.EncodeToString(
			bytes.Repeat([]byte{byte(index + 1)}, 32),
		)
		fixture.rawTokens = append(fixture.rawTokens, raw)
		token := GiftClaimToken{
			ID: ids.Next(), GiftID: gift.ID, TokenDigest: service.tokenDigest(raw),
			IssuedByCustomerID: fixture.giverID, ExpiresAt: now.Add(24 * time.Hour),
			CreatedAt: now,
		}
		if err := db.Create(&token).Error; err != nil {
			t.Fatalf("seed gift token: %v", err)
		}
	}
	return fixture
}

func cleanupMySQLGiftConcurrencyFixture(
	t *testing.T,
	db *gorm.DB,
	fixture mysqlGiftConcurrencyFixture,
) {
	t.Helper()
	var receiverLotIDs []uint64
	if err := db.Model(&core.Lot{}).Where("source_gift_id = ?", fixture.giftID).
		Pluck("id", &receiverLotIDs).Error; err != nil {
		t.Errorf("find gift receiver lots for cleanup: %v", err)
	}
	steps := []struct {
		name  string
		query *gorm.DB
	}{
		{
			name:  "outbox",
			query: db.Where("aggregate_type = ? AND aggregate_id = ?", "wine_ticket_gift", fixture.giftID).Delete(&giftTestOutbox{}),
		},
		{
			name: "audit",
			query: db.Where(
				"resource_type = ? AND resource_id = ?",
				"wine_ticket_gift",
				fixture.giftID,
			).Delete(&giftTestAudit{}),
		},
		{
			name: "idempotency",
			query: db.Where(
				"actor_type = ? AND actor_id = ? AND path = ?",
				"customer",
				fixture.receiverID,
				"/api/v1/wine-tickets/gift-claims",
			).Delete(&idempotency.Record{}),
		},
		{
			name:  "transactions",
			query: db.Where("biz_type = ? AND biz_id = ?", "gift", fixture.giftID).Delete(&core.Transaction{}),
		},
		{
			name:  "tokens",
			query: db.Where("gift_id = ?", fixture.giftID).Delete(&GiftClaimToken{}),
		},
		{
			name:  "allocations",
			query: db.Where("gift_id = ?", fixture.giftID).Delete(&GiftAllocation{}),
		},
		{
			name:  "receiver lots",
			query: db.Where("id IN ?", receiverLotIDs).Delete(&core.Lot{}),
		},
		{
			name:  "gift",
			query: db.Where("id = ?", fixture.giftID).Delete(&Gift{}),
		},
		{
			name:  "source lot",
			query: db.Where("id = ?", fixture.sourceLot).Delete(&core.Lot{}),
		},
		{
			name:  "realname",
			query: db.Table("customer_realname_verifications").Where("customer_id = ?", fixture.receiverID).Delete(nil),
		},
		{
			name:  "customers",
			query: db.Table("customers").Where("id IN ?", []uint64{fixture.giverID, fixture.receiverID}).Delete(nil),
		},
		{
			name:  "product",
			query: db.Table("products").Where("id = ?", fixture.productID).Delete(nil),
		},
	}
	for _, step := range steps {
		if step.query.Error != nil {
			t.Errorf("cleanup mysql gift %s: %v", step.name, step.query.Error)
		}
	}
}

// AC-WT-018/018B：使用全部有效分享令牌发起 100 次领取时，只有一次成功。
func TestMySQLGiftClaimConcurrent100ExactlyOneSuccess(t *testing.T) {
	ctx, db := openWineTicketMySQLAcceptance(t, 45*time.Second)
	now := time.Now().In(shanghaiLocation).Truncate(time.Millisecond)
	ids := snowflake.New(943)
	service := NewGiftService(
		db,
		ids,
		"mysql-gift-p0-pepper-with-at-least-32-bytes",
	)
	service.now = func() time.Time { return now }
	fixture := seedMySQLGiftConcurrencyFixture(t, db, ids, now, service)
	t.Cleanup(func() { cleanupMySQLGiftConcurrencyFixture(t, db, fixture) })
	claims := &auth.Claims{
		AccountType: "customer",
		CustomerID:  idString(fixture.receiverID),
		Permissions: []string{"wine_ticket_gift:claim"},
	}

	results := runMySQLConcurrentErrors(
		mysqlP0Concurrency,
		func(index int) error {
			_, err := service.Claim(
				ctx,
				claims,
				http.MethodPost,
				"/api/v1/wine-tickets/gift-claims",
				fmt.Sprintf("mysql-gift-claim-%03d", index),
				fixture.rawTokens[index%len(fixture.rawTokens)],
				GiftClaimRequest{},
			)
			return err
		},
	)

	successes := 0
	alreadyClaimed := 0
	for _, resultErr := range results {
		if resultErr == nil {
			successes++
			continue
		}
		if problem.FromError(resultErr).ErrorCode == "WT_GIFT_ALREADY_CLAIMED" {
			alreadyClaimed++
			continue
		}
		t.Fatalf("unexpected concurrent gift claim result: %v", resultErr)
	}
	if successes != 1 || alreadyClaimed != mysqlP0Concurrency-1 {
		t.Fatalf(
			"gift results success=%d already_claimed=%d",
			successes,
			alreadyClaimed,
		)
	}
	var stored Gift
	if err := db.First(&stored, fixture.giftID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != GiftStatusClaimed || stored.ReceiverCustomerID == nil ||
		*stored.ReceiverCustomerID != fixture.receiverID {
		t.Fatalf("gift terminal state=%+v", stored)
	}
	var receiverLots, claimTransactions, consumedTokens, revokedTokens int64
	if err := db.Model(&core.Lot{}).Where(
		"source_gift_id = ? AND owner_customer_id = ?",
		fixture.giftID,
		fixture.receiverID,
	).Count(&receiverLots).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&core.Transaction{}).Where(
		"biz_type = ? AND biz_id = ? AND transaction_type = ?",
		"gift",
		fixture.giftID,
		TransactionTypeGiftClaim,
	).Count(&claimTransactions).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&GiftClaimToken{}).Where(
		"gift_id = ? AND consumed_at IS NOT NULL",
		fixture.giftID,
	).Count(&consumedTokens).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&GiftClaimToken{}).Where(
		"gift_id = ? AND revoked_at IS NOT NULL",
		fixture.giftID,
	).Count(&revokedTokens).Error; err != nil {
		t.Fatal(err)
	}
	if receiverLots != 1 || claimTransactions != 1 || consumedTokens != 1 ||
		revokedTokens != int64(giftActiveTokenMax-1) {
		t.Fatalf(
			"gift effects receiver_lots=%d claim_tx=%d consumed=%d revoked=%d",
			receiverLots,
			claimTransactions,
			consumedTokens,
			revokedTokens,
		)
	}
}
