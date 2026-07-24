package idempotency

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

// TestPathResourceCommandsUseResourceScopedRequestHash 是针对 Gin 路径中
// 含资源参数命令的静态契约。FullPath 是路由模板，因此列出的每个命令
// 都必须把具体资源 ID 纳入请求哈希，而不能依赖路径唯一性。
func TestPathResourceCommandsUseResourceScopedRequestHash(t *testing.T) {
	type command struct {
		file, function, action, resource string
	}
	commands := []command{
		{"internal/modules/admin/service.go", "UpdateProduct", `"product.update"`, "productID"},
		{"internal/modules/admin/service.go", "ReviewMerchant", `"merchant.review"`, "merchantID"},
		{"internal/modules/aftersale/service.go", "AddEvidence", `"after_sale.add_evidence"`, "id"},
		{"internal/modules/aftersale/service.go", "review", `"after_sale.review"`, "id"},
		{"internal/modules/aftersale/service.go", "ReceiveReturn", `"after_sale.receive_return"`, "afterSaleID"},
		{"internal/modules/aftersale/service.go", "ReserveReplacement", `"after_sale.reserve_replacement"`, "afterSaleID"},
		{"internal/modules/compliance/service.go", "Review", `"identity_verification.review"`, "id"},
		{"internal/modules/customerlocation/admin_service.go", "UpdateAdminCity", `"service_city.update"`, "id"},
		{"internal/modules/customerlocation/admin_service.go", "SetAdminCityStatus", `"service_city.status"`, "id"},
		{"internal/modules/customerlocation/admin_service.go", "UpdateAdminPolicy", `"promise_policy.update"`, "id"},
		{"internal/modules/customerlocation/admin_service.go", "SetAdminPolicyStatus", `"promise_policy.status"`, "id"},
		{"internal/modules/notification/service.go", "Retry", `"notification.retry"`, "id"},
		{"internal/modules/notification/service.go", "UpdateTemplate", `"notification_template.update"`, "id"},
		{"internal/modules/ops/service.go", "assign", `"delivery." + kind`, "id"},
		{"internal/modules/ops/service.go", "RequestForceComplete", `"delivery.force_complete.request"`, "id"},
		{"internal/modules/ops/service.go", "ForceComplete", `"delivery.force_complete"`, "id"},
		{"internal/modules/order/service.go", "cancel", `"order.cancel"`, "orderID"},
		{"internal/modules/order/service.go", "MockPay", `"payment.mock"`, "orderID"},
		{"internal/modules/order/service.go", "CreatePayment", `"payment.create"`, "orderID"},
		{"internal/modules/printjob/service.go", "Retry", `"print_task.retry"`, "id"},
		{"internal/modules/provisioning/service.go", "CreateMerchantUser", `"merchant_user.create"`, "merchant"},
		{"internal/modules/provisioning/service.go", "AuthorizeShops", `"merchant_user.authorize_shops"`, "user"},
		{"internal/modules/provisioning/service.go", "UpdateMerchantUserRole", `"merchant_user.update_role"`, "userID"},
		{"internal/modules/provisioning/service.go", "AccountStatus", `"account.status"`, "id"},
		{"internal/modules/provisioning/service.go", "ResetPassword", `"account.reset_password"`, "id"},
		{"internal/modules/provisioning/service.go", "updateRider", `"rider." + kind`, "id"},
		{"internal/modules/riderapplication/admin_service.go", "Review", `"rider_application.review"`, "applicationID"},
		{"internal/modules/store/service.go", "UpdateBusinessStatus", `"shop.business_status.update"`, "shopID"},
		{"internal/modules/store/service.go", "UpdateShopProduct", `"shop_product.update"`, "shopProductID"},
		{"internal/modules/store/service.go", "AdjustStock", `"shop_product.stock.adjust"`, "shopProductID"},
	}

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve contract test path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "../../.."))
	fset := token.NewFileSet()
	for _, item := range commands {
		t.Run(item.function, func(t *testing.T) {
			parsed, err := parser.ParseFile(fset, filepath.Join(repoRoot, item.file), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			var function *ast.FuncDecl
			for _, declaration := range parsed.Decls {
				candidate, ok := declaration.(*ast.FuncDecl)
				if ok && candidate.Name.Name == item.function {
					function = candidate
					break
				}
			}
			if function == nil {
				t.Fatalf("function %s not found in %s", item.function, item.file)
			}

			found := false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "ResourceRequestHash" {
					return true
				}
				if len(call.Args) != 3 {
					t.Errorf("ResourceRequestHash args=%d, want action, resource, body", len(call.Args))
					return true
				}
				if got := expressionString(fset, call.Args[0]); got != item.action {
					t.Errorf("action expression=%q, want %q", got, item.action)
				}
				if got := expressionString(fset, call.Args[1]); got != item.resource {
					t.Errorf("resource expression=%q, want %q", got, item.resource)
				}
				found = true
				return true
			})
			if !found {
				t.Fatalf("%s must use ResourceRequestHash", item.function)
			}
		})
	}
}

func expressionString(fset *token.FileSet, expression ast.Expr) string {
	var output bytes.Buffer
	_ = printer.Fprint(&output, fset, expression)
	return output.String()
}
