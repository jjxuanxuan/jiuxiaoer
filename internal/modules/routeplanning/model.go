package routeplanning

import "time"

type Coordinate struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

type RouteStep struct {
	Instruction     string `json:"instruction"`
	DistanceM       uint64 `json:"distance_m"`
	DurationSeconds uint64 `json:"duration_seconds"`
	Polyline        string `json:"polyline"`
}

type RoutePlan struct {
	DeliveryOrderID  string      `json:"delivery_order_id"`
	OrderID          string      `json:"order_id"`
	Stage            string      `json:"stage"`
	Mode             string      `json:"mode"`
	CoordinateSystem string      `json:"coordinate_system"`
	Origin           Coordinate  `json:"origin"`
	Destination      Coordinate  `json:"destination"`
	DistanceM        uint64      `json:"distance_m"`
	DurationSeconds  uint64      `json:"duration_seconds"`
	Polyline         string      `json:"polyline"`
	Steps            []RouteStep `json:"steps"`
	Source           string      `json:"source"`
	Degraded         bool        `json:"degraded"`
	PlannedAt        time.Time   `json:"planned_at"`
	ExpiresAt        time.Time   `json:"expires_at"`
	Provider         string      `json:"provider"`
}

type ProviderRequest struct {
	Origin      Coordinate
	Destination Coordinate
	Mode        string
	Strategy    string
}

type ProviderResult struct {
	DistanceM       uint64
	DurationSeconds uint64
	Polyline        string
	Steps           []RouteStep
	Provider        string
}
