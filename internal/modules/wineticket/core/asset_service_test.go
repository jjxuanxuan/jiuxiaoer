package core

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

type assetTestIDs struct {
	next uint64
}

func (g *assetTestIDs) Next() uint64 {
	g.next++
	return g.next
}

type assetTestRepository struct {
	lots         map[uint64]Lot
	transactions map[string]Transaction
	updates      []LotAssetChange
}

func newAssetTestRepository(lots ...Lot) *assetTestRepository {
	repo := &assetTestRepository{
		lots:         make(map[uint64]Lot, len(lots)),
		transactions: make(map[string]Transaction),
	}
	for _, lot := range lots {
		repo.lots[lot.ID] = lot
	}
	return repo
}

func (r *assetTestRepository) TransactionByActionKey(
	_ context.Context,
	actionKey string,
) (Transaction, error) {
	row, ok := r.transactions[actionKey]
	if !ok {
		return Transaction{}, ErrAssetRecordNotFound
	}
	return row, nil
}

func (r *assetTestRepository) LockLot(
	_ context.Context,
	lotID uint64,
) (Lot, error) {
	row, ok := r.lots[lotID]
	if !ok {
		return Lot{}, ErrAssetRecordNotFound
	}
	return row, nil
}

func (r *assetTestRepository) CreateLot(
	_ context.Context,
	lot *Lot,
) error {
	if _, exists := r.lots[lot.ID]; exists {
		return fmt.Errorf("duplicate lot %d", lot.ID)
	}
	r.lots[lot.ID] = *lot
	return nil
}

func (r *assetTestRepository) UpdateLot(
	_ context.Context,
	lotID uint64,
	expectedVersion uint,
	change LotAssetChange,
) (bool, error) {
	lot, ok := r.lots[lotID]
	if !ok {
		return false, ErrAssetRecordNotFound
	}
	if lot.Version != expectedVersion {
		return false, nil
	}
	lot.AvailableQuantity = change.AvailableQuantity
	lot.Status = change.Status
	if change.SetEverUsed {
		lot.EverUsed = change.EverUsed
	}
	lot.Version++
	lot.UpdatedAt = change.UpdatedAt
	r.lots[lotID] = lot
	r.updates = append(r.updates, change)
	return true, nil
}

func (r *assetTestRepository) CreateTransaction(
	_ context.Context,
	transaction *Transaction,
) error {
	if _, exists := r.transactions[transaction.ActionKey]; exists {
		return fmt.Errorf("duplicate transaction action %s", transaction.ActionKey)
	}
	r.transactions[transaction.ActionKey] = *transaction
	return nil
}

