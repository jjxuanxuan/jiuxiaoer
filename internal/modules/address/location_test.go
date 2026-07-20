package address

import (
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/modules/customerlocation"
)

func TestFrontendFormattedAddressCannotMarkAddressVerified(t *testing.T) {
	req := AddressUpsertReq{
		CityCode: "440300", LocationSource: "map_pin", FormattedAddress: "frontend supplied",
		Latitude: float64Pointer(22.5), Longitude: float64Pointer(113.9), CoordinateSystem: "gcj02",
	}
	row := customerAddressFromCreate(1, 2, req, false, nil)
	if row.GeocodeStatus != "unverified" || row.GeocodeProvider != nil || row.GeocodedAt != nil {
		t.Fatalf("unverified address trusted frontend metadata: %+v", row)
	}

	geocodedAt := time.Date(2026, 7, 16, 8, 30, 0, 0, time.UTC)
	verified := &customerlocation.VerifiedAddress{GeocodedAt: geocodedAt}
	row = customerAddressFromCreate(1, 2, req, false, verified)
	if row.GeocodeStatus != "verified" || row.GeocodeProvider == nil || *row.GeocodeProvider != "amap" || row.GeocodedAt == nil || !row.GeocodedAt.Equal(geocodedAt) {
		t.Fatalf("backend verification metadata missing: %+v", row)
	}
}

func float64Pointer(value float64) *float64 { return &value }
