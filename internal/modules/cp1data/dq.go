package cp1data

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Checker struct {
	db      *gorm.DB
	options DQOptions
}

func NewChecker(db *gorm.DB, options DQOptions) (*Checker, error) {
	if db == nil {
		return nil, fmt.Errorf("database is required")
	}
	if options.SampleLimit == 0 {
		options.SampleLimit = 20
	}
	if options.SampleLimit < 1 || options.SampleLimit > 1000 {
		return nil, fmt.Errorf("sample limit must be between 1 and 1000")
	}
	if options.BatchSize == 0 {
		options.BatchSize = 500
	}
	if options.BatchSize < 1 || options.BatchSize > 2000 {
		return nil, fmt.Errorf("batch size must be between 1 and 2000")
	}
	if len(options.CheckIDs) == 0 {
		options.CheckIDs = DefaultCheckIDs()
	}
	for _, id := range options.CheckIDs {
		if _, ok := checkDescriptions[id]; !ok {
			return nil, fmt.Errorf("unknown data-quality check %q", id)
		}
	}
	return &Checker{db: db, options: options}, nil
}

func (c *Checker) Run(ctx context.Context) (DQReport, error) {
	report := DQReport{
		SchemaVersion: "cp1.dq-report.v1",
		GeneratedAt:   time.Now().UTC(),
		CutoverAt:     c.options.VerificationCutoverAt,
		Passed:        true,
		Results:       make([]CheckResult, 0, len(c.options.CheckIDs)),
	}
	checks := map[string]func(context.Context) (CheckResult, error){
		"DQ-001": c.checkOrderDeliveryStates,
		"DQ-002": c.checkStock,
		"DQ-003": c.checkOrderItemAmounts,
		"DQ-004": c.checkOrderPayable,
		"DQ-005": c.checkReceiptSnapshots,
		"DQ-006": c.checkInitialPrintDuplicates,
		"DQ-007": c.checkVerificationCutover,
		"DQ-008": c.checkActiveAssignments,
		"DQ-009": c.checkPrintSettingTemplates,
		"DQ-010": c.checkPermissionMatrix,
	}
	for _, id := range c.options.CheckIDs {
		result, err := checks[id](ctx)
		if err != nil {
			return DQReport{}, fmt.Errorf("%s: %w", id, err)
		}
		if result.Status != "pass" {
			report.Passed = false
		}
		report.Results = append(report.Results, result)
	}
	return report, nil
}

func resultFor(id string) CheckResult {
	return CheckResult{CheckID: id, Description: checkDescriptions[id], Status: "pass"}
}

func finishResult(result CheckResult) CheckResult {
	if result.Status == "blocked" {
		return result
	}
	if result.Violations > 0 {
		result.Status = "fail"
	} else {
		result.Status = "pass"
	}
	return result
}

func (c *Checker) addSample(result *CheckResult, finding Finding) {
	if len(result.Samples) < c.options.SampleLimit {
		result.Samples = append(result.Samples, finding)
	}
}

type orderDeliveryStateRow struct {
	OrderID        uint64
	DeliveryID     uint64
	OrderStatus    string
	DeliveryStatus string
}

