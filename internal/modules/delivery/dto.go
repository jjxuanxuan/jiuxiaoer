package delivery

type DeliveryOrderDTO struct {
	ID                  string         `json:"id"`
	OrderID             string         `json:"order_id"`
	ShopID              string         `json:"shop_id"`
	RiderID             string         `json:"rider_id,omitempty"`
	Status              string         `json:"status"`
	AssignmentVersion   uint           `json:"assignment_version"`
	DispatchStatus      string         `json:"dispatch_status"`
	PickupReadyStatus   string         `json:"pickup_ready_status"`
	PickupReadyAt       string         `json:"pickup_ready_at,omitempty"`
	PickupSnapshot      map[string]any `json:"pickup_snapshot,omitempty"`
	RecipientSnapshot   map[string]any `json:"recipient_snapshot,omitempty"`
	CreatedAt           string         `json:"created_at,omitempty"`
	AcceptedAt          string         `json:"accepted_at,omitempty"`
	PickedUpAt          string         `json:"picked_up_at,omitempty"`
	StartedAt           string         `json:"started_at,omitempty"`
	CompletedAt         string         `json:"completed_at,omitempty"`
	ShopName            string         `json:"shop_name,omitempty"`
	DestinationDistrict string         `json:"destination_district,omitempty"`
	ItemCount           int            `json:"item_count,omitempty"`
	PickupDistanceM     uint           `json:"pickup_distance_m,omitempty"`
	GrabExpiresAt       string         `json:"grab_expires_at,omitempty"`
}
