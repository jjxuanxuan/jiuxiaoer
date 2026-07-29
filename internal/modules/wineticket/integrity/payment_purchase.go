package integrity

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm"

	purchasedomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
	renewaldomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/renewal"
)

type reconciliationPayment struct {
	ID              uint64
	PaymentNo       string
	BizType         *string
	BizID           *uint64
	CustomerID      uint64
	Status          string
	Amount          int64
	Currency        string
	RefundedAmount  int64
	ProviderTradeNo *string
	ProviderStatus  *string
	UpdatedAt       time.Time
}

type purchaseIssueFacts struct {
	OriginalLotCount    int64 `json:"original_lot_count"`
	OriginalLotQuantity int64 `json:"original_lot_quantity"`
	IssueEntryCount     int64 `json:"issue_entry_count"`
	IssueLotCount       int64 `json:"issue_lot_count"`
	IssueQuantity       int64 `json:"issue_quantity"`
	InvalidIssueCount   int64 `json:"invalid_issue_count"`
}

type reconciliationPurchaseRefundCounts struct {
	PurchaseID               uint64
	AnyCompensation          int64
	NonCancelledCompensation int64
	Succeeded                int64
}

func (f purchaseIssueFacts) complete(expected uint) bool {
	return f.OriginalLotCount > 0 &&
		f.OriginalLotQuantity == int64(expected) &&
		f.IssueEntryCount == f.OriginalLotCount &&
		f.IssueLotCount == f.OriginalLotCount &&
		f.IssueQuantity == int64(expected) &&
		f.InvalidIssueCount == 0
}

func (s *IntegrityService) scanPayments(
	ctx context.Context,
	tx *gorm.DB,
	afterID uint64,
	upperID *uint64,
	limit int,
) (int, uint64, []reconciliationDiscrepancy, error) {
	rows, err := s.repo.listIntegrityPayments(
		ctx,
		tx,
		afterID,
		upperID,
		limit,
	)
	if err != nil {
		return 0, afterID, nil, err
	}

	purchaseIDs := make([]uint64, 0, len(rows))
	renewalIDs := make([]uint64, 0, len(rows))
	for _, payment := range rows {
		switch pointerString(payment.BizType) {
		case PurchasePaymentBusiness:
			if id := pointerUint64(payment.BizID); id != 0 {
				purchaseIDs = append(purchaseIDs, id)
			}
		case RenewalPaymentBusiness:
			if id := pointerUint64(payment.BizID); id != 0 {
				renewalIDs = append(renewalIDs, id)
			}
		}
	}
	purchases, err := s.repo.loadIntegrityPurchases(ctx, tx, purchaseIDs)
	if err != nil {
		return 0, afterID, nil, err
	}
	renewals, err := s.repo.loadIntegrityRenewals(ctx, tx, renewalIDs)
	if err != nil {
		return 0, afterID, nil, err
	}
	issueFacts, err := s.repo.loadPurchaseIssueFactsBatch(ctx, tx, purchaseIDs)
	if err != nil {
		return 0, afterID, nil, err
	}
	refundCounts, err := s.repo.loadPurchaseRefundCounts(ctx, tx, purchaseIDs)
	if err != nil {
		return 0, afterID, nil, err
	}

	discrepancies := make([]reconciliationDiscrepancy, 0)
	for _, payment := range rows {
		providerSucceeded := payment.ProviderStatus != nil &&
			strings.EqualFold(strings.TrimSpace(*payment.ProviderStatus), "SUCCESS")
		locallySucceeded := payment.Status == "succeeded"
		settlementException := payment.Status == "exception" && providerSucceeded
		if !locallySucceeded && !settlementException {
			continue
		}
		// 不可变 ID 游标推进后，支付机构已确认的资金事实不得被跳过。
		// 结算路径具备幂等性，回放期间有效异常的更新插入会吸收重复观测。
		switch pointerString(payment.BizType) {
		case PurchasePaymentBusiness:
			purchaseID := pointerUint64(payment.BizID)
			purchase, exists := purchases[purchaseID]
			fact := checkPurchasePayment(
				payment,
				purchase,
				exists,
				issueFacts[purchaseID],
				refundCounts[purchaseID],
			)
			if fact != nil {
				discrepancies = append(discrepancies, *fact)
			}
		case RenewalPaymentBusiness:
			renewal, exists := renewals[pointerUint64(payment.BizID)]
			fact := checkRenewalPayment(payment, renewal, exists)
			if fact != nil {
				discrepancies = append(discrepancies, *fact)
			}
		}
	}
	return len(rows), lastPaymentID(rows, afterID), discrepancies, nil
}

