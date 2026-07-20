package riderapplication

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

// GetOwn 获取Own。
func (s *Service) GetOwn(ctx context.Context, claims *auth.Claims) (ApplicationDTO, error) {
	if err := s.requireEnabled(); err != nil {
		return ApplicationDTO{}, err
	}
	accountID, applicationID, err := applicantIDs(claims, "rider_application:self_view")
	if err != nil {
		return ApplicationDTO{}, err
	}
	record, err := s.loadRecord(ctx, s.db, applicationID)
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && record.AccountID != accountID) {
		return ApplicationDTO{}, problem.NotFound("RIDER_APPLICATION_NOT_FOUND", "rider application not found")
	}
	if err != nil {
		return ApplicationDTO{}, err
	}
	dto := dtoFrom(record, false)
	latest, found, err := s.latestReview(ctx, s.db, applicationID, false)
	if err != nil {
		return ApplicationDTO{}, err
	}
	if found {
		dto.LatestReview = &latest
	}
	return dto, nil
}

// UpdateOwn 更新Own。
func (s *Service) UpdateOwn(ctx context.Context, claims *auth.Claims, method, path, key string, input UpdateRequest) (ApplicationDTO, error) {
	if err := s.requireEnabled(); err != nil {
		return ApplicationDTO{}, err
	}
	accountID, applicationID, err := applicantIDs(claims, "rider_application:self_update")
	if err != nil {
		return ApplicationDTO{}, err
	}
	req, shopIDs, err := input.normalized(s.cfg.MaxShops)
	if err != nil {
		return ApplicationDTO{}, err
	}
	if err := s.checkRate(ctx, "write_account", idString(accountID), s.cfg.WriteAccountRatePerMinute, time.Minute); err != nil {
		return ApplicationDTO{}, err
	}

	var out ApplicationDTO
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), applicantActorType, accountID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			return cachedResponse(ctx, s.idem, tx, applicantActorType, accountID, path, key, &out)
		}
		record, err := s.loadLockedRecord(ctx, tx, applicationID)
		if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && record.AccountID != accountID) {
			return problem.NotFound("RIDER_APPLICATION_NOT_FOUND", "rider application not found")
		}
		if err != nil {
			return err
		}
		if record.Status != StatusRejected {
			s.metric.incConflict("update_state")
			return stateConflict()
		}
		if record.Version != req.ExpectedVersion {
			s.metric.incConflict("update_version")
			return versionConflict()
		}
		if err := validateActiveShops(ctx, tx, shopIDs); err != nil {
			return err
		}

		scopeJSON, _ := json.Marshal(req.ServiceScope)
		updates := map[string]any{
			"name": req.Name, "service_scope": datatypes.JSON(scopeJSON),
			"version": gorm.Expr("version + 1"), "updated_by": accountID,
		}
		result := tx.WithContext(ctx).Model(&Application{}).
			Where("id = ? AND version = ? AND status = ?", applicationID, req.ExpectedVersion, StatusRejected).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return versionConflict()
		}
		if err := s.writeAudit(ctx, tx, applicantActorType, accountID, "rider_application.update", applicationID, map[string]any{
			"application_id": idString(applicationID), "version": req.ExpectedVersion + 1,
		}); err != nil {
			return err
		}
		record.Name = req.Name
		record.ServiceScope = datatypes.JSON(scopeJSON)
		record.Version++
		out = dtoFrom(record, false)
		return s.idem.Succeed(ctx, tx, applicantActorType, accountID, path, key, out)
	})
	if err != nil {
		return ApplicationDTO{}, err
	}
	return out, nil
}

// Resubmit 返回Resubmit。
func (s *Service) Resubmit(ctx context.Context, claims *auth.Claims, method, path, key string, req VersionRequest) (ApplicationDTO, error) {
	if err := s.requireEnabled(); err != nil {
		return ApplicationDTO{}, err
	}
	accountID, applicationID, err := applicantIDs(claims, "rider_application:self_resubmit")
	if err != nil {
		return ApplicationDTO{}, err
	}
	if err := req.validate(); err != nil {
		return ApplicationDTO{}, err
	}
	if err := s.checkRate(ctx, "resubmit_account", idString(accountID), s.cfg.ResubmitAccountRatePerDay, 24*time.Hour); err != nil {
		return ApplicationDTO{}, err
	}
	var out ApplicationDTO
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), applicantActorType, accountID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			return cachedResponse(ctx, s.idem, tx, applicantActorType, accountID, path, key, &out)
		}
		record, err := s.loadLockedRecord(ctx, tx, applicationID)
		if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && record.AccountID != accountID) {
			return problem.NotFound("RIDER_APPLICATION_NOT_FOUND", "rider application not found")
		}
		if err != nil {
			return err
		}
		if record.Status != StatusRejected {
			s.metric.incConflict("resubmit_state")
			return stateConflict()
		}
		if record.Version != req.ExpectedVersion {
			s.metric.incConflict("resubmit_version")
			return versionConflict()
		}
		now := time.Now()
		result := tx.WithContext(ctx).Model(&Application{}).
			Where("id = ? AND status = ? AND version = ?", applicationID, StatusRejected, req.ExpectedVersion).
			Updates(map[string]any{
				"status": StatusSubmitted, "submission_count": gorm.Expr("submission_count + 1"),
				"version": gorm.Expr("version + 1"), "last_submitted_at": now, "updated_by": accountID,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return versionConflict()
		}
		record.Status = StatusSubmitted
		record.SubmissionCount++
		record.Version++
		record.LastSubmittedAt = now
		if err := s.writeAudit(ctx, tx, applicantActorType, accountID, "rider_application.resubmit", applicationID, map[string]any{
			"application_id": idString(applicationID), "submission_no": record.SubmissionCount, "version": record.Version,
		}); err != nil {
			return err
		}
		if err := s.writeOutbox(ctx, tx, "rider.application.resubmitted", applicationID, map[string]any{
			"application_id": idString(applicationID), "application_no": record.ApplicationNo, "submission_no": record.SubmissionCount,
		}); err != nil {
			return err
		}
		out = dtoFrom(record, false)
		return s.idem.Succeed(ctx, tx, applicantActorType, accountID, path, key, out)
	})
	if err != nil {
		return ApplicationDTO{}, err
	}
	return out, nil
}

// latestReview 返回最新记录 Review。
func (s *Service) latestReview(ctx context.Context, db *gorm.DB, applicationID uint64, includeAdmin bool) (ReviewDTO, bool, error) {
	var review Review
	err := db.WithContext(ctx).Where("application_id = ?", applicationID).Order("submission_no DESC, id DESC").Take(&review).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ReviewDTO{}, false, nil
	}
	if err != nil {
		return ReviewDTO{}, false, err
	}
	return reviewDTO(review, includeAdmin), true, nil
}

// reviewDTO 审核DTO。
func reviewDTO(review Review, includeAdmin bool) ReviewDTO {
	dto := ReviewDTO{
		ID: idString(review.ID), SubmissionNo: review.SubmissionNo, Decision: review.Decision,
		Reason: review.Reason, CreatedAt: review.CreatedAt.Format(time.RFC3339),
	}
	if includeAdmin {
		dto.ReviewerAdminID = idString(review.ReviewerAdminID)
	}
	return dto
}
