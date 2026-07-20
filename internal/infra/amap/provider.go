package amap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 512 << 10

var (
	adcodePattern   = regexp.MustCompile(`^[0-9]{6}$`)
	townCodePattern = regexp.MustCompile(`^[0-9]{12}$`)
)

type Coordinate struct {
	Latitude  float64
	Longitude float64
}

type AdministrativeLocation struct {
	Province         string `json:"province"`
	City             string `json:"city"`
	District         string `json:"district"`
	DistrictCode     string `json:"district_code"`
	Township         string `json:"township,omitempty"`
	TownCode         string `json:"town_code,omitempty"`
	Street           string `json:"street,omitempty"`
	StreetNumber     string `json:"street_number,omitempty"`
	FormattedAddress string `json:"formatted_address"`
}

type RouteEstimate struct {
	DistanceM       uint64
	DurationSeconds uint64
	SchemaFallback  bool
}

type ReverseGeocoder interface {
	Reverse(context.Context, Coordinate) (AdministrativeLocation, error)
}

type RouteEstimator interface {
	Estimate(context.Context, Coordinate, Coordinate) (RouteEstimate, error)
}

type Provider interface {
	ReverseGeocoder
	RouteEstimator
}

type ErrorKind string

const (
	ErrorTimeout  ErrorKind = "timeout"
	ErrorQuota    ErrorKind = "quota"
	ErrorInvalid  ErrorKind = "invalid"
	ErrorNoResult ErrorKind = "no_result"
	ErrorFailure  ErrorKind = "failure"
)

type ProviderError struct {
	Kind ErrorKind
	Code string
}

func (e *ProviderError) Error() string { return "amap provider error: " + string(e.Kind) }

func IsErrorKind(err error, kind ErrorKind) bool {
	var target *ProviderError
	return errors.As(err, &target) && target.Kind == kind
}

type Client struct {
	baseURL     *url.URL
	key         string
	regeoClient *http.Client
	routeClient *http.Client
}