func checkPurchasePayment(
	payment reconciliationPayment,
	purchase purchasedomain.Purchase,
	purchaseExists bool,
	issueFacts purchaseIssueFacts,
	refundCounts reconciliationPurchaseRefundCounts,
) *reconciliationDiscrepancy {
	purchaseID := pointerUint64(payment.BizID)
	expected := map[string]any{
		"payment_status": "succeeded",
		"settlement":     "exactly_one_of_issued_or_issuance_compensation",
		"payment_id":     payment.ID,
	}
	actual := map[string]any{
		"payment_status": payment.Status,
		"payment_id":     payment.ID,
		"biz_id":         purchaseID,
	}
	anomaly := func(kind string, purchase *purchasedomain.Purchase) *reconciliationDiscrepancy {
		row := reconciliationDiscrepancy{
			Rule: reconciliationRulePaymentSettlement,
			Kind: kind, BizType: "payment", BizID: payment.ID,
			BizNo: &payment.PaymentNo, Severity: "P1",
			Expected: expected, Actual: actual,
		}
		if purchase != nil {
			row.IssuerMerchantID = &purchase.IssuerMerchantID
		}
		return &row
	}
	if purchaseID == 0 {
		return anomaly("purchase_business_link", nil)
	}

	if !purchaseExists {
		actual["purchase_exists"] = false
		return anomaly("purchase_business_link", nil)
	}
	actual["purchase_exists"] = true
	actual["purchase_status"] = purchase.Status
	actual["provider_status"] = pointerString(payment.ProviderStatus)
	if purchase.PaymentID != payment.ID ||
		purchase.CustomerID != payment.CustomerID ||
		purchase.PayableAmount != payment.Amount ||
		purchase.Currency != payment.Currency {
		actual["purchase_payment_id"] = purchase.PaymentID
		actual["purchase_customer_id"] = purchase.CustomerID
		actual["payment_customer_id"] = payment.CustomerID
		actual["purchase_payable_amount"] = purchase.PayableAmount
		actual["payment_amount"] = payment.Amount
		actual["purchase_currency"] = purchase.Currency
		actual["payment_currency"] = payment.Currency
		return anomaly("purchase_business_link", &purchase)
	}

	compensationCount := refundCounts.NonCancelledCompensation
	issued := issueFacts.complete(purchase.TotalBottleQuantity) &&
		reconciliationPurchaseHasIssuedLifecycle(purchase.Status)
	compensating := compensationCount == 1 &&
		(purchase.Status == PurchaseStatusRefundHolding ||
			purchase.Status == PurchaseStatusRefundException ||
			purchase.Status == PurchaseStatusRefunded)
	actual["issue_facts"] = issueFacts
	actual["issuance_compensation_count"] = compensationCount
	actual["issued_branch"] = issued
	actual["issuance_compensation_branch"] = compensating
	branchCount := 0
	if issued {
		branchCount++
	}
	if compensating {
		branchCount++
	}
	if branchCount != 1 || compensationCount > 1 {
		fact := anomaly("purchase_settlement_cardinality", &purchase)
		if branchCount > 1 || (compensationCount > 0 &&
			(issueFacts.OriginalLotCount > 0 || issueFacts.IssueEntryCount > 0)) {
			fact.Severity = "P0"
		}
		return fact
	}

	// 呈现退款状态的购买记录本身必须对应一条业务退款。
	// 即使退款阶段没有可作为起点的记录，也能据此发现孤立终态。
	if purchase.Status == PurchaseStatusRefunded {
		succeeded := refundCounts.Succeeded
		actual["succeeded_refund_count"] = succeeded
		if succeeded != 1 {
			return anomaly("purchase_refund_settlement_cardinality", &purchase)
		}
	}
	return nil
}

func reconciliationPurchaseHasIssuedLifecycle(status string) bool {
	switch status {
	case PurchaseStatusIssued,
		PurchaseStatusRefundHolding,
		PurchaseStatusRefundException,
		PurchaseStatusRefunded:
		return true
	default:
		return false
	}
}

