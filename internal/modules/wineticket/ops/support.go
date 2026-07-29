package ops

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

const maxWriteBodyBytes = 64 << 10

var (
	packageCodePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	businessNoPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	shanghaiLocation   = core.ShanghaiLocation
)

func formatShanghai(value time.Time) string {
	return core.FormatShanghai(value)
}

func idString(value uint64) string { return core.IDString(value) }

func jsonData(value any) datatypes.JSON { return core.JSONData(value) }

func stringPointer(value string) *string { return &value }
func uint64Ptr(value uint64) *uint64     { return &value }
func timePtr(value time.Time) *time.Time { return &value }

func validateBusinessNo(value, field string) error {
	return core.ValidateBusinessNo(strings.TrimSpace(value), field)
}

func adminIDWithPermission(
	claims *auth.Claims,
	permission string,
) (uint64, error) {
	if claims == nil || claims.AccountType != "admin" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "admin account required")
	}
	actorID, err := strconv.ParseUint(claims.AdminUserID, 10, 64)
	if err != nil || actorID == 0 {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "invalid admin identity")
	}
	for _, granted := range claims.Permissions {
		if granted == permission {
			return actorID, nil
		}
	}
	return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
}

func parseExternalID(raw, field string) (uint64, error) {
	if len(raw) < 1 || len(raw) > 20 || raw[0] == '0' {
		return 0, invalidExternalID(field)
	}
	for _, char := range raw {
		if char < '0' || char > '9' {
			return 0, invalidExternalID(field)
		}
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || value == 0 {
		return 0, invalidExternalID(field)
	}
	return value, nil
}

func invalidExternalID(field string) error {
	return problem.InvalidArgument(
		"VALIDATION_FAILED",
		field+" must be a positive decimal string",
	)
}

func adminClaims(c *gin.Context) (*auth.Claims, bool) {
	c.Header("Cache-Control", "private, no-store")
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(
			c,
			problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"),
		)
		return nil, false
	}
	return claims, true
}

func adminWineTicketClaims(c *gin.Context) (*auth.Claims, bool) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(
			c,
			problem.Unauthorized(
				"AUTH_UNAUTHORIZED",
				"admin authentication required",
			),
		)
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
			return problem.InvalidArgument(
				"VALIDATION_INVALID_QUERY",
				"unknown query parameter: "+key,
			)
		}
	}
	return nil
}

func decodeStrictJSON(c *gin.Context, out any) error {
	reader := http.MaxBytesReader(c.Writer, c.Request.Body, maxWriteBodyBytes)
	payload, err := io.ReadAll(reader)
	if err != nil {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"request body is too large or unreadable",
		)
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"request body must be a JSON object",
		)
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			safeJSONError(err),
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"request body must contain exactly one JSON object",
		)
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

func redemptionSlotWindow(
	serviceDate time.Time,
	startClock string,
	endClock string,
) (time.Time, time.Time, error) {
	return redemption.SlotWindow(serviceDate, startClock, endClock)
}

func normalizeRedemptionClock(value string) (string, error) {
	return redemption.NormalizeClock(value)
}

func sameMillisecond(left, right time.Time) bool {
	return redemption.SameMillisecond(left, right)
}

func validateLockedRedemptionSlot(
	slot redemption.DeliveryTimeSlot,
	startAt time.Time,
	endAt time.Time,
	now time.Time,
) error {
	return redemption.ValidateLockedSlot(slot, startAt, endAt, now)
}
