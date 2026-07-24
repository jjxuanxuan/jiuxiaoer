package provisioning

import (
	"testing"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
)

// TestRandomSecretAndAdminPermission 验证随机密钥和管理员权限。
func TestRandomSecretAndAdminPermission(t *testing.T) {
	a, err := randomSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, err := randomSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) < 40 || a == b {
		t.Fatal("reset secrets must be long and unique")
	}
	claims := &auth.Claims{AccountType: "admin", AdminUserID: "42", Permissions: []string{"merchant:provision"}}
	if id, err := adminID(claims, "merchant:provision"); err != nil || id != 42 {
		t.Fatalf("authorized admin rejected: id=%d err=%v", id, err)
	}
	if _, err := adminID(claims, "account:reset_password"); err == nil {
		t.Fatal("missing high-risk permission was accepted")
	}
}
