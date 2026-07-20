package dispatch

import "time"

type ScoreWeights struct {
	Distance  float64 `json:"distance"`
	Load      float64 `json:"load"`
	Idle      float64 `json:"idle"`
	Freshness float64 `json:"freshness"`
}

type PolicySnapshot struct {
	Mode                     string       `json:"mode"`
	AutoRounds               uint8        `json:"auto_rounds"`
	OfferTTLSeconds          uint         `json:"offer_ttl_seconds"`
	GrabTTLSeconds           uint         `json:"grab_ttl_seconds"`
	CandidateLimit           uint         `json:"candidate_limit"`
	OfferCandidateLimit      uint         `json:"offer_candidate_limit"`
	HeartbeatFreshSeconds    uint         `json:"heartbeat_fresh_seconds"`
	LocationFreshSeconds     uint         `json:"location_fresh_seconds"`
	MaxLocationAccuracyM     uint         `json:"max_location_accuracy_m"`
	MaxPickupDistanceM       uint         `json:"max_pickup_distance_m"`
	MaxActiveOrdersDefault   uint8        `json:"max_active_orders_default"`
	IdleFullScoreSeconds     uint         `json:"idle_full_score_seconds"`
	ScoreWeights             ScoreWeights `json:"score_weights"`
	RejectionCooldownSeconds uint         `json:"rejection_cooldown_seconds"`
	ScoreVersion             string       `json:"score_version"`
}

type PolicyCreateReq struct {
	PolicyCode               string       `json:"policy_code" binding:"required,max=64"`
	ScopeType                string       `json:"scope_type" binding:"required,oneof=global city shop"`
	ScopeID                  string       `json:"scope_id" binding:"required,max=64"`
	Mode                     string       `json:"mode" binding:"required,oneof=hybrid auto grab manual"`
	AutoRounds               uint8        `json:"auto_rounds" binding:"max=10"`
	OfferTTLSeconds          uint         `json:"offer_ttl_seconds" binding:"required,min=5,max=60"`
	GrabTTLSeconds           uint         `json:"grab_ttl_seconds" binding:"required,min=5,max=300"`
	CandidateLimit           uint         `json:"candidate_limit" binding:"required,min=1,max=500"`
	OfferCandidateLimit      uint         `json:"offer_candidate_limit" binding:"required,min=1,max=20"`
	HeartbeatFreshSeconds    uint         `json:"heartbeat_fresh_seconds" binding:"required,min=15,max=300"`
	LocationFreshSeconds     uint         `json:"location_fresh_seconds" binding:"required,min=30,max=600"`
	MaxLocationAccuracyM     uint         `json:"max_location_accuracy_m" binding:"required,min=20,max=1000"`
	MaxPickupDistanceM       uint         `json:"max_pickup_distance_m" binding:"required,min=500,max=50000"`
	MaxActiveOrdersDefault   uint8        `json:"max_active_orders_default" binding:"required,min=1,max=20"`
	IdleFullScoreSeconds     uint         `json:"idle_full_score_seconds" binding:"required,min=60,max=86400"`
	ScoreWeights             ScoreWeights `json:"score_weights" binding:"required"`
	RejectionCooldownSeconds uint         `json:"rejection_cooldown_seconds" binding:"max=3600"`
}

type VersionReq struct {
	ExpectedVersion uint `json:"expected_version" binding:"required,min=1"`
}

type JobRetryReq struct {
	ExpectedVersion uint   `json:"expected_version" binding:"required,min=1"`
	ReasonCode      string `json:"reason_code" binding:"required,max=64"`
	Reason          string `json:"reason" binding:"required,min=2,max=255"`
}

type WorkStatusReq struct {
	Status          string `json:"status" binding:"required,oneof=online offline"`
	ExpectedVersion uint   `json:"expected_version" binding:"required,min=1"`
	ReasonCode      string `json:"reason_code" binding:"max=64"`
}

type WorkStatusDTO struct {
	RiderID             string `json:"rider_id"`
	Status              string `json:"status"`
	Version             uint   `json:"version"`
	HasActiveDeliveries bool   `json:"has_active_deliveries"`
}

type HeartbeatReq struct {
	DeviceID         string   `json:"device_id" binding:"required,min=8,max=128"`
	Sequence         uint64   `json:"sequence" binding:"required,min=1"`
	CapturedAt       string   `json:"captured_at" binding:"required"`
	Latitude         *float64 `json:"latitude" binding:"required,gte=-90,lte=90"`
	Longitude        *float64 `json:"longitude" binding:"required,gte=-180,lte=180"`
	CoordinateSystem string   `json:"coordinate_system" binding:"required,oneof=gcj02 wgs84 bd09"`
	AccuracyM        *float64 `json:"accuracy_m" binding:"required,gte=0,lte=1000"`
	AppVersion       string   `json:"app_version" binding:"max=32"`
}

type HeartbeatDTO struct {
	ServerTime        string `json:"server_time"`
	AcceptedSequence  uint64 `json:"accepted_sequence"`
	PresenceExpiresAt string `json:"presence_expires_at"`
	Persisted         bool   `json:"persisted"`
	CoordinateSystem  string `json:"coordinate_system"`
}

type OfferActionReq struct {
	ExpectedOfferVersion      uint `json:"expected_offer_version" binding:"required,min=1"`
	ExpectedAssignmentVersion uint `json:"expected_assignment_version" binding:"required,min=1"`
}

type OfferRejectReq struct {
	ExpectedOfferVersion uint   `json:"expected_offer_version" binding:"required,min=1"`
	ReasonCode           string `json:"reason_code" binding:"required,oneof=busy too_far ending_shift other"`
	Remark               string `json:"remark" binding:"max=255"`
}

