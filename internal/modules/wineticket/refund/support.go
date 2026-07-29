package refund

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

const maxRefundWriteBodySize = 64 << 10

var shanghaiLocation = core.ShanghaiLocation

func idString(value uint64) string { return core.IDString(value) }

func formatShanghai(value time.Time) string { return core.FormatShanghai(value) }

func jsonData(value any) datatypes.JSON { return core.JSONData(value) }

func decodePolicyJSON(raw []byte, out any, required ...string) error {
	return core.DecodePolicyJSON(raw, out, required...)
}

func refundPolicySummary(policy core.RefundPolicy) string {
	return core.RefundPolicySummary(policy)
}

func validateBusinessNo(value, field string) error {
	return core.ValidateBusinessNo(strings.TrimSpace(value), field)
}

func customerIDWithPermission(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "customer" {
		return 0, problem.Unauthorized("AUTH_UNAUTHORIZED", "customer authentication required")
	}
	customerID, err := strconv.ParseUint(claims.CustomerID, 10, 64)
	if err != nil || customerID == 0 {
		return 0, problem.Unauthorized("AUTH_UNAUTHORIZED", "invalid customer identity")
	}
	for _, granted := range claims.Permissions {
		if granted == permission {
			return customerID, nil
		}
	}
	return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
}

func customerClaims(c *gin.Context) (*auth.Claims, bool) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims.AccountType != "customer" {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "customer authentication required"))
		return nil, false
	}
	return claims, true
}

func rejectUnknownPackageQuery(c *gin.Context, allowed ...string) error {
	allow := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allow[key] = struct{}{}
	}
	for key := range c.Request.URL.Query() {
		if _, ok := allow[key]; !ok {
			return problem.InvalidArgument("VALIDATION_INVALID_QUERY", "unknown query parameter: "+key)
		}
	}
	return nil
}

func decodeStrictJSON(c *gin.Context, out any) error {
	reader := http.MaxBytesReader(c.Writer, c.Request.Body, maxRefundWriteBodySize)
	payload, err := io.ReadAll(reader)
	if err != nil {
		return problem.InvalidArgument("VALIDATION_FAILED", "request body is too large or unreadable")
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return problem.InvalidArgument("VALIDATION_FAILED", "request body must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return problem.InvalidArgument("VALIDATION_FAILED", safeJSONError(err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return problem.InvalidArgument("VALIDATION_FAILED", "request body must contain exactly one JSON object")
	}
	return nil
}

func safeJSONError(err error) string {
	message := err.Error()
	if strings.Contains(message, "unknown field") ||
		strings.Contains(message, "cannot unmarshal") ||
		strings.Contains(message, "invalid character") ||
		strings.Contains(message, "unexpected EOF") {
		return message
	}
	return "invalid JSON request body"
}

func stringPointer(value string) *string { return &value }

func timePtr(value time.Time) *time.Time { return &value }
