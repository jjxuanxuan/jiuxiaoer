package routeplanning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxProviderResponseBytes = 1 << 20
	maxPolylineBytes         = 256 << 10
	maxStepPolylineBytes     = 64 << 10
	maxRouteSteps            = 200
)

type AmapProvider struct {
	baseURL *url.URL
	key     string
	client  *http.Client
}

func NewAmapProvider(baseURL, key string, timeout time.Duration) (*AmapProvider, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("invalid Amap base URL")
	}
	if key == "" {
		return nil, errors.New("Amap key is required")
	}
	return &AmapProvider{
		baseURL: parsed,
		key:     key,
		client: &http.Client{
			Timeout:       timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (p *AmapProvider) Plan(ctx context.Context, input ProviderRequest) (ProviderResult, error) {
	endpoint := "/v5/direction/electrobike"
	if input.Mode == "driving" {
		endpoint = "/v5/direction/driving"
	}
	target := *p.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + endpoint
	query := target.Query()
	query.Set("key", p.key)
	query.Set("origin", formatCoordinate(input.Origin))
	query.Set("destination", formatCoordinate(input.Destination))
	query.Set("show_fields", "cost,navi,polyline")
	query.Set("output", "json")
	if input.Mode == "driving" {
		strategy := input.Strategy
		if strategy == "" || strategy == "default" {
			strategy = "32"
		}
		query.Set("strategy", strategy)
	}
	target.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return ProviderResult{}, &ProviderError{Kind: ProviderFailure}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ProviderResult{}, &ProviderError{Kind: ProviderTimeout}
		}
		return ProviderResult{}, &ProviderError{Kind: ProviderFailure}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ProviderResult{}, &ProviderError{Kind: ProviderFailure, Code: strconv.Itoa(resp.StatusCode)}
	}
	limited := io.LimitReader(resp.Body, maxProviderResponseBytes+1)
	payload, err := io.ReadAll(limited)
	if err != nil || len(payload) > maxProviderResponseBytes {
		return ProviderResult{}, &ProviderError{Kind: ProviderFailure}
	}
	var decoded amapResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return ProviderResult{}, &ProviderError{Kind: ProviderFailure}
	}
	if decoded.Status != "1" || decoded.InfoCode != "10000" {
		kind := ProviderFailure
		if amapQuotaCode(decoded.InfoCode) {
			kind = ProviderQuota
		}
		return ProviderResult{}, &ProviderError{Kind: kind, Code: decoded.InfoCode}
	}
	if len(decoded.Route.Paths) == 0 {
		return ProviderResult{}, &ProviderError{Kind: ProviderInvalid}
	}
	path := decoded.Route.Paths[0]
	distance, err := parseNonNegative(string(path.Distance))
	if err != nil {
		return ProviderResult{}, &ProviderError{Kind: ProviderInvalid}
	}
	durationValue := string(path.Duration)
	if durationValue == "" {
		durationValue = string(path.Cost.Duration)
	}
	duration, err := parseNonNegative(durationValue)
	if err != nil {
		return ProviderResult{}, &ProviderError{Kind: ProviderInvalid}
	}
	if len(path.Steps) > maxRouteSteps {
		return ProviderResult{}, &ProviderError{Kind: ProviderInvalid}
	}
	steps := make([]RouteStep, 0, len(path.Steps))
	polylines := make([]string, 0, len(path.Steps))
	for _, step := range path.Steps {
		stepDistance, distanceErr := parseNonNegative(string(step.Distance))
		stepDuration, durationErr := parseOptionalNonNegative(string(step.Cost.Duration))
		if distanceErr != nil || durationErr != nil || len(step.Polyline) > maxStepPolylineBytes || len(step.Instruction) > 512 {
			return ProviderResult{}, &ProviderError{Kind: ProviderInvalid}
		}
		steps = append(steps, RouteStep{Instruction: step.Instruction, DistanceM: stepDistance, DurationSeconds: stepDuration, Polyline: step.Polyline})
		if step.Polyline != "" {
			polylines = append(polylines, step.Polyline)
		}
	}
	polyline := strings.Join(polylines, ";")
	if len(polyline) > maxPolylineBytes {
		return ProviderResult{}, &ProviderError{Kind: ProviderInvalid}
	}
	return ProviderResult{DistanceM: distance, DurationSeconds: duration, Polyline: polyline, Steps: steps, Provider: "amap"}, nil
}

type amapResponse struct {
	Status   string `json:"status"`
	InfoCode string `json:"infocode"`
	Route    struct {
		Paths []struct {
			Distance amapNumericText `json:"distance"`
			Duration amapNumericText `json:"duration"`
			Cost     struct {
				Duration amapNumericText `json:"duration"`
			} `json:"cost"`
			Steps []struct {
				Instruction string          `json:"instruction"`
				Distance    amapNumericText `json:"step_distance"`
				Polyline    string          `json:"polyline"`
				Cost        struct {
					Duration amapNumericText `json:"duration"`
				} `json:"cost"`
			} `json:"steps"`
		} `json:"paths"`
	} `json:"route"`
}

// amapNumericText 支持接受 JSON 数字和带引号的数字字符串。Amap 的
// v5路由模式不统一使用一种表示，因此数值范围
// 校验仍然保留在 parseNonNegative 中，在导线值标准化之后。
type amapNumericText string

func (value *amapNumericText) UnmarshalJSON(payload []byte) error {
	if len(payload) == 0 {
		return errors.New("empty Amap numeric value")
	}
	if payload[0] == '"' {
		var decoded string
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return err
		}
		*value = amapNumericText(decoded)
		return nil
	}
	if (payload[0] < '0' || payload[0] > '9') && payload[0] != '-' {
		return errors.New("invalid Amap numeric value")
	}
	*value = amapNumericText(string(payload))
	return nil
}

func parseNonNegative(value string) (uint64, error) {
	if value == "" {
		return 0, fmt.Errorf("missing numeric value")
	}
	return strconv.ParseUint(value, 10, 64)
}

func parseOptionalNonNegative(value string) (uint64, error) {
	if value == "" {
		return 0, nil
	}
	return parseNonNegative(value)
}

func amapQuotaCode(code string) bool {
	switch code {
	case "10003", "10004", "10019", "10020", "10021", "10044":
		return true
	default:
		return false
	}
}
