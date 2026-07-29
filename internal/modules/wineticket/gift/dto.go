package gift

import (
	"strings"
	"unicode/utf8"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

const (
	GiftStatusPending         = "pending"
	GiftStatusClaimed         = "claimed"
	GiftStatusCancelled       = "cancelled"
	GiftStatusExpiredReturned = "expired_returned"
	GiftStatusException       = "exception"

	GiftAllocationStatusHeld     = "held"
	GiftAllocationStatusClaimed  = "claimed"
	GiftAllocationStatusRestored = "restored"

	TransactionTypeGiftHold    = "gift_hold"
	TransactionTypeGiftClaim   = "gift_claim"
	TransactionTypeGiftRestore = "gift_restore"
	TransactionTypeExpiry      = "expiry"

	giftClaimTTL         = 48 * 60 * 60
	giftShareTokenTTL    = 24 * 60 * 60
	giftActiveTokenMax   = 3
	giftQuantityMax      = 1000
	giftMessageRuneMax   = 140
	giftTokenMinLength   = 43
	giftTokenMaxLength   = 256
	giftListDirectionIn  = "received"
	giftListDirectionOut = "sent"

	// ActiveTokenMax 仅为迁移期兼容性测试导出。
	// HTTP 调用方仍只能通过 RATE_LIMITED 响应感知该限制。
	ActiveTokenMax = giftActiveTokenMax
)

type GiftCreateRequest struct {
	SourceLotNo string  `json:"source_lot_no"`
	Quantity    uint    `json:"quantity"`
	Message     *string `json:"message,omitempty"`
}

type GiftShareTokenRequest struct {
	ExpectedGiftVersion uint `json:"expected_gift_version"`
}

type GiftExpectedVersionRequest struct {
	ExpectedVersion uint `json:"expected_version"`
}

// GiftClaimRequest 被有意设计为空结构。
// 领取目标只存在于 X-Wine-Gift-Token，因此不会被复制到 JSON 正文、
// 幂等响应、审计载荷或应用日志中。
type GiftClaimRequest struct{}

type GiftDTO struct {
	GiftNo              string                 `json:"gift_no"`
	Product             core.ProductSummaryDTO `json:"product"`
	Quantity            uint                   `json:"quantity"`
	Message             *string                `json:"message"`
	GiverDisplayName    *string                `json:"giver_display_name"`
	ReceiverDisplayName *string                `json:"receiver_display_name"`
	Status              string                 `json:"status"`
	ClaimDeadline       string                 `json:"claim_deadline"`
	EarliestExpiresAt   string                 `json:"earliest_expires_at"`
	Version             uint                   `json:"version"`
	CreatedAt           string                 `json:"created_at"`
	ClaimedAt           *string                `json:"claimed_at"`
}

type GiftShareTokenDTO struct {
	ShareToken      string `json:"share_token"`
	ExpiresAt       string `json:"expires_at"`
	MiniProgramPath string `json:"mini_program_path"`
}

type GiftPreviewDTO struct {
	GiverDisplayName  string                 `json:"giver_display_name"`
	Product           core.ProductSummaryDTO `json:"product"`
	Quantity          uint                   `json:"quantity"`
	Message           *string                `json:"message"`
	ClaimDeadline     string                 `json:"claim_deadline"`
	EarliestExpiresAt string                 `json:"earliest_expires_at"`
	ExpiryNotice      string                 `json:"expiry_notice"`
	Status            string                 `json:"status"`
	Claimable         bool                   `json:"claimable"`
	BlockedReasonCode *string                `json:"blocked_reason_code"`
}

type giftProjection struct {
	Gift `gorm:"embedded"`

	ProductName      string  `gorm:"column:product_name"`
	ProductBrandName *string `gorm:"column:product_brand_name"`
	ProductSpec      *string `gorm:"column:product_spec"`
	ProductImageURL  *string `gorm:"column:product_image_url"`
	GiverNickname    *string `gorm:"column:giver_nickname"`
	ReceiverNickname *string `gorm:"column:receiver_nickname"`
}

func normalizeGiftCreateRequest(req GiftCreateRequest) (GiftCreateRequest, error) {
	req.SourceLotNo = strings.TrimSpace(req.SourceLotNo)
	if err := validateBusinessNo(req.SourceLotNo, "source_lot_no"); err != nil {
		return GiftCreateRequest{}, err
	}
	if req.Quantity == 0 || req.Quantity > giftQuantityMax {
		return GiftCreateRequest{}, problem.InvalidArgument("VALIDATION_FAILED", "quantity must be between 1 and 1000")
	}
	if req.Message != nil {
		message := strings.TrimSpace(*req.Message)
		if !utf8.ValidString(message) || utf8.RuneCountInString(message) > giftMessageRuneMax {
			return GiftCreateRequest{}, problem.InvalidArgument("VALIDATION_FAILED", "message must contain at most 140 characters")
		}
		if message == "" {
			req.Message = nil
		} else {
			req.Message = &message
		}
	}
	return req, nil
}

func validGiftStatus(value string) bool {
	switch value {
	case "", GiftStatusPending, GiftStatusClaimed, GiftStatusCancelled, GiftStatusExpiredReturned, GiftStatusException:
		return true
	default:
		return false
	}
}

func validGiftDirection(value string) bool {
	return value == giftListDirectionOut || value == giftListDirectionIn
}

func giftDTO(row giftProjection) GiftDTO {
	return GiftDTO{
		GiftNo: row.GiftNo,
		Product: core.ProductSummaryDTO{
			ProductID: idString(row.ProductID),
			Name:      row.ProductName,
			BrandName: row.ProductBrandName,
			Spec:      row.ProductSpec,
			ImageURL:  row.ProductImageURL,
		},
		Quantity:            row.Quantity,
		Message:             row.Message,
		GiverDisplayName:    maskedGiftDisplayName(row.GiverNickname),
		ReceiverDisplayName: maskedGiftDisplayName(row.ReceiverNickname),
		Status:              row.Status,
		ClaimDeadline:       formatShanghai(row.ClaimDeadline),
		EarliestExpiresAt:   formatShanghai(row.EarliestExpiresAt),
		Version:             row.Version,
		CreatedAt:           formatShanghai(row.CreatedAt),
		ClaimedAt:           optionalTimeString(row.ClaimedAt),
	}
}

func giftPreviewDTO(row giftProjection) GiftPreviewDTO {
	giver := maskedGiftDisplayName(row.GiverNickname)
	giverName := "一位好友"
	if giver != nil {
		giverName = *giver
	}
	return GiftPreviewDTO{
		GiverDisplayName: giverName,
		Product: core.ProductSummaryDTO{
			ProductID: idString(row.ProductID),
			Name:      row.ProductName,
			BrandName: row.ProductBrandName,
			Spec:      row.ProductSpec,
			ImageURL:  row.ProductImageURL,
		},
		Quantity:          row.Quantity,
		Message:           row.Message,
		ClaimDeadline:     formatShanghai(row.ClaimDeadline),
		EarliestExpiresAt: formatShanghai(row.EarliestExpiresAt),
		ExpiryNotice:      "领取后不重置有效期，请在酒票到期前使用",
		Status:            row.Status,
		Claimable:         true,
	}
}

// maskedGiftDisplayName 不暴露客户标识、手机号或完整昵称，
// 可安全用于匿名预览和列表。
func maskedGiftDisplayName(nickname *string) *string {
	if nickname == nil {
		return nil
	}
	value := strings.TrimSpace(*nickname)
	if value == "" {
		return nil
	}
	runes := []rune(value)
	masked := string(runes[0]) + "***"
	return &masked
}
