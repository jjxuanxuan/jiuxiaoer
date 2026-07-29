package gift

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
	"jiuxiaoer-admin/backend-go/internal/testutil"
)

type giftTestProduct struct {
	ID        uint64
	Name      string
	BrandName *string
	Spec      *string
	ImageURL  *string
	DeletedAt *time.Time
}

func (giftTestProduct) TableName() string { return "products" }

type giftTestCustomer struct {
	ID        uint64
	Nickname  *string
	Status    string
	DeletedAt *time.Time
}

func (giftTestCustomer) TableName() string { return "customers" }

type giftTestRealname struct {
	CustomerID  uint64 `gorm:"primaryKey"`
	Status      string
	AdultResult string
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
}

func (giftTestRealname) TableName() string { return "customer_realname_verifications" }

type giftTestIdentityRequest struct {
	ID               uint64
	CustomerID       uint64
	Status           string
	SessionExpiresAt *time.Time
}

func (giftTestIdentityRequest) TableName() string { return "identity_verification_requests" }

type giftTestAudit struct {
	ID           uint64
	EventID      string
	ActorType    string
	ActorID      uint64
	AccountID    *uint64
	Action       string
	ResourceType string
	ResourceID   uint64
	BeforeData   datatypes.JSON
	AfterData    datatypes.JSON
	Result       string
	BeforeStatus *string
	AfterStatus  *string
	Version      uint64
	RequestID    *string
	IPHash       *string
	UserAgent    *string
	CreatedAt    time.Time
}

func (giftTestAudit) TableName() string { return "audit_logs" }

type giftTestOutbox struct {
	ID            uint64
	EventID       string
	EventType     string
	EventVersion  uint
	SpecVersion   string
	AggregateType string
	AggregateID   uint64
	Producer      *string
	Payload       datatypes.JSON
	Status        string
	RetryCount    uint
	RequestID     *string
	CreatedAt     time.Time
}

func (giftTestOutbox) TableName() string { return "outbox_events" }

