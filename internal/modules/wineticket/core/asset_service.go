package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
)

// IDGenerator 是资产核心所需的最小标识生成契约。
// snowflake.Generator 实现了该契约。
type IDGenerator interface {
	Next() uint64
}

// ErrAssetRecordNotFound 是事务级资产仓储使用的持久化无关缺失信号。
// 数据库适配器必须把驱动特有的未找到错误转换为该值。
var ErrAssetRecordNotFound = errors.New("wine ticket asset record not found")

// AssetLedgerRepository 被明确限定在事务作用域内。
// 子域调用 AssetService 变更前，必须把实现绑定到当前 MySQL 事务。
// 这样可让权益状态与台账证据在同一次提交中落库，同时避免 core 持有第二个事务边界。
type AssetLedgerRepository interface {
	TransactionByActionKey(
		ctx context.Context,
		actionKey string,
	) (Transaction, error)
	LockLot(ctx context.Context, lotID uint64) (Lot, error)
	UpdateLot(
		ctx context.Context,
		lotID uint64,
		expectedVersion uint,
		change LotAssetChange,
	) (bool, error)
	CreateTransaction(ctx context.Context, transaction *Transaction) error
}

// AssetIssuanceRepository 只为 Issue 和 Transfer 增加批次创建能力。
// 仅变更余额的操作应依赖 AssetLedgerRepository。
type AssetIssuanceRepository interface {
	AssetLedgerRepository
	CreateLot(ctx context.Context, lot *Lot) error
}

type LotAssetChange struct {
	AvailableQuantity uint
	Status            string
	EverUsed          bool
	SetEverUsed       bool
	UpdatedAt         time.Time
}

type AssetMutation struct {
	Lot          Lot
	Transactions []Transaction
	Replayed     bool
}

type AssetCommand struct {
	LotID           uint64
	OwnerCustomerID uint64
	Quantity        uint
	MarkUsed        bool
	TransactionType string
	BizType         string
	BizID           uint64
	ActionKey       string
	Metadata        any
	OccurredAt      time.Time
	ExpiryEvidence  *AssetEvidence
}

type IssueCommand struct {
	Lot             Lot
	TransactionType string
	BizType         string
	BizID           uint64
	ActionKey       string
	Metadata        any
	OccurredAt      time.Time
}

type TransferCommand struct {
	ReceiverLot     Lot
	TransactionType string
	BizType         string
	BizID           uint64
	ActionKey       string
	Metadata        any
	OccurredAt      time.Time
}

type ExpireCommand struct {
	LotID           uint64
	OwnerCustomerID uint64
	TransactionType string
	BizType         string
	BizID           uint64
	ActionKey       string
	Metadata        any
	OccurredAt      time.Time
}

type AssetEvidence struct {
	TransactionType string
	BizType         string
	BizID           uint64
	ActionKey       string
	Metadata        any
}

type AssetService struct {
	ids IDGenerator
	now func() time.Time
}

const TransactionTypeLotExpiry = TransactionTypeExpiry

func NewAssetService(ids IDGenerator) *AssetService {
	return &AssetService{ids: ids, now: time.Now}
}

func (s *AssetService) WithClock(now func() time.Time) *AssetService {
	if now != nil {
		s.now = now
	}
	return s
}