func (c *Checker) checkOrderDeliveryStates(ctx context.Context) (CheckResult, error) {
	result := resultFor("DQ-001")
	var rows []orderDeliveryStateRow
	err := c.db.WithContext(ctx).Raw(`
		SELECT o.id AS order_id, d.id AS delivery_id,
		       o.delivery_status AS order_status, d.status AS delivery_status
		FROM orders o
		JOIN delivery_orders d ON d.order_id=o.id AND d.deleted_at IS NULL
		WHERE o.deleted_at IS NULL
		  AND NOT (
		    (o.delivery_status='pending' AND d.status='pending_assign')
		    OR (o.delivery_status='pending_assign' AND d.status='pending_assign')
		    OR o.delivery_status=d.status
		  )
		ORDER BY o.id
		LIMIT ?`, c.options.SampleLimit).Scan(&rows).Error
	if err != nil {
		return result, err
	}
	var mismatchCount int64
	if err := c.db.WithContext(ctx).Raw(`
		SELECT COUNT(*) FROM orders o
		JOIN delivery_orders d ON d.order_id=o.id AND d.deleted_at IS NULL
		WHERE o.deleted_at IS NULL
		  AND NOT (
		    (o.delivery_status='pending' AND d.status='pending_assign')
		    OR (o.delivery_status='pending_assign' AND d.status='pending_assign')
		    OR o.delivery_status=d.status
		  )`).Scan(&mismatchCount).Error; err != nil {
		return result, err
	}
	result.Violations += mismatchCount
	for _, row := range rows {
		c.addSample(&result, Finding{ObjectType: "order", ObjectID: idText(row.OrderID), Code: "ORDER_DELIVERY_STATUS_MISMATCH", Detail: "order and delivery status combination is not allowed", Data: map[string]any{"delivery_id": idText(row.DeliveryID), "order_delivery_status": row.OrderStatus, "delivery_status": row.DeliveryStatus}})
	}

	type idRow struct{ ID uint64 }
	var orphans []idRow
	if err := c.db.WithContext(ctx).Raw(`
		SELECT d.id FROM delivery_orders d
		LEFT JOIN orders o ON o.id=d.order_id AND o.deleted_at IS NULL
		WHERE d.deleted_at IS NULL AND o.id IS NULL
		ORDER BY d.id LIMIT ?`, c.options.SampleLimit).Scan(&orphans).Error; err != nil {
		return result, err
	}
	var orphanCount int64
	if err := c.db.WithContext(ctx).Raw(`
		SELECT COUNT(*) FROM delivery_orders d
		LEFT JOIN orders o ON o.id=d.order_id AND o.deleted_at IS NULL
		WHERE d.deleted_at IS NULL AND o.id IS NULL`).Scan(&orphanCount).Error; err != nil {
		return result, err
	}
	result.Violations += orphanCount
	for _, row := range orphans {
		c.addSample(&result, Finding{ObjectType: "delivery_order", ObjectID: idText(row.ID), Code: "DELIVERY_ORDER_ORPHAN", Detail: "delivery order has no live order"})
	}

	var missing []idRow
	if err := c.db.WithContext(ctx).Raw(`
		SELECT o.id FROM orders o
		LEFT JOIN delivery_orders d ON d.order_id=o.id AND d.deleted_at IS NULL
		WHERE o.deleted_at IS NULL AND d.id IS NULL
		  AND o.delivery_status NOT IN ('pending','cancelled')
		ORDER BY o.id LIMIT ?`, c.options.SampleLimit).Scan(&missing).Error; err != nil {
		return result, err
	}
	var missingCount int64
	if err := c.db.WithContext(ctx).Raw(`
		SELECT COUNT(*) FROM orders o
		LEFT JOIN delivery_orders d ON d.order_id=o.id AND d.deleted_at IS NULL
		WHERE o.deleted_at IS NULL AND d.id IS NULL
		  AND o.delivery_status NOT IN ('pending','cancelled')`).Scan(&missingCount).Error; err != nil {
		return result, err
	}
	result.Violations += missingCount
	for _, row := range missing {
		c.addSample(&result, Finding{ObjectType: "order", ObjectID: idText(row.ID), Code: "ORDER_DELIVERY_MISSING", Detail: "order delivery status requires a delivery order"})
	}
	return finishResult(result), nil
}

