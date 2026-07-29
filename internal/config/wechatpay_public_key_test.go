package config

import (
	"strings"
	"testing"
)

func TestWeChatPayPublicKeyConfigIsOptionalButMustBePaired(t *testing.T) {
	t.Setenv("JXE_WECHAT_PAY_PUBLIC_KEY_ID", "")
	t.Setenv("JXE_WECHAT_PAY_PUBLIC_KEY_PATH", "")
	cfg := Load()
	if cfg.WeChat.PayPublicKeyID != "" || cfg.WeChat.PayPublicKeyPath != "" {
		t.Fatalf("public-key verification must default off: %+v", cfg.WeChat)
	}
	if problems := cfg.wechatPayRuntimeProblems(); len(problems) != 0 {
		t.Fatalf("empty optional public-key configuration must be accepted: %v", problems)
	}

	cfg.WeChat.PayPublicKeyID = "PUB_KEY_ID_0123456789"
	problems := strings.Join(cfg.wechatPayRuntimeProblems(), "; ")
	if !strings.Contains(problems, "must be configured together") {
		t.Fatalf("expected incomplete public-key configuration to fail closed, got %q", problems)
	}
}

func TestProductionWineTicketMoneyRequiresWeChatPayPublicKey(t *testing.T) {
	cfg := Load()
	cfg.App.Env = "production"
	cfg.WineTicket.Enabled = true
	cfg.WineTicket.PurchaseEnabled = true
	cfg.WeChat.PayMockEnabled = false
	cfg.WeChat.PayPublicKeyID = ""
	cfg.WeChat.PayPublicKeyPath = ""

	problems := strings.Join(cfg.wineTicketRuntimeProblems(), "; ")
	if !strings.Contains(problems, "JXE_WECHAT_PAY_PUBLIC_KEY_ID") {
		t.Fatalf("expected production wine-ticket money public-key gate, got %q", problems)
	}

	cfg.WeChat.PayPublicKeyID = "PUB_KEY_ID_0123456789"
	cfg.WeChat.PayPublicKeyPath = "/run/secrets/wechat-pay-public-key.pem"
	problems = strings.Join(cfg.wineTicketRuntimeProblems(), "; ")
	if strings.Contains(problems, "JXE_WECHAT_PAY_PUBLIC_KEY_ID") {
		t.Fatalf("complete public-key configuration was rejected: %q", problems)
	}
}
