// Command check-architecture 强制执行无需猜测业务意图即可验证的模块边界。
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const defaultTestLineLimit = 800

const (
	wineTicketImportPath   = "jiuxiaoer-admin/backend-go/internal/modules/wineticket"
	wineTicketRelativeRoot = "internal/modules/wineticket"
)

var wineTicketRootProductionFiles = map[string]struct{}{
	"contracts.go": {},
	"module.go":    {},
}

var wineTicketChildOwnedModels = map[string]string{
	"PurchaseQuota":                   "purchase",
	"Purchase":                        "purchase",
	"DeliveryTimeSlot":                "redemption",
	"Redemption":                      "redemption",
	"RedemptionAllocation":            "redemption",
	"Gift":                            "gift",
	"GiftAllocation":                  "gift",
	"GiftClaimToken":                  "gift",
	"Renewal":                         "renewal",
	"WineTicketRefund":                "refund",
	"RefundAllocation":                "refund",
	"Reminder":                        "reminder",
	"NotificationSubscriptionConsent": "reminder",
	"Exception":                       "integrity",
}

type finding struct {
	path    string
	line    int
	message string
}

func main() {
	root := flag.String("root", ".", "repository root")
	testLineLimit := flag.Int("test-line-limit", defaultTestLineLimit, "soft line limit for test files; zero disables warnings")
	flag.Parse()

	if err := check(*root, *testLineLimit, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func check(root string, testLineLimit int, stdout, stderr io.Writer) error {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve repository root: %w", err)
	}
	moduleRoot := filepath.Join(absoluteRoot, "internal", "modules")
	if stat, statErr := os.Stat(moduleRoot); statErr != nil || !stat.IsDir() {
		return fmt.Errorf("module root not found: %s", moduleRoot)
	}
	scanRoots := []string{moduleRoot}
	packageRoot := filepath.Join(absoluteRoot, "internal", "pkg")
	if stat, statErr := os.Stat(packageRoot); statErr == nil && stat.IsDir() {
		scanRoots = append(scanRoots, packageRoot)
	}

	var errorsFound []finding
	var largeTests []finding
	for _, scanRoot := range scanRoots {
		err = filepath.WalkDir(scanRoot, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" {
				return nil
			}

			relativePath, relativeErr := filepath.Rel(absoluteRoot, path)
			if relativeErr != nil {
				return relativeErr
			}
			relativePath = filepath.ToSlash(relativePath)

			if strings.HasSuffix(path, "_test.go") {
				if testLineLimit > 0 {
					lines, lineErr := lineCount(path)
					if lineErr != nil {
						return lineErr
					}
					if lines > testLineLimit {
						largeTests = append(largeTests, finding{
							path:    relativePath,
							line:    lines,
							message: fmt.Sprintf("test file exceeds the suggested %d-line limit", testLineLimit),
						})
					}
				}
				return nil
			}

			fileSet := token.NewFileSet()
			parsed, parseErr := parser.ParseFile(fileSet, path, nil, 0)
			if parseErr != nil {
				return fmt.Errorf("parse %s: %w", relativePath, parseErr)
			}
			errorsFound = append(errorsFound, productionFindings(fileSet, parsed, relativePath)...)
			return nil
		})
		if err != nil {
			return fmt.Errorf("scan %s: %w", scanRoot, err)
		}
	}
	routerPath := filepath.Join(absoluteRoot, "internal", "app", "router.go")
	if stat, statErr := os.Stat(routerPath); statErr == nil && !stat.IsDir() {
		fileSet := token.NewFileSet()
		parsed, parseErr := parser.ParseFile(fileSet, routerPath, nil, 0)
		if parseErr != nil {
			return fmt.Errorf("parse internal/app/router.go: %w", parseErr)
		}
		for _, item := range wineTicketRouterFindings(parsed) {
			errorsFound = append(errorsFound, finding{
				path:    "internal/app/router.go",
				line:    fileSet.Position(item.position).Line,
				message: item.message,
			})
		}
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("stat internal/app/router.go: %w", statErr)
	}

	sortFindings(errorsFound)
	sort.Slice(largeTests, func(i, j int) bool {
		if largeTests[i].line == largeTests[j].line {
			return largeTests[i].path < largeTests[j].path
		}
		return largeTests[i].line > largeTests[j].line
	})

	for _, item := range errorsFound {
		fmt.Fprintf(stderr, "ERROR %s:%d: %s\n", item.path, item.line, item.message)
	}
	if len(largeTests) > 0 {
		fmt.Fprintf(stdout, "WARN %d test file(s) exceed the suggested %d-line limit:\n", len(largeTests), testLineLimit)
		for _, item := range largeTests {
			fmt.Fprintf(stdout, "  %s (%d lines)\n", item.path, item.line)
		}
	}
	if len(errorsFound) > 0 {
		return fmt.Errorf("architecture check failed with %d error(s)", len(errorsFound))
	}

	fmt.Fprintln(stdout, "architecture check passed")
	return nil
}

