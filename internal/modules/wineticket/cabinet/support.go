package cabinet

import (
	"strconv"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func customerIDWithPermission(
	claims *auth.Claims,
	permission string,
) (uint64, error) {
	if claims == nil || claims.AccountType != "customer" {
		return 0, problem.Unauthorized(
			"AUTH_UNAUTHORIZED",
			"customer authentication required",
		)
	}
	customerID, err := strconv.ParseUint(claims.CustomerID, 10, 64)
	if err != nil || customerID == 0 {
		return 0, problem.Unauthorized(
			"AUTH_UNAUTHORIZED",
			"invalid customer identity",
		)
	}
	for _, granted := range claims.Permissions {
		if granted == permission {
			return customerID, nil
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

func stringPointer(value string) *string { return &value }