func TestGiftLifecycleFEFOReshareAndClaim(t *testing.T) {
	service, db, now := newGiftTestService(t)
	seedGiftCustomer(t, db, 101, "赠送人")
	seedGiftCustomer(t, db, 202, "领取人")
	seedGiftProduct(t, db)

	firstExpiry := now.Add(24 * time.Hour)
	secondExpiry := now.Add(48 * time.Hour)
	seedGiftLot(t, db, core.Lot{
		ID: 401, LotNo: "LOT_FEFO_FIRST", OwnerCustomerID: 101, PurchaseID: 701,
		SourceType: LotSourcePurchase, IssuerMerchantID: 801, ProductID: 301,
		RedeemCityCode: "310100", TotalQuantity: 2, AvailableQuantity: 2,
		OriginalExpiresAt: firstExpiry.Add(-30 * 24 * time.Hour), ExpiresAt: firstExpiry,
		ExpiryChangedAt: now, RenewalCount: 2, Status: LotStatusActive, Version: 1,
		CreatedAt: now, UpdatedAt: now,
	})
	seedGiftLot(t, db, core.Lot{
		ID: 402, LotNo: "LOT_ANCHOR_LATER", OwnerCustomerID: 101, PurchaseID: 702,
		SourceType: LotSourcePurchase, IssuerMerchantID: 801, ProductID: 301,
		RedeemCityCode: "310100", TotalQuantity: 2, AvailableQuantity: 2,
		OriginalExpiresAt: secondExpiry.Add(-60 * 24 * time.Hour), ExpiresAt: secondExpiry,
		ExpiryChangedAt: now, RenewalCount: 1, Status: LotStatusActive, Version: 1,
		CreatedAt: now, UpdatedAt: now,
	})

	giver := giftTestClaims(101,
		"wine_ticket_gift:create",
		"wine_ticket_gift:view",
		"wine_ticket_gift:share",
	)
	created, err := service.Create(
		context.Background(), giver, http.MethodPost, "/api/v1/wine-tickets/gifts",
		"gift-create-0001",
		GiftCreateRequest{SourceLotNo: "LOT_ANCHOR_LATER", Quantity: 3, Message: giftStringPtr("祝你开心")},
	)
	if err != nil {
		t.Fatalf("create gift: %v", err)
	}
	if created.Version != 1 || created.Quantity != 3 || strings.Contains(fmt.Sprintf("%+v", created), "token") {
		t.Fatalf("unsafe or invalid create response: %+v", created)
	}
	if created.EarliestExpiresAt != formatShanghai(firstExpiry) {
		t.Fatalf("earliest expiry=%s want %s", created.EarliestExpiresAt, formatShanghai(firstExpiry))
	}

	var sourceLots []core.Lot
	if err := db.Order("id ASC").Find(&sourceLots, []uint64{401, 402}).Error; err != nil {
		t.Fatal(err)
	}
	if sourceLots[0].AvailableQuantity != 0 || sourceLots[1].AvailableQuantity != 1 {
		t.Fatalf("FEFO hold quantities are wrong: %+v", sourceLots)
	}
	var allocations []GiftAllocation
	if err := db.Order("source_expires_at ASC").Find(&allocations).Error; err != nil {
		t.Fatal(err)
	}
	if len(allocations) != 2 || allocations[0].SourceLotID != 401 || allocations[0].Quantity != 2 ||
		allocations[1].SourceLotID != 402 || allocations[1].Quantity != 1 {
		t.Fatalf("unexpected FEFO allocations: %+v", allocations)
	}

	firstShare, err := service.CreateShareToken(
		context.Background(), giver, created.GiftNo,
		GiftShareTokenRequest{ExpectedGiftVersion: 1},
	)
	if err != nil {
		t.Fatalf("first share: %v", err)
	}
	secondShare, err := service.CreateShareToken(
		context.Background(), giver, created.GiftNo,
		GiftShareTokenRequest{ExpectedGiftVersion: 2},
	)
	if err != nil {
		t.Fatalf("second share: %v", err)
	}
	if firstShare.ShareToken == secondShare.ShareToken ||
		len(firstShare.ShareToken) < giftTokenMinLength ||
		!strings.Contains(firstShare.MiniProgramPath, firstShare.ShareToken) {
		t.Fatalf("invalid re-share tokens: first=%+v second=%+v", firstShare, secondShare)
	}
	preview, err := service.Preview(context.Background(), secondShare.ShareToken)
	if err != nil || !preview.Claimable || preview.Quantity != 3 || preview.Status != GiftStatusPending {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	if preview.GiverDisplayName == "赠送人" || preview.GiverDisplayName == "" {
		t.Fatalf("anonymous giver name is not masked: %q", preview.GiverDisplayName)
	}

	receiver := giftTestClaims(202, "wine_ticket_gift:claim", "wine_ticket_gift:view")
	claimed, err := service.Claim(
		context.Background(), receiver, http.MethodPost, "/api/v1/wine-tickets/gift-claims",
		"gift-claim-0001", secondShare.ShareToken, GiftClaimRequest{},
	)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.Status != GiftStatusClaimed || claimed.ClaimedAt == nil {
		t.Fatalf("unexpected claimed gift: %+v", claimed)
	}

	var receiverLots []core.Lot
	if err := db.Where("owner_customer_id = ? AND source_type = ?", 202, LotSourceGift).Order("id ASC").Find(&receiverLots).Error; err != nil {
		t.Fatal(err)
	}
	if len(receiverLots) != 2 {
		t.Fatalf("receiver lots=%d want 2", len(receiverLots))
	}
	sourceByID := map[uint64]core.Lot{401: sourceLots[0], 402: sourceLots[1]}
	var receiverQuantity uint
	for _, receiverLot := range receiverLots {
		source := sourceByID[*receiverLot.SourceLotID]
		receiverQuantity += receiverLot.AvailableQuantity
		if receiverLot.SourceGiftID == nil ||
			receiverLot.OriginalExpiresAt != source.OriginalExpiresAt ||
			receiverLot.ExpiresAt != source.ExpiresAt ||
			receiverLot.RenewalCount != source.RenewalCount ||
			!receiverLot.EverUsed {
			t.Fatalf("receiver lineage was reset: receiver=%+v source=%+v", receiverLot, source)
		}
	}
	if receiverQuantity != 3 {
		t.Fatalf("receiver quantity=%d want 3", receiverQuantity)
	}

	var tokens []GiftClaimToken
	if err := db.Order("id ASC").Find(&tokens).Error; err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 || tokens[0].RevokedAt == nil || tokens[1].ConsumedAt == nil {
		t.Fatalf("claim did not invalidate all tokens: %+v", tokens)
	}
	if _, err := service.Preview(context.Background(), firstShare.ShareToken); giftProblemCode(err) != "WT_GIFT_TOKEN_INVALID" {
		t.Fatalf("revoked preview err=%v", err)
	}

	var transactions []core.Transaction
	if err := db.Order("id ASC").Find(&transactions).Error; err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 4 {
		t.Fatalf("transactions=%d want 4: %+v", len(transactions), transactions)
	}
	var storedGift Gift
	if err := db.Where("gift_no = ?", created.GiftNo).Take(&storedGift).Error; err != nil {
		t.Fatal(err)
	}
	expectedEvidence := map[string]struct {
		transactionType string
		bizID           uint64
		delta           int
	}{
		fmt.Sprintf("gift_hold:%d:%d", storedGift.ID, uint64(401)): {
			transactionType: TransactionTypeGiftHold,
			bizID:           storedGift.ID,
			delta:           -2,
		},
		fmt.Sprintf("gift_hold:%d:%d", storedGift.ID, uint64(402)): {
			transactionType: TransactionTypeGiftHold,
			bizID:           storedGift.ID,
			delta:           -1,
		},
	}
	for _, receiverLot := range receiverLots {
		expectedEvidence[fmt.Sprintf("gift_claim:%d:%d", storedGift.ID, receiverLot.ID)] = struct {
			transactionType string
			bizID           uint64
			delta           int
		}{
			transactionType: TransactionTypeGiftClaim,
			bizID:           storedGift.ID,
			delta:           int(receiverLot.TotalQuantity),
		}
	}
	for _, transaction := range transactions {
		if transaction.QuantityDelta == 0 || transaction.ActionKey == "" {
			t.Fatalf("invalid entitlement transaction: %+v", transaction)
		}
		expected, ok := expectedEvidence[transaction.ActionKey]
		if !ok ||
			transaction.TransactionType != expected.transactionType ||
			transaction.BizType != "gift" ||
			transaction.BizID != expected.bizID ||
			transaction.QuantityDelta != expected.delta ||
			!transaction.CreatedAt.Equal(now) {
			t.Fatalf("gift asset evidence changed: %+v", transaction)
		}
		delete(expectedEvidence, transaction.ActionKey)
	}
	if len(expectedEvidence) != 0 {
		t.Fatalf("missing gift asset evidence: %+v", expectedEvidence)
	}

	var responseBodies []string
	if err := db.Model(&idempotency.Record{}).Pluck("CAST(response_body AS TEXT)", &responseBodies).Error; err != nil {
		t.Fatal(err)
	}
	var auditBefore, auditAfter, outboxPayloads []string
	if err := db.Model(&giftTestAudit{}).Pluck("CAST(before_data AS TEXT)", &auditBefore).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&giftTestAudit{}).Pluck("CAST(after_data AS TEXT)", &auditAfter).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&giftTestOutbox{}).Pluck("CAST(payload AS TEXT)", &outboxPayloads).Error; err != nil {
		t.Fatal(err)
	}
	persisted := strings.Join(append(append(responseBodies, auditBefore...), append(auditAfter, outboxPayloads...)...), "\n")
	for _, raw := range []string{firstShare.ShareToken, secondShare.ShareToken} {
		if strings.Contains(persisted, raw) {
			t.Fatal("raw gift token leaked into idempotency, audit, or outbox storage")
		}
		var count int64
		if err := db.Model(&GiftClaimToken{}).Where("token_digest = ?", raw).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatal("raw gift token was persisted as a digest")
		}
	}
	var claimedEvents int64
	if err := db.Model(&giftTestOutbox{}).
		Where("event_type = ? AND aggregate_type = ?", "wine_ticket.gift_claimed", "wine_ticket_gift").
		Count(&claimedEvents).Error; err != nil {
		t.Fatal(err)
	}
	if claimedEvents != 1 {
		t.Fatalf("claimed outbox events=%d want 1", claimedEvents)
	}
}