// Issue 创建新的权益批次及其正向台账证据。
func (s *AssetService) Issue(
	ctx context.Context,
	repo AssetIssuanceRepository,
	command IssueCommand,
) (AssetMutation, error) {
	if err := s.ready(repo); err != nil {
		return AssetMutation{}, err
	}
	if command.Lot.TotalQuantity == 0 ||
		command.Lot.AvailableQuantity != command.Lot.TotalQuantity {
		return AssetMutation{}, problem.Internal(
			"issued wine ticket lot must start with a positive full balance",
		)
	}
	if command.Lot.OwnerCustomerID == 0 ||
		command.Lot.PurchaseID == 0 ||
		command.Lot.ProductID == 0 ||
		command.Lot.IssuerMerchantID == 0 {
		return AssetMutation{}, problem.Internal(
			"issued wine ticket lot lineage is incomplete",
		)
	}
	if err := validateAssetEvidence(
		command.TransactionType,
		command.BizType,
		command.BizID,
		command.ActionKey,
	); err != nil {
		return AssetMutation{}, err
	}
	if command.Lot.OriginalExpiresAt.IsZero() ||
		command.Lot.ExpiresAt.IsZero() ||
		command.Lot.ExpiresAt.Before(command.Lot.OriginalExpiresAt) {
		return AssetMutation{}, problem.Internal(
			"issued wine ticket lot expiry is invalid",
		)
	}
	if replay, ok, err := replayIssueAssetMutation(
		ctx,
		repo,
		command,
	); err != nil || ok {
		return replay, err
	}

	now := s.occurredAt(command.OccurredAt)
	lot := command.Lot
	if lot.ID == 0 {
		lot.ID = s.ids.Next()
	}
	if lot.LotNo == "" {
		lot.LotNo = "WTL" + IDString(lot.ID)
	}
	if lot.ExpiryChangedAt.IsZero() {
		lot.ExpiryChangedAt = now
	}
	if lot.Status == "" {
		lot.Status = LotStatusActive
	}
	if lot.Version == 0 {
		lot.Version = 1
	}
	if lot.CreatedAt.IsZero() {
		lot.CreatedAt = now
	}
	lot.UpdatedAt = now
	if err := repo.CreateLot(ctx, &lot); err != nil {
		return AssetMutation{}, err
	}
	transaction := s.newTransaction(
		ctx,
		lot,
		command.TransactionType,
		int(lot.TotalQuantity),
		0,
		lot.TotalQuantity,
		command.BizType,
		command.BizID,
		command.ActionKey,
		command.Metadata,
		now,
	)
	if err := repo.CreateTransaction(ctx, &transaction); err != nil {
		return AssetMutation{}, err
	}
	return AssetMutation{
		Lot:          lot,
		Transactions: []Transaction{transaction},
	}, nil
}

// Freeze 从可用余额中扣除数量以预留权益。
func (s *AssetService) Freeze(
	ctx context.Context,
	repo AssetLedgerRepository,
	command AssetCommand,
) (AssetMutation, error) {
	return s.debit(ctx, repo, command, command.MarkUsed)
}

// Redeem 执行与 Freeze 相同的受保护扣减，并记录该批次已经进入使用流程。
// 最终履约仍归核销子域负责。
func (s *AssetService) Redeem(
	ctx context.Context,
	repo AssetLedgerRepository,
	command AssetCommand,
) (AssetMutation, error) {
	return s.debit(ctx, repo, command, true)
}

