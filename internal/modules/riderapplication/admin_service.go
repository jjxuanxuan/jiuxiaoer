package riderapplication

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
)

const defaultApplicationOrder = "last_submitted_at desc,id desc"

type applicationFilter struct {
	Status        string
	ApplicationNo string
	Phone         string
}

type pageCursor struct {
	Filter          string `json:"filter"`
	Order           string `json:"order"`
	LastSubmittedAt string `json:"last_submitted_at,omitempty"`
	CreatedAt       string `json:"created_at,omitempty"`
	Status          string `json:"status,omitempty"`
	SubmissionCount uint   `json:"submission_count,omitempty"`
	ID              uint64 `json:"id"`
}

// List 查询申请列表列表。
func (s *Service) List(ctx context.Context, claims *auth.Claims, pageSize int, rawToken, rawFilter, rawOrder string) (ApplicationList, error) {
	if err := s.requireEnabled(); err != nil {
		return ApplicationList{}, err
	}
	if _, err := adminID(claims, "rider_application:list"); err != nil {
		return ApplicationList{}, err
	}
	if pageSize == 0 {
		pageSize = 20
	}
	if pageSize < 1 || pageSize > 100 {
		return ApplicationList{}, invalid("page_size must be between 1 and 100")
	}
	if len(rawFilter) > 1024 || len(rawOrder) > 256 || len(rawToken) > 2048 {
		return ApplicationList{}, invalid("filter, order_by, or page_token is too long")
	}
	filter, canonicalFilter, err := parseApplicationFilter(rawFilter)
	if err != nil {
		return ApplicationList{}, err
	}
	order, err := normalizeApplicationOrder(rawOrder)
	if err != nil {
		return ApplicationList{}, err
	}
	var cursor *pageCursor
	if rawToken != "" {
		decoded, err := s.decodePageToken(rawToken)
		if err != nil || decoded.Filter != canonicalFilter || decoded.Order != order || decoded.ID == 0 {
			return ApplicationList{}, invalid("page_token does not match filter and order_by")
		}
		cursor = &decoded
	}

	query := s.db.WithContext(ctx).Table("rider_applications ra").
		Select("ra.*, a.phone, a.status AS account_status, a.credential_version, a.token_invalid_before").
		Joins("JOIN accounts a ON a.id = ra.account_id AND a.deleted_at IS NULL")
	query = applyApplicationFilter(query, filter)
	if cursor != nil {
		query, err = applyCursor(query, order, *cursor)
		if err != nil {
			return ApplicationList{}, err
		}
	}
	var records []applicationRecord
	if err := query.Order(sqlApplicationOrder(order)).Limit(pageSize + 1).Scan(&records).Error; err != nil {
		return ApplicationList{}, err
	}
	hasMore := len(records) > pageSize
	if hasMore {
		records = records[:pageSize]
	}
	fullPhone := hasPermission(claims, "rider_application:view_phone")
	items := make([]ApplicationDTO, 0, len(records))
	for _, record := range records {
		items = append(items, dtoFrom(record, fullPhone))
	}
	result := ApplicationList{Items: items}
	if hasMore && len(records) > 0 {
		last := records[len(records)-1]
		result.NextPageToken = s.encodePageToken(pageCursor{
			Filter: canonicalFilter, Order: order, LastSubmittedAt: last.LastSubmittedAt.Format(time.RFC3339Nano),
			CreatedAt: last.CreatedAt.Format(time.RFC3339Nano), Status: last.Status,
			SubmissionCount: last.SubmissionCount, ID: last.ID,
		})
	}
	return result, nil
}

// Detail 返回Detail。
func (s *Service) Detail(ctx context.Context, claims *auth.Claims, rawID string) (ApplicationDTO, error) {
	if err := s.requireEnabled(); err != nil {
		return ApplicationDTO{}, err
	}
	if _, err := adminID(claims, "rider_application:view"); err != nil {
		return ApplicationDTO{}, err
	}
	id, err := parseID(rawID)
	if err != nil {
		return ApplicationDTO{}, problem.NotFound("RIDER_APPLICATION_NOT_FOUND", "rider application not found")
	}
	record, err := s.loadRecord(ctx, s.db, id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ApplicationDTO{}, problem.NotFound("RIDER_APPLICATION_NOT_FOUND", "rider application not found")
	}
	if err != nil {
		return ApplicationDTO{}, err
	}
	var reviews []Review
	if err := s.db.WithContext(ctx).Where("application_id = ?", id).Order("submission_no ASC, id ASC").Find(&reviews).Error; err != nil {
		return ApplicationDTO{}, err
	}
	dto := dtoFrom(record, hasPermission(claims, "rider_application:view_phone"))
	dto.Reviews = make([]ReviewDTO, 0, len(reviews))
	for _, review := range reviews {
		dto.Reviews = append(dto.Reviews, reviewDTO(review, true))
	}
	return dto, nil
}