func TestAssetServiceIssueReplayValidatesImmutableLineage(t *testing.T) {
	now := assetTestTime()
	repo := newAssetTestRepository()
	service := NewAssetService(&assetTestIDs{next: 100}).WithClock(func() time.Time {
		return now
	})
	command := assetTestIssueCommand(now)

	issued, err := service.Issue(context.Background(), repo, command)
	if err != nil {
		t.Fatal(err)
	}
	if issued.Replayed {
		t.Fatal("first issue must not be a replay")
	}
	if issued.Lot.ID == 0 ||
		issued.Lot.AvailableQuantity != command.Lot.TotalQuantity {
		t.Fatalf("unexpected issued lot: %+v", issued.Lot)
	}
	if len(issued.Transactions) != 1 {
		t.Fatalf("expected one issuance transaction, got %d", len(issued.Transactions))
	}
	issuance := issued.Transactions[0]
	if issuance.BeforeAvailableQuantity != 0 ||
		issuance.AfterAvailableQuantity != command.Lot.TotalQuantity {
		t.Fatalf("invalid issuance evidence: %+v", issuance)
	}

	replayed, err := service.Issue(context.Background(), repo, command)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Lot.ID != issued.Lot.ID {
		t.Fatalf("unexpected issue replay: %+v", replayed)
	}

	otherID := uint64(999)
	cases := []struct {
		name   string
		mutate func(*IssueCommand)
	}{
		{
			name: "owner",
			mutate: func(candidate *IssueCommand) {
				candidate.Lot.OwnerCustomerID++
			},
		},
		{
			name: "purchase",
			mutate: func(candidate *IssueCommand) {
				candidate.Lot.PurchaseID++
			},
		},
		{
			name: "source type",
			mutate: func(candidate *IssueCommand) {
				candidate.Lot.SourceType = LotSourceGift
			},
		},
		{
			name: "source lot",
			mutate: func(candidate *IssueCommand) {
				candidate.Lot.SourceLotID = &otherID
			},
		},
		{
			name: "source gift",
			mutate: func(candidate *IssueCommand) {
				candidate.Lot.SourceGiftID = &otherID
			},
		},
		{
			name: "issuer",
			mutate: func(candidate *IssueCommand) {
				candidate.Lot.IssuerMerchantID++
			},
		},
		{
			name: "product",
			mutate: func(candidate *IssueCommand) {
				candidate.Lot.ProductID++
			},
		},
		{
			name: "city",
			mutate: func(candidate *IssueCommand) {
				candidate.Lot.RedeemCityCode = "310000"
			},
		},
		{
			name: "total",
			mutate: func(candidate *IssueCommand) {
				candidate.Lot.TotalQuantity++
				candidate.Lot.AvailableQuantity++
			},
		},
		{
			name: "original expiry",
			mutate: func(candidate *IssueCommand) {
				candidate.Lot.OriginalExpiresAt =
					candidate.Lot.OriginalExpiresAt.Add(time.Hour)
				candidate.Lot.ExpiresAt = candidate.Lot.OriginalExpiresAt
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := command
			testCase.mutate(&candidate)
			_, err := service.Issue(context.Background(), repo, candidate)
			requireAssetProblemCode(t, err, "WT_IDEMPOTENCY_CONFLICT")
		})
	}

	t.Run("issuance balance evidence", func(t *testing.T) {
		original := repo.transactions[command.ActionKey]
		for _, testCase := range []struct {
			name   string
			mutate func(*Transaction)
		}{
			{
				name: "before is not zero",
				mutate: func(candidate *Transaction) {
					candidate.BeforeAvailableQuantity = 1
				},
			},
			{
				name: "after is not total",
				mutate: func(candidate *Transaction) {
					candidate.AfterAvailableQuantity--
				},
			},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				corrupt := original
				testCase.mutate(&corrupt)
				repo.transactions[command.ActionKey] = corrupt
				t.Cleanup(func() {
					repo.transactions[command.ActionKey] = original
				})
				_, err := service.Issue(
					context.Background(),
					repo,
					command,
				)
				requireAssetProblemCode(
					t,
					err,
					"WT_IDEMPOTENCY_CONFLICT",
				)
			})
		}
	})
}

func TestAssetServiceFreezeMarkUsedAndRedeem(t *testing.T) {
	now := assetTestTime()
	cases := []struct {
		name         string
		freeze       bool
		markUsed     bool
		wantEverUsed bool
	}{
		{name: "freeze without usage", freeze: true},
		{
			name:         "freeze marks usage when requested",
			freeze:       true,
			markUsed:     true,
			wantEverUsed: true,
		},
		{name: "redeem always marks usage", wantEverUsed: true},
	}
	for index, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			lotID := uint64(index + 1)
			lot := assetTestSpendableLot(lotID, now)
			repo := newAssetTestRepository(lot)
			service := NewAssetService(
				&assetTestIDs{next: 200},
			).WithClock(func() time.Time {
				return now
			})
			command := AssetCommand{
				LotID:           lot.ID,
				OwnerCustomerID: lot.OwnerCustomerID,
				Quantity:        2,
				MarkUsed:        testCase.markUsed,
				TransactionType: TransactionTypeRedemptionHold,
				BizType:         "redemption",
				BizID:           900 + uint64(index),
				ActionKey:       fmt.Sprintf("debit:%d", index),
				OccurredAt:      now,
			}

			var (
				result AssetMutation
				err    error
			)
			if testCase.freeze {
				result, err = service.Freeze(
					context.Background(),
					repo,
					command,
				)
			} else {
				result, err = service.Redeem(
					context.Background(),
					repo,
					command,
				)
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.Lot.EverUsed != testCase.wantEverUsed {
				t.Fatalf(
					"EverUsed = %t, want %t",
					result.Lot.EverUsed,
					testCase.wantEverUsed,
				)
			}
			if result.Lot.AvailableQuantity != lot.AvailableQuantity-2 {
				t.Fatalf("unexpected lot balance: %+v", result.Lot)
			}
			transaction := result.Transactions[0]
			if transaction.QuantityDelta != -2 ||
				transaction.BeforeAvailableQuantity != lot.AvailableQuantity ||
				transaction.AfterAvailableQuantity !=
					lot.AvailableQuantity-2 ||
				!transaction.CreatedAt.Equal(now) {
				t.Fatalf("unexpected debit evidence: %+v", transaction)
			}
		})
	}
}

