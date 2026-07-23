package response

import (
	"encoding/json"
	"testing"
)

func TestPageBodyAlwaysCarriesNextPageToken(t *testing.T) {
	payload, err := json.Marshal(PageBody{Items: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	if value, exists := body["next_page_token"]; !exists || value != "" {
		t.Fatalf("terminal page must carry an empty next_page_token, got %s", payload)
	}
}
