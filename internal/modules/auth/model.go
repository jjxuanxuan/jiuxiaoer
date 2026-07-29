package auth

import (
	"time"

	"gorm.io/datatypes"
)

type Account struct {
	ID                 uint64
	AccountType        string
	Username           *string
	Phone              *string
	PasswordHash       *string
	Status             string
	LastLoginAt        *time.Time
	TokenInvalidBefore *time.Time
	CredentialVersion  uint
	CreatedAt          time.Time
	UpdatedAt          time.Time
	DeletedAt          *time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Account) TableName() string { return "accounts" }

type Customer struct {
	ID        uint64
	AccountID uint64
	Nickname  *string
	Phone     string
	Status    string
}

// TableName 返回当前数据模型对应的数据库表名。
func (Customer) TableName() string { return "customers" }

type CustomerIdentity struct {
	ID              uint64
	CustomerID      uint64
	Provider        string
	AppID           string `gorm:"column:app_id"`
	ProviderSubject string
	UnionSubject    *string
	Status          string
	LastLoginAt     *time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (CustomerIdentity) TableName() string { return "customer_identities" }

type AdminUser struct {
	ID           uint64
	AccountID    uint64
	RoleID       uint64
	AdminSubRole string
	Name         string
	Status       string
}

// TableName 返回当前数据模型对应的数据库表名。
func (AdminUser) TableName() string { return "admin_users" }

// AdminUserShop 是受限平台管理员的显式对象范围。
// 全局平台角色不会在此创建记录。
type AdminUserShop struct {
	ID          uint64
	AdminUserID uint64
	ShopID      uint64
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
	CreatedBy   *uint64
	UpdatedBy   *uint64
}

// TableName 返回当前数据模型对应的数据库表名。
func (AdminUserShop) TableName() string { return "admin_user_shops" }

type MerchantUser struct {
	ID         uint64
	AccountID  uint64
	MerchantID uint64
	RoleID     uint64
	Name       string
	Status     string
}

// TableName 返回当前数据模型对应的数据库表名。
func (MerchantUser) TableName() string { return "merchant_users" }

type Rider struct {
	ID         uint64
	AccountID  uint64
	Name       string
	Phone      *string
	Status     string
	WorkStatus string
}

// TableName 返回当前数据模型对应的数据库表名。
func (Rider) TableName() string { return "riders" }

type Cart struct {
	ID         uint64
	CustomerID uint64
}

// TableName 返回当前数据模型对应的数据库表名。
func (Cart) TableName() string { return "carts" }

type AuditLog struct {
	ID           uint64
	EventID      *string
	ActorType    string
	ActorID      uint64
	AccountID    *uint64
	Action       string
	ResourceType string
	ResourceID   uint64
	ShopID       *uint64
	OrderID      *uint64
	DeliveryID   *uint64
	BeforeData   datatypes.JSON
	AfterData    datatypes.JSON
	Result       string
	ErrorCode    *string
	ReasonCode   *string
	BeforeStatus *string
	AfterStatus  *string
	Version      *uint64
	RequestID    *string
	IP           *string
	IPHash       *string
	UserAgent    *string
}

// TableName 返回当前数据模型对应的数据库表名。
func (AuditLog) TableName() string { return "audit_logs" }