func TestAssetServiceDebitReplayRejectsOwnerMismatch(t *testing.T) {
	now := assetTestTime()
	lot := assetTestSpendableLot(21, now)
	repo := newAssetTestRepository(lot)
	service := NewAssetService(&assetTestIDs{next: 250})
	command := AssetCommand{
		LotID:           lot.ID,
		OwnerCustomerID: lot.OwnerCustomerID,
		Quantity:        1,
		TransactionType: TransactionTypeGiftHold,
		BizType:         "gift",
		BizID:           81,
		ActionKey:       "gift_hold:81:21",
		OccurredAt:      now,
	}
	if _, err := service.Freeze(
		context.Background(),
		repo,
		command,
	); err != nil {
		t.Fatal(err)
	}

	command.OwnerCustomerID++
	_, err := service.Freeze(context.Background(), repo, command)
	requireAssetProblemCode(t, err, "WT_IDEMPOTENCY_CONFLICT")
}

func TestAssetServiceRestoreRejectsRefundedLot(t *testing.T) {
	now := assetTestTime()
	lot := assetTestSpendableLot(22, now)
	lot.TotalQuantity = 2
	lot.AvailableQuantity = 0
	lot.Status = LotStatusRefunded
	repo := newAssetTestRepository(lot)
	service := NewAssetService(&assetTestIDs{next: 260})

	_, err := service.Restore(
		context.Background(),
		repo,
		AssetCommand{
			LotID:           lot.ID,
			OwnerCustomerID: lot.OwnerCustomerID,
			Quantity:        2,
			TransactionType: TransactionTypeRefundRestore,
			BizType:         "refund",
			BizID:           82,
			ActionKey:       "refund_restore:82:22",
			OccurredAt:      now,
		},
	)
	requireAssetProblemCode(t, err, "WT_LOT_INVALID_STATUS")
}

func TestAssetServiceTransferReplayValidatesGiftLineage(t *testing.T) {
	now := assetTestTime()
	sourceLotID := uint64(41)
	sourceGiftID := uint64(51)
	command := TransferCommand{
		ReceiverLot: Lot{
			OwnerCustomerID:   202,
			PurchaseID:        302,
			SourceType:        LotSourceGift,
			SourceLotID:       &sourceLotID,
			SourceGiftID:      &sourceGiftID,
			IssuerMerchantID:  402,
			ProductID:         502,
			RedeemCityCode:    "440100",
			TotalQuantity:     2,
			AvailableQuantity: 2,
			OriginalExpiresAt: now.Add(30 * 24 * time.Hour),
			ExpiresAt:         now.Add(30 * 24 * time.Hour),
		},
		TransactionType: TransactionTypeGiftClaim,
		BizType:         "gift",
		BizID:           sourceGiftID,
		ActionKey:       "gift:claim:51",
		OccurredAt:      now,
	}
	repo := newAssetTestRepository()
	service := NewAssetService(&assetTestIDs{next: 300})

	first, err := service.Transfer(context.Background(), repo, command)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.Transfer(context.Background(), repo, command)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Lot.ID != first.Lot.ID {
		t.Fatalf("unexpected transfer replay: %+v", replayed)
	}

	otherGiftID := sourceGiftID + 1
	command.ReceiverLot.SourceGiftID = &otherGiftID
	_, err = service.Transfer(context.Background(), repo, command)
	requireAssetProblemCode(t, err, "WT_IDEMPOTENCY_CONFLICT")
}