// Review 审核申请DTO。
func (s *Service) Review(ctx context.Context, claims *auth.Claims, ip, method, path, key, rawID string, input ReviewRequest) (ApplicationDTO, error) {
	if err := s.requireEnabled(); err != nil {
		return ApplicationDTO{}, err
	}
	adminUserID, err := adminID(claims, "rider_application:review")
	if err != nil {
		return ApplicationDTO{}, err
	}
	applicationID, err := parseID(rawID)
	if err != nil {
		return ApplicationDTO{}, problem.NotFound("RIDER_APPLICATION_NOT_FOUND", "rider application not found")
	}
	req, err := input.normalized()
	if err != nil {
		return ApplicationDTO{}, err
	}
	if err := s.checkRate(ctx, "review_admin", idString(adminUserID), s.cfg.ReviewAdminRatePerMinute, time.Minute); err != nil {
		return ApplicationDTO{}, err
	}
	reviewCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var out ApplicationDTO
	var openDuration time.Duration
	err = s.db.WithContext(reviewCtx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(reviewCtx, tx, s.ids.Next(), "admin", adminUserID, method, path, key, idempotency.ResourceRequestHash("rider_application.review", applicationID, req))
		if err != nil {
			return err
		}
		if !started {
			return cachedResponse(reviewCtx, s.idem, tx, "admin", adminUserID, path, key, &out)
		}
		record, err := s.loadLockedRecord(reviewCtx, tx, applicationID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("RIDER_APPLICATION_NOT_FOUND", "rider application not found")
		}
		if err != nil {
			return err
		}
		if record.Status != StatusSubmitted {
			s.metric.incConflict("review_state")
			return stateConflict()
		}
		if record.Version != req.ExpectedVersion {
			s.metric.incConflict("review_version")
			return versionConflict()
		}
		if record.AccountStatus != "disabled" {
			return stateConflict()
		}
		var scope ServiceScope
		if err := json.Unmarshal(record.ServiceScope, &scope); err != nil {
			return problem.Internal("invalid stored rider application service scope")
		}
		shopIDs, _, err := normalizeScope(scope, s.cfg.MaxShops)
		if err != nil {
			return err
		}
		if err := validateActiveShops(reviewCtx, tx, shopIDs); err != nil {
			return err
		}

		now := time.Now()
		snapshot, _ := json.Marshal(map[string]any{
			"application_id": idString(record.ID), "application_no": record.ApplicationNo,
			"name": record.Name, "phone": maskPhone(record.Phone),
			"service_scope": scope, "submission_no": record.SubmissionCount, "version": record.Version,
		})
		review := Review{
			ID: s.ids.Next(), ApplicationID: record.ID, SubmissionNo: record.SubmissionCount,
			Decision: req.Decision, Reason: req.Reason, ReviewerAdminID: adminUserID,
			ApplicationSnapshot: datatypes.JSON(snapshot), RequestID: requestctx.RequestIDPtr(reviewCtx), CreatedAt: now,
		}
		var riderID uint64
		if req.Decision == StatusApproved {
			openDuration = now.Sub(record.CreatedAt)
			riderID = s.ids.Next()
			if err := sensitiveSession(tx).WithContext(reviewCtx).Table("riders").Create(map[string]any{
				"id": riderID, "account_id": record.AccountID, "name": record.Name, "phone": record.Phone,
				"status": "active", "work_status": "offline", "review_status": "approved",
				"service_scope": record.ServiceScope, "created_by": adminUserID, "updated_by": adminUserID,
			}).Error; err != nil {
				return err
			}
			for _, shopID := range shopIDs {
				if err := tx.WithContext(reviewCtx).Table("rider_service_shops").Create(map[string]any{
					"id": s.ids.Next(), "rider_id": riderID, "shop_id": shopID, "status": "active",
					"source": "rider_application", "created_by": adminUserID, "updated_by": adminUserID,
				}).Error; err != nil {
					return err
				}
			}
			if err := tx.WithContext(reviewCtx).Table("rider_runtime_states").Create(map[string]any{
				"rider_id": riderID, "work_status": "offline", "last_sequence": 0, "version": 1,
			}).Error; err != nil {
				return err
			}
		}
		if err := tx.WithContext(reviewCtx).Create(&review).Error; err != nil {
			if isDuplicate(err) {
				return stateConflict()
			}
			return err
		}
		applicationUpdates := map[string]any{
			"status": req.Decision, "last_reviewed_at": now, "version": gorm.Expr("version + 1"),
			"updated_by": adminUserID,
		}
		if req.Decision == StatusApproved {
			applicationUpdates["rider_id"] = riderID
			applicationUpdates["approved_at"] = now
		}
		result := tx.WithContext(reviewCtx).Model(&Application{}).
			Where("id = ? AND status = ? AND version = ?", applicationID, StatusSubmitted, req.ExpectedVersion).
			Updates(applicationUpdates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return versionConflict()
		}
		if req.Decision == StatusApproved {
			invalidBefore := now.Add(-time.Second)
			result = tx.WithContext(reviewCtx).Table("accounts").Where("id = ? AND status = 'disabled'", record.AccountID).Updates(map[string]any{
				"status": "active", "credential_version": gorm.Expr("credential_version + 1"),
				"token_invalid_before": invalidBefore, "updated_by": adminUserID,
			})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return stateConflict()
			}
		}
		if err := s.writeAudit(reviewCtx, tx, "admin", adminUserID, "rider_application.review."+req.Decision, applicationID, map[string]any{
			"application_id": idString(applicationID), "decision": req.Decision,
			"submission_no": record.SubmissionCount, "version": record.Version + 1,
		}); err != nil {
			return err
		}
		if err := s.writeOutbox(reviewCtx, tx, "rider.application."+req.Decision, applicationID, map[string]any{
			"application_id": idString(applicationID), "application_no": record.ApplicationNo,
			"submission_no": record.SubmissionCount, "review_id": idString(review.ID),
		}); err != nil {
			return err
		}
		if req.Decision == StatusApproved {
			if err := s.writeOutboxAggregate(reviewCtx, tx, "rider.opened", "rider", riderID, map[string]any{
				"rider_id": idString(riderID), "application_id": idString(applicationID), "account_id": idString(record.AccountID),
			}); err != nil {
				return err
			}
			record.RiderID = &riderID
			record.ApprovedAt = &now
		}
		record.Status = req.Decision
		record.LastReviewedAt = &now
		record.Version++
		out = dtoFrom(record, hasPermission(claims, "rider_application:view_phone"))
		return s.idem.Succeed(reviewCtx, tx, "admin", adminUserID, path, key, out)
	})
	if err != nil {
		s.metric.incReview(req.Decision, metricResult(err))
		return ApplicationDTO{}, err
	}
	s.metric.incReview(req.Decision, "success")
	if req.Decision == StatusApproved {
		s.metric.observeOpen(openDuration)
	}
	return out, nil
}