func checkRenewalPayment(
	payment reconciliationPayment,
	renewal renewaldomain.Renewal,
	renewalExists bool,
) *reconciliationDiscrepancy {
	renewalID := pointerUint64(payment.BizID)
	expected := map[string]any{
		"payment_status": "succeeded",
		"settlement":     "exactly_one_of_applied_or_compensating_refund",
		"payment_id":     payment.ID,
	}
	actual := map[string]any{
		"payment_status": payment.Status,
		"payment_id":     payment.ID,
		"biz_id":         renewalID,
	}
	anomaly := func(kind string, renewal *renewaldomain.Renewal) *reconciliationDiscrepancy {
		row := reconciliationDiscrepancy{
			Rule: reconciliationRulePaymentSettlement,
			Kind: kind, BizType: "payment", BizID: payment.ID,
			BizNo: &payment.PaymentNo, Severity: "P1",
			Expected: expected, Actual: actual,
		}
		return &row
	}
	if renewalID == 0 {
		return anomaly("renewal_business_link", nil)
	}
	if !renewalExists {
		actual["renewal_exists"] = false
		return anomaly("renewal_business_link", nil)
	}
	actual["renewal_exists"] = true
	actual["renewal_status"] = renewal.Status
	actual["provider_status"] = pointerString(payment.ProviderStatus)
	if renewal.PaymentID == nil ||
		*renewal.PaymentID != payment.ID ||
		renewal.CustomerID != payment.CustomerID ||
		renewal.FeeAmount != payment.Amount ||
		renewal.Currency != payment.Currency {
		actual["renewal_payment_id"] = renewal.PaymentID
		actual["renewal_customer_id"] = renewal.CustomerID
		actual["payment_customer_id"] = payment.CustomerID
		actual["renewal_fee_amount"] = renewal.FeeAmount
		actual["payment_amount"] = payment.Amount
		actual["renewal_currency"] = renewal.Currency
		actual["payment_currency"] = payment.Currency
		return anomaly("renewal_business_link", &renewal)
	}
	applied := renewal.Status == RenewalStatusCompleted &&
		renewal.CompensatingRefundID == nil
	compensating := (renewal.Status == RenewalStatusCompensatingRefund ||
		renewal.Status == RenewalStatusRefundException ||
		renewal.Status == RenewalStatusRefunded) &&
		renewal.CompensatingRefundID != nil
	actual["applied_branch"] = applied
	actual["compensating_refund_branch"] = compensating
	branchCount := 0
	if applied {
		branchCount++
	}
	if compensating {
		branchCount++
	}
	if branchCount != 1 {
		return anomaly("renewal_settlement_cardinality", &renewal)
	}
	return nil
}

func (s *IntegrityService) scanPurchases(
	ctx context.Context,
	tx *gorm.DB,
	afterID uint64,
	upperID *uint64,
	limit int,
) (int, uint64, []reconciliationDiscrepancy, error) {
	rows, err := s.repo.listIntegrityPurchases(
		ctx,
		tx,
		afterID,
		upperID,
		limit,
	)
	if err != nil {
		return 0, afterID, nil, err
	}
	purchaseIDs := make([]uint64, 0, len(rows))
	for _, purchase := range rows {
		switch purchase.Status {
		case PurchaseStatusIssued,
			PurchaseStatusRefundHolding,
			PurchaseStatusRefundException,
			PurchaseStatusRefunded:
			purchaseIDs = append(purchaseIDs, purchase.ID)
		}
	}
	refundCounts, err := s.repo.loadPurchaseRefundCounts(ctx, tx, purchaseIDs)
	if err != nil {
		return 0, afterID, nil, err
	}
	issueFacts, err := s.repo.loadPurchaseIssueFactsBatch(ctx, tx, purchaseIDs)
	if err != nil {
		return 0, afterID, nil, err
	}
	discrepancies := make([]reconciliationDiscrepancy, 0)
	for _, purchase := range rows {
		switch purchase.Status {
		case PurchaseStatusIssued,
			PurchaseStatusRefundHolding,
			PurchaseStatusRefundException,
			PurchaseStatusRefunded:
		default:
			continue
		}
		// REC-WT-002 明确排除发放补偿；
		// 其零权益不变量由 REC-WT-007 检查。
		if refundCounts[purchase.ID].AnyCompensation != 0 {
			continue
		}
		facts := issueFacts[purchase.ID]
		if facts.complete(purchase.TotalBottleQuantity) {
			continue
		}
		discrepancies = append(discrepancies, reconciliationDiscrepancy{
			Rule:    reconciliationRulePurchaseIssue,
			Kind:    "purchase_issue_quantity",
			BizType: "wine_ticket_purchase", BizID: purchase.ID,
			BizNo:            &purchase.PurchaseNo,
			IssuerMerchantID: &purchase.IssuerMerchantID,
			Severity:         "P1",
			Expected: map[string]any{
				"total_bottle_quantity":      purchase.TotalBottleQuantity,
				"original_lot_quantity":      purchase.TotalBottleQuantity,
				"issue_quantity":             purchase.TotalBottleQuantity,
				"one_issue_per_original_lot": true,
			},
			Actual: facts,
		})
	}
	return len(rows), lastPurchaseID(rows, afterID), discrepancies, nil
}