func productionFindings(fileSet *token.FileSet, file *ast.File, path string) []finding {
	var findings []finding
	seen := make(map[string]struct{})
	add := func(position token.Pos, message string) {
		line := fileSet.Position(position).Line
		key := strconv.Itoa(line) + "\x00" + message
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		findings = append(findings, finding{path: path, line: line, message: message})
	}
	for _, item := range wineTicketPackageBoundaryFindings(file, path) {
		add(item.position, item.message)
	}
	for _, item := range wineTicketCoreOwnershipFindings(file, path) {
		add(item.position, item.message)
	}
	for _, item := range wineTicketTypeAliasFindings(file, path) {
		add(item.position, item.message)
	}
	for _, item := range wineTicketAssetServiceBoundaryFindings(file, path) {
		add(item.position, item.message)
	}
	for _, item := range wineTicketAssetMutationFindings(file, path) {
		add(item.position, item.message)
	}

	ast.Inspect(file, func(node ast.Node) bool {
		switch current := node.(type) {
		case *ast.SelectorExpr:
			if current.Sel.Name == "Dialector" {
				add(current.Pos(), "production module must not branch on a database dialector")
			}
		case *ast.BasicLit:
			if current.Kind != token.STRING {
				break
			}
			value, err := strconv.Unquote(current.Value)
			if err == nil && strings.Contains(strings.ToLower(value), "sqlite") {
				add(current.Pos(), "SQLite-specific behavior belongs in test code, not a production module")
			}
		case *ast.FuncDecl:
			if !isServiceConstructor(current.Name.Name) || current.Body == nil {
				break
			}
			ast.Inspect(current.Body, func(child ast.Node) bool {
				call, ok := child.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee := calledName(call.Fun)
				if callee != "" &&
					callee != current.Name.Name &&
					callee != "NewAssetService" &&
					isServiceConstructor(callee) {
					add(call.Pos(), "inject peer services; do not construct one Service inside another Service constructor")
				}
				return true
			})
			return false
		}
		return true
	})
	if strings.HasPrefix(path, "internal/modules/wineticket/") {
		for _, item := range servicePersistenceFindings(file, path) {
			add(item.position, item.message)
		}
		if isWineTicketServiceResponsibilityFile(path) {
			for _, item := range freeFunctionPersistenceFindings(file) {
				add(item.position, item.message)
			}
		}
	}
	return findings
}

func wineTicketTypeAliasFindings(
	file *ast.File,
	path string,
) []positionedFinding {
	if !strings.HasPrefix(path, wineTicketRelativeRoot+"/") {
		return nil
	}
	var findings []positionedFinding
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.TYPE {
			continue
		}
		for _, specification := range general.Specs {
			typeSpecification, ok := specification.(*ast.TypeSpec)
			if !ok || !typeSpecification.Assign.IsValid() {
				continue
			}
			findings = append(findings, positionedFinding{
				position: typeSpecification.Pos(),
				message: "wine-ticket production packages must use the " +
					"owning package type directly, not forwarding aliases",
			})
		}
	}
	return findings
}

