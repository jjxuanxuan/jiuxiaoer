package admin

import "encoding/json"

// 产品创建需求
type ProductCreateReq struct {
	CategoryID            string `json:"category_id" binding:"required"`
	Name                  string `json:"name" binding:"required,min=1,max=128"`
	BrandName             string `json:"brand_name" binding:"max=128"`
	Spec                  string `json:"spec" binding:"max=128"`
	ImageURL              string `json:"image_url" binding:"max=512"`
	Description           string `json:"description"`
	SalePriceAmount       int64  `json:"sale_price_amount" binding:"required,min=1"`
	OriginalPriceAmount   int64  `json:"original_price_amount" binding:"min=0"`
	Status                string `json:"status" binding:"omitempty,oneof=draft on_sale off_sale"`
	ReturnEligible        bool   `json:"return_eligible"`
	ReturnPolicyCode      string `json:"return_policy_code" binding:"omitempty,max=64"`
	ReturnPolicyVersion   string `json:"return_policy_version" binding:"omitempty,max=32"`
	SealedPackageRequired bool   `json:"sealed_package_required"`
	AgeRestricted         bool   `json:"age_restricted"`
}

// 产品更新需求
type ProductUpdateReq struct {
	CategoryID            *string `json:"category_id"`
	Name                  *string `json:"name" binding:"omitempty,min=1,max=128"`
	BrandName             *string `json:"brand_name" binding:"omitempty,max=128"`
	Spec                  *string `json:"spec" binding:"omitempty,max=128"`
	ImageURL              *string `json:"image_url" binding:"omitempty,max=512"`
	Description           *string `json:"description"`
	SalePriceAmount       *int64  `json:"sale_price_amount" binding:"omitempty,min=1"`
	OriginalPriceAmount   *int64  `json:"original_price_amount" binding:"omitempty,min=0"`
	Status                *string `json:"status" binding:"omitempty,oneof=draft on_sale off_sale"`
	ReturnEligible        *bool   `json:"return_eligible"`
	ReturnPolicyCode      *string `json:"return_policy_code" binding:"omitempty,max=64"`
	ReturnPolicyVersion   *string `json:"return_policy_version" binding:"omitempty,max=32"`
	SealedPackageRequired *bool   `json:"sealed_package_required"`
	AgeRestricted         *bool   `json:"age_restricted"`
}

// 产品数据
type ProductDTO struct {
	ID                    string `json:"id"`
	CategoryID            string `json:"category_id"`
	Name                  string `json:"name"`
	BrandName             string `json:"brand_name,omitempty"`
	Spec                  string `json:"spec,omitempty"`
	ImageURL              string `json:"image_url,omitempty"`
	Description           string `json:"description,omitempty"`
	SalePriceAmount       int64  `json:"sale_price_amount"`
	OriginalPriceAmount   int64  `json:"original_price_amount"`
	Status                string `json:"status"`
	ReturnEligible        bool   `json:"return_eligible"`
	ReturnPolicyCode      string `json:"return_policy_code"`
	ReturnPolicyVersion   string `json:"return_policy_version"`
	SealedPackageRequired bool   `json:"sealed_package_required"`
	AgeRestricted         bool   `json:"age_restricted"`
}

// 管理员命令数据
type AdminOrderDTO struct {
	ID                string         `json:"id"`
	OrderNo           string         `json:"order_no"`
	CustomerID        string         `json:"customer_id"`
	MerchantID        string         `json:"merchant_id"`
	ShopID            string         `json:"shop_id"`
	Status            string         `json:"status"`
	PayStatus         string         `json:"pay_status"`
	DeliveryStatus    string         `json:"delivery_status"`
	GoodsAmount       int64          `json:"goods_amount"`
	DiscountAmount    int64          `json:"discount_amount"`
	DeliveryFeeAmount int64          `json:"delivery_fee_amount"`
	PayableAmount     int64          `json:"payable_amount"`
	PaidAmount        int64          `json:"paid_amount"`
	Items             []OrderItemDTO `json:"items,omitempty"`
	CreatedAt         string         `json:"created_at,omitempty"`
}

// 订单项目数据
type OrderItemDTO struct {
	ID              string `json:"id"`
	ShopProductID   string `json:"shop_product_id"`
	ProductID       string `json:"product_id"`
	Name            string `json:"name"`
	Quantity        int    `json:"quantity"`
	SalePriceAmount int64  `json:"sale_price_amount"`
	TotalAmount     int64  `json:"total_amount"`
}

// 库存数据
type StockDTO struct {
	ID                string `json:"id"`
	ShopProductID     string `json:"shop_product_id"`
	ShopID            string `json:"shop_id"`
	MerchantID        string `json:"merchant_id"`
	ProductID         string `json:"product_id"`
	ProductName       string `json:"product_name"`
	AvailableQty      int    `json:"available_qty"`
	ReservedQty       int    `json:"reserved_qty"`
	LockedQty         int    `json:"locked_qty"`
	LowStockThreshold int    `json:"low_stock_threshold"`
	Version           int    `json:"version"`
}

// 库存调整要求
type StockAdjustReq struct {
	ShopProductID string `json:"shop_product_id" binding:"required"`
	QuantityDelta int    `json:"quantity_delta" binding:"required"`
	Reason        string `json:"reason" binding:"max=255"`
}

// 商家数据
type MerchantDTO struct {
	ID           string `json:"id"`
	Code         string `json:"code"`
	Name         string `json:"name"`
	ContactName  string `json:"contact_name,omitempty"`
	ContactPhone string `json:"contact_phone,omitempty"`
	LicenseNo    string `json:"license_no,omitempty"`
	Status       string `json:"status"`
	ReviewStatus string `json:"review_status"`
	ReviewRemark string `json:"review_remark,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
}

// 商家审核要求
type MerchantReviewReq struct {
	ReviewStatus string `json:"review_status" binding:"required,oneof=approved rejected"`
	ReviewRemark string `json:"review_remark" binding:"max=255"`
}

// 审计数据
type AuditLogDTO struct {
	ID           string          `json:"id"`
	EventID      string          `json:"event_id"`
	ActorType    string          `json:"actor_type"`
	ActorID      string          `json:"actor_id"`
	AccountID    string          `json:"account_id,omitempty"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id,omitempty"`
	ShopID       string          `json:"shop_id,omitempty"`
	OrderID      string          `json:"order_id,omitempty"`
	DeliveryID   string          `json:"delivery_id,omitempty"`
	BeforeData   json.RawMessage `json:"before_data,omitempty"`
	AfterData    json.RawMessage `json:"after_data,omitempty"`
	Result       string          `json:"result"`
	ErrorCode    string          `json:"error_code,omitempty"`
	ReasonCode   string          `json:"reason_code,omitempty"`
	BeforeStatus string          `json:"before_status,omitempty"`
	AfterStatus  string          `json:"after_status,omitempty"`
	Version      uint64          `json:"version,omitempty"`
	RequestID    string          `json:"request_id,omitempty"`
	IPHash       string          `json:"ip_hash,omitempty"`
	UserAgent    string          `json:"user_agent,omitempty"`
	CreatedAt    string          `json:"created_at"`
}