func (r *reconciliationRepository) listIntegrityPayments(
	ctx context.Context,
	tx *gorm.DB,
	afterID uint64,
	upperID *uint64,
	limit int,
) ([]reconciliationPayment, error) {
	var rows []reconciliationPayment
	query := tx.WithContext(ctx).Table("payments").
		Select(`
			id, payment_no, biz_type, biz_id, customer_id, status,
			amount, currency, refunded_amount, provider_trade_no,
			provider_status, updated_at
		`).
		Where(
			`biz_type IN ?
			 AND (status = ? OR (status = ? AND provider_status = ?))`,
			[]string{PurchasePaymentBusiness, RenewalPaymentBusiness},
			"succeeded",
			"exception",
			"SUCCESS",
		)
	query = r.idWindow(query, "id", afterID, upperID)
	err := query.Order("id").Limit(limit).Scan(&rows).Error
	return rows, err
}

func (r *reconciliationRepository) listIntegrityPurchases(
	ctx context.Context,
	tx *gorm.DB,
	afterID uint64,
	upperID *uint64,
	limit int,
) ([]purchasedomain.Purchase, error) {
	var rows []purchasedomain.Purchase
	query := r.idWindow(
		tx.WithContext(ctx).Model(&purchasedomain.Purchase{}),
		"id",
		afterID,
		upperID,
	)
	err := query.Order("id").Limit(limit).Find(&rows).Error
	return rows, err
}

func (r *reconciliationRepository) loadPurchaseIssueFacts(
	ctx context.Context,
	tx *gorm.DB,
	purchaseID uint64,
) (purchaseIssueFacts, error) {
	rows, err := r.loadPurchaseIssueFactsBatch(
		ctx,
		tx,
		[]uint64{purchaseID},
	)
	if err != nil {
		return purchaseIssueFacts{}, err
	}
	return rows[purchaseID], nil
}

func (r *reconciliationRepository) loadPurchaseIssueFactsBatch(
	ctx context.Context,
	tx *gorm.DB,
	purchaseIDs []uint64,
) (map[uint64]purchaseIssueFacts, error) {
	purchaseIDs = reconciliationUniqueIDs(purchaseIDs)
	result := make(map[uint64]purchaseIssueFacts, len(purchaseIDs))
	if len(purchaseIDs) == 0 {
		return result, nil
	}
	var lots []struct {
		PurchaseID          uint64
		OriginalLotCount    int64
		OriginalLotQuantity int64
	}
	if err := tx.WithContext(ctx).Table("wine_ticket_lots").
		Select(`
			purchase_id,
			COUNT(*) AS original_lot_count,
			COALESCE(SUM(total_quantity), 0) AS original_lot_quantity
		`).
		Where(
			"purchase_id IN ? AND source_type = ?",
			purchaseIDs,
			LotSourcePurchase,
		).
		Group("purchase_id").
		Scan(&lots).Error; err != nil {
		return nil, err
	}
	for _, row := range lots {
		fact := result[row.PurchaseID]
		fact.OriginalLotCount = row.OriginalLotCount
		fact.OriginalLotQuantity = row.OriginalLotQuantity
		result[row.PurchaseID] = fact
	}

	var entries []struct {
		PurchaseID        uint64
		IssueEntryCount   int64
		IssueLotCount     int64
		IssueQuantity     int64
		InvalidIssueCount int64
	}
	if err := tx.WithContext(ctx).Table("wine_ticket_transactions AS ledger").
		Select(`
			ledger.biz_id AS purchase_id,
			COUNT(*) AS issue_entry_count,
			COUNT(DISTINCT ledger.lot_id) AS issue_lot_count,
			COALESCE(SUM(ledger.quantity_delta), 0) AS issue_quantity,
			COALESCE(SUM(CASE
				WHEN lot.id IS NULL
					OR ledger.owner_customer_id <> lot.owner_customer_id
					OR ledger.before_available_quantity <> 0
					OR ledger.after_available_quantity <> lot.total_quantity
					OR ledger.quantity_delta <> lot.total_quantity
				THEN 1 ELSE 0
			END), 0) AS invalid_issue_count
		`).
		Joins(`
			LEFT JOIN wine_ticket_lots AS lot
			  ON lot.id = ledger.lot_id
			 AND lot.purchase_id = ledger.biz_id
			 AND lot.source_type = ?
		`, LotSourcePurchase).
		Where(
			"ledger.biz_type = ? AND ledger.biz_id IN ? AND ledger.transaction_type = ?",
			"purchase",
			purchaseIDs,
			TransactionTypePurchaseIssue,
		).
		Group("ledger.biz_id").
		Scan(&entries).Error; err != nil {
		return nil, err
	}
	for _, row := range entries {
		fact := result[row.PurchaseID]
		fact.IssueEntryCount = row.IssueEntryCount
		fact.IssueLotCount = row.IssueLotCount
		fact.IssueQuantity = row.IssueQuantity
		fact.InvalidIssueCount = row.InvalidIssueCount
		result[row.PurchaseID] = fact
	}
	return result, nil
}