func (c *Checker) checkStock(ctx context.Context) (CheckResult, error) {
	result := resultFor("DQ-002")
	type stockProblem struct {
		ID            uint64
		ShopProductID uint64
		Detail        string
	}
	queries := []struct {
		code   string
		detail string
		count  string
		sample string
	}{
		{
			code: "STOCK_NEGATIVE", detail: "one or more stock quantities are negative",
			count:  `SELECT COUNT(*) FROM product_stocks WHERE deleted_at IS NULL AND (available_qty<0 OR reserved_qty<0 OR locked_qty<0)`,
			sample: `SELECT id,shop_product_id,'negative quantity' AS detail FROM product_stocks WHERE deleted_at IS NULL AND (available_qty<0 OR reserved_qty<0 OR locked_qty<0) ORDER BY id LIMIT ?`,
		},
		{
			code: "STOCK_RECORD_TOTAL_FIELDS_NULL", detail: "total inventory ledger fields must all be populated",
			count: `SELECT COUNT(*) FROM stock_records WHERE deleted_at IS NULL
				AND (total_quantity_delta IS NULL OR before_total_qty IS NULL OR after_total_qty IS NULL)`,
			sample: `SELECT id,shop_product_id,'missing total inventory fact' AS detail FROM stock_records WHERE deleted_at IS NULL
				AND (total_quantity_delta IS NULL OR before_total_qty IS NULL OR after_total_qty IS NULL) ORDER BY id LIMIT ?`,
		},
		{
			code: "STOCK_AVAILABLE_EQUATION_MISMATCH", detail: "before_available_qty + quantity_delta does not equal after_available_qty",
			count:  `SELECT COUNT(*) FROM stock_records WHERE deleted_at IS NULL AND before_available_qty+quantity_delta<>after_available_qty`,
			sample: `SELECT id,shop_product_id,'available equation mismatch' AS detail FROM stock_records WHERE deleted_at IS NULL AND before_available_qty+quantity_delta<>after_available_qty ORDER BY id LIMIT ?`,
		},
		{
			code: "STOCK_TOTAL_EQUATION_MISMATCH", detail: "before_total_qty + total_quantity_delta does not equal after_total_qty",
			count: `SELECT COUNT(*) FROM stock_records WHERE deleted_at IS NULL
				AND before_total_qty+total_quantity_delta<>after_total_qty`,
			sample: `SELECT id,shop_product_id,'total equation mismatch' AS detail FROM stock_records WHERE deleted_at IS NULL
				AND before_total_qty+total_quantity_delta<>after_total_qty ORDER BY id LIMIT ?`,
		},
		{
			code: "STOCK_AVAILABLE_DISCONTINUITY", detail: "available opening quantity does not equal the previous available closing quantity",
			count: `SELECT COUNT(*) FROM (
				SELECT before_available_qty,
				       LAG(after_available_qty) OVER (PARTITION BY shop_product_id ORDER BY created_at,id) AS previous_after
				FROM stock_records WHERE deleted_at IS NULL
			) q WHERE previous_after IS NOT NULL AND before_available_qty<>previous_after`,
			sample: `SELECT id,shop_product_id,'available ledger discontinuity' AS detail FROM (
				SELECT id,shop_product_id,before_available_qty,
				       LAG(after_available_qty) OVER (PARTITION BY shop_product_id ORDER BY created_at,id) AS previous_after
				FROM stock_records WHERE deleted_at IS NULL
			) q WHERE previous_after IS NOT NULL AND before_available_qty<>previous_after ORDER BY id LIMIT ?`,
		},
		{
			code: "STOCK_TOTAL_DISCONTINUITY", detail: "total opening quantity does not equal the previous total closing quantity",
			count: `SELECT COUNT(*) FROM (
				SELECT before_total_qty,
				       LAG(after_total_qty) OVER (PARTITION BY shop_product_id ORDER BY created_at,id) AS previous_after
				FROM stock_records WHERE deleted_at IS NULL
			) q WHERE previous_after IS NOT NULL AND before_total_qty<>previous_after`,
			sample: `SELECT id,shop_product_id,'total ledger discontinuity' AS detail FROM (
				SELECT id,shop_product_id,before_total_qty,
				       LAG(after_total_qty) OVER (PARTITION BY shop_product_id ORDER BY created_at,id) AS previous_after
				FROM stock_records WHERE deleted_at IS NULL
			) q WHERE previous_after IS NOT NULL AND before_total_qty<>previous_after ORDER BY id LIMIT ?`,
		},
		{
			code: "STOCK_LEDGER_CURRENT_AVAILABLE_MISMATCH", detail: "latest stock record does not reconcile to current available quantity",
			count: `SELECT COUNT(*) FROM product_stocks ps
				JOIN (SELECT id,shop_product_id FROM (
					SELECT id,shop_product_id,ROW_NUMBER() OVER (PARTITION BY shop_product_id ORDER BY created_at DESC,id DESC) AS row_num
					FROM stock_records WHERE deleted_at IS NULL
				) ranked WHERE row_num=1) latest ON latest.shop_product_id=ps.shop_product_id
				JOIN stock_records sr ON sr.id=latest.id
				WHERE ps.deleted_at IS NULL AND ps.available_qty<>sr.after_available_qty`,
			sample: `SELECT ps.id,ps.shop_product_id,'current quantity mismatch' AS detail FROM product_stocks ps
				JOIN (SELECT id,shop_product_id FROM (
					SELECT id,shop_product_id,ROW_NUMBER() OVER (PARTITION BY shop_product_id ORDER BY created_at DESC,id DESC) AS row_num
					FROM stock_records WHERE deleted_at IS NULL
				) ranked WHERE row_num=1) latest ON latest.shop_product_id=ps.shop_product_id
				JOIN stock_records sr ON sr.id=latest.id
				WHERE ps.deleted_at IS NULL AND ps.available_qty<>sr.after_available_qty ORDER BY ps.id LIMIT ?`,
		},
		{
			code: "STOCK_LEDGER_CURRENT_TOTAL_MISMATCH", detail: "latest stock record does not reconcile to current derived total quantity",
			count: `SELECT COUNT(*) FROM product_stocks ps
				JOIN (SELECT id,shop_product_id FROM (
					SELECT id,shop_product_id,ROW_NUMBER() OVER (PARTITION BY shop_product_id ORDER BY created_at DESC,id DESC) AS row_num
					FROM stock_records WHERE deleted_at IS NULL
				) ranked WHERE row_num=1) latest ON latest.shop_product_id=ps.shop_product_id
				JOIN stock_records sr ON sr.id=latest.id
				WHERE ps.deleted_at IS NULL AND ps.available_qty+ps.reserved_qty+ps.locked_qty<>sr.after_total_qty`,
			sample: `SELECT ps.id,ps.shop_product_id,'current total quantity mismatch' AS detail FROM product_stocks ps
				JOIN (SELECT id,shop_product_id FROM (
					SELECT id,shop_product_id,ROW_NUMBER() OVER (PARTITION BY shop_product_id ORDER BY created_at DESC,id DESC) AS row_num
					FROM stock_records WHERE deleted_at IS NULL
				) ranked WHERE row_num=1) latest ON latest.shop_product_id=ps.shop_product_id
				JOIN stock_records sr ON sr.id=latest.id
				WHERE ps.deleted_at IS NULL AND ps.available_qty+ps.reserved_qty+ps.locked_qty<>sr.after_total_qty ORDER BY ps.id LIMIT ?`,
		},
	}
	for _, query := range queries {
		var count int64
		if err := c.db.WithContext(ctx).Raw(query.count).Scan(&count).Error; err != nil {
			return result, err
		}
		result.Violations += count
		if count == 0 || len(result.Samples) >= c.options.SampleLimit {
			continue
		}
		var rows []stockProblem
		if err := c.db.WithContext(ctx).Raw(query.sample, c.options.SampleLimit-len(result.Samples)).Scan(&rows).Error; err != nil {
			return result, err
		}
		for _, row := range rows {
			c.addSample(&result, Finding{ObjectType: "stock", ObjectID: idText(row.ID), Code: query.code, Detail: query.detail, Data: map[string]any{"shop_product_id": idText(row.ShopProductID)}})
		}
	}
	result.Notes = append(result.Notes, "quantity_delta is the available quantity change; total_quantity_delta is the derived total quantity change; total_qty is available_qty + reserved_qty + locked_qty")
	return finishResult(result), nil
}