// parseApplicationFilter 解析申请筛选条件。
func parseApplicationFilter(raw string) (applicationFilter, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return applicationFilter{}, "", nil
	}
	filter := applicationFilter{}
	values := map[string]string{}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ';' || r == ',' }) {
		pair := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(pair) != 2 || strings.TrimSpace(pair[1]) == "" {
			return applicationFilter{}, "", invalid("filter must use key=value entries separated by semicolons")
		}
		key, value := strings.TrimSpace(pair[0]), strings.TrimSpace(pair[1])
		if _, exists := values[key]; exists {
			return applicationFilter{}, "", invalid("filter fields must not repeat")
		}
		values[key] = value
		switch key {
		case "status":
			if value != StatusSubmitted && value != StatusRejected && value != StatusApproved && value != StatusCancelled {
				return applicationFilter{}, "", invalid("filter status is invalid")
			}
			filter.Status = value
		case "application_no":
			filter.ApplicationNo = value
		case "phone":
			if !phonePattern.MatchString(value) {
				return applicationFilter{}, "", invalid("filter phone is invalid")
			}
			filter.Phone = value
		default:
			return applicationFilter{}, "", invalid("unsupported filter field")
		}
	}
	keys := []string{"status", "application_no", "phone"}
	canonical := make([]string, 0, len(values))
	for _, key := range keys {
		if value := values[key]; value != "" {
			canonical = append(canonical, key+"="+value)
		}
	}
	return filter, strings.Join(canonical, ";"), nil
}

