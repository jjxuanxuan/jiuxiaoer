package auth

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Repository struct {
	db *gorm.DB
}

var ErrPhoneAlreadyBound = errors.New("phone is already bound to another customer")

// NewRepository 创建并初始化数据仓储。
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

// DBConfigured 判断数据库是否已配置。
func (r *Repository) DBConfigured() bool {
	return r.db != nil
}

// FindAccountByUsername 按用户名查找账户。
func (r *Repository) FindAccountByUsername(ctx context.Context, accountType string, username string) (Account, error) {
	var account Account
	err := r.db.WithContext(ctx).
		Where("account_type = ? AND username = ? AND deleted_at IS NULL", accountType, username).
		First(&account).Error
	return account, err
}

// FindAccountByPhone 按账号类型和手机号查找账号。
func (r *Repository) FindAccountByPhone(ctx context.Context, accountType string, phone string) (Account, error) {
	var account Account
	err := r.db.WithContext(ctx).
		Where("account_type = ? AND phone = ? AND deleted_at IS NULL", accountType, phone).
		First(&account).Error
	return account, err
}

// FindAccountByID 按 ID 查找账户。
func (r *Repository) FindAccountByID(ctx context.Context, accountID uint64) (Account, error) {
	var account Account
	err := r.db.WithContext(ctx).
		Where("id = ? AND deleted_at IS NULL", accountID).
		First(&account).Error
	return account, err
}

// FindCustomerForSMSLogin 只查找已完成当前小程序微信首登和手机号绑定的顾客。
// 短信登录不能在这里创建账号；微信身份条件与支付侧保持一致。
func (r *Repository) FindCustomerForSMSLogin(ctx context.Context, phone string, appID string) (Account, Customer, error) {
	var customer Customer
	err := r.db.WithContext(ctx).
		Model(&Customer{}).
		Select("customers.*").
		Joins("JOIN accounts AS a ON a.id = customers.account_id AND a.deleted_at IS NULL").
		Where("customers.phone = ? AND customers.deleted_at IS NULL", phone).
		Where("a.account_type = 'customer' AND a.phone = ?", phone).
		Where(`EXISTS (
			SELECT 1
			FROM customer_identities AS ci
			WHERE ci.customer_id = customers.id
				AND ci.provider = 'wechat_miniapp'
				AND ci.app_id = ?
				AND ci.status = 'active'
				AND ci.deleted_at IS NULL
		)`, appID).
		First(&customer).Error
	if err != nil {
		return Account{}, Customer{}, err
	}

	var account Account
	err = r.db.WithContext(ctx).
		Where("id = ? AND account_type = 'customer' AND phone = ? AND deleted_at IS NULL", customer.AccountID, phone).
		First(&account).Error
	return account, customer, err
}

// FindOrCreateCustomerByIdentity 按身份查找或创建客户。
func (r *Repository) FindOrCreateCustomerByIdentity(ctx context.Context, result WeChatIdentityResult, nextID func() uint64) (Account, Customer, CustomerIdentity, error) {
	var account Account
	var customer Customer
	var identity CustomerIdentity
	now := time.Now()
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		foundAccount, foundCustomer, foundIdentity, err := findCustomerIdentity(ctx, tx, result.AppID, result.OpenID)
		if err == nil {
			account, customer, identity = foundAccount, foundCustomer, foundIdentity
			return tx.WithContext(ctx).Model(&CustomerIdentity{}).Where("id = ?", identity.ID).Update("last_login_at", &now).Error
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		account = Account{ID: nextID(), AccountType: "customer", Status: "active", CredentialVersion: 1}
		if err := tx.WithContext(ctx).Create(&account).Error; err != nil {
			return err
		}
		customer = Customer{ID: nextID(), AccountID: account.ID, Phone: "", Status: "active"}
		if err := tx.WithContext(ctx).Create(&customer).Error; err != nil {
			return err
		}
		identity = CustomerIdentity{
			ID:              nextID(),
			CustomerID:      customer.ID,
			Provider:        "wechat_miniapp",
			AppID:           result.AppID,
			ProviderSubject: result.OpenID,
			Status:          "active",
			LastLoginAt:     &now,
		}
		if result.UnionID != "" {
			identity.UnionSubject = &result.UnionID
		}
		if err := tx.WithContext(ctx).Create(&identity).Error; err != nil {
			return err
		}
		return tx.WithContext(ctx).Create(&Cart{ID: nextID(), CustomerID: customer.ID}).Error
	})
	if err == nil {
		return account, customer, identity, nil
	}

	// 并发首次登录由 uk_identity 收敛；唯一键冲突后读取胜出的 identity。
	account, customer, identity, lookupErr := findCustomerIdentity(ctx, r.db, result.AppID, result.OpenID)
	if lookupErr == nil {
		return account, customer, identity, nil
	}
	return Account{}, Customer{}, CustomerIdentity{}, err
}

// findCustomerIdentity 查找用户身份。
func findCustomerIdentity(ctx context.Context, db *gorm.DB, appID string, openID string) (Account, Customer, CustomerIdentity, error) {
	var identity CustomerIdentity
	err := db.WithContext(ctx).
		Where("provider = 'wechat_miniapp' AND app_id = ? AND provider_subject = ? AND deleted_at IS NULL", appID, openID).
		First(&identity).Error
	if err != nil {
		return Account{}, Customer{}, CustomerIdentity{}, err
	}
	var customer Customer
	if err := db.WithContext(ctx).Where("id = ? AND deleted_at IS NULL", identity.CustomerID).First(&customer).Error; err != nil {
		return Account{}, Customer{}, CustomerIdentity{}, err
	}
	var account Account
	if err := db.WithContext(ctx).Where("id = ? AND deleted_at IS NULL", customer.AccountID).First(&account).Error; err != nil {
		return Account{}, Customer{}, CustomerIdentity{}, err
	}
	return account, customer, identity, nil
}