func (c *Checker) checkOrderItemAmounts(ctx context.Context) (CheckResult, error) {
	result := resultFor("DQ-003")
	type row struct {
		OrderID     uint64
		GoodsAmount int64
		ItemsAmount int64
	}
	base := ` FROM orders o LEFT JOIN order_items oi ON oi.order_id=o.id AND oi.deleted_at IS NULL
		WHERE o.deleted_at IS NULL GROUP BY o.id,o.goods_amount HAVING COALESCE(SUM(oi.total_amount),0)<>o.goods_amount`
	if err := c.db.WithContext(ctx).Raw(`SELECT COUNT(*) FROM (SELECT o.id` + base + `) q`).Scan(&result.Violations).Error; err != nil {
		return result, err
	}
	var rows []row
	if result.Violations > 0 {
		if err := c.db.WithContext(ctx).Raw(`SELECT o.id AS order_id,o.goods_amount,COALESCE(SUM(oi.total_amount),0) AS items_amount`+base+` ORDER BY o.id LIMIT ?`, c.options.SampleLimit).Scan(&rows).Error; err != nil {
			return result, err
		}
		for _, item := range rows {
			c.addSample(&result, Finding{ObjectType: "order", ObjectID: idText(item.OrderID), Code: "ORDER_GOODS_AMOUNT_MISMATCH", Detail: "order item total does not equal goods_amount", Data: map[string]any{"goods_amount": item.GoodsAmount, "items_amount": item.ItemsAmount}})
		}
	}
	return finishResult(result), nil
}

func (c *Checker) checkOrderPayable(ctx context.Context) (CheckResult, error) {
	result := resultFor("DQ-004")
	type row struct {
		ID                uint64
		GoodsAmount       int64
		DiscountAmount    int64
		DeliveryFeeAmount int64
		PayableAmount     int64
	}
	condition := `deleted_at IS NULL AND goods_amount-discount_amount+delivery_fee_amount<>payable_amount`
	if err := c.db.WithContext(ctx).Table("orders").Where(condition).Count(&result.Violations).Error; err != nil {
		return result, err
	}
	var rows []row
	if result.Violations > 0 {
		if err := c.db.WithContext(ctx).Table("orders").Select("id,goods_amount,discount_amount,delivery_fee_amount,payable_amount").Where(condition).Order("id").Limit(c.options.SampleLimit).Scan(&rows).Error; err != nil {
			return result, err
		}
		for _, item := range rows {
			c.addSample(&result, Finding{ObjectType: "order", ObjectID: idText(item.ID), Code: "ORDER_PAYABLE_AMOUNT_MISMATCH", Detail: "payable amount formula is inconsistent", Data: map[string]any{"goods_amount": item.GoodsAmount, "discount_amount": item.DiscountAmount, "delivery_fee_amount": item.DeliveryFeeAmount, "payable_amount": item.PayableAmount}})
		}
	}
	return finishResult(result), nil
}

type receiptTaskRow struct {
	ID              uint64
	OrderID         uint64
	ShopID          uint64
	TemplateVersion string
	RenderPayload   datatypes.JSON
}

type receiptEnvelope struct {
	SchemaVersion   string `json:"schema_version"`
	TemplateVersion string `json:"template_version"`
	Order           struct {
		OrderID string `json:"order_id"`
		OrderNo string `json:"order_no"`
	} `json:"order"`
	Shop struct {
		ShopID string `json:"shop_id"`
	} `json:"shop"`
	Items []struct {
		ProductID       string `json:"product_id"`
		ShopProductID   string `json:"shop_product_id"`
		Name            string `json:"name"`
		BrandName       string `json:"brand_name"`
		Spec            string `json:"spec"`
		UnitPriceAmount int64  `json:"unit_price_amount"`
		Quantity        int    `json:"quantity"`
		TotalAmount     int64  `json:"total_amount"`
	} `json:"items"`
	Amounts struct {
		Goods       int64 `json:"goods"`
		Discount    int64 `json:"discount"`
		DeliveryFee int64 `json:"delivery_fee"`
		Payable     int64 `json:"payable"`
		Paid        int64 `json:"paid"`
	} `json:"amounts"`
}

