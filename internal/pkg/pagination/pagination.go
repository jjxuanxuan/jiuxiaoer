package pagination

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

var (
	allowedOrderFields = map[string]struct{}{
		"id":                {},
		"created_at":        {},
		"updated_at":        {},
		"sort_order":        {},
		"sale_price_amount": {},
		"available_qty":     {},
		"reserved_qty":      {},
		"locked_qty":        {},
		"status":            {},
		"pay_status":        {},
		"delivery_status":   {},
		"review_status":     {},
		"name":              {},
		"product_name":      {},
		"shop_id":           {},
		"merchant_id":       {},
		"customer_id":       {},
		"shop_product_id":   {},
		"scope_type":        {},
		"scope_id":          {},
		"mode":              {},
		"published_at":      {},
		"version":           {},
		"policy_version":    {},
		"next_action_at":    {},
		"assigned_at":       {},
		"dispatch_status":   {},
		"reported_at":       {},
		"incident_no":       {},
		"rider_id":          {},
		"type":              {},
		"stage":             {},
	}
	filterExprPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*(==|!=|>=|<=|>|<|:)[^,()]+$`)
)

type Query struct {
	PageSize  int
	PageToken string
	OrderBy   string
	Filter    string
	Offset    int
	TokenHash string
	Cursor    []string
}

type pageCursor struct {
	Version   uint8    `json:"version"`
	Offset    int      `json:"offset"`
	QueryHash string   `json:"query_hash"`
	After     []string `json:"after,omitempty"`
	IssuedAt  int64    `json:"issued_at"`
}

// FromGin 解析列表接口统一使用的分页参数。
func FromGin(c *gin.Context, scope ...string) (Query, error) {
	pageSize := DefaultPageSize
	if raw := c.Query("page_size"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 || value > MaxPageSize {
			return Query{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "page_size must be between 1 and 100")
		}
		pageSize = value
	}

	orderBy := c.Query("order_by")
	if err := validateOrderBy(orderBy); err != nil {
		return Query{}, err
	}
	filter := c.Query("filter")
	if err := validateFilter(filter); err != nil {
		return Query{}, err
	}

	tokenHash := queryFingerprint(c, scope)
	cursor, err := decodePageToken(c.Query("page_token"), tokenHash)
	if err != nil {
		return Query{}, err
	}
	return Query{
		PageSize:  pageSize,
		PageToken: c.Query("page_token"),
		OrderBy:   orderBy,
		Filter:    filter,
		Offset:    cursor.Offset,
		TokenHash: tokenHash,
		Cursor:    cursor.After,
	}, nil
}

// NextPageToken 返回绑定当前完整查询参数的分页游标。
func NextPageToken(query Query) string {
	return nextPageToken(query, nil)
}

// NextPageTokenWithCursor creates a signed keyset cursor. Repositories using
// this form must apply the matching cursor predicate instead of Offset.
func NextPageTokenWithCursor(query Query, after ...string) string {
	return nextPageToken(query, after)
}

func nextPageToken(query Query, after []string) string {
	payload, _ := json.Marshal(pageCursor{Version: 1, Offset: query.Offset + query.PageSize, QueryHash: query.TokenHash, After: after, IssuedAt: time.Now().Unix()})
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	signature := hmac.New(sha256.New, pageTokenSigningKey())
	_, _ = signature.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(signature.Sum(nil))
}

// queryFingerprint 查询指纹。
func queryFingerprint(c *gin.Context, scope []string) string {
	values := c.Request.URL.Query()
	values.Del("page_token")
	path := c.FullPath()
	if path == "" {
		path = c.Request.URL.Path
	}
	value := strings.Join([]string{c.Request.Method, path, values.Encode(), strings.Join(scope, "\x00")}, "\n")
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// decodePageToken 解码分页令牌。
func decodePageToken(raw string, expectedHash string) (pageCursor, error) {
	if raw == "" {
		return pageCursor{}, nil
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return pageCursor{}, invalidPageToken("invalid page_token")
	}
	wantMAC := hmac.New(sha256.New, pageTokenSigningKey())
	_, _ = wantMAC.Write([]byte(parts[0]))
	providedMAC, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(providedMAC, wantMAC.Sum(nil)) {
		return pageCursor{}, invalidPageToken("invalid page_token signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return pageCursor{}, invalidPageToken("invalid page_token")
	}
	var cursor pageCursor
	now := time.Now().Unix()
	if err := json.Unmarshal(payload, &cursor); err != nil || cursor.Version != 1 || cursor.Offset < 0 || cursor.QueryHash != expectedHash || cursor.IssuedAt > now+300 || cursor.IssuedAt < now-24*60*60 {
		return pageCursor{}, invalidPageToken("page_token does not match query or has expired")
	}
	return cursor, nil
}

func invalidPageToken(detail string) error {
	return problem.InvalidArgument("PAGE_TOKEN_INVALID", detail)
}

func pageTokenSigningKey() []byte {
	secret := os.Getenv("JXE_JWT_ACCESS_SECRET")
	if secret == "" {
		secret = "local_access_secret_change_me"
	}
	derived := sha256.Sum256([]byte("jiuxiaoer.pagination.v1\x00" + secret))
	return derived[:]
}

// ApplyTimeIDCursor applies a stable two-column keyset boundary. The token is
// generated from the final row's RFC3339Nano timestamp and unsigned ID.
func ApplyTimeIDCursor(db *gorm.DB, query Query, timeColumn, idColumn, direction string) (*gorm.DB, error) {
	if len(query.Cursor) == 0 {
		return db, nil
	}
	if len(query.Cursor) != 2 {
		return nil, invalidPageToken("invalid keyset cursor")
	}
	value, err := time.Parse(time.RFC3339Nano, query.Cursor[0])
	if err != nil {
		return nil, invalidPageToken("invalid cursor timestamp")
	}
	id, err := strconv.ParseUint(query.Cursor[1], 10, 64)
	if err != nil || id == 0 {
		return nil, invalidPageToken("invalid cursor id")
	}
	switch strings.ToLower(direction) {
	case "desc":
		return db.Where("("+timeColumn+" < ?) OR ("+timeColumn+" = ? AND "+idColumn+" < ?)", value, value, id), nil
	case "asc":
		return db.Where("("+timeColumn+" > ?) OR ("+timeColumn+" = ? AND "+idColumn+" > ?)", value, value, id), nil
	default:
		return nil, problem.Internal("invalid keyset direction")
	}
}

// ApplyIntIDCursor applies a stable numeric business-sort plus ID boundary.
func ApplyIntIDCursor(db *gorm.DB, query Query, valueColumn, idColumn, direction string) (*gorm.DB, error) {
	if len(query.Cursor) == 0 {
		return db, nil
	}
	if len(query.Cursor) != 2 {
		return nil, invalidPageToken("invalid keyset cursor")
	}
	value, err := strconv.ParseInt(query.Cursor[0], 10, 64)
	if err != nil {
		return nil, invalidPageToken("invalid cursor value")
	}
	id, err := strconv.ParseUint(query.Cursor[1], 10, 64)
	if err != nil || id == 0 {
		return nil, invalidPageToken("invalid cursor id")
	}
	switch strings.ToLower(direction) {
	case "desc":
		return db.Where("("+valueColumn+" < ?) OR ("+valueColumn+" = ? AND "+idColumn+" < ?)", value, value, id), nil
	case "asc":
		return db.Where("("+valueColumn+" > ?) OR ("+valueColumn+" = ? AND "+idColumn+" > ?)", value, value, id), nil
	default:
		return nil, problem.Internal("invalid keyset direction")
	}
}

// ApplyIDCursor applies a stable single-ID boundary for lists ordered only by
// their immutable primary key.
func ApplyIDCursor(db *gorm.DB, query Query, idColumn, direction string) (*gorm.DB, error) {
	if len(query.Cursor) == 0 {
		return db, nil
	}
	if len(query.Cursor) != 1 {
		return nil, invalidPageToken("invalid id cursor")
	}
	id, err := strconv.ParseUint(query.Cursor[0], 10, 64)
	if err != nil || id == 0 {
		return nil, invalidPageToken("invalid cursor id")
	}
	switch strings.ToLower(direction) {
	case "desc":
		return db.Where(idColumn+" < ?", id), nil
	case "asc":
		return db.Where(idColumn+" > ?", id), nil
	default:
		return nil, problem.Internal("invalid keyset direction")
	}
}

// OffsetDB preserves compatibility for custom sorts that have not yet been
// migrated to keyset pagination. Keyset-backed queries deliberately ignore
// the legacy offset embedded for old callers.
func OffsetDB(db *gorm.DB, query Query) *gorm.DB {
	if len(query.Cursor) != 0 {
		return db
	}
	return db.Offset(query.Offset)
}

// validateOrderBy 校验订单 By是否合法。
func validateOrderBy(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	for _, item := range strings.Split(raw, ",") {
		parts := strings.Fields(strings.TrimSpace(item))
		if len(parts) == 0 || len(parts) > 2 {
			return problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid order_by")
		}
		field := parts[0]
		if _, ok := allowedOrderFields[field]; !ok {
			return problem.InvalidArgument("VALIDATION_INVALID_QUERY", "order_by field is not allowed")
		}
		if len(parts) == 2 {
			direction := strings.ToLower(parts[1])
			if direction != "asc" && direction != "desc" {
				return problem.InvalidArgument("VALIDATION_INVALID_QUERY", "order_by direction must be asc or desc")
			}
		}
	}
	return nil
}

// validateFilter 校验筛选条件是否合法。
func validateFilter(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	for _, item := range strings.Split(raw, ",") {
		expr := strings.TrimSpace(item)
		if expr == "" || !filterExprPattern.MatchString(expr) {
			return problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid filter syntax")
		}
	}
	return nil
}

// ApplyFilter 使用调用方传入的字段白名单生成 where 条件。
// 字段到列名的映射必须由各 repository 显式提供，避免客户端参数直接进入 SQL。
func ApplyFilter(db *gorm.DB, raw string, columns map[string]string) (*gorm.DB, error) {
	if strings.TrimSpace(raw) == "" {
		return db, nil
	}
	specs, err := parseFilter(raw)
	if err != nil {
		return nil, err
	}
	for _, spec := range specs {
		column, ok := columns[spec.Field]
		if !ok {
			return nil, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "filter field is not allowed")
		}
		switch spec.Operator {
		case ":":
			db = db.Where(column+" LIKE ?", "%"+spec.Value+"%")
		case "==":
			db = db.Where(column+" = ?", spec.Value)
		case "!=":
			db = db.Where(column+" <> ?", spec.Value)
		case ">", ">=", "<", "<=":
			db = db.Where(column+" "+spec.Operator+" ?", spec.Value)
		default:
			return nil, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid filter operator")
		}
	}
	return db, nil
}

// ApplyOrder 使用字段白名单生成 order by；没有 order_by 时使用调用方的默认排序。
func ApplyOrder(db *gorm.DB, raw string, columns map[string]string, fallback string) (*gorm.DB, error) {
	if strings.TrimSpace(raw) == "" {
		if fallback == "" {
			return db, nil
		}
		return db.Order(fallback), nil
	}
	specs, err := parseOrder(raw)
	if err != nil {
		return nil, err
	}
	for _, spec := range specs {
		column, ok := columns[spec.Field]
		if !ok {
			return nil, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "order_by field is not allowed")
		}
		db = db.Order(column + " " + spec.Direction)
	}
	return db, nil
}

type orderSpec struct {
	Field     string
	Direction string
}

type filterSpec struct {
	Field    string
	Operator string
	Value    string
}

// parseOrder 解析订单。
func parseOrder(raw string) ([]orderSpec, error) {
	if err := validateOrderBy(raw); err != nil {
		return nil, err
	}
	items := strings.Split(raw, ",")
	specs := make([]orderSpec, 0, len(items))
	for _, item := range items {
		parts := strings.Fields(strings.TrimSpace(item))
		direction := "ASC"
		if len(parts) == 2 {
			direction = strings.ToUpper(parts[1])
		}
		specs = append(specs, orderSpec{Field: parts[0], Direction: direction})
	}
	return specs, nil
}

// parseFilter 解析筛选条件。
func parseFilter(raw string) ([]filterSpec, error) {
	if err := validateFilter(raw); err != nil {
		return nil, err
	}
	items := strings.Split(raw, ",")
	specs := make([]filterSpec, 0, len(items))
	for _, item := range items {
		spec, err := parseFilterExpr(strings.TrimSpace(item))
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// parseFilterExpr 解析筛选条件 Expr。
func parseFilterExpr(expr string) (filterSpec, error) {
	for _, op := range []string{"==", "!=", ">=", "<=", ">", "<", ":"} {
		if idx := strings.Index(expr, op); idx > 0 {
			field := strings.TrimSpace(expr[:idx])
			value := strings.Trim(strings.TrimSpace(expr[idx+len(op):]), `"'`)
			if field == "" || value == "" {
				return filterSpec{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid filter syntax")
			}
			return filterSpec{Field: field, Operator: op, Value: value}, nil
		}
	}
	return filterSpec{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid filter syntax")
}