func (s *AssetService) debit(
	ctx context.Context,
	repo AssetLedgerRepository,
	command AssetCommand,
	markUsed bool,
) (AssetMutation, error) {
	if err := s.ready(repo); err != nil {
		return AssetMutation{}, err
	}
	if err := validateAssetCommand(command); err != nil {
		return AssetMutation{}, err
	}
	delta := -int(command.Quantity)
	if replay, ok, err := replayAssetMutation(
		ctx,
		repo,
		command.ActionKey,
		command.LotID,
		command.TransactionType,
		command.BizType,
		command.BizID,
		delta,
	); err != nil {
		return AssetMutation{}, err
	} else if ok {
		transaction := replay.Transactions[0]
		if transaction.OwnerCustomerID != command.OwnerCustomerID ||
			replay.Lot.OwnerCustomerID != command.OwnerCustomerID ||
			transaction.BeforeAvailableQuantity <
				transaction.AfterAvailableQuantity ||
			transaction.BeforeAvailableQuantity-
				transaction.AfterAvailableQuantity != command.Quantity {
			return AssetMutation{}, assetReplayConflict()
		}
		return replay, nil
	}
	lot, err := repo.LockLot(ctx, command.LotID)
	if errors.Is(err, ErrAssetRecordNotFound) {
		return AssetMutation{}, problem.NotFound(
			"WT_LOT_NOT_FOUND",
			"wine ticket lot not found",
		)
	}
	if err != nil {
		return AssetMutation{}, err
	}
	now := s.occurredAt(command.OccurredAt)
	if lot.OwnerCustomerID != command.OwnerCustomerID {
		return AssetMutation{}, problem.NotFound(
			"WT_LOT_NOT_FOUND",
			"wine ticket lot not found",
		)
	}
	if lot.Status != LotStatusActive ||
		!lot.ExpiresAt.After(now) ||
		command.Quantity > lot.AvailableQuantity {
		return AssetMutation{}, problem.Conflict(
			"WT_INSUFFICIENT_AVAILABLE_QUANTITY",
			"wine ticket available quantity is insufficient",
		)
	}
	after := lot.AvailableQuantity - command.Quantity
	status := LotStatusActive
	if after == 0 {
		status = LotStatusDepleted
	}
	changed, err := repo.UpdateLot(
		ctx,
		lot.ID,
		lot.Version,
		LotAssetChange{
			AvailableQuantity: after,
			Status:            status,
			EverUsed:          markUsed || lot.EverUsed,
			SetEverUsed:       markUsed,
			UpdatedAt:         now,
		},
	)
	if err != nil {
		return AssetMutation{}, err
	}
	if !changed {
		return AssetMutation{}, concurrentAssetMutation()
	}
	transaction := s.newTransaction(
		ctx,
		lot,
		command.TransactionType,
		delta,
		lot.AvailableQuantity,
		after,
		command.BizType,
		command.BizID,
		command.ActionKey,
		command.Metadata,
		now,
	)
	if err := repo.CreateTransaction(ctx, &transaction); err != nil {
		return AssetMutation{}, err
	}
	lot.AvailableQuantity = after
	lot.Status = status
	if markUsed {
		lot.EverUsed = true
	}
	lot.Version++
	lot.UpdatedAt = now
	return AssetMutation{
		Lot:          lot,
		Transactions: []Transaction{transaction},
	}, nil
}

// Transfer 将来源批次中已冻结的权益转化为接收方持有的新批次，
// 不会再次变更来源批次。
func (s *AssetService) Transfer(
	ctx context.Context,
	repo AssetIssuanceRepository,
	command TransferCommand,
) (AssetMutation, error) {
	if command.ReceiverLot.SourceType != LotSourceGift ||
		command.ReceiverLot.SourceLotID == nil ||
		command.ReceiverLot.SourceGiftID == nil {
		return AssetMutation{}, problem.Internal(
			"transferred wine ticket lot lineage is incomplete",
		)
	}
	return s.Issue(ctx, repo, IssueCommand{
		Lot:             command.ReceiverLot,
		TransactionType: command.TransactionType,
		BizType:         command.BizType,
		BizID:           command.BizID,
		ActionKey:       command.ActionKey,
		Metadata:        command.Metadata,
		OccurredAt:      command.OccurredAt,
	})
}