type receiptOrderSource struct {
	ID                uint64
	OrderNo           string
	ShopID            uint64
	GoodsAmount       int64
	DiscountAmount    int64
	DeliveryFeeAmount int64
	PayableAmount     int64
	PaidAmount        int64
}

type receiptItemSource struct {
	ShopProductID   uint64
	ProductID       uint64
	ProductSnapshot datatypes.JSON
	Quantity        int
	SalePriceAmount int64
	TotalAmount     int64
}

func (c *Checker) checkReceiptSnapshots(ctx context.Context) (CheckResult, error) {
	result := resultFor("DQ-005")
	cursor := uint64(0)
	for {
		var tasks []receiptTaskRow
		if err := c.db.WithContext(ctx).Table("print_tasks").
			Select("id,order_id,shop_id,template_version,render_payload").
			Where("id>? AND payload_schema_version='receipt.v1'", cursor).
			Order("id").Limit(c.options.BatchSize).Scan(&tasks).Error; err != nil {
			return result, err
		}
		if len(tasks) == 0 {
			break
		}
		for _, task := range tasks {
			cursor = task.ID
			if detail, err := c.receiptMismatch(ctx, task); err != nil {
				return result, err
			} else if detail != "" {
				result.Violations++
				c.addSample(&result, Finding{ObjectType: "print_task", ObjectID: idText(task.ID), Code: "RECEIPT_SNAPSHOT_MISMATCH", Detail: detail, Data: map[string]any{"order_id": idText(task.OrderID)}})
			}
		}
	}
	return finishResult(result), nil
}

func (c *Checker) receiptMismatch(ctx context.Context, task receiptTaskRow) (string, error) {
	var payload receiptEnvelope
	if err := json.Unmarshal(task.RenderPayload, &payload); err != nil {
		return "render_payload is not valid receipt.v1 JSON", nil
	}
	if payload.SchemaVersion != "receipt.v1" || payload.TemplateVersion != task.TemplateVersion {
		return "receipt envelope version differs from task metadata", nil
	}
	var order receiptOrderSource
	if err := c.db.WithContext(ctx).Table("orders").Select("id,order_no,shop_id,goods_amount,discount_amount,delivery_fee_amount,payable_amount,paid_amount").Where("id=? AND deleted_at IS NULL", task.OrderID).Take(&order).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return "receipt references a missing order", nil
		}
		return "", err
	}
	if payload.Order.OrderID != idText(order.ID) || payload.Order.OrderNo != order.OrderNo || payload.Shop.ShopID != idText(order.ShopID) || order.ShopID != task.ShopID {
		return "receipt order or shop identity differs from the order snapshot", nil
	}
	if payload.Amounts.Goods != order.GoodsAmount || payload.Amounts.Discount != order.DiscountAmount || payload.Amounts.DeliveryFee != order.DeliveryFeeAmount || payload.Amounts.Payable != order.PayableAmount || payload.Amounts.Paid != order.PaidAmount {
		return "receipt amounts differ from the order snapshot", nil
	}
	var sources []receiptItemSource
	if err := c.db.WithContext(ctx).Table("order_items").Select("shop_product_id,product_id,product_snapshot,quantity,sale_price_amount,total_amount").Where("order_id=? AND deleted_at IS NULL", order.ID).Order("id").Scan(&sources).Error; err != nil {
		return "", err
	}
	if len(payload.Items) != len(sources) {
		return "receipt item count differs from order items", nil
	}
	for index, source := range sources {
		item := payload.Items[index]
		var snapshot map[string]any
		if err := json.Unmarshal(source.ProductSnapshot, &snapshot); err != nil {
			return "order item product_snapshot is invalid JSON", nil
		}
		if item.ProductID != idText(source.ProductID) || item.ShopProductID != idText(source.ShopProductID) || item.Name != stringField(snapshot, "name") || item.BrandName != stringField(snapshot, "brand_name") || item.Spec != stringField(snapshot, "spec") || item.Quantity != source.Quantity || item.UnitPriceAmount != source.SalePriceAmount || item.TotalAmount != source.TotalAmount {
			return "receipt item differs from immutable order item data", nil
		}
	}
	return "", nil
}

func (c *Checker) checkInitialPrintDuplicates(ctx context.Context) (CheckResult, error) {
	result := resultFor("DQ-006")
	type row struct {
		EventID string
		Count   int64
	}
	base := ` FROM print_tasks WHERE reprint_seq=0 GROUP BY event_id HAVING COUNT(*)>1`
	if err := c.db.WithContext(ctx).Raw(`SELECT COUNT(*) FROM (SELECT event_id` + base + `) q`).Scan(&result.Violations).Error; err != nil {
		return result, err
	}
	var rows []row
	if result.Violations > 0 {
		if err := c.db.WithContext(ctx).Raw(`SELECT event_id,COUNT(*) AS count`+base+` ORDER BY event_id LIMIT ?`, c.options.SampleLimit).Scan(&rows).Error; err != nil {
			return result, err
		}
		for _, item := range rows {
			c.addSample(&result, Finding{ObjectType: "print_event", ObjectID: item.EventID, Code: "PRINT_INITIAL_TASK_DUPLICATED", Detail: "one event has more than one initial print task", Data: map[string]any{"task_count": item.Count}})
		}
	}
	return finishResult(result), nil
}

