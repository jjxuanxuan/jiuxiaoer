package config

import (
	"strings"
	"testing"
	"time"
)

func TestWineTicketBranchesRequireMasterSwitch(t *testing.T) {
	cfg := Load()
	cfg.WineTicket.PackageReadEnabled = true

	problems := strings.Join(cfg.wineTicketRuntimeProblems(), "; ")
	if !strings.Contains(problems, "JXE_WINE_TICKET_ENABLED=true") {
		t.Fatalf("expected master switch gate, got %q", problems)
	}
}

func TestWineTicketRuntimeAcceptsPinnedShanghaiBaseline(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	previous := time.Local
	time.Local = shanghai
	t.Cleanup(func() { time.Local = previous })

	cfg := Load()
	cfg.WineTicket.Enabled = true
	cfg.WineTicket.PackageReadEnabled = true
	cfg.MySQL.DSN = "jxe:secret@tcp(127.0.0.1:3306)/jxe?parseTime=true&loc=Local"
	cfg.MySQL.Required = true
	cfg.MySQL.RequiredTimeZone = "+08:00"
	cfg.MySQL.RequireWineTicketSchema = true

	if problems := cfg.wineTicketRuntimeProblems(); len(problems) != 0 {
		t.Fatalf("expected pinned runtime to pass, got %v", problems)
	}
}

func TestWineTicketRuntimeRejectsDSNWithoutLocalTime(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	previous := time.Local
	time.Local = shanghai
	t.Cleanup(func() { time.Local = previous })

	cfg := Load()
	cfg.WineTicket.Enabled = true
	cfg.MySQL.DSN = "jxe:secret@tcp(127.0.0.1:3306)/jxe?parseTime=true&loc=UTC"
	cfg.MySQL.Required = true
	cfg.MySQL.RequiredTimeZone = "+08:00"
	cfg.MySQL.RequireWineTicketSchema = true

	problems := strings.Join(cfg.wineTicketRuntimeProblems(), "; ")
	if !strings.Contains(problems, "loc=Local") {
		t.Fatalf("expected loc=Local gate, got %q", problems)
	}
}

func TestWineTicketFlagsLoadFailClosed(t *testing.T) {
	t.Setenv("JXE_WINE_TICKET_ENABLED", "true")
	t.Setenv("JXE_WINE_TICKET_PACKAGE_READ_ENABLED", "true")
	cfg := Load()
	if !cfg.WineTicket.Enabled || !cfg.WineTicket.PackageReadEnabled {
		t.Fatalf("wine-ticket flags not loaded: %+v", cfg.WineTicket)
	}
	if cfg.WineTicket.PurchaseEnabled || cfg.WineTicket.RedemptionEnabled || cfg.WineTicket.GiftEnabled {
		t.Fatalf("unconfigured branches must stay off: %+v", cfg.WineTicket)
	}
	if cfg.WineTicket.WeChatReminderEnabled {
		t.Fatalf("WeChat reminder channel must stay fail-closed: %+v", cfg.WineTicket)
	}
	if cfg.WineTicket.WeChatReminderProviderEnabled {
		t.Fatalf("WeChat reminder provider must stay fail-closed: %+v", cfg.WineTicket)
	}
	if cfg.MySQL.RequiredTimeZone != "+08:00" || !cfg.MySQL.RequireWineTicketSchema {
		t.Fatalf("wine-ticket runtime gates not derived: %+v", cfg.MySQL)
	}
	if cfg.WineTicket.MaintenanceOwner != WineTicketMaintenanceOwnerAPI {
		t.Fatalf("default maintenance owner must preserve API behavior: %+v", cfg.WineTicket)
	}
}

func TestWineTicketMaintenanceOwnerLoadsAndValidates(t *testing.T) {
	t.Setenv("JXE_WINE_TICKET_MAINTENANCE_OWNER", WineTicketMaintenanceOwnerWorker)
	cfg := Load()
	if cfg.WineTicket.MaintenanceOwner != WineTicketMaintenanceOwnerWorker {
		t.Fatalf("maintenance owner was not loaded: %q", cfg.WineTicket.MaintenanceOwner)
	}

	cfg.WineTicket.MaintenanceOwner = "both"
	problems := strings.Join(cfg.wineTicketRuntimeProblems(), "; ")
	if !strings.Contains(problems, "JXE_WINE_TICKET_MAINTENANCE_OWNER must be api or worker") {
		t.Fatalf("invalid maintenance owner passed validation: %q", problems)
	}
}