func NewClient(baseURL, key string, regeoTimeout, routeTimeout time.Duration) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid Amap base URL")
	}
	if parsed.Scheme != "https" && !isLoopbackHost(parsed.Hostname()) {
		return nil, errors.New("Amap base URL must use HTTPS")
	}
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("Amap key is required")
	}
	if regeoTimeout <= 0 || routeTimeout <= 0 {
		return nil, errors.New("Amap timeouts must be positive")
	}
	newHTTPClient := func(timeout time.Duration) *http.Client {
		return &http.Client{
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Client{baseURL: parsed, key: key, regeoClient: newHTTPClient(regeoTimeout), routeClient: newHTTPClient(routeTimeout)}, nil
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func (c *Client) Reverse(ctx context.Context, point Coordinate) (AdministrativeLocation, error) {
	if !validCoordinate(point) {
		return AdministrativeLocation{}, &ProviderError{Kind: ErrorInvalid}
	}
	query := url.Values{}
	query.Set("key", c.key)
	query.Set("location", formatCoordinate(point))
	query.Set("extensions", "base")
	query.Set("output", "JSON")
	var payload regeoResponse
	if err := c.get(ctx, c.regeoClient, "/v3/geocode/regeo", query, &payload); err != nil {
		return AdministrativeLocation{}, err
	}
	if payload.Status != "1" || payload.InfoCode != "10000" {
		return AdministrativeLocation{}, classifyBusinessError(payload.InfoCode)
	}
	component := payload.Regeocode.AddressComponent
	formatted, err := boundedRequired(payload.Regeocode.FormattedAddress.String(), 255)
	if err != nil {
		return AdministrativeLocation{}, &ProviderError{Kind: ErrorInvalid}
	}
	province, ok := boundedOptional(component.Province.String(), 64)
	if !ok {
		return AdministrativeLocation{}, &ProviderError{Kind: ErrorInvalid}
	}
	city, ok := boundedOptional(component.City.String(), 64)
	if !ok {
		return AdministrativeLocation{}, &ProviderError{Kind: ErrorInvalid}
	}
	district, ok := boundedOptional(component.District.String(), 64)
	if !ok || !adcodePattern.MatchString(component.ADCode.String()) {
		return AdministrativeLocation{}, &ProviderError{Kind: ErrorInvalid}
	}
	township, ok := boundedOptional(component.Township.String(), 64)
	if !ok {
		return AdministrativeLocation{}, &ProviderError{Kind: ErrorInvalid}
	}
	townCode := component.TownCode.String()
	if townCode != "" && !townCodePattern.MatchString(townCode) {
		return AdministrativeLocation{}, &ProviderError{Kind: ErrorInvalid}
	}
	street, ok := boundedOptional(component.StreetNumber.Street.String(), 128)
	if !ok {
		return AdministrativeLocation{}, &ProviderError{Kind: ErrorInvalid}
	}
	number, ok := boundedOptional(component.StreetNumber.Number.String(), 64)
	if !ok {
		return AdministrativeLocation{}, &ProviderError{Kind: ErrorInvalid}
	}
	return AdministrativeLocation{
		Province: province, City: city, District: district, DistrictCode: component.ADCode.String(),
		Township: township, TownCode: townCode, Street: street, StreetNumber: number, FormattedAddress: formatted,
	}, nil
}

func (c *Client) Estimate(ctx context.Context, origin, destination Coordinate) (RouteEstimate, error) {
	if !validCoordinate(origin) || !validCoordinate(destination) {
		return RouteEstimate{}, &ProviderError{Kind: ErrorInvalid}
	}
	query := url.Values{}
	query.Set("key", c.key)
	query.Set("origin", formatCoordinate(origin))
	query.Set("destination", formatCoordinate(destination))
	query.Set("show_fields", "cost")
	query.Set("alternative_route", "1")
	query.Set("output", "json")
	var payload routeResponse
	if err := c.get(ctx, c.routeClient, "/v5/direction/electrobike", query, &payload); err != nil {
		return RouteEstimate{}, err
	}
	if payload.Status != "1" || payload.InfoCode != "10000" {
		return RouteEstimate{}, classifyBusinessError(payload.InfoCode)
	}
	count, err := payload.Count.Uint64()
	if err != nil || count < 1 || len(payload.Route.Paths) == 0 {
		return RouteEstimate{}, &ProviderError{Kind: ErrorNoResult}
	}
	path := payload.Route.Paths[0]
	distance, err := path.Distance.Uint64()
	if err != nil {
		return RouteEstimate{}, &ProviderError{Kind: ErrorInvalid}
	}
	duration, err := path.Cost.Duration.Uint64()
	fallback := false
	if err != nil {
		duration, err = path.Duration.Uint64()
		fallback = err == nil
	}
	if err != nil {
		return RouteEstimate{}, &ProviderError{Kind: ErrorInvalid}
	}
	return RouteEstimate{DistanceM: distance, DurationSeconds: duration, SchemaFallback: fallback}, nil
}

func (c *Client) get(ctx context.Context, client *http.Client, endpoint string, query url.Values, output any) error {
	target := *c.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + endpoint
	target.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return &ProviderError{Kind: ErrorFailure}
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &ProviderError{Kind: ErrorTimeout}
		}
		return &ProviderError{Kind: ErrorFailure}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &ProviderError{Kind: ErrorFailure, Code: strconv.Itoa(resp.StatusCode)}
	}
	limited := io.LimitReader(resp.Body, maxResponseBytes+1)
	content, err := io.ReadAll(limited)
	if err != nil || len(content) > maxResponseBytes {
		return &ProviderError{Kind: ErrorInvalid}
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	if err := decoder.Decode(output); err != nil {
		return &ProviderError{Kind: ErrorInvalid}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return &ProviderError{Kind: ErrorInvalid}
	}
	return nil
}

func formatCoordinate(point Coordinate) string {
	return strconv.FormatFloat(point.Longitude, 'f', 6, 64) + "," + strconv.FormatFloat(point.Latitude, 'f', 6, 64)
}

func validCoordinate(point Coordinate) bool {
	return !math.IsNaN(point.Latitude) && !math.IsInf(point.Latitude, 0) &&
		!math.IsNaN(point.Longitude) && !math.IsInf(point.Longitude, 0) &&
		point.Latitude >= -90 && point.Latitude <= 90 && point.Longitude >= -180 && point.Longitude <= 180
}

func classifyBusinessError(code string) error {
	if quotaCode(code) {
		return &ProviderError{Kind: ErrorQuota, Code: safeCode(code)}
	}
	return &ProviderError{Kind: ErrorFailure, Code: safeCode(code)}
}

func quotaCode(code string) bool {
	switch code {
	case "10003", "10004", "10019", "10020", "10021", "10044":
		return true
	default:
		return false
	}
}

func safeCode(code string) string {
	if len(code) > 32 {
		return ""
	}
	for _, r := range code {
		if (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && r != '_' && r != '-' {
			return ""
		}
	}
	return code
}

func boundedRequired(value string, max int) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len([]rune(value)) > max {
		return "", fmt.Errorf("invalid provider string")
	}
	return value, nil
}

func boundedOptional(value string, max int) (string, bool) {
	value = strings.TrimSpace(value)
	return value, len([]rune(value)) <= max
}

// flexibleText accepts the string/empty-array variants returned by Amap for
// direct-controlled municipalities. Non-empty arrays are rejected.
type flexibleText string

func (v *flexibleText) UnmarshalJSON(payload []byte) error {
	if string(payload) == "null" {
		*v = ""
		return nil
	}
	if len(payload) > 0 && payload[0] == '"' {
		var value string
		if err := json.Unmarshal(payload, &value); err != nil {
			return err
		}
		*v = flexibleText(value)
		return nil
	}
	var values []json.RawMessage
	if err := json.Unmarshal(payload, &values); err == nil && len(values) == 0 {
		*v = ""
		return nil
	}
	return errors.New("invalid Amap text field")
}

func (v flexibleText) String() string { return string(v) }

type numericText string

func (v *numericText) UnmarshalJSON(payload []byte) error {
	if len(payload) == 0 {
		return errors.New("empty numeric value")
	}
	if payload[0] == '"' {
		var value string
		if err := json.Unmarshal(payload, &value); err != nil {
			return err
		}
		*v = numericText(value)
		return nil
	}
	if (payload[0] < '0' || payload[0] > '9') && payload[0] != '-' {
		return errors.New("invalid numeric value")
	}
	*v = numericText(string(payload))
	return nil
}

func (v numericText) Uint64() (uint64, error) {
	if v == "" {
		return 0, errors.New("missing numeric value")
	}
	return strconv.ParseUint(string(v), 10, 64)
}

type regeoResponse struct {
	Status    string `json:"status"`
	InfoCode  string `json:"infocode"`
	Regeocode struct {
		FormattedAddress flexibleText `json:"formatted_address"`
		AddressComponent struct {
			Province     flexibleText `json:"province"`
			City         flexibleText `json:"city"`
			District     flexibleText `json:"district"`
			ADCode       flexibleText `json:"adcode"`
			Township     flexibleText `json:"township"`
			TownCode     flexibleText `json:"towncode"`
			StreetNumber struct {
				Street flexibleText `json:"street"`
				Number flexibleText `json:"number"`
			} `json:"streetNumber"`
		} `json:"addressComponent"`
	} `json:"regeocode"`
}

type routeResponse struct {
	Status   string      `json:"status"`
	InfoCode string      `json:"infocode"`
	Count    numericText `json:"count"`
	Route    struct {
		Paths []struct {
			Distance numericText `json:"distance"`
			Duration numericText `json:"duration"`
			Cost     struct {
				Duration numericText `json:"duration"`
			} `json:"cost"`
		} `json:"paths"`
	} `json:"route"`
}

// UnavailableProvider is used when the feature is disabled or misconfigured;
// it never exposes configuration values through its error.
type UnavailableProvider struct{}

func (*UnavailableProvider) Reverse(context.Context, Coordinate) (AdministrativeLocation, error) {
	return AdministrativeLocation{}, &ProviderError{Kind: ErrorFailure}
}

func (*UnavailableProvider) Estimate(context.Context, Coordinate, Coordinate) (RouteEstimate, error) {
	return RouteEstimate{}, &ProviderError{Kind: ErrorFailure}
}

// FakeProvider is deterministic local-test infrastructure. It must never be
// enabled in production; configuration validation enforces that boundary.
type FakeProvider struct{}

func NewFakeProvider() *FakeProvider { return &FakeProvider{} }

func (*FakeProvider) Reverse(_ context.Context, point Coordinate) (AdministrativeLocation, error) {
	if point.Latitude < 20 || point.Latitude > 24.8 || point.Longitude < 109 || point.Longitude > 117.5 {
		return AdministrativeLocation{}, &ProviderError{Kind: ErrorNoResult}
	}
	return AdministrativeLocation{
		Province: "广东省", City: "深圳市", District: "南山区", DistrictCode: "440305",
		FormattedAddress: "广东省深圳市南山区",
	}, nil
}

func (*FakeProvider) Estimate(_ context.Context, origin, destination Coordinate) (RouteEstimate, error) {
	distance := haversine(origin, destination)
	return RouteEstimate{DistanceM: distance, DurationSeconds: uint64(math.Ceil(float64(distance) / 5.5))}, nil
}

func haversine(a, b Coordinate) uint64 {
	const radius = 6371000.0
	lat1, lat2 := a.Latitude*math.Pi/180, b.Latitude*math.Pi/180
	dLat := (b.Latitude - a.Latitude) * math.Pi / 180
	dLng := (b.Longitude - a.Longitude) * math.Pi / 180
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return uint64(math.Round(radius * 2 * math.Atan2(math.Sqrt(h), math.Sqrt(1-h))))
}