func (c *Checker) checkVerificationCutover(ctx context.Context) (CheckResult, error) {
	result := resultFor("DQ-007")
	if c.options.VerificationCutoverAt == nil || c.options.VerificationCutoverAt.IsZero() {
		result.Status = "blocked"
		result.Notes = append(result.Notes, "an explicit verification cutover time is required; no production cutover time is inferred")
		return result, nil
	}
	cutover := c.options.VerificationCutoverAt.UTC()
	if audit := c.options.VerificationAudit; audit != nil {
		if audit.SchemaVersion != "cp1.verification-migration-audit.v1" || audit.DryRun || !audit.Completed || !audit.CutoverAt.Equal(cutover) || strings.TrimSpace(audit.MappingReason) == "" {
			result.Status = "blocked"
			result.Notes = append(result.Notes, "the supplied verification migration audit is incomplete, dry-run, uses a different cutover, or has no mapping reason")
			return result, nil
		}
	}
	type deliveryRow struct{ ID uint64 }
	invalidCompletionSQL := `
		FROM delivery_orders d
		WHERE d.deleted_at IS NULL AND d.status='completed'
		  AND COALESCE(d.completed_at,d.updated_at,d.created_at)>=?
		  AND NOT EXISTS (
		    SELECT 1 FROM delivery_verifications v
		    WHERE v.delivery_order_id=d.id AND v.stage='delivery'
		      AND v.status='verified' AND v.mode_snapshot='enforce' AND v.verified_at IS NOT NULL
		  )
		  AND NOT EXISTS (
		    SELECT 1 FROM admin_override_approvals a
		    WHERE a.action='delivery.force_complete' AND a.resource_type='delivery_order'
		      AND a.resource_id=d.id AND a.status='approved' AND a.approved_at IS NOT NULL
		      AND a.maker_admin_id<>a.checker_admin_id
		  )`
	if err := c.db.WithContext(ctx).Raw(`SELECT COUNT(*) `+invalidCompletionSQL, cutover).Scan(&result.Violations).Error; err != nil {
		return result, err
	}
	var invalid []deliveryRow
	if result.Violations > 0 {
		if err := c.db.WithContext(ctx).Raw(`SELECT d.id `+invalidCompletionSQL+` ORDER BY d.id LIMIT ?`, cutover, c.options.SampleLimit).Scan(&invalid).Error; err != nil {
			return result, err
		}
		for _, row := range invalid {
			c.addSample(&result, Finding{ObjectType: "delivery_order", ObjectID: idText(row.ID), Code: "COMPLETED_WITHOUT_ENFORCED_PROOF", Detail: "post-cutover completion has neither enforce verification nor approved maker-checker override"})
		}
	}

	type observeRow struct {
		ID              uint64
		DeliveryOrderID uint64
		Source          string
	}
	var observes []observeRow
	observeSQL := `
		SELECT id,delivery_order_id,'verification' AS source FROM delivery_verifications WHERE created_at>=? AND mode_snapshot='observe'
		UNION ALL
		SELECT id,delivery_order_id,'attempt' AS source FROM delivery_verification_attempts WHERE created_at>=? AND mode_snapshot='observe'`
	var observeCount int64
	if err := c.db.WithContext(ctx).Raw(`SELECT COUNT(*) FROM (`+observeSQL+`) q`, cutover, cutover).Scan(&observeCount).Error; err != nil {
		return result, err
	}
	result.Violations += observeCount
	if observeCount > 0 && len(result.Samples) < c.options.SampleLimit {
		if err := c.db.WithContext(ctx).Raw(observeSQL+` ORDER BY id LIMIT ?`, cutover, cutover, c.options.SampleLimit-len(result.Samples)).Scan(&observes).Error; err != nil {
			return result, err
		}
		for _, row := range observes {
			c.addSample(&result, Finding{ObjectType: "delivery_verification_" + row.Source, ObjectID: idText(row.ID), Code: "OBSERVE_FACT_AFTER_CUTOVER", Detail: "observe verification data was created after enforce cutover", Data: map[string]any{"delivery_order_id": idText(row.DeliveryOrderID)}})
		}
	}

	type orphanAttemptRow struct {
		ID              uint64
		DeliveryOrderID uint64
		VerificationID  uint64
	}
	orphanAttemptSQL := `
		FROM delivery_verification_attempts a
		LEFT JOIN delivery_verifications v ON v.id=a.verification_id
		WHERE a.created_at<? AND v.id IS NULL`
	var orphanAttemptCount int64
	if err := c.db.WithContext(ctx).Raw(`SELECT COUNT(*) `+orphanAttemptSQL, cutover).Scan(&orphanAttemptCount).Error; err != nil {
		return result, err
	}
	result.Violations += orphanAttemptCount
	if orphanAttemptCount > 0 && len(result.Samples) < c.options.SampleLimit {
		var orphans []orphanAttemptRow
		if err := c.db.WithContext(ctx).Raw(`SELECT a.id,a.delivery_order_id,a.verification_id `+orphanAttemptSQL+` ORDER BY a.id LIMIT ?`, cutover, c.options.SampleLimit-len(result.Samples)).Scan(&orphans).Error; err != nil {
			return result, err
		}
		for _, row := range orphans {
			c.addSample(&result, Finding{ObjectType: "delivery_verification_attempt", ObjectID: idText(row.ID), Code: "HISTORICAL_ORPHAN_VERIFICATION_ATTEMPT", Detail: "pre-cutover attempt has no parent verification and cannot serve as migration evidence", Data: map[string]any{"delivery_order_id": idText(row.DeliveryOrderID), "verification_id": idText(row.VerificationID)}})
		}
	}

	historicalSQL := `
		SELECT d.id FROM delivery_orders d
		WHERE d.deleted_at IS NULL AND d.status='completed'
		  AND COALESCE(d.completed_at,d.updated_at,d.created_at)<?`
	var historical []deliveryRow
	if err := c.db.WithContext(ctx).Raw(historicalSQL, cutover).Scan(&historical).Error; err != nil {
		return result, err
	}
	if len(historical) > 0 && c.options.VerificationAudit == nil {
		result.Status = "blocked"
		result.Notes = append(result.Notes, fmt.Sprintf("%d pre-cutover completed deliveries require a verification migration audit file", len(historical)))
		return result, nil
	}
	for _, row := range historical {
		if c.options.VerificationAudit.containsDelivery(row.ID) {
			continue
		}
		result.Violations++
		c.addSample(&result, Finding{ObjectType: "delivery_order", ObjectID: idText(row.ID), Code: "HISTORICAL_EXCEPTION_NOT_AUDITED", Detail: "pre-cutover completion is not covered by the supplied migration audit"})
	}
	return finishResult(result), nil
}

