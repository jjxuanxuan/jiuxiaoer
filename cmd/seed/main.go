package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/pkg/logger"
	"jiuxiaoer-admin/backend-go/internal/pkg/securevalue"
)

type roleSeed struct {
	ID    uint64
	Code  string
	Name  string
	Scope string
}

type permissionSeed struct {
	ID          uint64
	Code        string
	Resource    string
	Action      string
	Description string
}

type categorySeed struct {
	ID   uint64
	Name string
	Sort int
}

type productSeed struct {
	ID          uint64
	CategoryID  uint64
	Name        string
	BrandName   string
	Spec        string
	PriceAmount int64
}

const (
	roleSuperAdmin      uint64 = 1001
	roleAdminManager    uint64 = 1002
	roleOperation       uint64 = 1003
	roleFinance         uint64 = 1004
	roleCustomerService uint64 = 1005

	accountAdmin    uint64 = 3001
	accountMerchant uint64 = 3002
	accountRider    uint64 = 3003

	adminUserDemo    uint64 = 3101
	merchantDemo     uint64 = 4001
	merchantUserDemo uint64 = 4101
	shopDemo         uint64 = 4201
	merchantShopAuth uint64 = 4301
	riderDemo        uint64 = 5001
)

var roles = []roleSeed{
	{ID: roleSuperAdmin, Code: "super_admin", Name: "超级管理员", Scope: "all"},
	{ID: roleAdminManager, Code: "admin_manager", Name: "管理员", Scope: "all"},
	{ID: roleOperation, Code: "operation", Name: "运营", Scope: "scoped"},
	{ID: roleFinance, Code: "finance", Name: "财务", Scope: "readonly"},
	{ID: roleCustomerService, Code: "customer_service", Name: "客服", Scope: "scoped"},
}