func wineTicketAssetServiceBoundaryFindings(
	file *ast.File,
	path string,
) []positionedFinding {
	if path != wineTicketRelativeRoot+"/core/asset_service.go" {
		return nil
	}
	var findings []positionedFinding
	for _, specification := range file.Imports {
		if importPath(specification) != "gorm.io/gorm" {
			continue
		}
		findings = append(findings, positionedFinding{
			position: specification.Pos(),
			message: "core.AssetService must depend on asset repository " +
				"ports, not GORM",
		})
	}
	return findings
}

func wineTicketAssetMutationFindings(
	file *ast.File,
	path string,
) []positionedFinding {
	if !strings.HasPrefix(path, wineTicketRelativeRoot+"/") ||
		strings.HasPrefix(path, wineTicketRelativeRoot+"/core/") ||
		!isWineTicketServiceResponsibilityFile(path) {
		return nil
	}
	var findings []positionedFinding
	ast.Inspect(file, func(node ast.Node) bool {
		switch current := node.(type) {
		case *ast.CompositeLit:
			if expressionTypeName(current.Type) == "Transaction" {
				findings = append(findings, positionedFinding{
					position: current.Pos(),
					message:  "wine-ticket entitlement ledger entries must be created through core.AssetService",
				})
			}
		case *ast.BasicLit:
			if current.Kind != token.STRING {
				break
			}
			value, err := strconv.Unquote(current.Value)
			if err == nil && value == "available_quantity" {
				findings = append(findings, positionedFinding{
					position: current.Pos(),
					message:  "wine-ticket available balance mutations must be delegated to core.AssetService",
				})
			}
		}
		return true
	})
	return findings
}

func expressionTypeName(expression ast.Expr) string {
	switch current := expression.(type) {
	case *ast.Ident:
		return current.Name
	case *ast.SelectorExpr:
		return current.Sel.Name
	default:
		return ""
	}
}

func wineTicketCoreOwnershipFindings(
	file *ast.File,
	path string,
) []positionedFinding {
	if !strings.HasPrefix(
		path,
		wineTicketRelativeRoot+"/core/",
	) {
		return nil
	}
	var findings []positionedFinding
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.TYPE {
			continue
		}
		for _, specification := range general.Specs {
			typeSpecification, ok := specification.(*ast.TypeSpec)
			if !ok {
				continue
			}
			owner, childOwned := wineTicketChildOwnedModels[typeSpecification.Name.Name]
			if !childOwned {
				continue
			}
			findings = append(findings, positionedFinding{
				position: typeSpecification.Pos(),
				message: fmt.Sprintf(
					"wine-ticket %s model %s must be declared in its owning subpackage, not core",
					owner,
					typeSpecification.Name.Name,
				),
			})
		}
	}
	return findings
}

type positionedFinding struct {
	position token.Pos
	message  string
}

func wineTicketPackageBoundaryFindings(
	file *ast.File,
	path string,
) []positionedFinding {
	if !strings.HasPrefix(path, wineTicketRelativeRoot+"/") {
		return nil
	}

	directory := filepath.ToSlash(filepath.Dir(path))
	if directory == wineTicketRelativeRoot {
		if _, allowed := wineTicketRootProductionFiles[filepath.Base(path)]; allowed {
			return nil
		}
		return []positionedFinding{{
			position: file.Package,
			message: "wine-ticket root package may contain only module.go and " +
				"contracts.go; move implementation into a domain subpackage",
		}}
	}

	var findings []positionedFinding
	for _, specification := range file.Imports {
		if importPath(specification) != wineTicketImportPath {
			continue
		}
		findings = append(findings, positionedFinding{
			position: specification.Pos(),
			message: "wine-ticket subpackages must not import the parent " +
				"wineticket package",
		})
	}
	return findings
}