func TestWineTicketWeChatReminderProviderRequiresControlledConfiguration(t *testing.T) {
	cfg := Load()
	cfg.WineTicket.Enabled = true
	cfg.WineTicket.ReminderEnabled = true
	cfg.WineTicket.WeChatReminderEnabled = true
	cfg.WineTicket.WeChatReminderProviderEnabled = true
	cfg.WeChat.MiniAppSecret = ""

	problems := strings.Join(cfg.wineTicketRuntimeProblems(), "; ")
	for _, expected := range []string{
		"Mini Program AppID and secret",
		"JXE_WINE_TICKET_WECHAT_REMINDER_PAGE",
		"template field mappings",
	} {
		if !strings.Contains(problems, expected) {
			t.Fatalf("missing provider gate %q in %q", expected, problems)
		}
	}
}

func TestWineTicketWeChatReminderRequiresInboxReminderBranch(t *testing.T) {
	t.Setenv("JXE_WINE_TICKET_ENABLED", "true")
	t.Setenv("JXE_WINE_TICKET_WECHAT_REMINDER_ENABLED", "true")
	cfg := Load()
	if !cfg.WineTicket.WeChatReminderEnabled {
		t.Fatal("WeChat reminder switch was not loaded")
	}
	problems := strings.Join(cfg.wineTicketRuntimeProblems(), "; ")
	if !strings.Contains(problems, "JXE_WINE_TICKET_REMINDER_ENABLED=true") {
		t.Fatalf("WeChat reminder bypassed inbox reminder branch: %q", problems)
	}
}

func TestWineTicketMoneyFlagsRequireManualContractProfile(t *testing.T) {
	t.Setenv("JXE_WINE_TICKET_ENABLED", "true")
	t.Setenv("JXE_WINE_TICKET_PURCHASE_ENABLED", "true")
	cfg := Load()
	if !cfg.MySQL.RequireWineTicketMoneyContract {
		t.Fatal("money branch must enable the manual CONTRACT startup verifier")
	}

	cfg.MySQL.RequireWineTicketMoneyContract = false
	problems := strings.Join(cfg.wineTicketRuntimeProblems(), "; ")
	if !strings.Contains(problems, "money registry CONTRACT gate") {
		t.Fatalf("expected money CONTRACT gate, got %q", problems)
	}
}

func TestWineTicketMoneyBranchesRequireSettlementAndRefundWorkers(t *testing.T) {
	cfg := Load()
	cfg.WineTicket.Enabled = true
	cfg.WineTicket.PurchaseEnabled = true
	cfg.Order.ExpiryWorkerEnabled = false
	cfg.AfterSale.RefundExecutionEnabled = false
	cfg.AfterSale.WorkerEnabled = false

	problems := strings.Join(cfg.wineTicketRuntimeProblems(), "; ")
	for _, expected := range []string{
		"JXE_ORDER_EXPIRY_WORKER_ENABLED=true",
		"JXE_REFUND_EXECUTION_ENABLED=true",
		"JXE_REFUND_WORKER_ENABLED=true",
	} {
		if !strings.Contains(problems, expected) {
			t.Fatalf("missing money-closure dependency %s in %q", expected, problems)
		}
	}
}

func TestProductionWineTicketMoneyBranchesRequireRefundCallbackAndIntegrityReconciliation(t *testing.T) {
	cfg := Load()
	cfg.App.Env = "production"
	cfg.WineTicket.Enabled = true
	cfg.WineTicket.RenewalEnabled = true
	cfg.WeChat.PayMockEnabled = false
	cfg.WeChat.RefundNotifyURL = ""
	cfg.WineTicket.ReconciliationEnabled = false

	problems := strings.Join(cfg.wineTicketRuntimeProblems(), "; ")
	for _, expected := range []string{
		"JXE_WECHAT_REFUND_NOTIFY_URL",
		"JXE_WINE_TICKET_RECONCILIATION_ENABLED=true",
	} {
		if !strings.Contains(problems, expected) {
			t.Fatalf("missing production money gate %s in %q", expected, problems)
		}
	}
}