var permissions = []permissionSeed{
	{2001, "product:list", "product", "list", "商品列表"},
	{2002, "product:view", "product", "view", "商品详情"},
	{2003, "product:create", "product", "create", "创建商品"},
	{2004, "product:update", "product", "update", "更新商品"},
	{2005, "inventory:view", "inventory", "view", "库存查看"},
	{2006, "inventory:adjust", "inventory", "adjust", "库存调整"},
	{2007, "order:list", "order", "list", "订单列表"},
	{2008, "order:view", "order", "view", "订单详情"},
	{2009, "order:cancel", "order", "cancel", "取消订单"},
	{2010, "merchant:list", "merchant", "list", "商户列表"},
	{2011, "merchant:review", "merchant", "review", "商户审核"},
	{2012, "audit_log:view", "audit_log", "view", "审计日志查看"},
	{2013, "store_order:list", "store_order", "list", "门店订单列表"},
	{2014, "store_order:accept", "store_order", "accept", "门店接单"},
	{2015, "store_order:prepare", "store_order", "prepare", "门店备货"},
	{2016, "shop_product:list", "shop_product", "list", "店铺商品列表"},
	{2017, "shop_product:create", "shop_product", "create", "店铺商品创建"},
	{2018, "shop_product:update", "shop_product", "update", "店铺商品更新"},
	{2019, "shop:business_status", "shop", "business_status", "门店营业状态"},
	{2020, "delivery:list", "delivery", "list", "配送单列表"},
	{2021, "delivery:accept", "delivery", "accept", "骑手接单"},
	{2022, "delivery:update_status", "delivery", "update_status", "配送状态更新"},
	{2023, "home_slot:list", "home_slot", "list", "首页运营位列表"},
	{2024, "home_slot:create", "home_slot", "create", "首页运营位创建"},
	{2025, "home_slot:update", "home_slot", "update", "首页运营位更新"},
	{2026, "home_slot:publish", "home_slot", "publish", "首页运营位发布"},
	{2027, "after_sale:list_shop", "after_sale", "list_shop", "门店售后列表"},
	{2028, "after_sale:view_shop", "after_sale", "view_shop", "门店售后详情"},
	{2029, "after_sale:review_shop", "after_sale", "review_shop", "门店售后审核"},
	{2030, "after_sale:receive_return", "after_sale", "receive_return", "售后退货验收"},
	{2031, "after_sale:create_replacement", "after_sale", "create_replacement", "创建售后补发"},
	{2032, "after_sale:list_all", "after_sale", "list_all", "平台售后列表"},
	{2033, "after_sale:view_all", "after_sale", "view_all", "平台售后详情"},
	{2034, "after_sale:review_platform", "after_sale", "review_platform", "平台售后审核"},
	{2035, "refund:list", "refund", "list", "退款列表"},
	{2036, "refund:view", "refund", "view", "退款详情"},
	{2037, "refund:retry", "refund", "retry", "退款重试"},
	{2038, "refund:exception", "refund", "exception", "退款异常处理"},
	{2039, "compensation:list", "compensation", "list", "补偿记录列表"},
	{2040, "compensation:approve", "compensation", "approve", "补偿审批"},
	{2041, "member:list", "member", "list", "会员列表"},
	{2042, "member:view", "member", "view", "会员详情"},
	{2043, "member_rule:list", "member_rule", "list", "会员等级规则列表"},
	{2044, "member_rule:create", "member_rule", "create", "创建会员等级规则"},
	{2045, "member_rule:activate", "member_rule", "activate", "激活会员等级规则"},
	{2046, "asset_transaction:list", "asset_transaction", "list", "资产流水列表"},
	{2047, "asset_transaction:view", "asset_transaction", "view", "资产流水详情"},
	{2048, "asset_adjustment:create", "asset_adjustment", "create", "创建资产调账"},
	{2049, "asset_adjustment:approve", "asset_adjustment", "approve", "复核资产调账"},
	{2050, "asset_reconcile:view", "asset_reconcile", "view", "查看资产对账"},
	{2051, "asset_reconcile:run", "asset_reconcile", "run", "执行资产对账"},
	{2052, "asset_reconcile:repair", "asset_reconcile", "repair", "修复资产投影"},
	{2053, "print_setting:view_shop", "print_setting", "view_shop", "查看门店打印设置"},
	{2054, "print_setting:update_shop", "print_setting", "update_shop", "更新门店打印设置"},
	{2055, "print_task:list_shop", "print_task", "list_shop", "查看门店打印任务"},
	{2056, "print_task:reprint_shop", "print_task", "reprint_shop", "门店人工重打"},
	{2057, "print_task:list_all", "print_task", "list_all", "查看全部打印任务"},
	{2058, "print_task:retry_all", "print_task", "retry_all", "重试打印任务"},
	{2059, "notification:list_all", "notification", "list_all", "查看通知任务"},
	{2060, "notification:retry", "notification", "retry", "重试通知任务"},
	{2061, "notification_template:list", "notification_template", "list", "查看通知模板"},
	{2062, "notification_template:create", "notification_template", "create", "创建通知模板"},
	{2063, "notification_template:publish", "notification_template", "publish", "发布通知模板"},
	{2064, "delivery_verification:view_shop", "delivery_verification", "view_shop", "查看门店取货码"},
	{2065, "delivery_verification:unlock", "delivery_verification", "unlock", "解锁配送核销"},
	{2066, "delivery:assign", "delivery", "assign", "人工派单"},
	{2067, "delivery:reassign", "delivery", "reassign", "人工改派"},
	{2068, "delivery:force_complete", "delivery", "force_complete", "强制完成配送"},
	{2069, "delivery_assignment:view", "delivery_assignment", "view", "查看派单历史"},
	{2070, "merchant:provision", "merchant", "provision", "原子开通商户"},
	{2071, "merchant_user:create", "merchant_user", "create", "创建商户用户"},
	{2072, "merchant_user:authorize_shop", "merchant_user", "authorize_shop", "授权商户门店"},
	{2073, "account:status_update", "account", "status_update", "启停账号"},
	{2074, "account:reset_password", "account", "reset_password", "重置账号凭证"},
	{2075, "rider:create", "rider", "create", "创建骑手"},
	{2076, "rider:review", "rider", "review", "审核骑手"},
	{2077, "rider:status_update", "rider", "status_update", "启停骑手"},
	{2078, "order:cancel_all", "order", "cancel_all", "后台取消订单"},
	{2079, "identity_verification:list", "identity_verification", "list", "查看实名记录"},
	{2080, "identity_verification:review", "identity_verification", "review", "人工复核实名"},
	{2081, "mq:health:view", "mq", "health_view", "查看消息队列健康"},
	{2082, "mq:dead_letter:list", "mq", "dead_letter_list", "查看消息死信"},
	{2083, "mq:dead_letter:replay", "mq", "dead_letter_replay", "重放单条消息死信"},
	{2084, "mq:topology:verify", "mq", "topology_verify", "验证消息拓扑"},
	{2085, "rider_work_status:update", "rider_work_status", "update", "骑手上下线"},
	{2086, "rider_location:update", "rider_location", "update", "骑手心跳定位"},
	{2087, "delivery_offer:list", "delivery_offer", "list", "查看本人派单邀约"},
	{2088, "delivery_offer:accept", "delivery_offer", "accept", "接受派单邀约"},
	{2089, "delivery_offer:reject", "delivery_offer", "reject", "拒绝派单邀约"},
	{2090, "dispatch_policy:list", "dispatch_policy", "list", "查看派单策略"},
	{2091, "dispatch_policy:create", "dispatch_policy", "create", "创建派单策略"},
	{2092, "dispatch_policy:validate", "dispatch_policy", "validate", "校验派单策略"},
	{2093, "dispatch_policy:publish", "dispatch_policy", "publish", "发布派单策略"},
	{2094, "dispatch_job:list", "dispatch_job", "list", "查看调度任务"},
	{2095, "dispatch_job:view", "dispatch_job", "view", "查看调度详情"},
	{2096, "dispatch_job:retry", "dispatch_job", "retry", "重试调度任务"},
	{2097, "dispatch_audit:view", "dispatch_audit", "view", "查看派单审计"},
	{2098, "rider_application:list", "rider_application", "list", "查看骑手申请列表"},
	{2099, "rider_application:view", "rider_application", "view", "查看骑手申请详情"},
	{2100, "rider_application:view_phone", "rider_application", "view_phone", "查看骑手申请完整手机号"},
	{2101, "rider_application:review", "rider_application", "review", "审核骑手申请"},
	{2102, "delivery:route", "delivery", "route", "配送路线规划"},
	{2103, "service_city:list", "service_city", "list", "查看开通城市配置"},
	{2104, "service_city:create", "service_city", "create", "创建开通城市草稿"},
	{2105, "service_city:update", "service_city", "update", "更新开通城市配置"},
	{2106, "service_city:publish", "service_city", "publish", "发布或停用开通城市"},
	{2107, "promise_policy:list", "promise_policy", "list", "查看配送承诺规则"},
	{2108, "promise_policy:create", "promise_policy", "create", "创建配送承诺规则版本"},
	{2109, "promise_policy:publish", "promise_policy", "publish", "发布或退役配送承诺规则"},
	{2110, "lbs_config:audit", "lbs_config", "audit", "查看位置服务开关与供应商观测"},
	{2111, "delivery_incident:create", "delivery_incident", "create", "骑手上报配送异常"},
	{2112, "delivery_incident:view_own", "delivery_incident", "view_own", "骑手查看本人配送异常"},
	{2113, "delivery_incident:evidence_add", "delivery_incident", "evidence_add", "骑手补充配送异常证据"},
	{2114, "delivery_incident:view_shop", "delivery_incident", "view_shop", "门店查看配送异常"},
	{2115, "delivery_incident:list_all", "delivery_incident", "list_all", "运营查看配送异常列表"},
	{2116, "delivery_incident:view_all", "delivery_incident", "view_all", "运营查看配送异常详情"},
	{2117, "delivery_incident:acknowledge", "delivery_incident", "acknowledge", "运营确认配送异常"},
	{2118, "delivery_incident:resolve", "delivery_incident", "resolve", "运营解决配送异常"},
	{2119, "delivery_incident:reject", "delivery_incident", "reject", "运营驳回配送异常"},
	{2120, "delivery_incident:audit", "delivery_incident", "audit", "审计配送异常"},
	{2121, "delivery_return:create", "delivery_return", "create", "骑手申请配送退回"},
	{2122, "delivery_return:view_own", "delivery_return", "view_own", "骑手查看本人配送退回"},
	{2123, "delivery_return:arrive", "delivery_return", "arrive", "骑手标记退回到店"},
	{2124, "delivery_return:list_shop", "delivery_return", "list_shop", "门店查看配送退回列表"},
	{2125, "delivery_return:view_shop", "delivery_return", "view_shop", "门店查看配送退回详情"},
	{2126, "delivery_return:receive_shop", "delivery_return", "receive_shop", "门店签收配送退回"},
	{2127, "delivery_return:list_all", "delivery_return", "list_all", "运营查看配送退回列表"},
	{2128, "delivery_return:view_all", "delivery_return", "view_all", "运营查看配送退回详情"},
	{2129, "delivery_return:approve", "delivery_return", "approve", "运营批准配送退回与退款"},
	{2130, "delivery_return:cancel", "delivery_return", "cancel", "运营撤销配送退回"},
	{2131, "delivery_return:audit", "delivery_return", "audit", "审计配送退回资金与库存动作"},
}