func TestAssetServiceExpiredRestoreCreatesAndReplaysBothTransitions(
	t *testing.T,
) {
	now := assetTestTime()
	cases := []struct {
		name     string
		evidence *AssetEvidence
	}{
		{name: "default expiry evidence"},
		{
			name: "specified expiry evidence",
			evidence: &AssetEvidence{
				TransactionType: TransactionTypeRedemptionReturnExpire,
				BizType:         "redemption_return",
				BizID:           802,
				ActionKey:       "restore:802:custom-expiry",
				Metadata:        map[string]any{"reason": "late_return"},
			},
		},
	}
	for index, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			lot := assetTestSpendableLot(uint64(70+index), now)
			lot.TotalQuantity = 10
			lot.AvailableQuantity = 2
			lot.ExpiresAt = now.Add(-time.Hour)
			lot.OriginalExpiresAt = lot.ExpiresAt
			lot.Version = 3
			repo := newAssetTestRepository(lot)
			service := NewAssetService(
				&assetTestIDs{next: 400},
			).WithClock(func() time.Time {
				return now
			})
			command := AssetCommand{
				LotID:           lot.ID,
				OwnerCustomerID: lot.OwnerCustomerID,
				Quantity:        3,
				TransactionType: TransactionTypeRedemptionRestore,
				BizType:         "redemption",
				BizID:           801 + uint64(index),
				ActionKey:       fmt.Sprintf("restore:%d", 801+index),
				OccurredAt:      now,
				ExpiryEvidence:  testCase.evidence,
			}

			result, err := service.Restore(
				context.Background(),
				repo,
				command,
			)
			if err != nil {
				t.Fatal(err)
			}
			if result.Lot.AvailableQuantity != 0 ||
				result.Lot.Status != LotStatusExpired ||
				result.Lot.Version != lot.Version+2 {
				t.Fatalf("unexpected expired restore lot: %+v", result.Lot)
			}
			if len(repo.updates) != 2 ||
				repo.updates[0].AvailableQuantity != 5 ||
				repo.updates[0].Status != LotStatusActive ||
				repo.updates[1].AvailableQuantity != 0 ||
				repo.updates[1].Status != LotStatusExpired {
				t.Fatalf("unexpected restore transitions: %+v", repo.updates)
			}
			if len(result.Transactions) != 2 {
				t.Fatalf(
					"expected restore and expiry transactions, got %d",
					len(result.Transactions),
				)
			}
			restore := result.Transactions[0]
			expiry := result.Transactions[1]
			if restore.QuantityDelta != 3 ||
				restore.BeforeAvailableQuantity != 2 ||
				restore.AfterAvailableQuantity != 5 {
				t.Fatalf("unexpected restore evidence: %+v", restore)
			}
			if expiry.QuantityDelta != -5 ||
				expiry.BeforeAvailableQuantity != 5 ||
				expiry.AfterAvailableQuantity != 0 ||
				!expiry.CreatedAt.Equal(restore.CreatedAt) {
				t.Fatalf("unexpected expiry evidence: %+v", expiry)
			}

			replayed, err := service.Restore(
				context.Background(),
				repo,
				command,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !replayed.Replayed || len(replayed.Transactions) != 2 {
				t.Fatalf("unexpected expired restore replay: %+v", replayed)
			}

			expiryActionKey := command.ActionKey + ":expiry"
			if command.ExpiryEvidence != nil {
				expiryActionKey = command.ExpiryEvidence.ActionKey
			}
			savedExpiry := repo.transactions[expiryActionKey]
			delete(repo.transactions, expiryActionKey)
			_, err = service.Restore(context.Background(), repo, command)
			requireAssetProblemCode(t, err, "WT_IDEMPOTENCY_CONFLICT")
			repo.transactions[expiryActionKey] = savedExpiry

			terminal := repo.lots[lot.ID]
			terminal.AvailableQuantity = 1
			terminal.Status = LotStatusActive
			repo.lots[lot.ID] = terminal
			_, err = service.Restore(context.Background(), repo, command)
			requireAssetProblemCode(t, err, "WT_IDEMPOTENCY_CONFLICT")
		})
	}
}

func TestAssetServiceExpireOwnsTerminalBalanceAndReplay(t *testing.T) {
	now := assetTestTime()
	lot := assetTestSpendableLot(91, now)
	lot.AvailableQuantity = 3
	lot.TotalQuantity = 3
	lot.ExpiresAt = now.Add(-time.Minute)
	lot.OriginalExpiresAt = lot.ExpiresAt
	repo := newAssetTestRepository(lot)
	service := NewAssetService(&assetTestIDs{next: 500}).WithClock(
		func() time.Time { return now },
	)
	command := ExpireCommand{
		LotID:           lot.ID,
		OwnerCustomerID: lot.OwnerCustomerID,
		TransactionType: TransactionTypeExpiry,
		BizType:         "wine_ticket_expiry",
		BizID:           lot.ID,
		ActionKey:       "expiry:91",
		OccurredAt:      now,
	}

	result, err := service.Expire(context.Background(), repo, command)
	if err != nil {
		t.Fatal(err)
	}
	if result.Replayed ||
		result.Lot.Status != LotStatusExpired ||
		result.Lot.AvailableQuantity != 0 ||
		result.Lot.Version != lot.Version+1 ||
		len(result.Transactions) != 1 {
		t.Fatalf("unexpected expiry result: %+v", result)
	}
	transaction := result.Transactions[0]
	if transaction.QuantityDelta != -3 ||
		transaction.BeforeAvailableQuantity != 3 ||
		transaction.AfterAvailableQuantity != 0 {
		t.Fatalf("unexpected expiry evidence: %+v", transaction)
	}

	replayed, err := service.Expire(context.Background(), repo, command)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed ||
		replayed.Lot.Status != LotStatusExpired ||
		len(replayed.Transactions) != 1 {
		t.Fatalf("unexpected expiry replay: %+v", replayed)
	}

	command.BizID++
	_, err = service.Expire(context.Background(), repo, command)
	requireAssetProblemCode(t, err, "WT_EXPIRY_LEDGER_CONFLICT")
}

