package wechatpay

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wechatpay-apiv3/wechatpay-go/core/auth/verifiers"

	"jiuxiaoer-admin/backend-go/internal/config"
)

func TestLoadWechatPayPublicKeyFailsClosed(t *testing.T) {
	if publicKey, err := loadWechatPayPublicKey(config.WeChatConfig{}); err != nil || publicKey != nil {
		t.Fatalf("empty optional public-key configuration: key=%v err=%v", publicKey, err)
	}

	_, err := loadWechatPayPublicKey(config.WeChatConfig{PayPublicKeyID: "PUB_KEY_ID_0123456789"})
	if err == nil || !strings.Contains(err.Error(), "configured together") {
		t.Fatalf("expected incomplete public-key configuration error, got %v", err)
	}

	_, err = loadWechatPayPublicKey(config.WeChatConfig{
		PayPublicKeyID:   "PUB_KEY_ID_0123456789",
		PayPublicKeyPath: filepath.Join(t.TempDir(), "missing.pem"),
	})
	if err == nil || !strings.Contains(err.Error(), "load WeChat Pay public key") {
		t.Fatalf("expected unreadable public-key path error, got %v", err)
	}
}

func TestConfiguredPublicKeyUsesCombinedVerifier(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	publicKeyDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	publicKeyPath := filepath.Join(t.TempDir(), "wechatpay-public-key.pem")
	if err := os.WriteFile(publicKeyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicKeyDER}), 0o600); err != nil {
		t.Fatal(err)
	}

	publicKey, err := loadWechatPayPublicKey(config.WeChatConfig{
		PayPublicKeyID:   "PUB_KEY_ID_0123456789",
		PayPublicKeyPath: publicKeyPath,
	})
	if err != nil {
		t.Fatalf("load public key: %v", err)
	}
	if _, ok := newWechatPayVerifier(nil, "PUB_KEY_ID_0123456789", publicKey).(*verifiers.SHA256WithRSACombinedVerifier); !ok {
		t.Fatal("configured public key must retain certificate fallback through the combined verifier")
	}
	if _, ok := newWechatPayVerifier(nil, "", nil).(*verifiers.SHA256WithRSAVerifier); !ok {
		t.Fatal("unconfigured public key must preserve the existing certificate verifier")
	}
}