// normalizeApplicationOrder 规范化申请订单。
func normalizeApplicationOrder(raw string) (string, error) {
	order := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(raw)), " "))
	order = strings.ReplaceAll(order, ", ", ",")
	if order == "" {
		order = defaultApplicationOrder
	}
	allowed := map[string]bool{
		"last_submitted_at desc,id desc":                       true,
		"last_submitted_at asc,id asc":                         true,
		"created_at desc,id desc":                              true,
		"created_at asc,id asc":                                true,
		"status asc,last_submitted_at desc,id desc":            true,
		"submission_count desc,last_submitted_at desc,id desc": true,
	}
	if !allowed[order] {
		return "", invalid("unsupported order_by")
	}
	return order, nil
}

// sqlApplicationOrder 返回sql 申请订单。
func sqlApplicationOrder(order string) string {
	parts := strings.Split(order, ",")
	for index, part := range parts {
		parts[index] = "ra." + strings.TrimSpace(part)
	}
	return strings.Join(parts, ",")
}

// applyApplicationFilter 应用申请筛选条件。
func applyApplicationFilter(query *gorm.DB, filter applicationFilter) *gorm.DB {
	if filter.Status != "" {
		query = query.Where("ra.status = ?", filter.Status)
	}
	if filter.ApplicationNo != "" {
		query = query.Where("ra.application_no = ?", filter.ApplicationNo)
	}
	if filter.Phone != "" {
		query = query.Where("a.phone = ?", filter.Phone)
	}
	return query
}

// applyCursor 应用Cursor。
func applyCursor(query *gorm.DB, order string, cursor pageCursor) (*gorm.DB, error) {
	lastSubmittedAt, err := time.Parse(time.RFC3339Nano, cursor.LastSubmittedAt)
	if err != nil && strings.Contains(order, "last_submitted_at") {
		return nil, invalid("invalid page_token")
	}
	createdAt, createdErr := time.Parse(time.RFC3339Nano, cursor.CreatedAt)
	switch order {
	case "last_submitted_at desc,id desc":
		return query.Where("(ra.last_submitted_at < ? OR (ra.last_submitted_at = ? AND ra.id < ?))", lastSubmittedAt, lastSubmittedAt, cursor.ID), nil
	case "last_submitted_at asc,id asc":
		return query.Where("(ra.last_submitted_at > ? OR (ra.last_submitted_at = ? AND ra.id > ?))", lastSubmittedAt, lastSubmittedAt, cursor.ID), nil
	case "created_at desc,id desc":
		if createdErr != nil {
			return nil, invalid("invalid page_token")
		}
		return query.Where("(ra.created_at < ? OR (ra.created_at = ? AND ra.id < ?))", createdAt, createdAt, cursor.ID), nil
	case "created_at asc,id asc":
		if createdErr != nil {
			return nil, invalid("invalid page_token")
		}
		return query.Where("(ra.created_at > ? OR (ra.created_at = ? AND ra.id > ?))", createdAt, createdAt, cursor.ID), nil
	case "status asc,last_submitted_at desc,id desc":
		return query.Where("(ra.status > ? OR (ra.status = ? AND (ra.last_submitted_at < ? OR (ra.last_submitted_at = ? AND ra.id < ?))))", cursor.Status, cursor.Status, lastSubmittedAt, lastSubmittedAt, cursor.ID), nil
	case "submission_count desc,last_submitted_at desc,id desc":
		return query.Where("(ra.submission_count < ? OR (ra.submission_count = ? AND (ra.last_submitted_at < ? OR (ra.last_submitted_at = ? AND ra.id < ?))))", cursor.SubmissionCount, cursor.SubmissionCount, lastSubmittedAt, lastSubmittedAt, cursor.ID), nil
	default:
		return nil, invalid("invalid page_token order")
	}
}

// encodePageToken 编码分页令牌。
func (s *Service) encodePageToken(cursor pageCursor) string {
	raw, _ := json.Marshal(cursor)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + s.hmacString("page:"+payload)
}

// decodePageToken 解码分页令牌。
func (s *Service) decodePageToken(raw string) (pageCursor, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 2 || !hmacEqual(parts[1], s.hmacString("page:"+parts[0])) {
		return pageCursor{}, fmt.Errorf("invalid signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return pageCursor{}, err
	}
	var cursor pageCursor
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return pageCursor{}, err
	}
	return cursor, nil
}

// hmacEqual 判断HMAC Equal。
func hmacEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	var diff byte
	for index := range left {
		diff |= left[index] ^ right[index]
	}
	return diff == 0
}

// parsePageSize 解析分页 Size。
func parsePageSize(raw string) (int, error) {
	if raw == "" {
		return 20, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, invalid("page_size must be an integer")
	}
	return value, nil
}