func TestAssetServiceExpireRejectsPrematureLotAndMarksEmptyDueLot(t *testing.T) {
	now := assetTestTime()
	service := NewAssetService(&assetTestIDs{next: 600}).WithClock(
		func() time.Time { return now },
	)
	command := ExpireCommand{
		OwnerCustomerID: 101,
		TransactionType: TransactionTypeExpiry,
		BizType:         "wine_ticket_expiry",
		BizID:           92,
		ActionKey:       "expiry:92",
		OccurredAt:      now,
	}

	future := assetTestSpendableLot(92, now)
	command.LotID = future.ID
	repo := newAssetTestRepository(future)
	_, err := service.Expire(context.Background(), repo, command)
	requireAssetProblemCode(t, err, "WT_LOT_NOT_EXPIRED")

	empty := assetTestSpendableLot(93, now)
	empty.AvailableQuantity = 0
	empty.Status = LotStatusDepleted
	empty.ExpiresAt = now.Add(-time.Minute)
	empty.OriginalExpiresAt = empty.ExpiresAt
	command.LotID = empty.ID
	command.BizID = empty.ID
	command.ActionKey = "expiry:93"
	repo = newAssetTestRepository(empty)
	result, err := service.Expire(context.Background(), repo, command)
	if err != nil {
		t.Fatal(err)
	}
	if result.Lot.Status != LotStatusExpired ||
		result.Lot.Version != empty.Version+1 ||
		len(result.Transactions) != 0 {
		t.Fatalf("unexpected empty expiry result: %+v", result)
	}
}

func assetTestIssueCommand(now time.Time) IssueCommand {
	expiresAt := now.Add(30 * 24 * time.Hour)
	return IssueCommand{
		Lot: Lot{
			OwnerCustomerID:   11,
			PurchaseID:        21,
			SourceType:        LotSourcePurchase,
			IssuerMerchantID:  31,
			ProductID:         41,
			RedeemCityCode:    "440100",
			TotalQuantity:     4,
			AvailableQuantity: 4,
			OriginalExpiresAt: expiresAt,
			ExpiresAt:         expiresAt,
		},
		TransactionType: TransactionTypePurchaseIssue,
		BizType:         "purchase",
		BizID:           21,
		ActionKey:       "purchase:21:issue",
		OccurredAt:      now,
	}
}

func assetTestSpendableLot(id uint64, now time.Time) Lot {
	return Lot{
		ID:                id,
		LotNo:             fmt.Sprintf("WTL%d", id),
		OwnerCustomerID:   101,
		PurchaseID:        201,
		SourceType:        LotSourcePurchase,
		IssuerMerchantID:  301,
		ProductID:         401,
		RedeemCityCode:    "440100",
		TotalQuantity:     8,
		AvailableQuantity: 8,
		OriginalExpiresAt: now.Add(24 * time.Hour),
		ExpiresAt:         now.Add(24 * time.Hour),
		Status:            LotStatusActive,
		Version:           1,
	}
}

func assetTestTime() time.Time {
	return time.Date(2026, 7, 28, 10, 0, 0, 0, ShanghaiLocation)
}

func requireAssetProblemCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected problem %s, got nil", want)
	}
	var details *problem.Details
	if !errors.As(err, &details) {
		t.Fatalf("expected problem %s, got %T: %v", want, err, err)
	}
	if details.ErrorCode != want {
		t.Fatalf("problem code = %s, want %s", details.ErrorCode, want)
	}
}
