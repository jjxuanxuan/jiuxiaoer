package coordinate

import (
	"errors"
	"math"
	"strings"
)

const (
	GCJ02 = "gcj02"
	WGS84 = "wgs84"
	BD09  = "bd09"
)

var ErrInvalid = errors.New("invalid coordinate")

const (
	chinaA  = 6378245.0
	chinaEE = 0.00669342162296594323
)

// Normalize validates a coordinate and converts it to the internal GCJ-02 contract.
func Normalize(latitude, longitude float64, system string) (float64, float64, error) {
	if !Valid(latitude, longitude) {
		return 0, 0, ErrInvalid
	}
	switch strings.ToLower(strings.TrimSpace(system)) {
	case GCJ02:
		return round7(latitude), round7(longitude), nil
	case WGS84:
		latitude, longitude = wgs84ToGCJ02(latitude, longitude)
		return round7(latitude), round7(longitude), nil
	case BD09:
		latitude, longitude = bd09ToGCJ02(latitude, longitude)
		return round7(latitude), round7(longitude), nil
	default:
		return 0, 0, ErrInvalid
	}
}

func Valid(latitude, longitude float64) bool {
	return !math.IsNaN(latitude) && !math.IsInf(latitude, 0) &&
		!math.IsNaN(longitude) && !math.IsInf(longitude, 0) &&
		latitude >= -90 && latitude <= 90 && longitude >= -180 && longitude <= 180
}

func wgs84ToGCJ02(latitude, longitude float64) (float64, float64) {
	if outsideChina(latitude, longitude) {
		return latitude, longitude
	}
	dLat := transformLat(longitude-105, latitude-35)
	dLng := transformLng(longitude-105, latitude-35)
	radLat := latitude / 180 * math.Pi
	magic := math.Sin(radLat)
	magic = 1 - chinaEE*magic*magic
	sqrtMagic := math.Sqrt(magic)
	dLat = (dLat * 180) / ((chinaA * (1 - chinaEE) / (magic * sqrtMagic)) * math.Pi)
	dLng = (dLng * 180) / (chinaA / sqrtMagic * math.Cos(radLat) * math.Pi)
	return latitude + dLat, longitude + dLng
}

func bd09ToGCJ02(latitude, longitude float64) (float64, float64) {
	x := longitude - 0.0065
	y := latitude - 0.006
	z := math.Sqrt(x*x+y*y) - 0.00002*math.Sin(y*math.Pi*3000/180)
	theta := math.Atan2(y, x) - 0.000003*math.Cos(x*math.Pi*3000/180)
	return z * math.Sin(theta), z * math.Cos(theta)
}

func outsideChina(latitude, longitude float64) bool {
	return longitude < 72.004 || longitude > 137.8347 || latitude < 0.8293 || latitude > 55.8271
}

func transformLat(x, y float64) float64 {
	value := -100 + 2*x + 3*y + 0.2*y*y + 0.1*x*y + 0.2*math.Sqrt(math.Abs(x))
	value += (20*math.Sin(6*x*math.Pi) + 20*math.Sin(2*x*math.Pi)) * 2 / 3
	value += (20*math.Sin(y*math.Pi) + 40*math.Sin(y/3*math.Pi)) * 2 / 3
	value += (160*math.Sin(y/12*math.Pi) + 320*math.Sin(y*math.Pi/30)) * 2 / 3
	return value
}

func transformLng(x, y float64) float64 {
	value := 300 + x + 2*y + 0.1*x*x + 0.1*x*y + 0.1*math.Sqrt(math.Abs(x))
	value += (20*math.Sin(6*x*math.Pi) + 20*math.Sin(2*x*math.Pi)) * 2 / 3
	value += (20*math.Sin(x*math.Pi) + 40*math.Sin(x/3*math.Pi)) * 2 / 3
	value += (150*math.Sin(x/12*math.Pi) + 300*math.Sin(x/30*math.Pi)) * 2 / 3
	return value
}

func round7(value float64) float64 { return math.Round(value*1e7) / 1e7 }
