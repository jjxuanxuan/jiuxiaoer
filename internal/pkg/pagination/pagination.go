package pagination

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

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
}

type pageCursor struct {
	Offset    int    `json:"offset"`
	QueryHash string `json:"query_hash"`
}

// FromGin 解析列表接口统一使用的分页参数。
func FromGin(c *gin.Context) (Query, error) {
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

	tokenHash := queryFingerprint(c)
	offset, err := decodePageToken(c.Query("page_token"), tokenHash)
	if err != nil {
		return Query{}, err
	}
	return Query{
		PageSize:  pageSize,
		PageToken: c.Query("page_token"),
		OrderBy:   orderBy,
		Filter:    filter,
		Offset:    offset,
		TokenHash: tokenHash,
	}, nil
}

// NextPageToken 返回绑定当前完整查询参数的分页游标。
func NextPageToken(query Query) string {
	payload, _ := json.Marshal(pageCursor{Offset: query.Offset + query.PageSize, QueryHash: query.TokenHash})
	return base64.RawURLEncoding.EncodeToString(payload)
}

// queryFingerprint 查询指纹。
func queryFingerprint(c *gin.Context) string {
	values := c.Request.URL.Query()
	values.Del("page_token")
	sum := sha256.Sum256([]byte(values.Encode()))
	return hex.EncodeToString(sum[:])
}

// decodePageToken 解码分页令牌。
func decodePageToken(raw string, expectedHash string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid page_token")
	}
	var cursor pageCursor
	if err := json.Unmarshal(payload, &cursor); err != nil || cursor.Offset < 0 || cursor.QueryHash != expectedHash {
		return 0, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "page_token does not match query")
	}
	return cursor.Offset, nil
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
