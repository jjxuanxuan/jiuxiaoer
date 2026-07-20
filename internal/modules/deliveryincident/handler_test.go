package deliveryincident

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestBindIncidentRejectsUnknownInvalidAndMultipleJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, body := range map[string]string{
		"unknown field":   `{"expected_version":1,"evidence_tokens":["12345678901234567890"],"incident_id":"2"}`,
		"invalid version": `{"expected_version":0,"evidence_tokens":["12345678901234567890"]}`,
		"multiple values": `{"expected_version":1,"evidence_tokens":["12345678901234567890"]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			context, recorder := incidentBindingContext(body)
			var request AddEvidenceReq
			if bindIncident(context, &request) {
				t.Fatal("invalid request was accepted")
			}
			if recorder.Code != 400 {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestBindIncidentAcceptsValidRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, recorder := incidentBindingContext(`{"expected_version":2,"evidence_tokens":["12345678901234567890"]}`)
	var request AddEvidenceReq
	if !bindIncident(context, &request) {
		t.Fatalf("valid request rejected: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if request.ExpectedVersion != 2 || len(request.EvidenceTokens) != 1 {
		t.Fatalf("unexpected decoded request: %+v", request)
	}
}

func incidentBindingContext(body string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("POST", "/", strings.NewReader(body))
	context.Request.Header.Set("Content-Type", "application/json")
	return context, recorder
}
