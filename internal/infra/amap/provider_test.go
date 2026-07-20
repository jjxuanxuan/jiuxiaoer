package amap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReverseUsesWhitelistedFieldsAndIgnoresTelephoneCityCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/geocode/regeo" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		query := r.URL.Query()
		if query.Get("location") != "113.931000,22.541000" || query.Get("extensions") != "base" || query.Get("output") != "JSON" || query.Get("key") != "secret" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		if len(query) != 4 {
			t.Fatalf("unexpected query fields: %v", query)
		}
		_, _ = w.Write([]byte(`{"status":"1","infocode":"10000","regeocode":{"formatted_address":"广东省深圳市南山区","addressComponent":{"province":"广东省","city":"深圳市","citycode":"0755","district":"南山区","adcode":"440305","township":[],"towncode":"440305001000","streetNumber":{"street":"科技路","number":"1号"}}}}`))
	}))
	defer server.Close()
	provider := testClient(t, server.URL)
	location, err := provider.Reverse(context.Background(), Coordinate{Latitude: 22.541, Longitude: 113.931})
	if err != nil {
		t.Fatal(err)
	}
	if location.DistrictCode != "440305" || location.City != "深圳市" || location.Township != "" {
		t.Fatalf("location = %#v", location)
	}
}

func TestReverseSupportsMunicipalityEmptyCityAndRejectsInvalidAdcode(t *testing.T) {
	for _, test := range []struct {
		name    string
		city    string
		adcode  string
		wantErr bool
	}{
		{name: "empty_array", city: `[]`, adcode: `"110105"`},
		{name: "invalid_adcode", city: `"北京"`, adcode: `"010"`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"status":"1","infocode":"10000","regeocode":{"formatted_address":"北京市朝阳区","addressComponent":{"province":"北京市","city":` + test.city + `,"district":"朝阳区","adcode":` + test.adcode + `}}}`))
			}))
			defer server.Close()
			provider := testClient(t, server.URL)
			_, err := provider.Reverse(context.Background(), Coordinate{Latitude: 39.9, Longitude: 116.4})
			if (err != nil) != test.wantErr {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestEstimateRequestsCostOnlyAndParsesNumberOrString(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if r.URL.Path != "/v5/direction/electrobike" || query.Get("show_fields") != "cost" || query.Get("alternative_route") != "1" || len(query) != 6 {
			t.Fatalf("request = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"status":"1","infocode":"10000","count":1,"route":{"paths":[{"distance":"2150","cost":{"duration":510},"steps":[{"polyline":"must be ignored"}]}]}}`))
	}))
	defer server.Close()
	provider := testClient(t, server.URL)
	estimate, err := provider.Estimate(context.Background(), Coordinate{22.54, 113.93}, Coordinate{22.541, 113.931})
	if err != nil {
		t.Fatal(err)
	}
	if estimate.DistanceM != 2150 || estimate.DurationSeconds != 510 || estimate.SchemaFallback {
		t.Fatalf("estimate = %#v", estimate)
	}
}

func TestEstimateFallbackAndFailureClassification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"1","infocode":"10000","count":"1","route":{"paths":[{"distance":10,"duration":"4"}]}}`))
	}))
	defer server.Close()
	provider := testClient(t, server.URL)
	estimate, err := provider.Estimate(context.Background(), Coordinate{}, Coordinate{})
	if err != nil || !estimate.SchemaFallback || estimate.DurationSeconds != 4 {
		t.Fatalf("estimate=%#v err=%v", estimate, err)
	}

	quota := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"0","infocode":"10003"}`))
	}))
	defer quota.Close()
	provider = testClient(t, quota.URL)
	if _, err := provider.Estimate(context.Background(), Coordinate{}, Coordinate{}); !IsErrorKind(err, ErrorQuota) {
		t.Fatalf("err = %v", err)
	}
}

func TestProviderRejectsRedirectAndOversizedResponse(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	provider := testClient(t, redirect.URL)
	if _, err := provider.Reverse(context.Background(), Coordinate{}); !IsErrorKind(err, ErrorFailure) || redirected {
		t.Fatalf("err=%v redirected=%t", err, redirected)
	}

	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxResponseBytes+1)))
	}))
	defer large.Close()
	provider = testClient(t, large.URL)
	if _, err := provider.Reverse(context.Background(), Coordinate{}); !IsErrorKind(err, ErrorInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestProviderRejectsTrailingJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"1","infocode":"10000"}{"unexpected":true}`))
	}))
	defer server.Close()
	provider := testClient(t, server.URL)
	if _, err := provider.Reverse(context.Background(), Coordinate{}); !IsErrorKind(err, ErrorInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestProviderRejectsInvalidCoordinateBeforeHTTP(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	provider := testClient(t, server.URL)
	if _, err := provider.Reverse(context.Background(), Coordinate{Latitude: 91}); !IsErrorKind(err, ErrorInvalid) || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func testClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	provider, err := NewClient(baseURL, "secret", time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}