func (c *Checker) checkActiveAssignments(ctx context.Context) (CheckResult, error) {
	result := resultFor("DQ-008")
	type row struct {
		DeliveryOrderID uint64
		Count           int64
	}
	base := ` FROM delivery_assignments WHERE status='active' GROUP BY delivery_order_id HAVING COUNT(*)>1`
	if err := c.db.WithContext(ctx).Raw(`SELECT COUNT(*) FROM (SELECT delivery_order_id` + base + `) q`).Scan(&result.Violations).Error; err != nil {
		return result, err
	}
	var rows []row
	if result.Violations > 0 {
		if err := c.db.WithContext(ctx).Raw(`SELECT delivery_order_id,COUNT(*) AS count`+base+` ORDER BY delivery_order_id LIMIT ?`, c.options.SampleLimit).Scan(&rows).Error; err != nil {
			return result, err
		}
		for _, item := range rows {
			c.addSample(&result, Finding{ObjectType: "delivery_order", ObjectID: idText(item.DeliveryOrderID), Code: "MULTIPLE_ACTIVE_ASSIGNMENTS", Detail: "delivery order has more than one active assignment", Data: map[string]any{"active_count": item.Count}})
		}
	}
	return finishResult(result), nil
}

func (c *Checker) checkPrintSettingTemplates(ctx context.Context) (CheckResult, error) {
	result := resultFor("DQ-009")
	type row struct {
		ID         uint64
		ShopID     uint64
		TemplateID uint64
		Status     *string
		Schema     *string
		Code       *string
	}
	condition := `ps.enabled=1 AND (pt.id IS NULL OR pt.status<>'published' OR pt.payload_schema_version<>'receipt.v1' OR pt.template_code<>'store_receipt')`
	base := ` FROM print_settings ps LEFT JOIN print_templates pt ON pt.id=ps.template_id WHERE ` + condition
	if err := c.db.WithContext(ctx).Raw(`SELECT COUNT(*)` + base).Scan(&result.Violations).Error; err != nil {
		return result, err
	}
	var rows []row
	if result.Violations > 0 {
		if err := c.db.WithContext(ctx).Raw(`SELECT ps.id,ps.shop_id,ps.template_id,pt.status,pt.payload_schema_version AS schema,pt.template_code AS code`+base+` ORDER BY ps.id LIMIT ?`, c.options.SampleLimit).Scan(&rows).Error; err != nil {
			return result, err
		}
		for _, item := range rows {
			c.addSample(&result, Finding{ObjectType: "print_setting", ObjectID: idText(item.ID), Code: "PRINT_SETTING_TEMPLATE_INVALID", Detail: "enabled print setting does not reference a published receipt.v1 store_receipt template", Data: map[string]any{"shop_id": idText(item.ShopID), "template_id": idText(item.TemplateID), "template_status": item.Status, "payload_schema_version": item.Schema, "template_code": item.Code}})
		}
	}
	return finishResult(result), nil
}