func (r *reconciliationRepository) loadIntegrityPurchases(
	ctx context.Context,
	tx *gorm.DB,
	ids []uint64,
) (map[uint64]purchasedomain.Purchase, error) {
	ids = reconciliationUniqueIDs(ids)
	result := make(map[uint64]purchasedomain.Purchase, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	var rows []purchasedomain.Purchase
	if err := tx.WithContext(ctx).Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.ID] = row
	}
	return result, nil
}

func (r *reconciliationRepository) loadIntegrityRenewals(
	ctx context.Context,
	tx *gorm.DB,
	ids []uint64,
) (map[uint64]renewaldomain.Renewal, error) {
	ids = reconciliationUniqueIDs(ids)
	result := make(map[uint64]renewaldomain.Renewal, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	var rows []renewaldomain.Renewal
	if err := tx.WithContext(ctx).Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.ID] = row
	}
	return result, nil
}

func (r *reconciliationRepository) loadPurchaseRefundCounts(
	ctx context.Context,
	tx *gorm.DB,
	purchaseIDs []uint64,
) (map[uint64]reconciliationPurchaseRefundCounts, error) {
	purchaseIDs = reconciliationUniqueIDs(purchaseIDs)
	result := make(
		map[uint64]reconciliationPurchaseRefundCounts,
		len(purchaseIDs),
	)
	if len(purchaseIDs) == 0 {
		return result, nil
	}
	var rows []reconciliationPurchaseRefundCounts
	if err := tx.WithContext(ctx).Table("wine_ticket_refunds").
		Select(`
			purchase_id,
			COALESCE(SUM(CASE
				WHEN refund_kind = ? THEN 1 ELSE 0
			END), 0) AS any_compensation,
			COALESCE(SUM(CASE
				WHEN refund_kind = ? AND status <> ? THEN 1 ELSE 0
			END), 0) AS non_cancelled_compensation,
			COALESCE(SUM(CASE
				WHEN status = ? THEN 1 ELSE 0
			END), 0) AS succeeded
		`,
			RefundKindIssueCompensation,
			RefundKindIssueCompensation,
			RefundStatusCancelled,
			RefundStatusSucceeded,
		).
		Where("purchase_id IN ?", purchaseIDs).
		Group("purchase_id").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.PurchaseID] = row
	}
	return result, nil
}

func pointerString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func pointerUint64(value *uint64) uint64 {
	if value == nil {
		return 0
	}
	return *value
}

func lastPaymentID(rows []reconciliationPayment, fallback uint64) uint64 {
	if len(rows) == 0 {
		return fallback
	}
	return rows[len(rows)-1].ID
}

func lastPurchaseID(rows []purchasedomain.Purchase, fallback uint64) uint64 {
	if len(rows) == 0 {
		return fallback
	}
	return rows[len(rows)-1].ID
}