var categories = []categorySeed{
	{6001, "白酒", 10},
	{6002, "啤酒", 20},
	{6003, "葡萄酒", 30},
	{6004, "洋酒", 40},
	{6005, "饮料", 50},
}

var products = []productSeed{
	{7001, 6001, "酱香白酒 500ml", "九小二", "500ml", 12900},
	{7002, 6002, "精酿啤酒 330ml*6", "九小二", "330ml*6", 5900},
	{7003, 6003, "赤霞珠干红 750ml", "九小二", "750ml", 9900},
	{7004, 6004, "威士忌 700ml", "九小二", "700ml", 19900},
	{7005, 6005, "无糖气泡水 330ml*12", "九小二", "330ml*12", 3900},
}

// main 作为当前命令的程序入口，完成依赖初始化并启动运行。
func main() {
	// Seed 数据用于 P0 本地验收：管理员、商户、骑手、门店、商品和库存都从这里初始化。
	// 线上环境应改成受控迁移或运营后台创建，不能直接依赖默认密码。
	cfg := config.Load()
	cfg.MySQL.Required = true
	log := logger.New(cfg.App.Env)

	if cfg.Security.AdminBootstrapPassword == "admin123" {
		log.Warn("using default local admin password; set JXE_ADMIN_BOOTSTRAP_PASSWORD outside local development")
	}

	ctx := context.Background()
	db, err := mysql.Open(ctx, cfg.MySQL, log)
	if err != nil {
		log.Error("failed to connect mysql", slog.Any("error", err))
		os.Exit(1)
	}

	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := seedRoles(tx); err != nil {
			return err
		}
		if err := seedPermissions(tx); err != nil {
			return err
		}
		if err := seedRolePermissions(tx); err != nil {
			return err
		}
		if err := seedAccounts(tx, cfg); err != nil {
			return err
		}
		if err := seedMerchantAndShop(tx); err != nil {
			return err
		}
		if err := seedCustomerLBS(tx); err != nil {
			return err
		}
		if err := seedDispatch(tx); err != nil {
			return err
		}
		if err := seedCatalog(tx); err != nil {
			return err
		}
		if err := seedConfigs(tx); err != nil {
			return err
		}
		if err := seedCP1(tx, cfg); err != nil {
			return err
		}
		return nil
	}); err != nil {
		log.Error("seed failed", slog.Any("error", err))
		os.Exit(1)
	}

	log.Info("p0 seed completed")
}