var phaseOnePermissionCodes = []string{
	"cart:view", "cart:update", "order:create", "payment:create", "payment:view",
	"delivery_verification:view_customer", "store_order:view", "print_setting:test_shop",
	"delivery:view_own", "delivery:pickup", "delivery:complete",
}

var merchantRoleMatrix = map[string][]string{
	"merchant_owner": {
		"store_order:list", "store_order:view", "store_order:accept", "store_order:prepare",
		"shop_product:list", "shop_product:create", "shop_product:update", "shop:business_status",
		"inventory:view", "inventory:adjust", "after_sale:list_shop", "after_sale:view_shop",
		"after_sale:review_shop", "after_sale:receive_return", "after_sale:create_replacement",
		"print_setting:view_shop", "print_setting:update_shop", "print_setting:test_shop",
		"print_task:list_shop", "print_task:reprint_shop", "delivery_verification:view_shop",
		"delivery_incident:view_shop", "delivery_return:list_shop", "delivery_return:view_shop",
		"delivery_return:receive_shop",
	},
	"merchant_order_operator": {
		"store_order:list", "store_order:view", "store_order:accept", "store_order:prepare",
		"print_setting:view_shop", "print_setting:update_shop", "print_setting:test_shop",
		"print_task:list_shop", "print_task:reprint_shop", "delivery_verification:view_shop",
	},
	"merchant_inventory_clerk": {"inventory:view", "inventory:adjust"},
}

func (c *Checker) checkPermissionMatrix(ctx context.Context) (CheckResult, error) {
	result := resultFor("DQ-010")
	type permissionRow struct {
		Code   string
		Status string
		Count  int64
	}
	var permissions []permissionRow
	if err := c.db.WithContext(ctx).Table("permissions").Select("code,status,COUNT(*) AS count").Where("deleted_at IS NULL AND code IN ?", phaseOnePermissionCodes).Group("code,status").Scan(&permissions).Error; err != nil {
		return result, err
	}
	found := make(map[string]permissionRow, len(permissions))
	for _, row := range permissions {
		found[row.Code] = row
	}
	for _, code := range phaseOnePermissionCodes {
		row, ok := found[code]
		if !ok || row.Status != "active" || row.Count != 1 {
			result.Violations++
			c.addSample(&result, Finding{ObjectType: "permission", ObjectID: code, Code: "PERMISSION_CATALOG_INVALID", Detail: "required permission is missing, duplicated, or inactive"})
		}
	}

	for roleCode, expectedCodes := range merchantRoleMatrix {
		type roleRow struct {
			ID     uint64
			Status string
		}
		var role roleRow
		if err := c.db.WithContext(ctx).Table("roles").Select("id,status").Where("code=? AND deleted_at IS NULL", roleCode).Take(&role).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				result.Violations++
				c.addSample(&result, Finding{ObjectType: "role", ObjectID: roleCode, Code: "MERCHANT_ROLE_MISSING", Detail: "required merchant role is missing"})
				continue
			}
			return result, err
		}
		if role.Status != "active" {
			result.Violations++
			c.addSample(&result, Finding{ObjectType: "role", ObjectID: roleCode, Code: "MERCHANT_ROLE_INACTIVE", Detail: "required merchant role is inactive"})
		}
		var actualCodes []string
		if err := c.db.WithContext(ctx).Table("role_permissions rp").Select("p.code").Joins("JOIN permissions p ON p.id=rp.permission_id AND p.deleted_at IS NULL AND p.status='active'").Where("rp.role_id=? AND rp.deleted_at IS NULL", role.ID).Order("p.code").Scan(&actualCodes).Error; err != nil {
			return result, err
		}
		expected := append([]string(nil), expectedCodes...)
		sort.Strings(expected)
		sort.Strings(actualCodes)
		missing, extra := setDifference(expected, actualCodes), setDifference(actualCodes, expected)
		for _, code := range missing {
			result.Violations++
			c.addSample(&result, Finding{ObjectType: "role", ObjectID: roleCode, Code: "ROLE_PERMISSION_MISSING", Detail: "default merchant role is missing a required permission", Data: map[string]any{"permission": code}})
		}
		for _, code := range extra {
			result.Violations++
			c.addSample(&result, Finding{ObjectType: "role", ObjectID: roleCode, Code: "ROLE_PERMISSION_EXCESS", Detail: "default merchant role has an out-of-matrix permission", Data: map[string]any{"permission": code}})
		}
	}
	result.Notes = append(result.Notes, "customer and rider claims are compile-time projections; their target permission sets remain covered by auth unit tests in addition to this persistent catalog check")
	return finishResult(result), nil
}

func setDifference(left, right []string) []string {
	rightSet := make(map[string]struct{}, len(right))
	for _, value := range right {
		rightSet[value] = struct{}{}
	}
	var result []string
	for _, value := range left {
		if _, ok := rightSet[value]; !ok {
			result = append(result, value)
		}
	}
	return result
}

func idText(id uint64) string { return strconv.FormatUint(id, 10) }

func stringField(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}
