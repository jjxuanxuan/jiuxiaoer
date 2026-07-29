package app

import "jiuxiaoer-admin/backend-go/internal/config"

// apiOwnsSharedMaintenance 默认让既有零售订单过期和退款任务继续归 API 进程负责，
// 只有启用酒票维护并明确转交给专用任务进程时才迁移所有权。
// 共享任务会从同一支付和退款队列认领记录，无法按进程安全拆分，
// 因此所有业务类型的所有权必须原子迁移。
func apiOwnsSharedMaintenance(cfg config.Config) bool {
	return !cfg.WineTicket.Enabled ||
		cfg.WineTicket.MaintenanceOwner == config.WineTicketMaintenanceOwnerAPI
}

func apiOwnsWineTicketMaintenance(cfg config.Config) bool {
	return cfg.WineTicket.Enabled &&
		cfg.WineTicket.MaintenanceOwner == config.WineTicketMaintenanceOwnerAPI
}

func workerOwnsWineTicketMaintenance(cfg config.Config) bool {
	return cfg.WineTicket.Enabled &&
		cfg.WineTicket.MaintenanceOwner == config.WineTicketMaintenanceOwnerWorker
}