// seedCP1 写入CP 1种子数据。
func seedCP1(tx *gorm.DB, cfg config.Config) error {
	if err := tx.Exec("UPDATE categories SET age_restricted = CASE WHEN id IN (6001,6002,6003,6004) THEN 1 ELSE 0 END").Error; err != nil {
		return err
	}
	if err := tx.Exec("UPDATE products SET age_restricted = CASE WHEN id IN (7001,7002,7003,7004) THEN 1 ELSE 0 END").Error; err != nil {
		return err
	}
	device, err := securevalue.Seal(cfg.CP1.DataEncryptionKey, "fake-device-4201")
	if err != nil {
		return err
	}
	if err := tx.Exec(`
		INSERT INTO print_settings (id,shop_id,provider,device_id_ciphertext,device_id_mask,template_id,copies,auto_print_events,enabled,version,created_by,updated_by)
		VALUES (9001,4201,'fake',?,'fa***01',9001,1,JSON_ARRAY('order_accepted','order_prepared'),0,1,3101,3101)
		ON DUPLICATE KEY UPDATE provider=VALUES(provider),device_id_ciphertext=VALUES(device_id_ciphertext),device_id_mask=VALUES(device_id_mask),auto_print_events=VALUES(auto_print_events)
	`, device).Error; err != nil {
		return err
	}
	events := []struct {
		id                 uint64
		event, title, body string
	}{
		{9101, "payment.succeeded", "支付成功", "您的订单已支付成功"},
		{9102, "store.order.accepted", "门店已接单", "门店正在处理您的订单"},
		{9103, "store.order.prepared", "备货完成", "订单已备好，等待骑手取货"},
		{9104, "delivery.picked_up", "骑手已取货", "您的订单已由骑手取货"},
		{9105, "delivery.completed", "订单已送达", "感谢您的购买"},
		{9106, "order.cancelled", "订单已取消", "订单已取消"},
		{9107, "refund.succeeded", "退款成功", "退款已原路退回"},
		{9108, "dispatch.offer.created", "新配送邀约", "您有一个新的配送订单邀约"},
		{9109, "dispatch.grab.opened", "新订单可抢", "有新的配送订单进入抢单池"},
		{9110, "dispatch.manual_required", "订单待人工派单", "订单自动调度未完成，请人工处理"},
		{9111, "delivery.incident.reported", "配送异常待处理", "有新的配送异常需要核实"},
		{9112, "delivery.incident.evidence_added", "配送异常已补证", "骑手已补充配送异常证据"},
		{9113, "delivery.incident.acknowledged", "配送异常已确认", "运营已接手配送异常"},
		{9114, "delivery.incident.resolved", "配送异常已解决", "配送异常已完成处置"},
		{9115, "delivery.incident.rejected", "配送异常已驳回", "配送异常已完成核实"},
		{9116, "delivery.return_requested", "配送退回待审核", "有新的配送退回申请需要处理"},
		{9117, "delivery.return_approved", "配送退回已批准", "订单退款处理中，商品正在退回门店"},
		{9118, "delivery.return_arrived", "退回商品已到店", "请核对交接码并验收退回商品"},
		{9119, "delivery.return_received", "退回商品已签收", "门店已完成退回商品验收"},
		{9120, "delivery.return_closed", "配送退回已完成", "退款和商品退回均已完成"},
		{9121, "delivery.return_sla_reminder", "配送退回即将逾期", "配送退回尚未完成门店签收"},
		{9122, "delivery.return_sla_breached", "配送退回已逾期", "配送退回超过签收时限，请立即处理"},
		{9123, "delivery.return_disputed", "配送退回待复核", "配送退回存在冲突，需要人工复核"},
		{9124, "delivery.return_exception", "配送退回异常", "配送退回需要人工处理"},
	}
	for _, item := range events {
		if err := tx.Exec(`INSERT INTO notification_templates
			(id,template_code,event_type,channel,provider_template_id,version,title_template,body_template,allowed_fields,status,created_by,published_by)
			VALUES (?,CONCAT('cp1_',REPLACE(?,'.','_')),?,'wechat',NULL,'v1',?,?,JSON_ARRAY(),'draft',3101,NULL)
			ON DUPLICATE KEY UPDATE title_template=VALUES(title_template),body_template=VALUES(body_template)
		`, item.id, item.event, item.event, item.title, item.body).Error; err != nil {
			return err
		}
	}
	return nil
}