// Restore 返还此前冻结的数量。批次已过期时，返还的余额会立即再次扣除，
// 并记录独立的过期证据。
func (s *AssetService) Restore(
	ctx context.Context,
	repo AssetLedgerRepository,
	command AssetCommand,
) (AssetMutation, error) {
	if err := s.ready(repo); err != nil {
		return AssetMutation{}, err
	}
	if err := validateAssetCommand(command); err != nil {
		return AssetMutation{}, err
	}
	if replay, ok, err := replayAssetMutation(
		ctx,
		repo,
		command.ActionKey,
		command.LotID,
		command.TransactionType,
		command.BizType,
		command.BizID,
		int(command.Quantity),
	); err != nil {
		return AssetMutation{}, err
	} else if ok {
		return validateRestoreReplay(ctx, repo, command, replay)
	}
	lot, err := repo.LockLot(ctx, command.LotID)
	if errors.Is(err, ErrAssetRecordNotFound) {
		return AssetMutation{}, problem.NotFound(
			"WT_LOT_NOT_FOUND",
			"wine ticket lot not found",
		)
	}
	if err != nil {
		return AssetMutation{}, err
	}
	if lot.Status == LotStatusRefunded {
		return AssetMutation{}, problem.Conflict(
			"WT_LOT_INVALID_STATUS",
			"refunded wine ticket lot cannot be restored",
		)
	}
	if lot.OwnerCustomerID != command.OwnerCustomerID ||
		command.Quantity > lot.TotalQuantity-lot.AvailableQuantity {
		return AssetMutation{}, problem.Internal(
			"wine ticket restore would exceed lot quantity",
		)
	}
	now := s.occurredAt(command.OccurredAt)
	restored := lot.AvailableQuantity + command.Quantity
	expired := !lot.ExpiresAt.After(now)
	var expiryEvidence AssetEvidence
	if expired {
		expiryEvidence = restoreExpiryEvidence(command)
		if err := validateAssetEvidence(
			expiryEvidence.TransactionType,
			expiryEvidence.BizType,
			expiryEvidence.BizID,
			expiryEvidence.ActionKey,
		); err != nil {
			return AssetMutation{}, err
		}
	}
	changed, err := repo.UpdateLot(
		ctx,
		lot.ID,
		lot.Version,
		LotAssetChange{
			AvailableQuantity: restored,
			Status:            LotStatusActive,
			UpdatedAt:         now,
		},
	)
	if err != nil {
		return AssetMutation{}, err
	}
	if !changed {
		return AssetMutation{}, concurrentAssetMutation()
	}
	restore := s.newTransaction(
		ctx,
		lot,
		command.TransactionType,
		int(command.Quantity),
		lot.AvailableQuantity,
		restored,
		command.BizType,
		command.BizID,
		command.ActionKey,
		command.Metadata,
		now,
	)
	if err := repo.CreateTransaction(ctx, &restore); err != nil {
		return AssetMutation{}, err
	}
	transactions := []Transaction{restore}
	lot.AvailableQuantity = restored
	lot.Status = LotStatusActive
	lot.Version++
	lot.UpdatedAt = now
	if expired {
		changed, err = repo.UpdateLot(
			ctx,
			lot.ID,
			lot.Version,
			LotAssetChange{
				AvailableQuantity: 0,
				Status:            LotStatusExpired,
				UpdatedAt:         now,
			},
		)
		if err != nil {
			return AssetMutation{}, err
		}
		if !changed {
			return AssetMutation{}, concurrentAssetMutation()
		}
		expiry := s.newTransaction(
			ctx,
			lot,
			expiryEvidence.TransactionType,
			-int(restored),
			restored,
			0,
			expiryEvidence.BizType,
			expiryEvidence.BizID,
			expiryEvidence.ActionKey,
			expiryEvidence.Metadata,
			now,
		)
		if err := repo.CreateTransaction(ctx, &expiry); err != nil {
			return AssetMutation{}, err
		}
		transactions = append(transactions, expiry)
		lot.AvailableQuantity = 0
		lot.Status = LotStatusExpired
		lot.Version++
	}
	return AssetMutation{Lot: lot, Transactions: transactions}, nil
}

func (s *AssetService) ready(repo AssetLedgerRepository) error {
	if s == nil || s.ids == nil || s.now == nil || repo == nil {
		return problem.Internal("wine ticket asset service is not configured")
	}
	return nil
}

func (s *AssetService) occurredAt(value time.Time) time.Time {
	if value.IsZero() {
		return NowShanghai(s.now)
	}
	return value.In(ShanghaiLocation).Truncate(time.Millisecond)
}

func (s *AssetService) newTransaction(
	ctx context.Context,
	lot Lot,
	transactionType string,
	delta int,
	before uint,
	after uint,
	bizType string,
	bizID uint64,
	actionKey string,
	metadata any,
	now time.Time,
) Transaction {
	id := s.ids.Next()
	return Transaction{
		ID:                      id,
		TransactionNo:           "WTT" + IDString(id),
		LotID:                   lot.ID,
		OwnerCustomerID:         lot.OwnerCustomerID,
		TransactionType:         transactionType,
		QuantityDelta:           delta,
		BeforeAvailableQuantity: before,
		AfterAvailableQuantity:  after,
		BizType:                 bizType,
		BizID:                   bizID,
		ActionKey:               actionKey,
		MetadataJSON:            JSONData(metadata),
		RequestID:               requestctx.RequestIDPtr(ctx),
		CreatedAt:               now,
	}
}

