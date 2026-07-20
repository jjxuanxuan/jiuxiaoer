package deliveryincident

import "time"

type ItemInput struct {
	OrderItemID uint64 `json:"order_item_id" binding:"required,min=1"`
	Quantity    uint   `json:"quantity" binding:"required,min=1"`
}

type ContactAttemptsInput struct {
	Count   uint      `json:"count" binding:"required,min=2"`
	FirstAt time.Time `json:"first_at" binding:"required"`
	LastAt  time.Time `json:"last_at" binding:"required"`
}

type LocationInput struct {
	Longitude  float64   `json:"longitude" binding:"gte=-180,lte=180"`
	Latitude   float64   `json:"latitude" binding:"gte=-90,lte=90"`
	AccuracyM  float64   `json:"accuracy_m" binding:"gte=0,lte=10000"`
	CapturedAt time.Time `json:"captured_at" binding:"required"`
}

type CreateReq struct {
	Type            string                `json:"type" binding:"required,oneof=out_of_stock alcohol_damaged customer_refused customer_unreachable"`
	ReasonCode      string                `json:"reason_code" binding:"max=64"`
	Description     string                `json:"description" binding:"required,max=1000"`
	Items           []ItemInput           `json:"items" binding:"max=50,dive"`
	ContactAttempts *ContactAttemptsInput `json:"contact_attempts"`
	EvidenceTokens  []string              `json:"evidence_tokens" binding:"max=9,dive,min=20,max=8192"`
	Location        *LocationInput        `json:"location"`
}

type AddEvidenceReq struct {
	ExpectedVersion uint     `json:"expected_version" binding:"required,min=1"`
	EvidenceTokens  []string `json:"evidence_tokens" binding:"required,min=1,max=9,dive,min=20,max=8192"`
}

type AcknowledgeReq struct {
	ExpectedVersion uint   `json:"expected_version" binding:"required,min=1"`
	Note            string `json:"note" binding:"max=1000"`
}

type ResolveReq struct {
	ExpectedVersion uint   `json:"expected_version" binding:"required,min=1"`
	ResolutionCode  string `json:"resolution_code" binding:"required,oneof=issue_cleared_resume return_required returned_to_store refund_followup other"`
	ResolutionNote  string `json:"resolution_note" binding:"required,max=1000"`
}

type RejectReq struct {
	ExpectedVersion uint   `json:"expected_version" binding:"required,min=1"`
	ReasonCode      string `json:"reason_code" binding:"required,max=64"`
	Reason          string `json:"reason" binding:"required,max=1000"`
}

type ItemDTO struct {
	ID           string         `json:"id"`
	OrderItemID  string         `json:"order_item_id"`
	Quantity     uint           `json:"quantity"`
	ItemSnapshot map[string]any `json:"item_snapshot"`
}

type EvidenceDTO struct {
	ID            string `json:"id"`
	MimeType      string `json:"mime_type"`
	SizeBytes     uint64 `json:"size_bytes"`
	SHA256Suffix  string `json:"sha256_suffix"`
	ScanStatus    string `json:"scan_status"`
	ViewAvailable bool   `json:"view_available"`
	CreatedAt     string `json:"created_at"`
}

type EvidenceViewDTO struct {
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

type HistoryDTO struct {
	ID         string `json:"id"`
	ActorType  string `json:"actor_type"`
	ActorID    string `json:"actor_id,omitempty"`
	Action     string `json:"action"`
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status"`
	ReasonCode string `json:"reason_code,omitempty"`
	Remark     string `json:"remark,omitempty"`
	CreatedAt  string `json:"created_at"`
}

type DTO struct {
	ID                        string        `json:"id"`
	IncidentNo                string        `json:"incident_no"`
	DeliveryOrderID           string        `json:"delivery_order_id"`
	OrderID                   string        `json:"order_id"`
	ShopID                    string        `json:"shop_id"`
	RiderID                   string        `json:"rider_id"`
	Type                      string        `json:"type"`
	Stage                     string        `json:"stage"`
	Status                    string        `json:"status"`
	Priority                  string        `json:"priority"`
	ReasonCode                string        `json:"reason_code,omitempty"`
	Description               string        `json:"description"`
	DeliveryStatusSnapshot    string        `json:"delivery_status_snapshot"`
	AssignmentVersionSnapshot uint          `json:"assignment_version_snapshot"`
	ContactAttemptCount       uint          `json:"contact_attempt_count"`
	FirstContactAt            string        `json:"first_contact_at,omitempty"`
	LastContactAt             string        `json:"last_contact_at,omitempty"`
	DistanceToDestinationM    *uint         `json:"distance_to_destination_m,omitempty"`
	LocationAccuracyM         *float64      `json:"location_accuracy_m,omitempty"`
	LocationCapturedAt        string        `json:"location_captured_at,omitempty"`
	AcknowledgedBy            string        `json:"acknowledged_by,omitempty"`
	AcknowledgedAt            string        `json:"acknowledged_at,omitempty"`
	ResolvedBy                string        `json:"resolved_by,omitempty"`
	ResolvedAt                string        `json:"resolved_at,omitempty"`
	ResolutionCode            string        `json:"resolution_code,omitempty"`
	ResolutionNote            string        `json:"resolution_note,omitempty"`
	RejectedBy                string        `json:"rejected_by,omitempty"`
	RejectedAt                string        `json:"rejected_at,omitempty"`
	RejectionCode             string        `json:"rejection_code,omitempty"`
	RejectionReason           string        `json:"rejection_reason,omitempty"`
	Version                   uint          `json:"version"`
	ReportedAt                string        `json:"reported_at"`
	CreatedAt                 string        `json:"created_at"`
	UpdatedAt                 string        `json:"updated_at"`
	DeliveryReturnID          string        `json:"delivery_return_id,omitempty"`
	Items                     []ItemDTO     `json:"items,omitempty"`
	Evidence                  []EvidenceDTO `json:"evidence,omitempty"`
	History                   []HistoryDTO  `json:"history,omitempty"`
}