func wineTicketRouterFindings(file *ast.File) []positionedFinding {
	var findings []positionedFinding
	childPrefix := wineTicketImportPath + "/"
	for _, specification := range file.Imports {
		if !strings.HasPrefix(importPath(specification), childPrefix) {
			continue
		}
		findings = append(findings, positionedFinding{
			position: specification.Pos(),
			message: "internal/app/router.go must depend only on the " +
				"wineticket composition root, not a wineticket subpackage",
		})
	}
	return findings
}

func importPath(specification *ast.ImportSpec) string {
	if specification == nil || specification.Path == nil {
		return ""
	}
	value, err := strconv.Unquote(specification.Path.Value)
	if err != nil {
		return ""
	}
	return value
}

var directGORMPersistenceMethods = map[string]struct{}{
	"Count":           {},
	"Create":          {},
	"CreateInBatches": {},
	"Delete":          {},
	"Exec":            {},
	"Find":            {},
	"FindInBatches":   {},
	"First":           {},
	"FirstOrCreate":   {},
	"Model":           {},
	"Pluck":           {},
	"Raw":             {},
	"Row":             {},
	"Rows":            {},
	"Save":            {},
	"Scan":            {},
	"Table":           {},
	"Take":            {},
	"Update":          {},
	"UpdateColumn":    {},
	"UpdateColumns":   {},
	"Updates":         {},
	"Where":           {},
}

func servicePersistenceFindings(file *ast.File, path string) []positionedFinding {
	var findings []positionedFinding
	checkedReceivers := persistenceBoundaryReceiverTypes(file, path)
	for _, declaration := range file.Decls {
		method, ok := declaration.(*ast.FuncDecl)
		if !ok || method.Body == nil ||
			!isPersistenceBoundaryReceiver(method, checkedReceivers) {
			continue
		}

		receiverName := method.Recv.List[0].Names[0].Name
		databases := gormDBParameterNames(method)
		collectNestedGORMDBParameterNames(method.Body, databases)
		collectDerivedGORMVariables(method.Body, receiverName, databases)

		ast.Inspect(method.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if _, forbidden := directGORMPersistenceMethods[selector.Sel.Name]; !forbidden {
				return true
			}
			if !gormExpressionRootedAtServiceDB(
				selector.X,
				receiverName,
				databases,
			) {
				return true
			}
			findings = append(findings, positionedFinding{
				position: call.Pos(),
				message:  "wine-ticket orchestration methods must delegate GORM persistence to a repository",
			})
			return true
		})
	}
	return findings
}

func persistenceBoundaryReceiverTypes(
	file *ast.File,
	path string,
) map[string]struct{} {
	result := make(map[string]struct{})
	methods := make(map[string]map[string]struct{})
	settlementFile := strings.HasSuffix(
		filepath.Base(path),
		"_settlement.go",
	)
	for _, declaration := range file.Decls {
		method, ok := declaration.(*ast.FuncDecl)
		if !ok || method.Recv == nil || len(method.Recv.List) != 1 {
			continue
		}
		typeName := receiverTypeName(method.Recv.List[0].Type)
		if typeName == "" {
			continue
		}
		if typeName == "serviceCore" ||
			strings.HasSuffix(typeName, "Service") ||
			strings.HasSuffix(typeName, "Worker") {
			result[typeName] = struct{}{}
		}
		if settlementFile && !strings.HasSuffix(typeName, "Repository") {
			result[typeName] = struct{}{}
		}
		if methods[typeName] == nil {
			methods[typeName] = make(map[string]struct{})
		}
		methods[typeName][method.Name.Name] = struct{}{}
	}
	for typeName, methodSet := range methods {
		_, hasBusinessType := methodSet["BusinessType"]
		_, hasLockAndApply := methodSet["LockAndApply"]
		if hasBusinessType && hasLockAndApply {
			result[typeName] = struct{}{}
		}
	}
	return result
}

