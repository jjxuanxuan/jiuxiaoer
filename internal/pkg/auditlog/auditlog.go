package auditlog

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/securevalue"
)

const createCallbackName = "jxe:auditlog:enrich"

// Register installs the audit write invariant on a GORM handle. It covers both
// typed models and map-based writers, which prevents individual modules from
// silently omitting the phase-one structured audit fields.
func Register(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	if db.Callback().Create().Get(createCallbackName) != nil {
		return nil
	}
	return db.Callback().Create().Before("gorm:create").Register(createCallbackName, enrich)
}

func enrich(tx *gorm.DB) {
	if tx == nil || tx.Statement == nil || tableName(tx) != "audit_logs" {
		return
	}
	if batchSize(tx.Statement.Dest) > 1 {
		tx.AddError(fmt.Errorf("audit_logs batch create is forbidden: each action requires an independent audit event"))
		return
	}

	eventID := uuid.NewString()
	setIfEmpty(tx, "event_id", &eventID)
	setIfEmpty(tx, "request_id", requestctx.RequestIDPtr(tx.Statement.Context))
	if accountID := requestctx.AccountID(tx.Statement.Context); accountID != 0 {
		setColumn(tx, "account_id", &accountID)
	}

	ipHash := requestctx.IPHashPtr(tx.Statement.Context)
	if ipHash != nil {
		// The middleware-derived value is canonical and must override any value
		// supplied by a module or request body.
		setColumn(tx, "ip_hash", ipHash)
	} else if rawIP := stringValue(readColumn(tx, "ip")); rawIP != "" {
		hash := securevalue.Digest(rawIP)
		setColumn(tx, "ip_hash", &hash)
	} else if existing := stringValue(readColumn(tx, "ip_hash")); existing == "" || !validDigest(existing) {
		setColumn(tx, "ip_hash", nil)
	}
	setColumn(tx, "ip", nil)

	resourceType := stringValue(readColumn(tx, "resource_type"))
	resourceID := uint64Value(readColumn(tx, "resource_id"))
	before := jsonObject(readColumn(tx, "before_data"))
	after := jsonObject(readColumn(tx, "after_data"))

	shopID := findUint(after, before, "shop_id")
	orderID := findUint(after, before, "order_id")
	deliveryID := findUint(after, before, "delivery_id", "delivery_order_id")
	switch resourceType {
	case "shop":
		if shopID == 0 {
			shopID = resourceID
		}
	case "order":
		if orderID == 0 {
			orderID = resourceID
		}
	case "delivery_order", "delivery_verification":
		if deliveryID == 0 {
			deliveryID = resourceID
		}
	}
	setUintIfPresent(tx, "shop_id", shopID)
	setUintIfPresent(tx, "order_id", orderID)
	setUintIfPresent(tx, "delivery_id", deliveryID)

	setStringIfPresent(tx, "error_code", findString(after, before, "error_code"))
	setStringIfPresent(tx, "reason_code", findString(after, before, "reason_code"))
	beforeStatus := findString(before, nil, "status", "order_status", "delivery_status")
	if beforeStatus == "" {
		beforeStatus = findString(after, nil, "before_status", "previous_status")
	}
	setStringIfPresent(tx, "before_status", beforeStatus)
	setStringIfPresent(tx, "after_status", findString(after, nil, "after_status", "status", "order_status", "delivery_status"))
	version := findUint(after, before, "version", "assignment_version", "expected_version")
	setUintIfPresent(tx, "version", version)

	setSanitizedJSON(tx, "before_data", before)
	setSanitizedJSON(tx, "after_data", after)
}

func tableName(tx *gorm.DB) string {
	if tx.Statement.Table != "" {
		return tx.Statement.Table
	}
	if tx.Statement.Schema != nil {
		return tx.Statement.Schema.Table
	}
	return ""
}

func batchSize(dest any) int {
	value := reflect.ValueOf(dest)
	for value.IsValid() && (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) {
		if value.IsNil() {
			return 0
		}
		value = value.Elem()
	}
	if value.IsValid() && (value.Kind() == reflect.Slice || value.Kind() == reflect.Array) {
		return value.Len()
	}
	return 1
}

func supportsColumn(tx *gorm.DB, name string) bool {
	switch tx.Statement.Dest.(type) {
	case map[string]any, []map[string]any:
		return true
	}
	return tx.Statement.Schema != nil && tx.Statement.Schema.LookUpField(name) != nil
}

func setColumn(tx *gorm.DB, name string, value any) {
	if supportsColumn(tx, name) {
		tx.Statement.SetColumn(name, value, true)
	}
}

func setIfEmpty(tx *gorm.DB, name string, value any) {
	if value == nil || !isEmpty(readColumn(tx, name)) {
		return
	}
	setColumn(tx, name, value)
}

func setStringIfPresent(tx *gorm.DB, name, value string) {
	if value != "" {
		setIfEmpty(tx, name, &value)
	}
}

func setUintIfPresent(tx *gorm.DB, name string, value uint64) {
	if value != 0 {
		setIfEmpty(tx, name, &value)
	}
}

func readColumn(tx *gorm.DB, name string) any {
	if values, ok := tx.Statement.Dest.(map[string]any); ok {
		return values[name]
	}
	if tx.Statement.Schema == nil {
		return nil
	}
	field := tx.Statement.Schema.LookUpField(name)
	if field == nil {
		return nil
	}
	value := tx.Statement.ReflectValue
	for value.IsValid() && (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return nil
	}
	result, _ := field.ValueOf(tx.Statement.Context, value)
	return result
}

