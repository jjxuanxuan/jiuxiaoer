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

// DBConfigured 判断数据库 Configured。
func (r *Repository) DBConfigured() bool {
	return r.db != nil
}

// FindAccountByUsername 查找账户 By Username。
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

// FindAccountByID 查找账户 By ID。
func (r *Repository) FindAccountByID(ctx context.Context, accountID uint64) (Account, error) {
	var account Account
	err := r.db.WithContext(ctx).
		Where("id = ? AND deleted_at IS NULL", accountID).
		First(&account).Error
	return account, err
}

// FindOrCreateCustomerByPhone 查找Or Create 用户 By 手机号。
func (r *Repository) FindOrCreateCustomerByPhone(ctx context.Context, phone string, nextID func() uint64) (Account, Customer, error) {
	var account Account
	var customer Customer

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Where("account_type = ? AND phone = ? AND deleted_at IS NULL", "customer", phone).First(&account).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			phoneCopy := phone
			account = Account{
				ID:                nextID(),
				AccountType:       "customer",
				Phone:             &phoneCopy,
				Status:            "active",
				CredentialVersion: 1,
			}
			if err := tx.Create(&account).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}

		err = tx.Where("account_id = ? AND deleted_at IS NULL", account.ID).First(&customer).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			customer = Customer{
				ID:        nextID(),
				AccountID: account.ID,
				Phone:     phone,
				Status:    "active",
			}
			if err := tx.Create(&customer).Error; err != nil {
				return err
			}

			cart := Cart{ID: nextID(), CustomerID: customer.ID}
			if err := tx.Create(&cart).Error; err != nil {
				return err
			}
			return nil
		}
		return err
	})

	return account, customer, err
}

// FindOrCreateCustomerByIdentity 查找Or Create 用户 By 身份。
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

// TouchLastLogin 返回Touch Last Login。
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

// PermissionCodesByRole 返回权限 Codes By 角色。
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