func isPersistenceBoundaryReceiver(
	method *ast.FuncDecl,
	checked map[string]struct{},
) bool {
	if method.Recv == nil || len(method.Recv.List) != 1 ||
		len(method.Recv.List[0].Names) != 1 {
		return false
	}
	_, ok := checked[receiverTypeName(method.Recv.List[0].Type)]
	return ok
}

func gormDBParameterNames(method *ast.FuncDecl) map[string]struct{} {
	result := make(map[string]struct{})
	if method.Type.Params == nil {
		return result
	}
	for _, field := range method.Type.Params.List {
		if !isGORMDBType(field.Type) {
			continue
		}
		for _, name := range field.Names {
			result[name.Name] = struct{}{}
		}
	}
	return result
}

func collectNestedGORMDBParameterNames(
	body *ast.BlockStmt,
	databases map[string]struct{},
) {
	ast.Inspect(body, func(node ast.Node) bool {
		function, ok := node.(*ast.FuncLit)
		if !ok || function.Type.Params == nil {
			return true
		}
		for _, field := range function.Type.Params.List {
			if !isGORMDBType(field.Type) {
				continue
			}
			for _, name := range field.Names {
				databases[name.Name] = struct{}{}
			}
		}
		return true
	})
}

func isGORMDBType(expression ast.Expr) bool {
	pointer, ok := expression.(*ast.StarExpr)
	if !ok {
		return false
	}
	selector, ok := pointer.X.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "DB" {
		return false
	}
	packageName, ok := selector.X.(*ast.Ident)
	return ok && packageName.Name == "gorm"
}

func freeFunctionPersistenceFindings(file *ast.File) []positionedFinding {
	var findings []positionedFinding
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv != nil || function.Body == nil {
			continue
		}
		databases := gormDBNamesFromFields(function.Type.Params)
		collectNestedGORMDBParameterNames(function.Body, databases)
		orchestrationOwners := orchestrationParameterNames(function.Type.Params)
		collectDerivedGORMVariables(function.Body, "", databases)
		for owner := range orchestrationOwners {
			collectDerivedGORMVariables(function.Body, owner, databases)
		}

		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if _, forbidden := directGORMPersistenceMethods[selector.Sel.Name]; !forbidden {
				return true
			}
			rooted := gormExpressionRootedAtServiceDB(
				selector.X,
				"",
				databases,
			)
			for owner := range orchestrationOwners {
				rooted = rooted || gormExpressionRootedAtServiceDB(
					selector.X,
					owner,
					databases,
				)
			}
			if rooted {
				findings = append(findings, positionedFinding{
					position: call.Pos(),
					message:  "wine-ticket orchestration helpers must delegate GORM persistence to a repository",
				})
			}
			return true
		})
	}
	return findings
}

func gormDBNamesFromFields(fields *ast.FieldList) map[string]struct{} {
	result := make(map[string]struct{})
	if fields == nil {
		return result
	}
	for _, field := range fields.List {
		if !isGORMDBType(field.Type) {
			continue
		}
		for _, name := range field.Names {
			result[name.Name] = struct{}{}
		}
	}
	return result
}

func orchestrationParameterNames(fields *ast.FieldList) map[string]struct{} {
	result := make(map[string]struct{})
	if fields == nil {
		return result
	}
	for _, field := range fields.List {
		typeName := receiverTypeName(field.Type)
		if typeName != "serviceCore" &&
			!strings.HasSuffix(typeName, "Service") &&
			!strings.HasSuffix(typeName, "Worker") {
			continue
		}
		for _, name := range field.Names {
			result[name.Name] = struct{}{}
		}
	}
	return result
}

func receiverTypeName(expression ast.Expr) string {
	if pointer, ok := expression.(*ast.StarExpr); ok {
		expression = pointer.X
	}
	identifier, _ := expression.(*ast.Ident)
	if identifier == nil {
		return ""
	}
	return identifier.Name
}

func isWineTicketServiceResponsibilityFile(path string) bool {
	base := filepath.Base(path)
	return base == "core.go" ||
		base == "expiry_helper.go" ||
		base == "service.go" ||
		strings.HasSuffix(base, "_service.go") ||
		strings.HasSuffix(base, "_settlement.go") ||
		strings.HasSuffix(base, "_worker.go")
}