func isEmpty(value any) bool {
	if value == nil {
		return true
	}
	ref := reflect.ValueOf(value)
	for ref.IsValid() && (ref.Kind() == reflect.Pointer || ref.Kind() == reflect.Interface) {
		if ref.IsNil() {
			return true
		}
		ref = ref.Elem()
	}
	if !ref.IsValid() {
		return true
	}
	switch ref.Kind() {
	case reflect.String:
		return strings.TrimSpace(ref.String()) == ""
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return ref.Uint() == 0
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return ref.Int() == 0
	}
	return false
}

func jsonObject(value any) map[string]any {
	if value == nil {
		return nil
	}
	if pointer, ok := value.(*[]byte); ok && pointer != nil {
		value = *pointer
	}
	var raw []byte
	switch typed := value.(type) {
	case []byte:
		raw = typed
	case json.RawMessage:
		raw = typed
	case string:
		raw = []byte(typed)
	case *string:
		if typed != nil {
			raw = []byte(*typed)
		}
	case map[string]any:
		return typed
	default:
		raw, _ = json.Marshal(typed)
	}
	if len(raw) == 0 {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var result map[string]any
	if decoder.Decode(&result) != nil {
		return nil
	}
	return result
}

func setSanitizedJSON(tx *gorm.DB, column string, object map[string]any) {
	if object == nil {
		return
	}
	raw, err := json.Marshal(sanitizeObject(object))
	if err != nil {
		tx.AddError(fmt.Errorf("sanitize audit %s: %w", column, err))
		return
	}
	setColumn(tx, column, datatypes.JSON(raw))
}

func sanitizeObject(object map[string]any) map[string]any {
	result := make(map[string]any, len(object))
	for key, value := range object {
		if sensitiveAuditKey(key) {
			continue
		}
		switch typed := value.(type) {
		case map[string]any:
			result[key] = sanitizeObject(typed)
		case []any:
			items := make([]any, 0, len(typed))
			for _, item := range typed {
				if child, ok := item.(map[string]any); ok {
					items = append(items, sanitizeObject(child))
				} else {
					items = append(items, item)
				}
			}
			result[key] = items
		default:
			result[key] = value
		}
	}
	return result
}

func sensitiveAuditKey(key string) bool {
	normalized := canonicalKey(key)
	if normalized == "" {
		return false
	}
	// These are controlled enums/identifiers, not operator/provider free text.
	switch normalized {
	case "errorcode", "reasoncode", "failurecode", "policycode", "returnpolicycode", "cancellationreasoncode":
		return false
	}
	for _, fragment := range []string{
		"phone", "mobile", "email", "idcard", "identitynumber", "identityno",
		"contactname", "realname", "recipientname", "doorplate", "latitude", "longitude",
		"formattedaddress", "addressdetail", "addresssnapshot", "recipientsnapshot", "pickupsnapshot",
		"providersubject", "subjectreference", "licenseno", "clientip",
		"reason", "remark", "detail", "description", "message", "lasterror", "failuretext",
		"secret", "password", "privatekey", "apikey", "amapkey", "accesstoken", "refreshtoken", "authorization",
	} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	switch normalized {
	case "code", "pickupcode", "deliverycode", "verificationcode", "codehash", "codeciphertext", "codemask",
		"token", "ip", "address":
		return true
	default:
		return false
	}
}

func findString(primary, secondary map[string]any, keys ...string) string {
	for _, object := range []map[string]any{primary, secondary} {
		if value, ok := findValue(object, keys...); ok {
			if result := stringValue(value); result != "" {
				return result
			}
		}
	}
	return ""
}

func findUint(primary, secondary map[string]any, keys ...string) uint64 {
	for _, object := range []map[string]any{primary, secondary} {
		if value, ok := findValue(object, keys...); ok {
			if result := uint64Value(value); result != 0 {
				return result
			}
		}
	}
	return 0
}

func findValue(object map[string]any, keys ...string) (any, bool) {
	if len(object) == 0 {
		return nil, false
	}
	for _, key := range keys {
		if value, ok := object[key]; ok {
			return value, true
		}
	}
	wanted := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		wanted[canonicalKey(key)] = struct{}{}
	}
	for key, value := range object {
		if _, ok := wanted[canonicalKey(key)]; ok {
			return value, true
		}
	}
	for _, value := range object {
		switch nested := value.(type) {
		case map[string]any:
			if result, ok := findValue(nested, keys...); ok {
				return result, true
			}
		case []any:
			for _, item := range nested {
				if child, ok := item.(map[string]any); ok {
					if result, ok := findValue(child, keys...); ok {
						return result, true
					}
				}
			}
		}
	}
	return nil, false
}

func canonicalKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	replacer := strings.NewReplacer("_", "", "-", "", ".", "", " ", "")
	return replacer.Replace(value)
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case *string:
		if typed != nil {
			return strings.TrimSpace(*typed)
		}
	}
	return ""
}

func uint64Value(value any) uint64 {
	if value == nil {
		return 0
	}
	ref := reflect.ValueOf(value)
	for ref.IsValid() && (ref.Kind() == reflect.Pointer || ref.Kind() == reflect.Interface) {
		if ref.IsNil() {
			return 0
		}
		ref = ref.Elem()
	}
	if !ref.IsValid() {
		return 0
	}
	switch ref.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return ref.Uint()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if ref.Int() > 0 {
			return uint64(ref.Int())
		}
	case reflect.Float32, reflect.Float64:
		if ref.Float() > 0 {
			return uint64(ref.Float())
		}
	case reflect.String:
		result, _ := strconv.ParseUint(strings.TrimSpace(ref.String()), 10, 64)
		return result
	}
	if number, ok := value.(json.Number); ok {
		result, _ := strconv.ParseUint(number.String(), 10, 64)
		return result
	}
	return 0
}
