package coordinate

import (
	"math"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name             string
		lat, lng         float64
		system           string
		wantLat, wantLng float64
	}{
		{name: "gcj02", lat: 39.90909, lng: 116.434307, system: GCJ02, wantLat: 39.90909, wantLng: 116.434307},
		{name: "wgs84_beijing", lat: 39.908823, lng: 116.397470, system: WGS84, wantLat: 39.9102265, wantLng: 116.4037136},
		{name: "bd09_beijing", lat: 39.9169795, lng: 116.4100865, system: BD09, wantLat: 39.9106400, wantLng: 116.4037144},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lat, lng, err := Normalize(tt.lat, tt.lng, tt.system)
			if err != nil {
				t.Fatalf("Normalize() error = %v", err)
			}
			if math.Abs(lat-tt.wantLat) > 0.00002 || math.Abs(lng-tt.wantLng) > 0.00002 {
				t.Fatalf("Normalize() = %.7f,%.7f want %.7f,%.7f", lat, lng, tt.wantLat, tt.wantLng)
			}
		})
	}
}

func TestNormalizeRejectsInvalidInput(t *testing.T) {
	for _, input := range []struct {
		lat, lng float64
		system   string
	}{
		{91, 1, GCJ02}, {1, 181, GCJ02}, {math.NaN(), 1, GCJ02}, {1, 1, ""}, {1, 1, "unknown"},
	} {
		if _, _, err := Normalize(input.lat, input.lng, input.system); err == nil {
			t.Fatalf("expected invalid coordinate for %#v", input)
		}
	}
}