// seedRoles 写入角色种子数据。
func seedRoles(tx *gorm.DB) error {
	for _, role := range roles {
		if err := tx.Exec(`
			INSERT INTO roles (id, code, name, scope, status)
			VALUES (?, ?, ?, ?, 'active')
			ON DUPLICATE KEY UPDATE name = VALUES(name), scope = VALUES(scope), status = 'active'
		`, role.ID, role.Code, role.Name, role.Scope).Error; err != nil {
			return err
		}
	}
	return nil
}

// seedPermissions 写入权限种子数据。
func seedPermissions(tx *gorm.DB) error {
	for _, perm := range permissions {
		if err := tx.Exec(`
			INSERT INTO permissions (id, code, resource, action, description, status)
			VALUES (?, ?, ?, ?, ?, 'active')
			ON DUPLICATE KEY UPDATE resource = VALUES(resource), action = VALUES(action), description = VALUES(description), status = 'active'
		`, perm.ID, perm.Code, perm.Resource, perm.Action, perm.Description).Error; err != nil {
			return err
		}
	}
	return nil
}

// seedRolePermissions 写入角色权限种子数据。
func seedRolePermissions(tx *gorm.DB) error {
	all := permissionIDs()
	assignments := []struct {
		roleID        uint64
		permissionIDs []uint64
	}{
		{roleSuperAdmin, all},
		{roleAdminManager, withoutPermissions(all, 2068, 2074, 2080, 2083, 2093)},
		{roleOperation, []uint64{2001, 2002, 2003, 2004, 2005, 2007, 2008, 2010, 2023, 2024, 2025, 2026, 2032, 2033, 2041, 2042, 2043, 2044, 2045, 2057, 2059, 2061, 2062, 2065, 2066, 2067, 2069, 2070, 2071, 2072, 2073, 2075, 2076, 2077, 2078, 2090, 2091, 2092, 2094, 2095, 2096, 2097, 2103, 2104, 2105, 2106, 2107, 2108, 2109, 2115, 2116, 2117, 2118, 2119, 2120, 2127, 2128, 2129, 2130, 2131}},
		{roleFinance, []uint64{2005, 2007, 2008, 2012, 2032, 2033, 2035, 2036, 2037, 2038, 2039, 2046, 2047, 2048, 2049, 2050, 2051, 2127, 2128, 2131}},
		{roleCustomerService, []uint64{2007, 2008, 2009, 2012, 2032, 2033, 2034, 2035, 2036, 2039, 2040, 2115, 2116, 2117, 2119}},
	}

	for _, assignment := range assignments {
		for _, permissionID := range assignment.permissionIDs {
			id := assignment.roleID*1000 + permissionID
			if err := tx.Exec(`
				INSERT INTO role_permissions (id, role_id, permission_id)
				VALUES (?, ?, ?)
				ON DUPLICATE KEY UPDATE updated_at = CURRENT_TIMESTAMP(3)
			`, id, assignment.roleID, permissionID).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// withoutPermissions 返回without 权限。
func withoutPermissions(values []uint64, excluded ...uint64) []uint64 {
	blocked := make(map[uint64]bool, len(excluded))
	for _, value := range excluded {
		blocked[value] = true
	}
	result := make([]uint64, 0, len(values))
	for _, value := range values {
		if !blocked[value] {
			result = append(result, value)
		}
	}
	return result
}

// permissionIDs 返回权限 I Ds。
func permissionIDs() []uint64 {
	ids := make([]uint64, 0, len(permissions))
	for _, perm := range permissions {
		ids = append(ids, perm.ID)
	}
	return ids
}

// seedAccounts 写入Accounts种子数据。
func seedAccounts(tx *gorm.DB, cfg config.Config) error {
	adminHash, err := bcryptHash(cfg.Security.AdminBootstrapPassword)
	if err != nil {
		return err
	}
	merchantHash, err := bcryptHash(cfg.Security.MerchantBootstrapPassword)
	if err != nil {
		return err
	}
	if err := upsertAccount(tx, accountAdmin, "admin", "admin", "", adminHash); err != nil {
		return err
	}
	if err := tx.Exec(`
		INSERT INTO admin_users (id, account_id, role_id, admin_sub_role, name, status)
		VALUES (?, ?, ?, 'super_admin', '平台管理员', 'active')
		ON DUPLICATE KEY UPDATE role_id = VALUES(role_id), admin_sub_role = VALUES(admin_sub_role), name = VALUES(name), status = 'active'
	`, adminUserDemo, accountAdmin, roleSuperAdmin).Error; err != nil {
		return err
	}

	if err := upsertAccount(tx, accountMerchant, "merchant", "merchant_demo", "", merchantHash); err != nil {
		return err
	}
	if err := upsertAccount(tx, accountRider, "rider", "", "13800000003", ""); err != nil {
		return err
	}

	return nil
}

// seedMerchantAndShop 写入商户 And 门店种子数据。
func seedMerchantAndShop(tx *gorm.DB) error {
	if err := tx.Exec(`
		INSERT INTO merchants (id, code, name, contact_name, contact_phone, status, review_status)
		VALUES (?, 'merchant_demo', '示例商户', '商户联系人', '13800000001', 'active', 'approved')
		ON DUPLICATE KEY UPDATE name = VALUES(name), status = 'active', review_status = 'approved'
	`, merchantDemo).Error; err != nil {
		return err
	}

	if err := tx.Exec(`
		INSERT INTO merchant_users (id, account_id, merchant_id, name, status)
		VALUES (?, ?, ?, '示例商家账号', 'active')
		ON DUPLICATE KEY UPDATE merchant_id = VALUES(merchant_id), name = VALUES(name), status = 'active'
	`, merchantUserDemo, accountMerchant, merchantDemo).Error; err != nil {
		return err
	}

	if err := tx.Exec(`
		INSERT INTO shops (
			id, merchant_id, name, phone, province, city, city_code, district, address,
			latitude, longitude, status, business_status, service_mode, service_radius_m,
			service_area_version, priority, delivery_fee_amount, free_delivery_threshold_amount,
			delivery_eta_min, delivery_eta_max, overtime_policy_code, overtime_policy_version
		)
		VALUES (
			?, ?, '示例门店', '13800000002', '广东省', '深圳市', '440300', '南山区', '科技园示例路 1 号',
			22.5400000, 113.9300000, 'active', 'open', 'radius', 20000,
			1, 0, 500, 9900, 15, 25, 'STANDARD_30', 1
		)
		ON DUPLICATE KEY UPDATE
			name = VALUES(name), phone = VALUES(phone), city_code = VALUES(city_code),
			status = 'active', business_status = 'open', service_mode = VALUES(service_mode),
			service_radius_m = VALUES(service_radius_m), priority = VALUES(priority),
			delivery_fee_amount = VALUES(delivery_fee_amount),
			free_delivery_threshold_amount = VALUES(free_delivery_threshold_amount),
			delivery_eta_min = VALUES(delivery_eta_min), delivery_eta_max = VALUES(delivery_eta_max),
			overtime_policy_code = VALUES(overtime_policy_code)
	`, shopDemo, merchantDemo).Error; err != nil {
		return err
	}

	for day := 1; day <= 7; day++ {
		if err := tx.Exec(`
			INSERT INTO shop_business_hours (id, shop_id, day_of_week, open_time, close_time, status)
			VALUES (?, ?, ?, '00:00:00', '23:59:59', 'active')
			ON DUPLICATE KEY UPDATE open_time = VALUES(open_time), close_time = VALUES(close_time), status = 'active', deleted_at = NULL
		`, uint64(4400+day), shopDemo, day).Error; err != nil {
			return err
		}
	}

	if err := tx.Exec(`
		INSERT INTO merchant_user_shops (id, merchant_user_id, merchant_id, shop_id)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE merchant_id = VALUES(merchant_id), updated_at = CURRENT_TIMESTAMP(3)
	`, merchantShopAuth, merchantUserDemo, merchantDemo, shopDemo).Error; err != nil {
		return err
	}

	if err := tx.Exec(`
		INSERT INTO riders (id, account_id, name, phone, status, work_status, review_status, service_scope)
		VALUES (?, ?, '示例骑手', '13800000003', 'active', 'online', 'approved', JSON_OBJECT('shop_ids', JSON_ARRAY(CAST(? AS CHAR))))
		ON DUPLICATE KEY UPDATE name = VALUES(name), phone = VALUES(phone), status = 'active', work_status = 'online', review_status = 'approved', service_scope = VALUES(service_scope)
	`, riderDemo, accountRider, shopDemo).Error; err != nil {
		return err
	}
	if err := tx.Exec(`
		INSERT INTO rider_service_shops (id,rider_id,shop_id,status,source,created_by,updated_by)
		VALUES (?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE status='active',source=VALUES(source),updated_by=VALUES(updated_by)
	`, uint64(5101), riderDemo, shopDemo, "active", "migration", adminUserDemo, adminUserDemo).Error; err != nil {
		return err
	}
	if err := tx.Exec(`
		INSERT INTO rider_runtime_states (rider_id,work_status,latitude,longitude,accuracy_m,captured_at,heartbeat_at,last_sequence,online_since,max_active_orders,version)
		VALUES (?, 'online', 22.5410000, 113.9310000, 20, CURRENT_TIMESTAMP(3), CURRENT_TIMESTAMP(3), 1, CURRENT_TIMESTAMP(3), 3, 1)
		ON DUPLICATE KEY UPDATE work_status='online',latitude=VALUES(latitude),longitude=VALUES(longitude),accuracy_m=VALUES(accuracy_m),captured_at=VALUES(captured_at),heartbeat_at=VALUES(heartbeat_at),max_active_orders=3
	`, riderDemo).Error; err != nil {
		return err
	}

	return nil
}

type customerLBSADCodeSeed struct {
	id                uint64
	code, name, level string
}

var customerLBSADCodes = []customerLBSADCodeSeed{
	{9610, "440300", "深圳市", "city"},
	{9611, "440305", "南山区", "district"},
	{9612, "440304", "福田区", "district"},
}

// seedCustomerLBS 写入本地验收所需的开通城市、行政区映射与配送承诺规则。
func seedCustomerLBS(tx *gorm.DB) error {
	if err := tx.Exec(`
		INSERT INTO delivery_promise_policies (
			id, policy_code, version, title, summary, status, published_at, published_by, created_by, updated_by
		)
		VALUES (9701, 'STANDARD_30', 1, '超时保障', '超过配送承诺时间时，按平台展示规则处理', 'published', CURRENT_TIMESTAMP(3), ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			title = VALUES(title), summary = VALUES(summary), updated_by = VALUES(updated_by)
	`, adminUserDemo, adminUserDemo, adminUserDemo).Error; err != nil {
		return err
	}
	if err := tx.Exec(`
		INSERT INTO service_cities (
			id, city_code, province_code, name, pinyin, status, sort_order,
			default_browse_shop_id, version, published_at, created_by, updated_by
		)
		VALUES (9601, '440300', '440000', '深圳市', 'shenzhen', 'published', 10, ?, 1, CURRENT_TIMESTAMP(3), ?, ?)
		ON DUPLICATE KEY UPDATE
			province_code = VALUES(province_code), name = VALUES(name), pinyin = VALUES(pinyin),
			status = 'published', sort_order = VALUES(sort_order),
			default_browse_shop_id = VALUES(default_browse_shop_id),
			published_at = COALESCE(published_at, CURRENT_TIMESTAMP(3)), updated_by = VALUES(updated_by)
	`, shopDemo, adminUserDemo, adminUserDemo).Error; err != nil {
		return err
	}
	for _, item := range customerLBSADCodes {
		if err := tx.Exec(`
			INSERT INTO service_city_adcodes (
				id, service_city_id, adcode, standard_name, level, created_by, updated_by
			)
			VALUES (?, 9601, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				service_city_id = VALUES(service_city_id), standard_name = VALUES(standard_name),
				level = VALUES(level), updated_by = VALUES(updated_by)
		`, item.id, item.code, item.name, item.level, adminUserDemo, adminUserDemo).Error; err != nil {
			return err
		}
	}
	return tx.Exec(`
		UPDATE shops
		SET overtime_policy_version = 1
		WHERE overtime_policy_code = 'STANDARD_30' AND overtime_policy_version IS NULL
	`).Error
}

// seedDispatch 写入调度种子数据。
func seedDispatch(tx *gorm.DB) error {
	return tx.Exec(`
		INSERT INTO dispatch_policies (
			id,policy_code,scope_type,scope_id,version,mode,auto_rounds,offer_ttl_seconds,grab_ttl_seconds,
			candidate_limit,offer_candidate_limit,heartbeat_fresh_seconds,location_fresh_seconds,
			max_location_accuracy_m,max_pickup_distance_m,max_active_orders_default,idle_full_score_seconds,
			score_weights,rejection_cooldown_seconds,status,published_at,published_by,row_version,created_by,updated_by
		) VALUES (
			9201,'default-hybrid','global','0',1,'hybrid',3,10,30,100,3,60,120,200,5000,3,1800,
			JSON_OBJECT('distance',0.45,'load',0.30,'idle',0.20,'freshness',0.05),120,'published',CURRENT_TIMESTAMP(3),?,1,?,?
		)
		ON DUPLICATE KEY UPDATE mode=VALUES(mode),status='published',score_weights=VALUES(score_weights),updated_by=VALUES(updated_by)
	`, adminUserDemo, adminUserDemo, adminUserDemo).Error
}

// seedCatalog 写入Catalog种子数据。
func seedCatalog(tx *gorm.DB) error {
	for _, category := range categories {
		if err := tx.Exec(`
			INSERT INTO categories (id, name, sort_order, status)
			VALUES (?, ?, ?, 'active')
			ON DUPLICATE KEY UPDATE name = VALUES(name), sort_order = VALUES(sort_order), status = 'active'
		`, category.ID, category.Name, category.Sort).Error; err != nil {
			return err
		}
	}

	for idx, product := range products {
		if err := tx.Exec(`
			INSERT INTO products (id, category_id, name, brand_name, spec, sale_price_amount, original_price_amount, status)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'on_sale')
			ON DUPLICATE KEY UPDATE category_id = VALUES(category_id), name = VALUES(name), sale_price_amount = VALUES(sale_price_amount), status = 'on_sale'
		`, product.ID, product.CategoryID, product.Name, product.BrandName, product.Spec, product.PriceAmount, product.PriceAmount).Error; err != nil {
			return err
		}

		shopProductID := uint64(8001 + idx)
		if err := tx.Exec(`
			INSERT INTO shop_products (id, merchant_id, shop_id, product_id, sale_price_amount, status, sort_order)
			VALUES (?, ?, ?, ?, ?, 'on_sale', ?)
			ON DUPLICATE KEY UPDATE sale_price_amount = VALUES(sale_price_amount), status = 'on_sale', sort_order = VALUES(sort_order)
		`, shopProductID, merchantDemo, shopDemo, product.ID, product.PriceAmount, (idx+1)*10).Error; err != nil {
			return err
		}

		if err := tx.Exec(`
			INSERT INTO product_stocks (id, shop_product_id, shop_id, product_id, available_qty, reserved_qty, locked_qty, low_stock_threshold, version)
			VALUES (?, ?, ?, ?, 100, 0, 0, 10, 0)
			ON DUPLICATE KEY UPDATE available_qty = VALUES(available_qty), reserved_qty = VALUES(reserved_qty), locked_qty = VALUES(locked_qty), low_stock_threshold = VALUES(low_stock_threshold)
		`, uint64(8501+idx), shopProductID, shopDemo, product.ID).Error; err != nil {
			return err
		}
	}
	return nil
}

// seedConfigs 写入Configs种子数据。
func seedConfigs(tx *gorm.DB) error {
	configs := []struct {
		id    uint64
		key   string
		value string
	}{
		{9501, "payment.mock_enabled", "true"},
		{9502, "sms.mock_enabled", "true"},
		{9503, "order.idempotency_enabled", "true"},
		{9504, "stock.reserve_enabled", "true"},
		{9505, "mq.publisher_enabled", "true"},
	}

	for _, configItem := range configs {
		if err := tx.Exec(`
			INSERT INTO system_configs (id, config_key, config_value, value_type, description, status)
			VALUES (?, ?, ?, 'bool', ?, 'active')
			ON DUPLICATE KEY UPDATE config_value = VALUES(config_value), value_type = VALUES(value_type), status = 'active'
		`, configItem.id, configItem.key, configItem.value, fmt.Sprintf("P0 seed config: %s", configItem.key)).Error; err != nil {
			return err
		}
	}
	return nil
}

// upsertAccount 新增或更新账户。
func upsertAccount(tx *gorm.DB, id uint64, accountType string, username string, phone string, passwordHash string) error {
	return tx.Exec(`
		INSERT INTO accounts (id, account_type, username, phone, password_hash, status)
		VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), 'active')
		ON DUPLICATE KEY UPDATE username = VALUES(username), phone = VALUES(phone), password_hash = VALUES(password_hash), status = 'active', updated_at = CURRENT_TIMESTAMP(3)
	`, id, accountType, username, phone, passwordHash).Error
}

// bcryptHash 返回bcrypt 哈希。
func bcryptHash(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// init 初始化当前包所需的默认配置或注册项。
func init() {
	time.Local = time.FixedZone("Asia/Shanghai", 8*60*60)
}
