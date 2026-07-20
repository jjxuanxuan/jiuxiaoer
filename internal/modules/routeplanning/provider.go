package routeplanning

import (
	"context"
	"errors"
	"math"
	"strconv"
)

type Provider interface {
	Plan(context.Context, ProviderRequest) (ProviderResult, error)
}

type ProviderErrorKind string

const (
	ProviderTimeout ProviderErrorKind = "timeout"
	ProviderQuota   ProviderErrorKind = "quota"
	ProviderInvalid ProviderErrorKind = "invalid"
	ProviderFailure ProviderErrorKind = "failure"
)

type ProviderError struct {
	Kind ProviderErrorKind
	Code string
}

func (e *ProviderError) Error() string { return "map route provider error: " + string(e.Kind) }

func IsProviderError(err error, kind ProviderErrorKind) bool {
	var target *ProviderError
	return errors.As(err, &target) && target.Kind == kind
}

type FakeProvider struct{}

func NewFakeProvider() *FakeProvider { return &FakeProvider{} }

type UnavailableProvider struct{}

func (*UnavailableProvider) Plan(context.Context, ProviderRequest) (ProviderResult, error) {
	return ProviderResult{}, &ProviderError{Kind: ProviderFailure}
}

func (*FakeProvider) Plan(_ context.Context, req ProviderRequest) (ProviderResult, error) {
	distance := haversineMeters(req.Origin, req.Destination)
	duration := uint64(math.Ceil(float64(distance) / 5.5))
	polyline := formatCoordinate(req.Origin) + ";" + formatCoordinate(req.Destination)
	return ProviderResult{
		DistanceM: distance, DurationSeconds: duration, Polyline: polyline, Provider: "fake",
		Steps: []RouteStep{{Instruction: "前往目的地", DistanceM: distance, DurationSeconds: duration, Polyline: polyline}},
	}, nil
}

func haversineMeters(a, b Coordinate) uint64 {
	const radius = 6371000.0
	lat1, lat2 := a.Latitude*math.Pi/180, b.Latitude*math.Pi/180
	dLat := (b.Latitude - a.Latitude) * math.Pi / 180
	dLng := (b.Longitude - a.Longitude) * math.Pi / 180
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return uint64(math.Round(radius * 2 * math.Atan2(math.Sqrt(h), math.Sqrt(1-h))))
}

func formatCoordinate(value Coordinate) string {
	return strconv.FormatFloat(value.Longitude, 'f', 6, 64) + "," + strconv.FormatFloat(value.Latitude, 'f', 6, 64)
}
