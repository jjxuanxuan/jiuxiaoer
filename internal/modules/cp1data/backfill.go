package cp1data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/printjob"
)

type Backfiller struct {
	db      *gorm.DB
	options BackfillOptions
}

func NewBackfiller(db *gorm.DB, options BackfillOptions) (*Backfiller, error) {
	if db == nil {
		return nil, fmt.Errorf("database is required")
	}
	if options.BatchSize == 0 {
		options.BatchSize = 500
	}
	if options.RowsPerSecond == 0 {
		options.RowsPerSecond = 500
	}
	if options.SampleLimit == 0 {
		options.SampleLimit = 100
	}
	if options.MaxRetries == 0 {
		options.MaxRetries = 5
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	return &Backfiller{db: db, options: options}, nil
}

func (b *Backfiller) Run(ctx context.Context) (BackfillReport, error) {
	startedAt := time.Now().UTC()
	report := BackfillReport{
		SchemaVersion: "cp1.backfill-report.v1",
		Job:           b.options.Job,
		DryRun:        !b.options.Execute,
		StartedAt:     startedAt,
		Range:         normalizedRange(b.options.Range),
	}
	fingerprint := b.options.fingerprint()
	if b.options.Resume {
		checkpoint, err := LoadCheckpoint(b.options.CheckpointFile)
		if err != nil {
			return report, err
		}
		if checkpoint.Job != b.options.Job || checkpoint.Fingerprint != fingerprint {
			return report, fmt.Errorf("checkpoint does not match this job and option fingerprint")
		}
		report.LastID = checkpoint.LastID
		report.Completed = checkpoint.Completed
		report.Progress = checkpoint.Progress
		if checkpoint.Completed {
			report.FinishedAt = time.Now().UTC()
			return report, nil
		}
	}
	if report.LastID < report.Range.Min && report.Range.Min > 0 {
		report.LastID = report.Range.Min - 1
	}

	var err error
	switch b.options.Job {
	case "print-tasks":
		err = b.runPrintTasks(ctx, &report, fingerprint)
	case "print-settings":
		err = b.runPrintSettings(ctx, &report, fingerprint)
	case "verification-history":
		err = b.runVerificationHistory(ctx, &report, fingerprint)
	}
	if err != nil {
		report.FinishedAt = time.Now().UTC()
		return report, err
	}
	report.Completed = true
	report.FinishedAt = time.Now().UTC()
	if err := b.saveCheckpoint(fingerprint, report); err != nil {
		return report, err
	}
	return report, nil
}

func normalizedRange(value IDRange) IDRange {
	if value.Max == 0 {
		// Generated IDs stay in the signed-63-bit Snowflake domain. This is
		// portable across MySQL drivers and the SQLite verification fixtures.
		value.Max = maxBackfillID
	}
	return value
}

func (b *Backfiller) saveCheckpoint(fingerprint string, report BackfillReport) error {
	if strings.TrimSpace(b.options.CheckpointFile) == "" {
		return nil
	}
	return saveJSONAtomic(b.options.CheckpointFile, Checkpoint{
		Version:     CheckpointVersion,
		Job:         b.options.Job,
		Fingerprint: fingerprint,
		LastID:      report.LastID,
		Completed:   report.Completed,
		Progress:    report.Progress,
		UpdatedAt:   time.Now().UTC(),
	})
}

func (b *Backfiller) addManual(report *BackfillReport, finding Finding) {
	report.Progress.Skipped++
	// Unlike DQ samples, this is the operator's repair queue. Never truncate it:
	// every unmappable setting/task/credential must remain actionable.
	report.Progress.Manual = append(report.Progress.Manual, finding)
}

type printTaskPlan struct {
	row      printjob.Task
	template printjob.Template
	payload  datatypes.JSON
}

func (b *Backfiller) runPrintTasks(ctx context.Context, report *BackfillReport, fingerprint string) error {
	for {
		batchStarted := time.Now()
		var rows []printjob.Task
		if err := b.db.WithContext(ctx).Where(
			"id>? AND id<=? AND status IN ? AND payload_schema_version<>'receipt.v1'",
			report.LastID, report.Range.Max, []string{"pending", "retry_wait"},
		).Order("id").Limit(b.options.BatchSize).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		plans := make([]printTaskPlan, 0, len(rows))
		for _, row := range rows {
			report.LastID = row.ID
			report.Progress.Scanned++
			template, err := b.templateForLegacy(ctx, row.TemplateID)
			if err != nil {
				b.addManual(report, Finding{ObjectType: "print_task", ObjectID: idText(row.ID), Code: "PRINT_TEMPLATE_MAPPING_REQUIRED", Detail: err.Error(), Data: map[string]any{"template_id": idText(row.TemplateID)}})
				continue
			}
			if detail, err := b.receiptSourceIncomplete(ctx, row.OrderID, row.ShopID); err != nil {
				return err
			} else if detail != "" {
				b.addManual(report, Finding{ObjectType: "print_task", ObjectID: idText(row.ID), Code: "PRINT_RECEIPT_SOURCE_INCOMPLETE", Detail: detail, Data: map[string]any{"order_id": idText(row.OrderID)}})
				continue
			}
			payload, err := printjob.RenderReceiptV1ForBackfill(b.db.WithContext(ctx), row.OrderID, row.ShopID, template)
			if err != nil {
				b.addManual(report, Finding{ObjectType: "print_task", ObjectID: idText(row.ID), Code: "PRINT_RECEIPT_RENDER_FAILED", Detail: safeError(err), Data: map[string]any{"order_id": idText(row.OrderID)}})
				continue
			}
			var collision int64
			if err := b.db.WithContext(ctx).Model(&printjob.Task{}).
				Where("id<>? AND shop_id=? AND order_id=? AND event_type=? AND template_version=? AND reprint_seq=?", row.ID, row.ShopID, row.OrderID, row.EventType, template.Version, row.ReprintSeq).
				Count(&collision).Error; err != nil {
				return err
			}
			if collision > 0 {
				b.addManual(report, Finding{ObjectType: "print_task", ObjectID: idText(row.ID), Code: "PRINT_TASK_TARGET_KEY_CONFLICT", Detail: "receipt.v1 template version would conflict with another immutable print task"})
				continue
			}
			plans = append(plans, printTaskPlan{row: row, template: template, payload: payload})
			report.Progress.Planned++
		}
		if b.options.Execute && len(plans) > 0 {
			var conflicts []uint64
			err := b.retryTransaction(ctx, func(tx *gorm.DB) error {
				conflicts = conflicts[:0]
				for _, plan := range plans {
					if err := ensureTemplateStillPublished(tx, plan.template.ID); err != nil {
						return err
					}
					updated := tx.Model(&printjob.Task{}).
						Where("id=? AND status=? AND payload_schema_version=?", plan.row.ID, plan.row.Status, plan.row.PayloadSchemaVersion).
						Updates(map[string]any{
							"template_id": plan.template.ID, "template_version": plan.template.Version,
							"render_payload": plan.payload, "payload_schema_version": "receipt.v1",
						})
					if updated.Error != nil {
						return updated.Error
					}
					if updated.RowsAffected != 1 {
						conflicts = append(conflicts, plan.row.ID)
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			report.Progress.Updated += int64(len(plans) - len(conflicts))
			for _, id := range conflicts {
				b.addManual(report, Finding{ObjectType: "print_task", ObjectID: idText(id), Code: "PRINT_TASK_CHANGED_DURING_BACKFILL", Detail: "task state or schema changed after it was scanned; no overwrite was performed"})
			}
		}
		if err := b.afterBatch(ctx, fingerprint, report, batchStarted, len(rows)); err != nil {
			return err
		}
	}
}

func (b *Backfiller) templateForLegacy(ctx context.Context, currentID uint64) (printjob.Template, error) {
	targetID := currentID
	if mapped := b.options.TemplateMap[currentID]; mapped != 0 {
		targetID = mapped
	} else if b.options.FallbackTemplateID != 0 {
		targetID = b.options.FallbackTemplateID
	}
	var template printjob.Template
	if err := b.db.WithContext(ctx).First(&template, targetID).Error; err != nil {
		return template, fmt.Errorf("no published receipt.v1 mapping exists")
	}
	if template.Status != "published" || template.TemplateCode != "store_receipt" || template.PayloadSchemaVersion != "receipt.v1" || (template.PaperWidthMM != 58 && template.PaperWidthMM != 80) {
		return template, fmt.Errorf("mapped template is not a published compatible store_receipt")
	}
	return template, nil
}

func (b *Backfiller) receiptSourceIncomplete(ctx context.Context, orderID, shopID uint64) (string, error) {
	type orderSource struct {
		AddressSnapshot datatypes.JSON
		ShopID          uint64
		OrderNo         string
	}
	var order orderSource
	if err := b.db.WithContext(ctx).Table("orders").Select("shop_id,order_no,address_snapshot").Where("id=? AND deleted_at IS NULL", orderID).Take(&order).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "order is missing", nil
		}
		return "", err
	}
	if order.ShopID != shopID || strings.TrimSpace(order.OrderNo) == "" {
		return "order identity or shop snapshot is incomplete", nil
	}
	var address map[string]any
	if len(order.AddressSnapshot) == 0 || json.Unmarshal(order.AddressSnapshot, &address) != nil {
		return "address_snapshot is missing or invalid", nil
	}
	formatted := stringField(address, "formatted_address")
	if formatted == "" && stringField(address, "address_detail") == "" {
		return "address_snapshot has no printable address", nil
	}
	type itemSource struct {
		ProductSnapshot datatypes.JSON
		Quantity        int
		SalePriceAmount int64
		TotalAmount     int64
	}
	var items []itemSource
	if err := b.db.WithContext(ctx).Table("order_items").Select("product_snapshot,quantity,sale_price_amount,total_amount").Where("order_id=? AND deleted_at IS NULL", orderID).Order("id").Scan(&items).Error; err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "order has no live item snapshots", nil
	}
	for _, item := range items {
		var snapshot map[string]any
		if item.Quantity <= 0 || item.SalePriceAmount < 0 || item.TotalAmount != int64(item.Quantity)*item.SalePriceAmount || json.Unmarshal(item.ProductSnapshot, &snapshot) != nil || stringField(snapshot, "name") == "" {
			return "one or more product snapshots are incomplete or inconsistent", nil
		}
	}
	return "", nil
}

type printSettingPlan struct {
	row        printjob.Setting
	templateID uint64
	disable    bool
}

func (b *Backfiller) runPrintSettings(ctx context.Context, report *BackfillReport, fingerprint string) error {
	for {
		batchStarted := time.Now()
		var rows []printjob.Setting
		if err := b.db.WithContext(ctx).Where("id>? AND id<=?", report.LastID, report.Range.Max).Order("id").Limit(b.options.BatchSize).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		plans := make([]printSettingPlan, 0, len(rows))
		for _, row := range rows {
			report.LastID = row.ID
			report.Progress.Scanned++
			if _, err := b.templateByExactID(ctx, row.TemplateID); err == nil {
				continue
			}
			targetID := b.options.TemplateMap[row.TemplateID]
			if targetID == 0 {
				targetID = b.options.FallbackTemplateID
			}
			if targetID != 0 {
				if _, err := b.templateByExactID(ctx, targetID); err == nil {
					plans = append(plans, printSettingPlan{row: row, templateID: targetID})
					report.Progress.Planned++
					continue
				}
			}
			b.addManual(report, Finding{ObjectType: "print_setting", ObjectID: idText(row.ID), Code: "PRINT_SETTING_TEMPLATE_UNMAPPED", Detail: "no valid published receipt.v1 template mapping exists; setting must remain disabled", Data: map[string]any{"shop_id": idText(row.ShopID), "template_id": idText(row.TemplateID)}})
			if row.Enabled {
				plans = append(plans, printSettingPlan{row: row, disable: true})
				report.Progress.Planned++
			}
		}
		if b.options.Execute && len(plans) > 0 {
			var conflicts []uint64
			err := b.retryTransaction(ctx, func(tx *gorm.DB) error {
				conflicts = conflicts[:0]
				for _, plan := range plans {
					if !plan.disable {
						if err := ensureTemplateStillPublished(tx, plan.templateID); err != nil {
							return err
						}
					}
					updates := map[string]any{"version": gorm.Expr("version+1")}
					if plan.disable {
						updates["enabled"] = false
					} else {
						updates["template_id"] = plan.templateID
					}
					updated := tx.Model(&printjob.Setting{}).Where("id=? AND version=?", plan.row.ID, plan.row.Version).Updates(updates)
					if updated.Error != nil {
						return updated.Error
					}
					if updated.RowsAffected != 1 {
						conflicts = append(conflicts, plan.row.ID)
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			report.Progress.Updated += int64(len(plans) - len(conflicts))
			for _, id := range conflicts {
				b.addManual(report, Finding{ObjectType: "print_setting", ObjectID: idText(id), Code: "PRINT_SETTING_CHANGED_DURING_BACKFILL", Detail: "setting version changed after it was scanned; no overwrite was performed"})
			}
		}
		if err := b.afterBatch(ctx, fingerprint, report, batchStarted, len(rows)); err != nil {
			return err
		}
	}
}

func (b *Backfiller) templateByExactID(ctx context.Context, id uint64) (printjob.Template, error) {
	var template printjob.Template
	if id == 0 {
		return template, gorm.ErrRecordNotFound
	}
	if err := b.db.WithContext(ctx).First(&template, id).Error; err != nil {
		return template, err
	}
	if template.Status != "published" || template.TemplateCode != "store_receipt" || template.PayloadSchemaVersion != "receipt.v1" || (template.PaperWidthMM != 58 && template.PaperWidthMM != 80) {
		return template, fmt.Errorf("template is not compatible")
	}
	return template, nil
}

func ensureTemplateStillPublished(tx *gorm.DB, id uint64) error {
	var count int64
	if err := tx.Model(&printjob.Template{}).Where("id=? AND status='published' AND template_code='store_receipt' AND payload_schema_version='receipt.v1' AND paper_width_mm IN ?", id, []uint16{58, 80}).Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("mapped print template changed during the batch")
	}
	return nil
}

type verificationPlan struct {
	row                verificationBackfillRow
	updateVerification bool
	updateAttempts     int64
}

type verificationBackfillRow struct {
	ID              uint64
	DeliveryOrderID uint64
	Stage           string
	ModeSnapshot    string
	Status          string
	CreatedAt       time.Time
}

type orphanVerificationAttemptRow struct {
	ID              uint64
	VerificationID  uint64
	DeliveryOrderID uint64
	Stage           string
	ModeSnapshot    string
}

func (b *Backfiller) runVerificationHistory(ctx context.Context, report *BackfillReport, fingerprint string) error {
	cutover := b.options.VerificationCutoverAt.UTC()
	for {
		batchStarted := time.Now()
		var rows []verificationBackfillRow
		if err := b.db.WithContext(ctx).Table("delivery_verifications").Select("id,delivery_order_id,stage,mode_snapshot,status,created_at").Where("id>? AND id<=? AND created_at<?", report.LastID, report.Range.Max, cutover).Order("id").Limit(b.options.BatchSize).Scan(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return b.recordOrphanVerificationAttempts(ctx, report, fingerprint, cutover)
		}
		ids := make([]uint64, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		type attemptCount struct {
			VerificationID uint64
			Count          int64
		}
		var attemptCounts []attemptCount
		if err := b.db.WithContext(ctx).Table("delivery_verification_attempts").Select("verification_id,COUNT(*) AS count").Where("verification_id IN ? AND created_at<? AND mode_snapshot<>'observe'", ids, cutover).Group("verification_id").Scan(&attemptCounts).Error; err != nil {
			return err
		}
		attemptByVerification := make(map[uint64]int64, len(attemptCounts))
		for _, count := range attemptCounts {
			attemptByVerification[count.VerificationID] = count.Count
		}
		plans := make([]verificationPlan, 0, len(rows))
		for _, row := range rows {
			report.LastID = row.ID
			report.Progress.Scanned++
			plan := verificationPlan{row: row, updateAttempts: attemptByVerification[row.ID]}
			mapping := VerificationMapping{VerificationID: row.ID, DeliveryOrderID: row.DeliveryOrderID, Stage: row.Stage, CreatedAt: row.CreatedAt.UTC(), Status: row.Status, PreviousMode: row.ModeSnapshot, ResultingMode: row.ModeSnapshot}
			if row.ModeSnapshot == "observe" {
				mapping.Action = "already_observe"
			} else if row.Status == "active" || row.Status == "locked" {
				mapping.Action = "manual_active_credential"
				b.addManual(report, Finding{ObjectType: "delivery_verification", ObjectID: idText(row.ID), Code: "ACTIVE_HISTORICAL_VERIFICATION_REQUIRES_ROTATION", Detail: "active or locked historical credentials are not downgraded in place; invalidate and rotate through the authorized business command", Data: map[string]any{"delivery_order_id": idText(row.DeliveryOrderID), "stage": row.Stage}})
			} else {
				plan.updateVerification = true
				mapping.ResultingMode = "observe"
				mapping.Action = "mapped_to_observe"
			}
			if plan.updateVerification || plan.updateAttempts > 0 {
				plans = append(plans, plan)
				report.Progress.Planned += boolCount(plan.updateVerification) + plan.updateAttempts
			}
			report.Progress.VerificationMappings = append(report.Progress.VerificationMappings, mapping)
		}
		if b.options.Execute && len(plans) > 0 {
			var updatedRows int64
			err := b.retryTransaction(ctx, func(tx *gorm.DB) error {
				updatedRows = 0
				for _, plan := range plans {
					if plan.updateVerification {
						updated := tx.Table("delivery_verifications").Where("id=? AND mode_snapshot=? AND status=?", plan.row.ID, plan.row.ModeSnapshot, plan.row.Status).Update("mode_snapshot", "observe")
						if updated.Error != nil {
							return updated.Error
						}
						updatedRows += updated.RowsAffected
					}
					if plan.updateAttempts > 0 {
						updated := tx.Table("delivery_verification_attempts").Where("verification_id=? AND created_at<? AND mode_snapshot<>'observe'", plan.row.ID, cutover).Update("mode_snapshot", "observe")
						if updated.Error != nil {
							return updated.Error
						}
						updatedRows += updated.RowsAffected
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			report.Progress.Updated += updatedRows
		}
		if err := b.afterBatch(ctx, fingerprint, report, batchStarted, len(rows)); err != nil {
			return err
		}
	}
}

// Orphan attempts cannot be rewritten safely: without their parent verification
// there is no authoritative credential/stage fact to map. Keep every orphan in
// the operator repair queue and independent audit instead of inventing facts.
func (b *Backfiller) recordOrphanVerificationAttempts(ctx context.Context, report *BackfillReport, fingerprint string, cutover time.Time) error {
	known := make(map[string]struct{}, len(report.Progress.Manual))
	for _, finding := range report.Progress.Manual {
		if finding.Code == "ORPHAN_HISTORICAL_VERIFICATION_ATTEMPT" {
			known[finding.ObjectID] = struct{}{}
		}
	}
	cursor := uint64(0)
	for {
		batchStarted := time.Now()
		var rows []orphanVerificationAttemptRow
		err := b.db.WithContext(ctx).Raw(`
			SELECT a.id,a.verification_id,a.delivery_order_id,a.stage,a.mode_snapshot
			FROM delivery_verification_attempts a
			LEFT JOIN delivery_verifications v ON v.id=a.verification_id
			WHERE a.id>? AND a.verification_id>=? AND a.verification_id<=?
			  AND a.created_at<? AND v.id IS NULL
			ORDER BY a.id LIMIT ?`, cursor, report.Range.Min, report.Range.Max, cutover, b.options.BatchSize).Scan(&rows).Error
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			cursor = row.ID
			objectID := idText(row.ID)
			if _, exists := known[objectID]; exists {
				continue
			}
			known[objectID] = struct{}{}
			report.Progress.Scanned++
			b.addManual(report, Finding{
				ObjectType: "delivery_verification_attempt",
				ObjectID:   objectID,
				Code:       "ORPHAN_HISTORICAL_VERIFICATION_ATTEMPT",
				Detail:     "attempt has no parent verification and cannot be safely remapped; investigate the protected source facts",
				Data: map[string]any{
					"verification_id":   idText(row.VerificationID),
					"delivery_order_id": idText(row.DeliveryOrderID),
					"stage":             row.Stage,
					"mode_snapshot":     row.ModeSnapshot,
				},
			})
		}
		if err := b.afterBatch(ctx, fingerprint, report, batchStarted, len(rows)); err != nil {
			return err
		}
	}
}

func boolCount(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func (b *Backfiller) afterBatch(ctx context.Context, fingerprint string, report *BackfillReport, started time.Time, rows int) error {
	if err := b.saveCheckpoint(fingerprint, *report); err != nil {
		return err
	}
	expected := time.Duration(float64(time.Second) * (float64(rows) / float64(b.options.RowsPerSecond)))
	remaining := expected - time.Since(started)
	if remaining <= 0 {
		return nil
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (b *Backfiller) retryTransaction(ctx context.Context, operation func(*gorm.DB) error) error {
	var err error
	for attempt := 0; attempt < b.options.MaxRetries; attempt++ {
		err = b.db.WithContext(ctx).Transaction(operation)
		if err == nil {
			return nil
		}
		if attempt+1 == b.options.MaxRetries {
			break
		}
		backoff := time.Duration(100*(1<<attempt)) * time.Millisecond
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("batch transaction failed after %d attempts: %w", b.options.MaxRetries, err)
}

func safeError(err error) string {
	if err == nil {
		return "unknown error"
	}
	text := strings.ToLower(err.Error())
	for _, sensitive := range []string{"password", "secret", "token", "cipher", "dsn"} {
		if strings.Contains(text, sensitive) {
			return "source data could not be rendered; inspect the protected migration log"
		}
	}
	if len(err.Error()) > 200 {
		return err.Error()[:200]
	}
	return err.Error()
}

func BuildVerificationAudit(report BackfillReport, cutover time.Time, reason string) VerificationAudit {
	return VerificationAudit{
		SchemaVersion: "cp1.verification-migration-audit.v1",
		GeneratedAt:   time.Now().UTC(),
		DryRun:        report.DryRun,
		Completed:     report.Completed,
		CutoverAt:     cutover.UTC(),
		MappingReason: strings.TrimSpace(reason),
		IDRange:       report.Range,
		Mappings:      report.Progress.VerificationMappings,
		Manual:        report.Progress.Manual,
	}
}