type GrabReq struct {
	ExpectedAssignmentVersion uint `json:"expected_assignment_version" binding:"required,min=1"`
}

type OfferDTO struct {
	ID                  string `json:"offer_id"`
	DeliveryOrderID     string `json:"delivery_order_id"`
	AssignmentVersion   uint   `json:"assignment_version"`
	ShopID              string `json:"shop_id"`
	ShopName            string `json:"shop_name"`
	DestinationDistrict string `json:"destination_district,omitempty"`
	DistanceM           uint   `json:"distance_m,omitempty"`
	ItemCount           int    `json:"item_count"`
	ExpiresAt           string `json:"expires_at"`
	Version             uint   `json:"version"`
	SoundKey            string `json:"sound_key"`
	PickupReadyStatus   string `json:"pickup_ready_status"`
}

type AssignmentResult struct {
	DeliveryOrderID   string `json:"delivery_order_id"`
	OrderID           string `json:"order_id"`
	ShopID            string `json:"shop_id"`
	RiderID           string `json:"rider_id"`
	Status            string `json:"status"`
	DispatchStatus    string `json:"dispatch_status"`
	AssignmentVersion uint   `json:"assignment_version"`
	PickupReadyStatus string `json:"pickup_ready_status"`
	PickupReadyAt     string `json:"pickup_ready_at,omitempty"`
	AcceptedAt        string `json:"accepted_at"`
}

type PolicyDTO struct {
	ID          string         `json:"id"`
	PolicyCode  string         `json:"policy_code"`
	ScopeType   string         `json:"scope_type"`
	ScopeID     string         `json:"scope_id"`
	Version     uint           `json:"version"`
	Mode        string         `json:"mode"`
	Status      string         `json:"status"`
	Snapshot    PolicySnapshot `json:"snapshot"`
	RowVersion  uint           `json:"row_version"`
	PublishedAt string         `json:"published_at,omitempty"`
	CreatedAt   string         `json:"created_at"`
	UpdatedAt   string         `json:"updated_at"`
}

type JobDTO struct {
	ID              string `json:"id"`
	JobNo           string `json:"job_no"`
	DeliveryOrderID string `json:"delivery_order_id"`
	OrderID         string `json:"order_id"`
	ShopID          string `json:"shop_id"`
	DispatchSeq     uint   `json:"dispatch_seq"`
	PolicyVersion   string `json:"policy_version"`
	Mode            string `json:"mode"`
	Status          string `json:"status"`
	RoundNo         uint8  `json:"round_no"`
	CandidateCursor uint   `json:"candidate_cursor"`
	NextActionAt    string `json:"next_action_at"`
	GrabExpiresAt   string `json:"grab_expires_at,omitempty"`
	AssignedRiderID string `json:"assigned_rider_id,omitempty"`
	Version         uint   `json:"version"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

type JobDetailDTO struct {
	Job         JobDTO                  `json:"job"`
	Candidates  []CandidateDTO          `json:"candidates"`
	Offers      []OfferTimelineDTO      `json:"offers"`
	Assignments []AssignmentTimelineDTO `json:"assignments"`
}

type CandidateDTO struct {
	ID                  string    `json:"id"`
	RiderID             string    `json:"rider_id"`
	RankNo              uint      `json:"rank_no"`
	Eligible            bool      `json:"eligible"`
	ExclusionCodes      []string  `json:"exclusion_codes"`
	DistanceM           *uint     `json:"distance_m,omitempty"`
	ActiveOrders        uint8     `json:"active_orders"`
	MaxActiveOrders     uint8     `json:"max_active_orders"`
	HeartbeatAgeSeconds *uint     `json:"heartbeat_age_seconds,omitempty"`
	LocationAgeSeconds  *uint     `json:"location_age_seconds,omitempty"`
	DistanceScore       *float64  `json:"distance_score,omitempty"`
	LoadScore           *float64  `json:"load_score,omitempty"`
	IdleScore           *float64  `json:"idle_score,omitempty"`
	FreshnessScore      *float64  `json:"freshness_score,omitempty"`
	RejectionPenalty    *float64  `json:"rejection_penalty,omitempty"`
	FinalScore          *float64  `json:"final_score,omitempty"`
	ScoreVersion        string    `json:"score_version"`
	CreatedAt           time.Time `json:"created_at"`
}

type OfferTimelineDTO struct {
	ID              string `json:"id"`
	DeliveryOrderID string `json:"delivery_order_id"`
	RiderID         string `json:"rider_id"`
	RoundNo         uint8  `json:"round_no"`
	CandidateID     string `json:"candidate_id"`
	Status          string `json:"status"`
	ExpiresAt       string `json:"expires_at"`
	RespondedAt     string `json:"responded_at,omitempty"`
	Version         uint   `json:"version"`
}

type AssignmentTimelineDTO struct {
	ID              string `json:"id"`
	DeliveryOrderID string `json:"delivery_order_id"`
	DispatchJobID   string `json:"dispatch_job_id,omitempty"`
	OfferID         string `json:"offer_id,omitempty"`
	FromRiderID     string `json:"from_rider_id,omitempty"`
	ToRiderID       string `json:"to_rider_id"`
	AssignmentType  string `json:"assignment_type"`
	Status          string `json:"status"`
	ReasonCode      string `json:"reason_code,omitempty"`
	Reason          string `json:"reason,omitempty"`
	ActorType       string `json:"actor_type"`
	ActorID         string `json:"actor_id"`
	VersionBefore   uint   `json:"version_before"`
	VersionAfter    uint   `json:"version_after"`
	CreatedAt       string `json:"created_at"`
}