func TestGiftReshareActiveLimit(t *testing.T) {
	service, db, now := newGiftTestService(t)
	seedGiftCustomer(t, db, 101, "赠送人")
	seedGiftProduct(t, db)
	seedGiftLot(t, db, baseGiftLot(now, 401, "LOT_RESHARE", 101, now.Add(72*time.Hour)))
	giver := giftTestClaims(101, "wine_ticket_gift:create", "wine_ticket_gift:share")

	gift, err := service.Create(
		context.Background(), giver, http.MethodPost, "/api/v1/wine-tickets/gifts",
		"gift-create-reshare", GiftCreateRequest{SourceLotNo: "LOT_RESHARE", Quantity: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	rawTokens := make([]string, 0, giftActiveTokenMax)
	for version := uint(1); version <= giftActiveTokenMax; version++ {
		issued, err := service.CreateShareToken(
			context.Background(), giver, gift.GiftNo,
			GiftShareTokenRequest{ExpectedGiftVersion: version},
		)
		if err != nil {
			t.Fatalf("share version %d: %v", version, err)
		}
		rawTokens = append(rawTokens, issued.ShareToken)
	}
	_, err = service.CreateShareToken(
		context.Background(), giver, gift.GiftNo,
		GiftShareTokenRequest{ExpectedGiftVersion: giftActiveTokenMax + 1},
	)
	detail := problem.FromError(err)
	if detail.Status != http.StatusTooManyRequests || detail.ErrorCode != "RATE_LIMITED" {
		t.Fatalf("fourth share problem=%+v", detail)
	}
	data, ok := detail.Data.(map[string]any)
	if !ok || data["retry_after_seconds"].(int) < 1 {
		t.Fatalf("missing positive retry hint: %+v", detail.Data)
	}

	var tokens []GiftClaimToken
	if err := db.Find(&tokens).Error; err != nil {
		t.Fatal(err)
	}
	if len(tokens) != giftActiveTokenMax {
		t.Fatalf("active tokens=%d want %d", len(tokens), giftActiveTokenMax)
	}
	for index, token := range tokens {
		if token.TokenDigest == rawTokens[index] || len(token.TokenDigest) != 64 {
			t.Fatalf("token %d not stored as digest: %+v", index, token)
		}
	}
}

func TestGiftExpiredCancelRestoresThenExpiresInSameTransaction(t *testing.T) {
	service, db, now := newGiftTestService(t)
	seedGiftCustomer(t, db, 101, "赠送人")
	seedGiftProduct(t, db)
	expiresAt := now.Add(time.Hour)
	seedGiftLot(t, db, baseGiftLot(now, 401, "LOT_EXPIRING", 101, expiresAt))
	giver := giftTestClaims(101, "wine_ticket_gift:create", "wine_ticket_gift:cancel")

	gift, err := service.Create(
		context.Background(), giver, http.MethodPost, "/api/v1/wine-tickets/gifts",
		"gift-create-expiry", GiftCreateRequest{SourceLotNo: "LOT_EXPIRING", Quantity: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return expiresAt }
	_, err = service.Cancel(
		context.Background(), giver, http.MethodPost, "/api/v1/wine-tickets/gifts/:gift_no/cancel",
		"gift-cancel-expiry", gift.GiftNo, GiftExpectedVersionRequest{ExpectedVersion: 1},
	)
	if giftProblemCode(err) != "WT_GIFT_EXPIRED" {
		t.Fatalf("cancel at deadline err=%v", err)
	}

	var storedGift Gift
	if err := db.Where("gift_no = ?", gift.GiftNo).Take(&storedGift).Error; err != nil {
		t.Fatal(err)
	}
	if storedGift.Status != GiftStatusExpiredReturned || storedGift.ReturnedAt == nil {
		t.Fatalf("timeout did not commit: %+v", storedGift)
	}
	var lot core.Lot
	if err := db.First(&lot, 401).Error; err != nil {
		t.Fatal(err)
	}
	if lot.AvailableQuantity != 0 || lot.Status != LotStatusExpired {
		t.Fatalf("restored expired lot must end at zero/expired: %+v", lot)
	}
	var allocation GiftAllocation
	if err := db.First(&allocation).Error; err != nil {
		t.Fatal(err)
	}
	if allocation.Status != GiftAllocationStatusRestored {
		t.Fatalf("allocation=%+v", allocation)
	}
	var transactions []core.Transaction
	if err := db.Order("id ASC").Find(&transactions).Error; err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 3 ||
		transactions[1].TransactionType != TransactionTypeGiftRestore ||
		transactions[2].TransactionType != TransactionTypeExpiry ||
		transactions[1].QuantityDelta != 1 ||
		transactions[2].QuantityDelta != -1 ||
		transactions[1].BizType != "gift" ||
		transactions[2].BizType != "gift" ||
		transactions[1].BizID != storedGift.ID ||
		transactions[2].BizID != storedGift.ID ||
		transactions[1].ActionKey != fmt.Sprintf("gift_restore:%d:%d", storedGift.ID, lot.ID) ||
		transactions[2].ActionKey != fmt.Sprintf(
			"expiry:%d:%d:after:gift_restore:%d",
			lot.ID,
			lot.ExpiresAt.UnixMilli(),
			storedGift.ID,
		) {
		t.Fatalf("restore/expiry ledger is not closed: %+v", transactions)
	}
	var returnedEvents int64
	if err := db.Model(&giftTestOutbox{}).
		Where("event_type = ?", "wine_ticket.gift_returned").
		Count(&returnedEvents).Error; err != nil {
		t.Fatal(err)
	}
	if returnedEvents != 1 {
		t.Fatalf("returned outbox events=%d want 1", returnedEvents)
	}
}

func TestGiftExpireDueWorkerIsIdempotent(t *testing.T) {
	service, db, now := newGiftTestService(t)
	seedGiftCustomer(t, db, 101, "赠送人")
	seedGiftProduct(t, db)
	expiresAt := now.Add(30 * time.Minute)
	seedGiftLot(t, db, baseGiftLot(now, 401, "LOT_WORKER_EXPIRY", 101, expiresAt))
	giver := giftTestClaims(101, "wine_ticket_gift:create")
	gift, err := service.Create(
		context.Background(), giver, http.MethodPost, "/api/v1/wine-tickets/gifts",
		"gift-create-worker-expiry", GiftCreateRequest{SourceLotNo: "LOT_WORKER_EXPIRY", Quantity: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return expiresAt }
	count, err := service.ExpireDue(context.Background(), 100)
	if err != nil || count != 1 {
		t.Fatalf("first expiry count=%d err=%v", count, err)
	}
	count, err = service.ExpireDue(context.Background(), 100)
	if err != nil || count != 0 {
		t.Fatalf("replayed expiry count=%d err=%v", count, err)
	}
	var stored Gift
	if err := db.Where("gift_no = ?", gift.GiftNo).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != GiftStatusExpiredReturned {
		t.Fatalf("worker terminal gift=%+v", stored)
	}
	var restoreCount, expiryCount, outboxCount int64
	if err := db.Model(&core.Transaction{}).Where("transaction_type = ?", TransactionTypeGiftRestore).Count(&restoreCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&core.Transaction{}).Where("transaction_type = ?", TransactionTypeExpiry).Count(&expiryCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&giftTestOutbox{}).Where("event_type = ?", "wine_ticket.gift_returned").Count(&outboxCount).Error; err != nil {
		t.Fatal(err)
	}
	if restoreCount != 1 || expiryCount != 1 || outboxCount != 1 {
		t.Fatalf("worker replay duplicated effects: restore=%d expiry=%d outbox=%d", restoreCount, expiryCount, outboxCount)
	}
}

func TestGiftClaimCancelConcurrentSingleTerminalState(t *testing.T) {
	service, db, now := newGiftTestService(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	// 单个 SQLite 连接会串行化竞争事务，但两个 goroutine 仍会同时进入竞争。
	// 这样终态契约具有确定性，并且在 go test -race 下仍能发挥作用。
	sqlDB.SetMaxOpenConns(1)

	seedGiftCustomer(t, db, 101, "赠送人")
	seedGiftCustomer(t, db, 202, "领取人")
	seedGiftProduct(t, db)
	seedGiftLot(t, db, baseGiftLot(now, 401, "LOT_RACE", 101, now.Add(72*time.Hour)))
	giver := giftTestClaims(101, "wine_ticket_gift:create", "wine_ticket_gift:share", "wine_ticket_gift:cancel")
	receiver := giftTestClaims(202, "wine_ticket_gift:claim")

	gift, err := service.Create(
		context.Background(), giver, http.MethodPost, "/api/v1/wine-tickets/gifts",
		"gift-create-race", GiftCreateRequest{SourceLotNo: "LOT_RACE", Quantity: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := service.CreateShareToken(
		context.Background(), giver, gift.GiftNo, GiftShareTokenRequest{ExpectedGiftVersion: 1},
	)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		_, claimErr := service.Claim(
			context.Background(), receiver, http.MethodPost, "/api/v1/wine-tickets/gift-claims",
			"gift-claim-race", issued.ShareToken, GiftClaimRequest{},
		)
		results <- claimErr
	}()
	go func() {
		defer wait.Done()
		<-start
		_, cancelErr := service.Cancel(
			context.Background(), giver, http.MethodPost, "/api/v1/wine-tickets/gifts/:gift_no/cancel",
			"gift-cancel-race", gift.GiftNo, GiftExpectedVersionRequest{ExpectedVersion: 2},
		)
		results <- cancelErr
	}()
	close(start)
	wait.Wait()
	close(results)

	successes := 0
	for operationErr := range results {
		if operationErr == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("claim/cancel successes=%d want exactly 1", successes)
	}
	var stored Gift
	if err := db.Where("gift_no = ?", gift.GiftNo).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	var source core.Lot
	if err := db.First(&source, 401).Error; err != nil {
		t.Fatal(err)
	}
	var receiverLots int64
	if err := db.Model(&core.Lot{}).Where("owner_customer_id = ? AND source_type = ?", 202, LotSourceGift).Count(&receiverLots).Error; err != nil {
		t.Fatal(err)
	}
	switch stored.Status {
	case GiftStatusClaimed:
		if source.AvailableQuantity != 0 || receiverLots != 1 {
			t.Fatalf("claimed terminal state is inconsistent: gift=%+v source=%+v receiver_lots=%d", stored, source, receiverLots)
		}
	case GiftStatusCancelled:
		if source.AvailableQuantity != 1 || receiverLots != 0 {
			t.Fatalf("cancelled terminal state is inconsistent: gift=%+v source=%+v receiver_lots=%d", stored, source, receiverLots)
		}
	default:
		t.Fatalf("unexpected race terminal status %q", stored.Status)
	}
}

func TestGiftClaimIdempotencyKeyCannotCrossToken(t *testing.T) {
	service, db, now := newGiftTestService(t)
	seedGiftCustomer(t, db, 101, "赠送人")
	seedGiftCustomer(t, db, 202, "领取人")
	seedGiftProduct(t, db)
	lot := baseGiftLot(now, 401, "LOT_TWO_GIFTS", 101, now.Add(72*time.Hour))
	lot.TotalQuantity = 2
	lot.AvailableQuantity = 2
	seedGiftLot(t, db, lot)
	giver := giftTestClaims(101, "wine_ticket_gift:create", "wine_ticket_gift:share")
	receiver := giftTestClaims(202, "wine_ticket_gift:claim")

	createAndShare := func(key string) (GiftDTO, GiftShareTokenDTO) {
		t.Helper()
		gift, err := service.Create(
			context.Background(), giver, http.MethodPost, "/api/v1/wine-tickets/gifts",
			key, GiftCreateRequest{SourceLotNo: "LOT_TWO_GIFTS", Quantity: 1},
		)
		if err != nil {
			t.Fatal(err)
		}
		share, err := service.CreateShareToken(
			context.Background(), giver, gift.GiftNo, GiftShareTokenRequest{ExpectedGiftVersion: 1},
		)
		if err != nil {
			t.Fatal(err)
		}
		return gift, share
	}
	firstGift, firstShare := createAndShare("gift-create-cross-1")
	secondGift, secondShare := createAndShare("gift-create-cross-2")

	_, err := service.Claim(
		context.Background(), receiver, http.MethodPost, "/api/v1/wine-tickets/gift-claims",
		"gift-claim-cross-token", firstShare.ShareToken, GiftClaimRequest{},
	)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	_, err = service.Claim(
		context.Background(), receiver, http.MethodPost, "/api/v1/wine-tickets/gift-claims",
		"gift-claim-cross-token", secondShare.ShareToken, GiftClaimRequest{},
	)
	if giftProblemCode(err) != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("cross-token key reuse err=%v", err)
	}
	var firstStored, secondStored Gift
	if err := db.Where("gift_no = ?", firstGift.GiftNo).Take(&firstStored).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("gift_no = ?", secondGift.GiftNo).Take(&secondStored).Error; err != nil {
		t.Fatal(err)
	}
	if firstStored.Status != GiftStatusClaimed || secondStored.Status != GiftStatusPending {
		t.Fatalf("cross-token replay mutated wrong gift: first=%+v second=%+v", firstStored, secondStored)
	}
}

func newGiftTestService(t *testing.T) (*GiftService, *gorm.DB, time.Time) {
	t.Helper()
	dsn := testutil.UniqueSQLiteMemoryDSN(t, "_busy_timeout=5000")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&giftTestProduct{},
		&giftTestCustomer{},
		&giftTestRealname{},
		&giftTestIdentityRequest{},
		&core.Lot{},
		&core.Transaction{},
		&Gift{},
		&GiftAllocation{},
		&GiftClaimToken{},
		&activeRenewalGuard{},
		&idempotency.Record{},
		&giftTestAudit{},
		&giftTestOutbox{},
	); err != nil {
		t.Fatal(err)
	}
	indexes := []string{
		"CREATE UNIQUE INDEX uk_gift_test_lot_no ON wine_ticket_lots(lot_no)",
		"CREATE UNIQUE INDEX uk_gift_test_gift_no ON wine_ticket_gifts(gift_no)",
		"CREATE UNIQUE INDEX uk_gift_test_allocation ON wine_ticket_gift_allocations(gift_id, source_lot_id)",
		"CREATE UNIQUE INDEX uk_gift_test_receiver_lot ON wine_ticket_gift_allocations(receiver_lot_id)",
		"CREATE UNIQUE INDEX uk_gift_test_token_digest ON wine_ticket_gift_claim_tokens(token_digest)",
		"CREATE UNIQUE INDEX uk_gift_test_action_key ON wine_ticket_transactions(action_key)",
		"CREATE UNIQUE INDEX uk_gift_test_idempotency ON idempotency_keys(actor_type, actor_id, path, key_hash)",
		"CREATE UNIQUE INDEX uk_gift_test_audit_event ON audit_logs(event_id)",
		"CREATE UNIQUE INDEX uk_gift_test_outbox_event ON outbox_events(event_id)",
	}
	for _, statement := range indexes {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 7, 27, 12, 0, 0, 123000000, shanghaiLocation)
	service := NewGiftService(db, snowflake.New(91), "gift-test-pepper-with-at-least-32-bytes")
	service.now = func() time.Time { return now }
	return service, db, now
}

func seedGiftCustomer(t *testing.T, db *gorm.DB, customerID uint64, nickname string) {
	t.Helper()
	if err := db.Create(&giftTestCustomer{ID: customerID, Nickname: &nickname, Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&giftTestRealname{CustomerID: customerID, Status: "verified", AdultResult: "adult"}).Error; err != nil {
		t.Fatal(err)
	}
}

func seedGiftProduct(t *testing.T, db *gorm.DB) {
	t.Helper()
	brand, spec, image := "测试酒庄", "750ml", "https://example.com/wine.png"
	if err := db.Create(&giftTestProduct{
		ID: 301, Name: "测试葡萄酒", BrandName: &brand, Spec: &spec, ImageURL: &image,
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func seedGiftLot(t *testing.T, db *gorm.DB, lot core.Lot) {
	t.Helper()
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
}

func giftIDByNo(t *testing.T, db *gorm.DB, giftNo string) uint64 {
	t.Helper()
	var gift Gift
	if err := db.Where("gift_no = ?", giftNo).Take(&gift).Error; err != nil {
		t.Fatal(err)
	}
	return gift.ID
}

func baseGiftLot(now time.Time, id uint64, lotNo string, ownerID uint64, expiresAt time.Time) core.Lot {
	return core.Lot{
		ID: id, LotNo: lotNo, OwnerCustomerID: ownerID, PurchaseID: 701,
		SourceType: LotSourcePurchase, IssuerMerchantID: 801, ProductID: 301,
		RedeemCityCode: "310100", TotalQuantity: 1, AvailableQuantity: 1,
		OriginalExpiresAt: expiresAt.Add(-30 * 24 * time.Hour), ExpiresAt: expiresAt,
		ExpiryChangedAt: now, Status: LotStatusActive, Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}
}

func giftTestClaims(customerID uint64, permissions ...string) *auth.Claims {
	return &auth.Claims{
		AccountType: "customer",
		CustomerID:  strconv.FormatUint(customerID, 10),
		Permissions: permissions,
	}
}

func giftProblemCode(err error) string {
	if err == nil {
		return ""
	}
	return problem.FromError(err).ErrorCode
}

func giftStringPtr(value string) *string { return &value }