// Expire 在批次到期后扣除剩余可用余额，并记录终态台账证据。
// 传入的仓储必须绑定到调用方当前业务事务。
func (s *AssetService) Expire(
	ctx context.Context,
	repo AssetLedgerRepository,
	command ExpireCommand,
) (AssetMutation, error) {
	if err := s.ready(repo); err != nil {
		return AssetMutation{}, err
	}
	if command.LotID == 0 || command.OwnerCustomerID == 0 {
		return AssetMutation{}, problem.Internal(
			"wine ticket expiry command is incomplete",
		)
	}
	if err := validateAssetEvidence(
		command.TransactionType,
		command.BizType,
		command.BizID,
		command.ActionKey,
	); err != nil {
		return AssetMutation{}, err
	}

	existing, err := repo.TransactionByActionKey(ctx, command.ActionKey)
	switch {
	case err == nil:
		if existing.LotID != command.LotID ||
			existing.OwnerCustomerID != command.OwnerCustomerID ||
			existing.TransactionType != command.TransactionType ||
			existing.BizType != command.BizType ||
			existing.BizID != command.BizID ||
			existing.QuantityDelta >= 0 ||
			existing.BeforeAvailableQuantity == 0 ||
			existing.QuantityDelta !=
				-int(existing.BeforeAvailableQuantity) ||
			existing.AfterAvailableQuantity != 0 {
			return AssetMutation{}, problem.Conflict(
				"WT_EXPIRY_LEDGER_CONFLICT",
				"wine ticket expiry action key was used by another mutation",
			)
		}
		lot, lockErr := repo.LockLot(ctx, existing.LotID)
		if lockErr != nil {
			return AssetMutation{}, lockErr
		}
		if lot.ID != command.LotID ||
			lot.OwnerCustomerID != command.OwnerCustomerID ||
			lot.AvailableQuantity != 0 ||
			lot.Status != LotStatusExpired {
			return AssetMutation{}, problem.Conflict(
				"WT_EXPIRY_LEDGER_CONFLICT",
				"wine ticket expiry replay does not match the terminal lot",
			)
		}
		return AssetMutation{
			Lot:          lot,
			Transactions: []Transaction{existing},
			Replayed:     true,
		}, nil
	case !errors.Is(err, ErrAssetRecordNotFound):
		return AssetMutation{}, err
	}

	lot, err := repo.LockLot(ctx, command.LotID)
	if errors.Is(err, ErrAssetRecordNotFound) {
		return AssetMutation{}, problem.NotFound(
			"WT_LOT_NOT_FOUND",
			"wine ticket lot not found",
		)
	}
	if err != nil {
		return AssetMutation{}, err
	}
	if lot.OwnerCustomerID != command.OwnerCustomerID {
		return AssetMutation{}, problem.NotFound(
			"WT_LOT_NOT_FOUND",
			"wine ticket lot not found",
		)
	}
	now := s.occurredAt(command.OccurredAt)
	if lot.ExpiresAt.After(now) {
		return AssetMutation{}, problem.Conflict(
			"WT_LOT_NOT_EXPIRED",
			"wine ticket lot is not expired",
		)
	}
	if lot.Status == LotStatusRefunded {
		return AssetMutation{}, problem.Conflict(
			"WT_LOT_INVALID_STATUS",
			"refunded wine ticket lot cannot expire",
		)
	}
	if lot.AvailableQuantity == 0 && lot.Status == LotStatusExpired {
		return AssetMutation{Lot: lot, Replayed: true}, nil
	}
	if lot.Status != LotStatusActive && lot.Status != LotStatusDepleted {
		return AssetMutation{}, problem.Conflict(
			"WT_LOT_INVALID_STATUS",
			"wine ticket lot cannot enter expired status",
		)
	}

	before := lot.AvailableQuantity
	changed, err := repo.UpdateLot(
		ctx,
		lot.ID,
		lot.Version,
		LotAssetChange{
			AvailableQuantity: 0,
			Status:            LotStatusExpired,
			UpdatedAt:         now,
		},
	)
	if err != nil {
		return AssetMutation{}, err
	}
	if !changed {
		return AssetMutation{}, concurrentAssetMutation()
	}
	lot.AvailableQuantity = 0
	lot.Status = LotStatusExpired
	lot.Version++
	lot.UpdatedAt = now
	if before == 0 {
		return AssetMutation{Lot: lot}, nil
	}

	transaction := s.newTransaction(
		ctx,
		lot,
		command.TransactionType,
		-int(before),
		before,
		0,
		command.BizType,
		command.BizID,
		command.ActionKey,
		command.Metadata,
		now,
	)
	if err := repo.CreateTransaction(ctx, &transaction); err != nil {
		return AssetMutation{}, err
	}
	return AssetMutation{
		Lot:          lot,
		Transactions: []Transaction{transaction},
	}, nil
}