func collectDerivedGORMVariables(
	body *ast.BlockStmt,
	receiverName string,
	databases map[string]struct{},
) {
	changed := true
	for changed {
		changed = false
		ast.Inspect(body, func(node ast.Node) bool {
			switch statement := node.(type) {
			case *ast.AssignStmt:
				for index, right := range statement.Rhs {
					if !gormExpressionRootedAtServiceDB(
						right,
						receiverName,
						databases,
					) {
						continue
					}
					if index >= len(statement.Lhs) {
						continue
					}
					identifier, ok := statement.Lhs[index].(*ast.Ident)
					if !ok {
						continue
					}
					if _, exists := databases[identifier.Name]; !exists {
						databases[identifier.Name] = struct{}{}
						changed = true
					}
				}
			case *ast.DeclStmt:
				declaration, ok := statement.Decl.(*ast.GenDecl)
				if !ok {
					break
				}
				for _, specification := range declaration.Specs {
					value, ok := specification.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for index, right := range value.Values {
						if index >= len(value.Names) ||
							!gormExpressionRootedAtServiceDB(
								right,
								receiverName,
								databases,
							) {
							continue
						}
						name := value.Names[index].Name
						if _, exists := databases[name]; !exists {
							databases[name] = struct{}{}
							changed = true
						}
					}
				}
			}
			return true
		})
	}
}

func gormExpressionRootedAtServiceDB(
	expression ast.Expr,
	receiverName string,
	databases map[string]struct{},
) bool {
	switch current := expression.(type) {
	case *ast.Ident:
		_, ok := databases[current.Name]
		return ok
	case *ast.SelectorExpr:
		if (current.Sel.Name == "db" || current.Sel.Name == "DB") &&
			selectorChainRootedAtOwner(current.X, receiverName) {
			return true
		}
		return gormExpressionRootedAtServiceDB(
			current.X,
			receiverName,
			databases,
		)
	case *ast.CallExpr:
		if selector, ok := current.Fun.(*ast.SelectorExpr); ok {
			if (selector.Sel.Name == "DB" ||
				selector.Sel.Name == "dbConn") &&
				isServiceRepositoryExpression(selector.X, receiverName) {
				return true
			}
			return gormExpressionRootedAtServiceDB(
				selector.X,
				receiverName,
				databases,
			)
		}
	case *ast.ParenExpr:
		return gormExpressionRootedAtServiceDB(
			current.X,
			receiverName,
			databases,
		)
	}
	return false
}

func isServiceRepositoryExpression(
	expression ast.Expr,
	receiverName string,
) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok ||
		!strings.HasSuffix(
			strings.ToLower(selector.Sel.Name),
			"repo",
		) {
		return false
	}
	return selectorChainRootedAtOwner(selector.X, receiverName)
}

func selectorChainRootedAtOwner(
	expression ast.Expr,
	ownerName string,
) bool {
	switch current := expression.(type) {
	case *ast.Ident:
		return ownerName != "" && current.Name == ownerName
	case *ast.SelectorExpr:
		return selectorChainRootedAtOwner(current.X, ownerName)
	case *ast.ParenExpr:
		return selectorChainRootedAtOwner(current.X, ownerName)
	}
	return false
}

func calledName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		return value.Sel.Name
	default:
		return ""
	}
}

func isServiceConstructor(name string) bool {
	return strings.HasPrefix(name, "New") && strings.HasSuffix(name, "Service") && len(name) >= len("NewService")
}

func lineCount(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	count := 0
	for scanner.Scan() {
		count++
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	return count, nil
}

func sortFindings(findings []finding) {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].path != findings[j].path {
			return findings[i].path < findings[j].path
		}
		if findings[i].line != findings[j].line {
			return findings[i].line < findings[j].line
		}
		return findings[i].message < findings[j].message
	})
}
