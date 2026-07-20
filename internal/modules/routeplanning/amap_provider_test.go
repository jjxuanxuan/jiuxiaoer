package routeplanning

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestAmapProviderPlansElectricBicycleRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v5/direction/electrobike" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		query := r.URL.Query()
		if query.Get("origin") != "116.434307,39.909090" || query.Get("destination") != "116.434446,39.908160" {
			t.Fatalf("unexpected coordinate query: %s", r.URL.RawQuery)
		}
		if query.Get("key") != "test-key" || query.Get("show_fields") != "cost,navi,polyline" {
			t.Fatalf("missing provider query fields")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"1","infocode":"10000","route":{"paths":[{"distance":"321","duration":"65","steps":[{"instruction":"向南骑行","step_distance":321,"polyline":"116.434307,39.909090;116.434446,39.908160","cost":{"duration":"65"}}]}]}}`))
	}))
	defer server.Close()

	provider, err := NewAmapProvider(server.URL, "test-key", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Plan(context.Background(), ProviderRequest{
		Origin: Coordinate{Latitude: 39.90909, Longitude: 116.434307}, Destination: Coordinate{Latitude: 39.90816, Longitude: 116.434446}, Mode: "electric_bicycle",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != "amap" || result.DistanceM != 321 || result.DurationSeconds != 65 || len(result.Steps) != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestAmapProviderUsesDrivingEndpointAndDefaultStrategy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v5/direction/driving" || r.URL.Query().Get("strategy") != "32" {
			t.Fatalf("unexpected driving request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"status":"1","infocode":"10000","route":{"paths":[{"distance":"1","cost":{"duration":"1"},"steps":[]}]}}`))
	}))
	defer server.Close()
	provider, _ := NewAmapProvider(server.URL, "test-key", time.Second)
	result, err := provider.Plan(context.Background(), ProviderRequest{Origin: Coordinate{1, 1}, Destination: Coordinate{2, 2}, Mode: "driving", Strategy: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if result.DurationSeconds != 1 {
		t.Fatalf("driving cost.duration was not parsed: %#v", result)
	}
}

func TestAmapProviderClassifiesQuotaAndTimeout(t *testing.T) {
	t.Run("quota", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":"0","info":"DAILY_QUERY_OVER_LIMIT","infocode":"10003"}`))
		}))
		defer server.Close()
		provider, _ := NewAmapProvider(server.URL, "test-key", time.Second)
		_, err := provider.Plan(context.Background(), ProviderRequest{Origin: Coordinate{1, 1}, Destination: Coordinate{2, 2}})
		if !IsProviderError(err, ProviderQuota) {
			t.Fatalf("expected quota error, got %v", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(100 * time.Millisecond)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer server.Close()
		provider, _ := NewAmapProvider(server.URL, "test-key", 20*time.Millisecond)
		_, err := provider.Plan(context.Background(), ProviderRequest{Origin: Coordinate{1, 1}, Destination: Coordinate{2, 2}})
		if !IsProviderError(err, ProviderTimeout) {
			t.Fatalf("expected timeout error, got %v", err)
		}
	})
}

func TestAmapProviderRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxProviderResponseBytes+1)))
	}))
	defer server.Close()
	provider, _ := NewAmapProvider(server.URL, "test-key", time.Second)
	if _, err := provider.Plan(context.Background(), ProviderRequest{Origin: Coordinate{1, 1}, Destination: Coordinate{2, 2}}); !IsProviderError(err, ProviderFailure) {
		t.Fatalf("expected bounded response failure, got %v", err)
	}
}

func TestAmapProviderRejectsMalformedAndInvalidResponses(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		kind       ProviderErrorKind
	}{
		{name: "http_400", statusCode: http.StatusBadRequest, body: `{}`, kind: ProviderFailure},
		{name: "http_503", statusCode: http.StatusServiceUnavailable, body: `{}`, kind: ProviderFailure},
		{name: "malformed_json", statusCode: http.StatusOK, body: `{`, kind: ProviderFailure},
		{name: "unknown_business_error", statusCode: http.StatusOK, body: `{"status":"0","infocode":"19999"}`, kind: ProviderFailure},
		{name: "empty_paths", statusCode: http.StatusOK, body: `{"status":"1","infocode":"10000","route":{"paths":[]}}`, kind: ProviderInvalid},
		{name: "invalid_distance", statusCode: http.StatusOK, body: `{"status":"1","infocode":"10000","route":{"paths":[{"distance":"NaN","cost":{"duration":"1"},"steps":[]}]}}`, kind: ProviderInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			provider, _ := NewAmapProvider(server.URL, "test-key", time.Second)
			_, err := provider.Plan(context.Background(), ProviderRequest{Origin: Coordinate{1, 1}, Destination: Coordinate{2, 2}})
			if !IsProviderError(err, test.kind) {
				t.Fatalf("error = %v, want kind %s", err, test.kind)
			}
		})
	}
}

func TestAmapProviderDoesNotFollowRedirects(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer server.Close()
	provider, _ := NewAmapProvider(server.URL, "test-key", time.Second)
	_, err := provider.Plan(context.Background(), ProviderRequest{Origin: Coordinate{1, 1}, Destination: Coordinate{2, 2}})
	if !IsProviderError(err, ProviderFailure) || redirected {
		t.Fatalf("redirect handling: err=%v redirected=%t", err, redirected)
	}
}

func TestProviderProblemMapsHTTP5xxToServiceUnavailable(t *testing.T) {
	details := problem.FromError(providerProblem(&ProviderError{Kind: ProviderFailure, Code: "503"}))
	if details.Status != http.StatusServiceUnavailable || details.ErrorCode != "ROUTE_PROVIDER_ERROR" {
		t.Fatalf("details = %#v", details)
	}
}