func validateAssetCommand(command AssetCommand) error {
	if command.LotID == 0 ||
		command.OwnerCustomerID == 0 ||
		command.Quantity == 0 {
		return problem.Internal("wine ticket asset command is incomplete")
	}
	return validateAssetEvidence(
		command.TransactionType,
		command.BizType,
		command.BizID,
		command.ActionKey,
	)
}

func validateAssetEvidence(
	transactionType string,
	bizType string,
	bizID uint64,
	actionKey string,
) error {
	if transactionType == "" || bizType == "" || bizID == 0 ||
		len(actionKey) == 0 || len(actionKey) > 160 {
		return problem.Internal("wine ticket asset evidence is incomplete")
	}
	return nil
}

func replayIssueAssetMutation(
	ctx context.Context,
	repo AssetIssuanceRepository,
	command IssueCommand,
) (AssetMutation, bool, error) {
	replay, ok, err := replayAssetMutation(
		ctx,
		repo,
		command.ActionKey,
		command.Lot.ID,
		command.TransactionType,
		command.BizType,
		command.BizID,
		int(command.Lot.TotalQuantity),
	)
	if err != nil || !ok {
		return replay, ok, err
	}
	issuance := replay.Transactions[0]
	if issuance.OwnerCustomerID != command.Lot.OwnerCustomerID ||
		issuance.BeforeAvailableQuantity != 0 ||
		issuance.AfterAvailableQuantity != command.Lot.TotalQuantity ||
		!sameLotLineage(replay.Lot, command.Lot) {
		return AssetMutation{}, true, assetReplayConflict()
	}
	return replay, true, nil
}

func sameLotLineage(actual Lot, expected Lot) bool {
	return actual.OwnerCustomerID == expected.OwnerCustomerID &&
		actual.PurchaseID == expected.PurchaseID &&
		actual.SourceType == expected.SourceType &&
		sameOptionalID(actual.SourceLotID, expected.SourceLotID) &&
		sameOptionalID(actual.SourceGiftID, expected.SourceGiftID) &&
		actual.IssuerMerchantID == expected.IssuerMerchantID &&
		actual.ProductID == expected.ProductID &&
		actual.RedeemCityCode == expected.RedeemCityCode &&
		actual.TotalQuantity == expected.TotalQuantity &&
		actual.OriginalExpiresAt.Equal(expected.OriginalExpiresAt)
}

