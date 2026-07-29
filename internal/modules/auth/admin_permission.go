package auth

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ActiveAdminHasPermission 在高风险写事务内读取当前管理员、账户、角色、
// 映射和权限状态。调用方仍需先校验 token permission；本函数用于阻断
// 权限撤销后尚未过期的旧 token。
func ActiveAdminHasPermission(
	ctx context.Context,
	tx *gorm.DB,
	adminID uint64,
	permissionCode string,
) (bool, error) {
	var row struct {
		ID uint64
	}
	err := tx.WithContext(ctx).
		Table("admin_users au").
		Select("au.id").
		Joins("JOIN accounts a ON a.id = au.account_id").
		Joins("JOIN roles r ON r.id = au.role_id").
		Joins("JOIN role_permissions rp ON rp.role_id = r.id").
		Joins("JOIN permissions p ON p.id = rp.permission_id").
		Where(`au.id = ?
			AND au.status = 'active' AND au.deleted_at IS NULL
			AND a.account_type = 'admin' AND a.status = 'active' AND a.deleted_at IS NULL
			AND r.status = 'active' AND r.deleted_at IS NULL
			AND rp.deleted_at IS NULL
			AND p.code = ? AND p.status = 'active' AND p.deleted_at IS NULL`,
			adminID,
			permissionCode,
		).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return err == nil, err
}
