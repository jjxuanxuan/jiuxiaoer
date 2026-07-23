package provisioning

import (
	"time"

	"gorm.io/datatypes"
)

const (
	MerchantRoleOwner          = "merchant_owner"
	MerchantRoleOrderOperator  = "merchant_order_operator"
	MerchantRoleInventoryClerk = "merchant_inventory_clerk"
)

type Operation struct {
	ID                 uint64
	OperationNo        string
	OperationType      string
	IdempotencyKeyHash string
	RequestHash        string
	ActorID            uint64
	Status             string
	TargetType         *string
	TargetID           *uint64
	StepState          datatypes.JSON
	FailureCode        *string
	StartedAt          time.Time
	FinishedAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Operation) TableName() string { return "provisioning_operations" }

type MerchantProvisionReq struct {
	Merchant         MerchantInput `json:"merchant" binding:"required"`
	Shop             ShopInput     `json:"shop" binding:"required"`
	Account          AccountInput  `json:"account" binding:"required"`
	MerchantUserName string        `json:"merchant_user_name" binding:"required,max=64"`
	RoleCode         string        `json:"role_code" binding:"omitempty,oneof=merchant_owner merchant_order_operator merchant_inventory_clerk"`
}
type MerchantInput struct {
	Code         string `json:"code" binding:"required,max=64"`
	Name         string `json:"name" binding:"required,max=128"`
	ContactName  string `json:"contact_name" binding:"max=64"`
	ContactPhone string `json:"contact_phone" binding:"max=32"`
	LicenseNo    string `json:"license_no" binding:"max=64"`
}
type ShopInput struct {
	Name             string   `json:"name" binding:"required,max=128"`
	Phone            string   `json:"phone" binding:"max=32"`
	Province         string   `json:"province" binding:"max=64"`
	City             string   `json:"city" binding:"required,max=64"`
	CityCode         string   `json:"city_code" binding:"max=32"`
	District         string   `json:"district" binding:"required,max=64"`
	Address          string   `json:"address" binding:"required,max=255"`
	Latitude         *float64 `json:"latitude"`
	Longitude        *float64 `json:"longitude"`
	CoordinateSystem string   `json:"coordinate_system"`
}
type AccountInput struct {
	Username string `json:"username" binding:"required,min=3,max=64"`
	Phone    string `json:"phone" binding:"max=32"`
}
type MerchantUserReq struct {
	Account  AccountInput `json:"account" binding:"required"`
	Name     string       `json:"name" binding:"required,max=64"`
	ShopIDs  []string     `json:"shop_ids" binding:"required,min=1"`
	RoleCode string       `json:"role_code" binding:"required,oneof=merchant_owner merchant_order_operator merchant_inventory_clerk"`
}
type ShopAuthorizationReq struct {
	ShopIDs []string `json:"shop_ids" binding:"required"`
}
type MerchantUserRoleReq struct {
	RoleCode string `json:"role_code" binding:"required,oneof=merchant_owner merchant_order_operator merchant_inventory_clerk"`
}
type AccountStatusReq struct {
	Status string `json:"status" binding:"required,oneof=active disabled"`
	Reason string `json:"reason" binding:"required,min=2,max=255"`
}
type ResetPasswordReq struct {
	Reason string `json:"reason" binding:"required,min=2,max=255"`
}
type RiderCreateReq struct {
	Name         string         `json:"name" binding:"required,max=64"`
	Phone        string         `json:"phone" binding:"required,max=32"`
	ServiceScope map[string]any `json:"service_scope" binding:"required"`
}
type RiderReviewReq struct {
	Decision string `json:"decision" binding:"required,oneof=approved rejected"`
	Reason   string `json:"reason" binding:"required,min=2,max=255"`
}
type RiderStatusReq struct {
	Status string `json:"status" binding:"required,oneof=active disabled"`
	Reason string `json:"reason" binding:"required,min=2,max=255"`
}
type OperationDTO struct {
	ID            string            `json:"id"`
	OperationNo   string            `json:"operation_no"`
	OperationType string            `json:"operation_type"`
	Status        string            `json:"status"`
	TargetType    string            `json:"target_type,omitempty"`
	TargetID      string            `json:"target_id,omitempty"`
	ResourceIDs   map[string]string `json:"resource_ids,omitempty"`
	StartedAt     string            `json:"started_at"`
	FinishedAt    string            `json:"finished_at,omitempty"`
}
type RiderDTO struct {
	ID           string         `json:"id"`
	AccountID    string         `json:"account_id"`
	Name         string         `json:"name"`
	Phone        string         `json:"phone"`
	Status       string         `json:"status"`
	ReviewStatus string         `json:"review_status"`
	ServiceScope map[string]any `json:"service_scope"`
}
