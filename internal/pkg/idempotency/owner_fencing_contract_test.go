package idempotency

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

// TestCrossTransactionIdempotencyFlowsCarryOwnerToken 防止跨事务流程
// 静默退化为仅按自然键完成。
// 此处列出的每个调用都跨越外部服务商调用或独立提交的业务操作，
// 因此其最新认领 ID 就是隔离令牌。
func TestCrossTransactionIdempotencyFlowsCarryOwnerToken(t *testing.T) {
	type flow struct {
		file     string
		function string
		required []string
	}
	flows := []flow{
		{
			file:     "internal/modules/order/service.go",
			function: "CreatePayment",
			required: []string{"Start", "SucceedOwned", "FailOwned"},
		},
		{
			file:     "internal/modules/order/payment_state.go",
			function: "ConfirmPaymentIdempotent",
			required: []string{"Start", "SucceedOwned", "releasePaymentConfirmClaim"},
		},
		{
			file:     "internal/modules/order/payment_state.go",
			function: "releasePaymentConfirmClaim",
			required: []string{"FailOwned"},
		},
		{
			file:     "internal/modules/wineticket/purchase/service.go",
			function: "CreatePurchase",
			required: []string{"claimIdempotencyWithID", "SucceedOwned", "releaseCustomerIdempotencyOwned"},
		},
		{
			file:     "internal/modules/wineticket/purchase/service.go",
			function: "ConfirmPurchasePayment",
			required: []string{"claimIdempotencyWithID", "SucceedOwned", "releaseCustomerIdempotencyOwned"},
		},
		{
			file:     "internal/modules/wineticket/purchase/service.go",
			function: "releaseCustomerIdempotencyOwned",
			required: []string{"FailOwned"},
		},
		{
			file:     "internal/modules/wineticket/renewal/service.go",
			function: "CreateRenewal",
			required: []string{"claimIdempotencyWithID", "SucceedOwned", "releaseCustomerIdempotencyOwned"},
		},
		{
			file:     "internal/modules/wineticket/renewal/service.go",
			function: "ConfirmRenewalPayment",
			required: []string{"claimIdempotencyWithID", "SucceedOwned", "releaseCustomerIdempotencyOwned"},
		},
		{
			file:     "internal/modules/reconciliation/admin.go",
			function: "RunBillManual",
			required: []string{"Start", "SucceedOwned", "FailOwned"},
		},
	}

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve contract test path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "../../.."))

	for _, item := range flows {
		item := item
		t.Run(item.function, func(t *testing.T) {
			fset := token.NewFileSet()
			parsed, err := parser.ParseFile(
				fset,
				filepath.Join(repoRoot, item.file),
				nil,
				0,
			)
			if err != nil {
				t.Fatal(err)
			}
			function := findFunction(parsed, item.function)
			if function == nil {
				t.Fatalf("function %s not found in %s", item.function, item.file)
			}

			found := make(map[string]int, len(item.required))
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := calledFunctionName(call)
				if name == "Succeed" || name == "Fail" {
					t.Errorf(
						"%s must not use legacy natural-key-only %s",
						item.function,
						name,
					)
				}
				if !contains(item.required, name) {
					return true
				}
				found[name]++
				if !callHasExpression(fset, call, "claimID") {
					t.Errorf(
						"%s call %s must carry claimID",
						item.function,
						name,
					)
				}
				return true
			})
			for _, required := range item.required {
				if found[required] == 0 {
					t.Errorf(
						"%s must call %s",
						item.function,
						required,
					)
				}
			}
		})
	}
}

func findFunction(file *ast.File, name string) *ast.FuncDecl {
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == name {
			return function
		}
	}
	return nil
}

func calledFunctionName(call *ast.CallExpr) string {
	switch function := call.Fun.(type) {
	case *ast.SelectorExpr:
		return function.Sel.Name
	case *ast.Ident:
		return function.Name
	default:
		return ""
	}
}

func callHasExpression(
	fset *token.FileSet,
	call *ast.CallExpr,
	want string,
) bool {
	for _, argument := range call.Args {
		if expressionString(fset, argument) == want {
			return true
		}
	}
	return false
}

func contains(items []string, candidate string) bool {
	for _, item := range items {
		if item == candidate {
			return true
		}
	}
	return false
}