// BindCustomerPhone 绑定用户手机号。
func (r *Repository) BindCustomerPhone(ctx context.Context, tx *gorm.DB, customerID uint64, phone string) (Customer, error) {
	var customer Customer
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND deleted_at IS NULL", customerID).First(&customer).Error; err != nil {
		return Customer{}, err
	}
	var conflict Account
	err := tx.WithContext(ctx).
		Where("account_type = 'customer' AND phone = ? AND id <> ? AND deleted_at IS NULL", phone, customer.AccountID).
		First(&conflict).Error
	if err == nil {
		return Customer{}, ErrPhoneAlreadyBound
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return Customer{}, err
	}
	if err := tx.WithContext(ctx).Model(&Account{}).Where("id = ?", customer.AccountID).Update("phone", phone).Error; err != nil {
		return Customer{}, err
	}
	if err := tx.WithContext(ctx).Model(&Customer{}).Where("id = ?", customer.ID).Update("phone", phone).Error; err != nil {
		return Customer{}, err
	}
	customer.Phone = phone
	return customer, nil
}

// TouchLastLogin 更新最近登录时间。
func (r *Repository) TouchLastLogin(ctx context.Context, accountID uint64) error {
	now := time.Now()
	return r.db.WithContext(ctx).Model(&Account{}).
		Where("id = ?", accountID).
		Updates(map[string]any{"last_login_at": &now}).Error
}

// AdminProfile 返回管理端资料。
func (r *Repository) AdminProfile(ctx context.Context, accountID uint64) (AdminUser, string, []string, error) {
	var admin AdminUser
	if err := r.db.WithContext(ctx).Where("account_id = ? AND deleted_at IS NULL", accountID).First(&admin).Error; err != nil {
		return AdminUser{}, "", nil, err
	}

	var roleCode string
	if err := r.db.WithContext(ctx).Table("roles").Select("code").Where("id = ?", admin.RoleID).Scan(&roleCode).Error; err != nil {
		return AdminUser{}, "", nil, err
	}

	perms, err := r.PermissionCodesByRole(ctx, admin.RoleID)
	if err != nil {
		return AdminUser{}, "", nil, err
	}
	return admin, roleCode, perms, nil
}

// PermissionCodesByRole 返回角色对应的权限代码。
func (r *Repository) PermissionCodesByRole(ctx context.Context, roleID uint64) ([]string, error) {
	var codes []string
	err := r.db.WithContext(ctx).
		Table("permissions p").
		Select("p.code").
		Joins("JOIN role_permissions rp ON rp.permission_id = p.id AND rp.deleted_at IS NULL").
		Where("rp.role_id = ? AND p.status = 'active' AND p.deleted_at IS NULL", roleID).
		Order("p.code").
		Scan(&codes).Error
	return codes, err
}

// MerchantProfile 返回商户资料、当前数据库角色及其权限。商家权限与门店
// 范围都必须在每次登录/刷新时从数据库重建，不能按 account_type 推导。
func (r *Repository) MerchantProfile(ctx context.Context, accountID uint64) (MerchantUser, string, []uint64, []string, error) {
	var merchantUser MerchantUser
	if err := r.db.WithContext(ctx).Where("account_id = ? AND deleted_at IS NULL", accountID).First(&merchantUser).Error; err != nil {
		return MerchantUser{}, "", nil, nil, err
	}

	var roleCode string
	if err := r.db.WithContext(ctx).
		Table("roles").
		Select("code").
		Where("id = ? AND scope = 'merchant' AND status = 'active' AND deleted_at IS NULL", merchantUser.RoleID).
		Scan(&roleCode).Error; err != nil {
		return MerchantUser{}, "", nil, nil, err
	}
	if roleCode == "" {
		return MerchantUser{}, "", nil, nil, gorm.ErrRecordNotFound
	}
	permissions, err := r.PermissionCodesByRole(ctx, merchantUser.RoleID)
	if err != nil {
		return MerchantUser{}, "", nil, nil, err
	}

	var shopIDs []uint64
	err = r.db.WithContext(ctx).
		Table("merchant_user_shops").
		Select("shop_id").
		Where("merchant_user_id = ? AND deleted_at IS NULL", merchantUser.ID).
		Order("shop_id").
		Scan(&shopIDs).Error
	return merchantUser, roleCode, shopIDs, permissions, err
}

// RiderProfile 返回骑手资料。
func (r *Repository) RiderProfile(ctx context.Context, accountID uint64) (Rider, error) {
	var rider Rider
	err := r.db.WithContext(ctx).Where("account_id = ? AND deleted_at IS NULL", accountID).First(&rider).Error
	return rider, err
}

// CustomerProfile 返回用户资料。
func (r *Repository) CustomerProfile(ctx context.Context, accountID uint64) (Customer, error) {
	var customer Customer
	err := r.db.WithContext(ctx).Where("account_id = ? AND deleted_at IS NULL", accountID).First(&customer).Error
	return customer, err
}

// CreateAuditLog 创建审计日志。
func (r *Repository) CreateAuditLog(ctx context.Context, row AuditLog) error {
	if r.db == nil {
		return nil
	}
	return r.db.WithContext(ctx).Create(&row).Error
}
