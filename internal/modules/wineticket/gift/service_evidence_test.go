package gift

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestGiftClaimFailsClosedWhenAllocationTotalDoesNotMatchGift(t *testing.T) {
	service, db, now := newGiftTestService(t)
	seedGiftCustomer(t, db, 101, "赠送人")
	seedGiftCustomer(t, db, 202, "领取人")
	seedGiftProduct(t, db)
	lot := baseGiftLot(
		now,
		451,
		"LOT_CLAIM_EVIDENCE",
		101,
		now.Add(72*time.Hour),
	)
	lot.TotalQuantity = 2
	lot.AvailableQuantity = 2
	seedGiftLot(t, db, lot)

	giver := giftTestClaims(
		101,
		"wine_ticket_gift:create",
		"wine_ticket_gift:share",
	)
	created, err := service.Create(
		context.Background(),
		giver,
		http.MethodPost,
		"/api/v1/wine-tickets/gifts",
		"gift-create-claim-evidence",
		GiftCreateRequest{
			SourceLotNo: "LOT_CLAIM_EVIDENCE",
			Quantity:    2,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	share, err := service.CreateShareToken(
		context.Background(),
		giver,
		created.GiftNo,
		GiftShareTokenRequest{ExpectedGiftVersion: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&Gift{}).
		Where("gift_no = ?", created.GiftNo).
		UpdateColumn("quantity", 3).Error; err != nil {
		t.Fatal(err)
	}

	_, err = service.Claim(
		context.Background(),
		giftTestClaims(202, "wine_ticket_gift:claim"),
		http.MethodPost,
		"/api/v1/wine-tickets/gift-claims",
		"gift-claim-invalid-allocation-total",
		share.ShareToken,
		GiftClaimRequest{},
	)
	if err == nil || problem.FromError(err).Status != http.StatusInternalServerError {
		t.Fatalf("claim must fail closed on allocation total mismatch: %v", err)
	}
	assertGiftEvidenceFailureDidNotMutate(
		t,
		db,
		created.GiftNo,
		lot.ID,
		202,
	)
}

func TestGiftCancelFailsClosedWhenGiftHoldLedgerIsNotEqual(t *testing.T) {
	service, db, now := newGiftTestService(t)
	seedGiftCustomer(t, db, 101, "赠送人")
	seedGiftProduct(t, db)
	lot := baseGiftLot(
		now,
		452,
		"LOT_RESTORE_EVIDENCE",
		101,
		now.Add(72*time.Hour),
	)
	lot.TotalQuantity = 2
	lot.AvailableQuantity = 2
	seedGiftLot(t, db, lot)

	giver := giftTestClaims(
		101,
		"wine_ticket_gift:create",
		"wine_ticket_gift:cancel",
	)
	created, err := service.Create(
		context.Background(),
		giver,
		http.MethodPost,
		"/api/v1/wine-tickets/gifts",
		"gift-create-restore-evidence",
		GiftCreateRequest{
			SourceLotNo: "LOT_RESTORE_EVIDENCE",
			Quantity:    2,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&core.Transaction{}).
		Where(
			"action_key = ?",
			fmt.Sprintf("gift_hold:%d:%d", giftIDByNo(t, db, created.GiftNo), lot.ID),
		).
		Updates(map[string]any{
			"quantity_delta":            -1,
			"before_available_quantity": 2,
			"after_available_quantity":  1,
		}).Error; err != nil {
		t.Fatal(err)
	}

	_, err = service.Cancel(
		context.Background(),
		giver,
		http.MethodPost,
		"/api/v1/wine-tickets/gifts/:gift_no/cancel",
		"gift-cancel-invalid-hold",
		created.GiftNo,
		GiftExpectedVersionRequest{ExpectedVersion: 1},
	)
	if err == nil || problem.FromError(err).Status != http.StatusInternalServerError {
		t.Fatalf("cancel must fail closed on unequal gift hold: %v", err)
	}
	assertGiftEvidenceFailureDidNotMutate(
		t,
		db,
		created.GiftNo,
		lot.ID,
		0,
	)
}

func assertGiftEvidenceFailureDidNotMutate(
	t *testing.T,
	db *gorm.DB,
	giftNo string,
	sourceLotID uint64,
	receiverCustomerID uint64,
) {
	t.Helper()
	var storedGift Gift
	if err := db.Where("gift_no = ?", giftNo).Take(&storedGift).Error; err != nil {
		t.Fatal(err)
	}
	var source core.Lot
	if err := db.First(&source, sourceLotID).Error; err != nil {
		t.Fatal(err)
	}
	var allocation GiftAllocation
	if err := db.Where("gift_id = ?", storedGift.ID).Take(&allocation).Error; err != nil {
		t.Fatal(err)
	}
	var restoreOrClaimCount int64
	if err := db.Model(&core.Transaction{}).
		Where(
			"biz_type = ? AND biz_id = ? AND transaction_type IN ?",
			"gift",
			storedGift.ID,
			[]string{TransactionTypeGiftClaim, TransactionTypeGiftRestore},
		).
		Count(&restoreOrClaimCount).Error; err != nil {
		t.Fatal(err)
	}
	var receiverLotCount int64
	if receiverCustomerID != 0 {
		if err := db.Model(&core.Lot{}).
			Where(
				"owner_customer_id = ? AND source_gift_id = ?",
				receiverCustomerID,
				storedGift.ID,
			).
			Count(&receiverLotCount).Error; err != nil {
			t.Fatal(err)
		}
	}
	if storedGift.Status != GiftStatusPending ||
		source.AvailableQuantity != 0 ||
		allocation.Status != GiftAllocationStatusHeld ||
		allocation.ReceiverLotID != nil ||
		restoreOrClaimCount != 0 ||
		receiverLotCount != 0 {
		t.Fatalf(
			"invalid evidence changed gift state: gift=%+v source=%+v allocation=%+v effects=%d receiver_lots=%d",
			storedGift,
			source,
			allocation,
			restoreOrClaimCount,
			receiverLotCount,
		)
	}
}
