package integrity

import (
	"context"
	"time"

	"gorm.io/gorm"

	redemptiondomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
	reminderdomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/reminder"
)

func (s *IntegrityService) scanSlots(
	ctx context.Context,
	tx *gorm.DB,
	afterID uint64,
	upperID *uint64,
	limit int,
) (int, uint64, []reconciliationDiscrepancy, error) {
	rows, err := s.repo.listIntegritySlots(
		ctx,
		tx,
		afterID,
		upperID,
		limit,
	)
	if err != nil {
		return 0, afterID, nil, err
	}
	slotIDs := make([]uint64, 0, len(rows))
	for _, slot := range rows {
		slotIDs = append(slotIDs, slot.ID)
	}
	expectedBySlot := make(map[uint64]int64, len(slotIDs))
	if len(slotIDs) > 0 {
		counts, err := s.repo.reconciliationSlotReservationCounts(
			ctx,
			tx,
			slotIDs,
		)
		if err != nil {
			return 0, afterID, nil, err
		}
		for _, count := range counts {
			expectedBySlot[count.DeliveryTimeSlotID] = count.Count
		}
	}
	discrepancies := make([]reconciliationDiscrepancy, 0)
	for _, slot := range rows {
		expected := expectedBySlot[slot.ID]
		if uint64(slot.ReservedOrders) == uint64(expected) &&
			slot.ReservedOrders <= slot.CapacityOrders {
			continue
		}
		discrepancies = append(discrepancies, reconciliationDiscrepancy{
			Rule:    reconciliationRuleSlot,
			Kind:    "slot_reservation_count",
			BizType: "delivery_time_slot", BizID: slot.ID,
			Severity: "P1",
			Expected: map[string]any{
				"reserved_orders":  expected,
				"count_definition": "redemption_status_not_cancelled",
				"within_capacity":  true,
			},
			Actual: map[string]any{
				"reserved_orders": slot.ReservedOrders,
				"capacity_orders": slot.CapacityOrders,
				"status":          slot.Status,
			},
		})
	}
	if len(rows) == 0 {
		return 0, afterID, discrepancies, nil
	}
	return len(rows), rows[len(rows)-1].ID, discrepancies, nil
}

type reconciliationReminderGroup struct {
	LotID      uint64
	ExpiresAt  time.Time
	RemindDays uint8
	Channel    string
	Count      int64
	MinID      uint64
}

func (s *IntegrityService) scanReminders(
	ctx context.Context,
	tx *gorm.DB,
	afterID uint64,
	upperID *uint64,
	limit int,
) (int, uint64, []reconciliationDiscrepancy, error) {
	rows, err := s.repo.listIntegrityReminders(
		ctx,
		tx,
		afterID,
		upperID,
		limit,
	)
	if err != nil {
		return 0, afterID, nil, err
	}
	lotIDs := make([]uint64, 0, len(rows))
	for _, reminder := range rows {
		lotIDs = append(lotIDs, reminder.LotID)
	}
	groups := make(map[string]reconciliationReminderGroup, len(rows))
	if len(lotIDs) > 0 {
		groupedRows, err := s.repo.reconciliationReminderGroups(
			ctx,
			tx,
			lotIDs,
		)
		if err != nil {
			return 0, afterID, nil, err
		}
		for _, group := range groupedRows {
			groups[reminderGroupKey(
				group.LotID,
				group.ExpiresAt,
				group.RemindDays,
				group.Channel,
			)] = group
		}
	}
	discrepancies := make([]reconciliationDiscrepancy, 0)
	seen := make(map[string]struct{})
	for _, reminder := range rows {
		key := reminderGroupKey(
			reminder.LotID,
			reminder.ExpiresAt,
			reminder.RemindDays,
			reminder.Channel,
		)
		group := groups[key]
		if group.Count <= 1 || group.MinID == 0 {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		discrepancies = append(discrepancies, reconciliationDiscrepancy{
			Rule:    reconciliationRuleReminder,
			Kind:    "t7_reminder_uniqueness",
			BizType: "wine_ticket_reminder", BizID: group.MinID,
			Severity: "P2",
			Expected: map[string]any{
				"maximum_rows": 1,
				"key": map[string]any{
					"lot_id":      reminder.LotID,
					"expires_at":  reminder.ExpiresAt,
					"remind_days": reminder.RemindDays,
					"channel":     reminder.Channel,
				},
			},
			Actual: map[string]any{
				"row_count": group.Count,
				"anchor_id": group.MinID,
			},
		})
	}
	if len(rows) == 0 {
		return 0, afterID, discrepancies, nil
	}
	return len(rows), rows[len(rows)-1].ID, discrepancies, nil
}

type reconciliationSlotReservationCount struct {
	DeliveryTimeSlotID uint64
	Count              int64
}

func (r *reconciliationRepository) listIntegritySlots(
	ctx context.Context,
	tx *gorm.DB,
	afterID uint64,
	upperID *uint64,
	limit int,
) ([]redemptiondomain.DeliveryTimeSlot, error) {
	var rows []redemptiondomain.DeliveryTimeSlot
	query := r.idWindow(
		tx.WithContext(ctx).Model(&redemptiondomain.DeliveryTimeSlot{}),
		"id",
		afterID,
		upperID,
	)
	err := query.Order("id").Limit(limit).Find(&rows).Error
	return rows, err
}

func (r *reconciliationRepository) reconciliationSlotReservationCounts(
	ctx context.Context,
	tx *gorm.DB,
	slotIDs []uint64,
) ([]reconciliationSlotReservationCount, error) {
	var rows []reconciliationSlotReservationCount
	err := tx.WithContext(ctx).Model(&redemptiondomain.Redemption{}).
		Select("delivery_time_slot_id, COUNT(*) AS count").
		Where(
			"delivery_time_slot_id IN ? AND status <> ?",
			slotIDs,
			RedemptionStatusCancelled,
		).
		Group("delivery_time_slot_id").
		Scan(&rows).Error
	return rows, err
}

func (r *reconciliationRepository) listIntegrityReminders(
	ctx context.Context,
	tx *gorm.DB,
	afterID uint64,
	upperID *uint64,
	limit int,
) ([]reminderdomain.Reminder, error) {
	var rows []reminderdomain.Reminder
	query := r.idWindow(
		tx.WithContext(ctx).Model(&reminderdomain.Reminder{}),
		"id",
		afterID,
		upperID,
	)
	err := query.
		Where("remind_days = ?", 7).
		Order("id").
		Limit(limit).
		Find(&rows).Error
	return rows, err
}

func (r *reconciliationRepository) reconciliationReminderGroups(
	ctx context.Context,
	tx *gorm.DB,
	lotIDs []uint64,
) ([]reconciliationReminderGroup, error) {
	var rows []reconciliationReminderGroup
	err := tx.WithContext(ctx).Table("wine_ticket_reminders").
		Select(`
			lot_id, expires_at, remind_days, channel,
			COUNT(*) AS count, MIN(id) AS min_id
		`).
		Where(
			"lot_id IN ? AND remind_days = ?",
			reconciliationUniqueIDs(lotIDs),
			7,
		).
		Group("lot_id, expires_at, remind_days, channel").
		Scan(&rows).Error
	return rows, err
}

func reminderGroupKey(
	lotID uint64,
	expiresAt time.Time,
	remindDays uint8,
	channel string,
) string {
	return idString(lotID) + "|" +
		expiresAt.UTC().Format(time.RFC3339Nano) + "|" +
		idString(uint64(remindDays)) + "|" +
		channel
}
