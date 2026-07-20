package riderapplication

import (
	"time"

	"gorm.io/datatypes"
)

const (
	StatusSubmitted = "submitted"
	StatusRejected  = "rejected"
	StatusApproved  = "approved"
	StatusCancelled = "cancelled"
)

type Application struct {
	ID                       uint64
	ApplicationNo            string
	AccountID                uint64
	RiderID                  *uint64
	Name                     string
	ServiceScope             datatypes.JSON
	Status                   string
	SubmissionCount          uint
	Version                  uint
	CreateIdempotencyKeyHash string
	CreateRequestHash        string
	LastSubmittedAt          time.Time
	LastReviewedAt           *time.Time
	ApprovedAt               *time.Time
	CreatedAt                time.Time
	UpdatedAt                time.Time
	CreatedBy                *uint64
	UpdatedBy                *uint64
}

// TableName 返回当前数据模型对应的数据库表名。
func (Application) TableName() string { return "rider_applications" }

type Review struct {
	ID                  uint64
	ApplicationID       uint64
	SubmissionNo        uint
	Decision            string
	Reason              string
	ReviewerAdminID     uint64
	ApplicationSnapshot datatypes.JSON
	RequestID           *string
	CreatedAt           time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Review) TableName() string { return "rider_application_reviews" }

type applicationRecord struct {
	Application
	Phone              string     `gorm:"column:phone"`
	AccountStatus      string     `gorm:"column:account_status"`
	CredentialVersion  uint       `gorm:"column:credential_version"`
	TokenInvalidBefore *time.Time `gorm:"column:token_invalid_before"`
}

type ServiceScope struct {
	ShopIDs []string `json:"shop_ids"`
}

type SubmitRequest struct {
	Name         string       `json:"name"`
	Phone        string       `json:"phone"`
	Code         string       `json:"code"`
	ServiceScope ServiceScope `json:"service_scope"`
}

type LoginRequest struct {
	Phone string `json:"phone"`
	Code  string `json:"code"`
}

type UpdateRequest struct {
	Name            string       `json:"name"`
	ServiceScope    ServiceScope `json:"service_scope"`
	ExpectedVersion uint         `json:"expected_version"`
}

type VersionRequest struct {
	ExpectedVersion uint `json:"expected_version"`
}

type ReviewRequest struct {
	Decision        string `json:"decision"`
	Reason          string `json:"reason"`
	ExpectedVersion uint   `json:"expected_version"`
}

type LoginResponse struct {
	ApplicationAccessToken string `json:"application_access_token"`
	ExpiresIn              int64  `json:"expires_in"`
	ApplicationID          string `json:"application_id"`
	ApplicationStatus      string `json:"application_status"`
}

type ApplicationDTO struct {
	ID              string       `json:"id"`
	ApplicationNo   string       `json:"application_no"`
	AccountID       string       `json:"account_id,omitempty"`
	RiderID         string       `json:"rider_id,omitempty"`
	Name            string       `json:"name"`
	Phone           string       `json:"phone,omitempty"`
	ServiceScope    ServiceScope `json:"service_scope"`
	Status          string       `json:"status"`
	SubmissionCount uint         `json:"submission_count"`
	Version         uint         `json:"version"`
	LastSubmittedAt string       `json:"last_submitted_at"`
	LastReviewedAt  string       `json:"last_reviewed_at,omitempty"`
	ApprovedAt      string       `json:"approved_at,omitempty"`
	CreatedAt       string       `json:"created_at,omitempty"`
	LatestReview    *ReviewDTO   `json:"latest_review,omitempty"`
	Reviews         []ReviewDTO  `json:"reviews,omitempty"`
}

type ReviewDTO struct {
	ID              string `json:"id"`
	SubmissionNo    uint   `json:"submission_no"`
	Decision        string `json:"decision"`
	Reason          string `json:"reason"`
	ReviewerAdminID string `json:"reviewer_admin_id,omitempty"`
	CreatedAt       string `json:"created_at"`
}

type ApplicationList struct {
	Items         []ApplicationDTO `json:"items"`
	NextPageToken string           `json:"next_page_token,omitempty"`
}