func sameOptionalID(left *uint64, right *uint64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func restoreExpiryEvidence(command AssetCommand) AssetEvidence {
	if command.ExpiryEvidence != nil {
		return *command.ExpiryEvidence
	}
	return AssetEvidence{
		TransactionType: TransactionTypeExpiry,
		BizType:         command.BizType,
		BizID:           command.BizID,
		ActionKey:       command.ActionKey + ":expiry",
		Metadata: map[string]any{
			"restore_action_key": command.ActionKey,
		},
	}
}

func validateRestoreReplay(
	ctx context.Context,
	repo AssetLedgerRepository,
	command AssetCommand,
	replay AssetMutation,
) (AssetMutation, error) {
	restore := replay.Transactions[0]
	if restore.OwnerCustomerID != command.OwnerCustomerID ||
		replay.Lot.OwnerCustomerID != command.OwnerCustomerID ||
		restore.BeforeAvailableQuantity+command.Quantity !=
			restore.AfterAvailableQuantity {
		return AssetMutation{}, assetReplayConflict()
	}
	if replay.Lot.ExpiresAt.After(restore.CreatedAt) {
		return replay, nil
	}

	evidence := restoreExpiryEvidence(command)
	if err := validateAssetEvidence(
		evidence.TransactionType,
		evidence.BizType,
		evidence.BizID,
		evidence.ActionKey,
	); err != nil {
		return AssetMutation{}, err
	}
	expiry, err := repo.TransactionByActionKey(ctx, evidence.ActionKey)
	if err != nil {
		if errors.Is(err, ErrAssetRecordNotFound) {
			return AssetMutation{}, assetReplayConflict()
		}
		return AssetMutation{}, err
	}
	if expiry.LotID != restore.LotID ||
		expiry.OwnerCustomerID != command.OwnerCustomerID ||
		expiry.TransactionType != evidence.TransactionType ||
		expiry.BizType != evidence.BizType ||
		expiry.BizID != evidence.BizID ||
		expiry.QuantityDelta != -int(restore.AfterAvailableQuantity) ||
		expiry.BeforeAvailableQuantity != restore.AfterAvailableQuantity ||
		expiry.AfterAvailableQuantity != 0 ||
		!expiry.CreatedAt.Equal(restore.CreatedAt) ||
		replay.Lot.AvailableQuantity != 0 ||
		replay.Lot.Status != LotStatusExpired {
		return AssetMutation{}, assetReplayConflict()
	}
	replay.Transactions = append(replay.Transactions, expiry)
	return replay, nil
}

func replayAssetMutation(
	ctx context.Context,
	repo AssetLedgerRepository,
	actionKey string,
	lotID uint64,
	transactionType string,
	bizType string,
	bizID uint64,
	delta int,
) (AssetMutation, bool, error) {
	existing, err := repo.TransactionByActionKey(ctx, actionKey)
	if errors.Is(err, ErrAssetRecordNotFound) {
		return AssetMutation{}, false, nil
	}
	if err != nil {
		return AssetMutation{}, false, err
	}
	if (lotID != 0 && existing.LotID != lotID) ||
		existing.TransactionType != transactionType ||
		existing.BizType != bizType ||
		existing.BizID != bizID ||
		existing.QuantityDelta != delta {
		return AssetMutation{}, true, problem.Conflict(
			"WT_IDEMPOTENCY_CONFLICT",
			"wine ticket asset action key was used by another mutation",
		)
	}
	lot, err := repo.LockLot(ctx, existing.LotID)
	if err != nil {
		return AssetMutation{}, true, err
	}
	return AssetMutation{
		Lot:          lot,
		Transactions: []Transaction{existing},
		Replayed:     true,
	}, true, nil
}

func assetReplayConflict() error {
	return problem.Conflict(
		"WT_IDEMPOTENCY_CONFLICT",
		"wine ticket asset replay does not match the original mutation",
	)
}

func concurrentAssetMutation() error {
	return problem.Conflict(
		"WT_CONCURRENT_MODIFICATION",
		"wine ticket lot changed concurrently",
	)
}

func ValidateAssetTransaction(transaction Transaction) error {
	if transaction.QuantityDelta == 0 {
		return fmt.Errorf("wine ticket transaction delta must not be zero")
	}
	calculated := int64(transaction.BeforeAvailableQuantity) +
		int64(transaction.QuantityDelta)
	if calculated < 0 ||
		calculated != int64(transaction.AfterAvailableQuantity) {
		return fmt.Errorf("wine ticket transaction balance equation is invalid")
	}
	return nil
}
