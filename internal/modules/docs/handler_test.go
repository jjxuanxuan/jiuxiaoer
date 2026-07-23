package docs

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-yaml"
)

// TestOpenAPIAndSwaggerRoutes 验证打开 API And Swagger Routes的预期行为。
func TestOpenAPIAndSwaggerRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router.Group("/api/v1"))

	openAPI := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/docs/openapi.yaml", nil)
	router.ServeHTTP(openAPI, req)
	if openAPI.Code != http.StatusOK {
		t.Fatalf("expected openapi status 200, got %d", openAPI.Code)
	}
	if !strings.Contains(openAPI.Body.String(), "openapi: 3.0.3") {
		t.Fatal("expected openapi document")
	}
	for _, required := range []string{
		"/store/orders/{id}/start-preparing:",
		"#/components/parameters/IdempotencyKey",
		"#/components/responses/Problem",
		"ProblemDetails:",
		"IDEMPOTENCY_KEY_REUSED",
		"IDEMPOTENCY_IN_PROGRESS",
		"AdminStockAdjustReq:",
		"/payments/{provider}/callbacks:",
		"PaymentCreateReq:",
		"/store/print-settings:",
		"/messages/read-all:",
		"/delivery/orders/{id}/pickup:",
		"/delivery/orders/{id}/route:",
		"/admin/merchants/provision:",
		"/auth/rider/send-code:",
		"/auth/rider/sms-login:",
		"/auth/rider-application/sms-login:",
		"/admin/riders:",
		"/admin/riders/{id}/review:",
		"RiderCreateReq:",
		"RiderReviewReq:",
		"/admin/deliveries/{id}/reassign:",
		"/identity-verifications:",
		"pickup_code: { type: string, pattern: \"^[0-9]{6}$\" }",
		"IdentitySessionReq:",
	} {
		if !strings.Contains(openAPI.Body.String(), required) {
			t.Fatalf("expected openapi document to contain %s", required)
		}
	}
	operationCount := len(regexp.MustCompile(`(?m)^    (get|post|put|patch|delete):$`).FindAllString(openAPI.Body.String(), -1))
	if count := strings.Count(openAPI.Body.String(), "default: { $ref: \"#/components/responses/Problem\" }"); count != operationCount {
		t.Fatalf("expected all %d operations to define the standard problem response, got %d", operationCount, count)
	}
	assertOpenAPIRefsResolve(t, openAPI.Body.Bytes())
	assertPhaseOneBusinessContracts(t, openAPI.Body.Bytes())

	swagger := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/swagger/index.html", nil)
	router.ServeHTTP(swagger, req)
	if swagger.Code != http.StatusOK {
		t.Fatalf("expected swagger status 200, got %d", swagger.Code)
	}
	if !strings.Contains(swagger.Body.String(), "SwaggerUIBundle") {
		t.Fatal("expected swagger ui html")
	}
}

type operationContract struct {
	method      string
	path        string
	permission  string
	objectScope string
	responseRef string
}

// assertPhaseOneBusinessContracts prevents the public document from falling
// back to permissive Page/Success schemas or losing the exact authorization
// boundary while handlers continue to enforce it.
func assertPhaseOneBusinessContracts(t *testing.T, content []byte) {
	t.Helper()
	var document map[string]any
	if err := yaml.Unmarshal(content, &document); err != nil {
		t.Fatalf("parse openapi document: %v", err)
	}

	contracts := []operationContract{
		{http.MethodGet, "/categories", "", "", "#/components/schemas/CategoryListEnvelope"},
		{http.MethodGet, "/products", "", "", "#/components/schemas/ProductListEnvelope"},
		{http.MethodGet, "/orders", "order:list", "customer.self_orders", "#/components/schemas/CustomerOrderPageEnvelope"},
		{http.MethodGet, "/store/orders", "store_order:list", "merchant_and_authorized_shop", "#/components/schemas/StoreOrderPageEnvelope"},
		{http.MethodGet, "/store/orders/{id}", "store_order:view", "merchant_and_authorized_shop", "#/components/schemas/StoreOrderDetailEnvelope"},
		{http.MethodPost, "/store/orders/{id}/accept", "store_order:accept", "merchant_and_authorized_shop", "#/components/schemas/StoreOrderDetailEnvelope"},
		{http.MethodPost, "/store/orders/{id}/start-preparing", "store_order:prepare", "merchant_and_authorized_shop", "#/components/schemas/StoreOrderDetailEnvelope"},
		{http.MethodPost, "/store/orders/{id}/prepare", "store_order:prepare", "merchant_and_authorized_shop", "#/components/schemas/StoreOrderDetailEnvelope"},
		{http.MethodPatch, "/store/shops/{id}/business-status", "shop:business_status", "authorized_shop", "#/components/schemas/StoreBusinessStatusEnvelope"},
		{http.MethodGet, "/store/shop-products", "inventory:view", "authorized_shop", "#/components/schemas/ShopProductPageEnvelope"},
		{http.MethodPost, "/store/shop-products", "shop_product:create", "authorized_shop", "#/components/schemas/ShopProductEnvelope"},
		{http.MethodPatch, "/store/shop-products/{id}", "shop_product:update", "authorized_shop", "#/components/schemas/ShopProductMutationEnvelope"},
		{http.MethodPatch, "/store/shop-products/{id}/stock", "inventory:adjust", "authorized_shop", "#/components/schemas/ShopProductEnvelope"},
		{http.MethodPut, "/delivery/riders/me/work-status", "rider_work_status:update", "rider_self", "#/components/schemas/RiderWorkStatusEnvelope"},
		{http.MethodPost, "/delivery/riders/me/heartbeat", "rider_location:update", "rider_self", "#/components/schemas/RiderHeartbeatEnvelope"},
		{http.MethodGet, "/delivery/orders/{id}/route", "delivery:route", "assigned_rider", ""},
		{http.MethodGet, "/delivery/offers", "delivery_offer:list", "rider_self_pending_offers", "#/components/schemas/DeliveryOfferPageEnvelope"},
		{http.MethodPost, "/delivery/offers/{id}/accept", "delivery_offer:accept", "rider_self_pending_offer", "#/components/schemas/DeliveryAssignmentEnvelope"},
		{http.MethodPost, "/delivery/offers/{id}/reject", "delivery_offer:reject", "rider_self_pending_offer", "#/components/schemas/DeliveryOfferRejectEnvelope"},
		{http.MethodGet, "/delivery/orders", "delivery:list", "eligible_candidate_or_assigned_rider", "#/components/schemas/DeliveryOrderPageEnvelope"},
		{http.MethodGet, "/delivery/orders/{id}", "delivery:view_own", "assigned_rider", "#/components/schemas/DeliveryDetailEnvelope"},
		{http.MethodPost, "/delivery/orders/{id}/accept", "delivery:accept", "eligible_rider", "#/components/schemas/DeliveryOrderEnvelope"},
		{http.MethodPost, "/delivery/orders/{id}/pickup", "delivery:pickup", "assigned_rider", "#/components/schemas/DeliveryOrderEnvelope"},
		{http.MethodPost, "/delivery/orders/{id}/complete", "delivery:complete", "assigned_rider", "#/components/schemas/DeliveryOrderEnvelope"},
		{http.MethodGet, "/store/print-settings", "print_setting:view_shop", "authorized_shop", "#/components/schemas/PrintSettingEnvelope"},
		{http.MethodPost, "/store/print-settings", "print_setting:update_shop", "authorized_shop", "#/components/schemas/PrintSettingEnvelope"},
		{http.MethodPatch, "/store/print-settings/{id}", "print_setting:update_shop", "authorized_shop", "#/components/schemas/PrintSettingEnvelope"},
		{http.MethodPost, "/store/print-settings/{id}/test", "print_setting:test_shop", "authorized_shop", "#/components/schemas/TestPrintEnvelope"},
		{http.MethodGet, "/store/print-tasks", "print_task:list_shop", "authorized_shop", "#/components/schemas/PrintTaskPageEnvelope"},
		{http.MethodGet, "/store/print-tasks/{id}", "print_task:list_shop", "authorized_shop", "#/components/schemas/PrintTaskEnvelope"},
		{http.MethodPost, "/store/print-tasks/{id}/reprint", "print_task:reprint_shop", "authorized_shop", "#/components/schemas/PrintTaskEnvelope"},
		{http.MethodGet, "/admin/print-tasks", "print_task:list_all", "global", "#/components/schemas/PrintTaskPageEnvelope"},
		{http.MethodPost, "/admin/print-tasks/{id}/retry", "print_task:retry_all", "global", "#/components/schemas/PrintTaskEnvelope"},
		{http.MethodGet, "/store/orders/{id}/verification", "delivery_verification:view_shop", "merchant_and_authorized_shop_ready_order", "#/components/schemas/PickupVerificationEnvelope"},
		{http.MethodGet, "/orders/{id}/verification", "delivery_verification:view_customer", "customer_self_delivering_order", "#/components/schemas/DeliveryVerificationEnvelope"},
		{http.MethodPost, "/admin/deliveries/{id}/verification/unlock", "delivery_verification:unlock", "admin_delivery", "#/components/schemas/VerificationUnlockEnvelope"},
	}
	for _, contract := range contracts {
		op := openAPIOperation(t, document, contract.path, contract.method)
		if got, _ := op["x-permission"].(string); got != contract.permission {
			t.Errorf("%s %s x-permission=%q, want %q", contract.method, contract.path, got, contract.permission)
		}
		if got, _ := op["x-object-scope"].(string); got != contract.objectScope {
			t.Errorf("%s %s x-object-scope=%q, want %q", contract.method, contract.path, got, contract.objectScope)
		}
		if contract.responseRef != "" {
			if got := operationResponseSchemaRef(t, op); got != contract.responseRef {
				t.Errorf("%s %s response schema=%q, want %q", contract.method, contract.path, got, contract.responseRef)
			}
		}
	}
	customerOrders := openAPIOperation(t, document, "/orders", http.MethodGet)
	assertOperationParameterRefs(t, customerOrders, []string{
		"#/components/parameters/PageSize",
		"#/components/parameters/PageToken",
	})
	assertOperationNamedParameters(t, customerOrders, []string{"status", "order_by"})

	product := openAPIOperation(t, document, "/products/{id}", http.MethodGet)
	if got := operationResponseSchemaRef(t, product); got != "#/components/schemas/ProductDetailEnvelope" {
		t.Errorf("product detail response schema=%q", got)
	}
	if got, _ := product["x-auth-mode"].(string); got != "public_optional_identity" {
		t.Errorf("product detail x-auth-mode=%q", got)
	}
	if got, _ := product["x-object-scope"].(string); got != "public_optional_location_context" {
		t.Errorf("product detail x-object-scope=%q", got)
	}
	for _, path := range []string{"/store/orders/{id}", "/delivery/orders/{id}", "/delivery/orders/{id}/route"} {
		op := openAPIOperation(t, document, path, http.MethodGet)
		if sensitive, _ := op["x-sensitive-response"].(bool); !sensitive {
			t.Errorf("GET %s must be marked as a sensitive response", path)
		}
	}
	for _, path := range []string{"/store/orders/{id}/accept", "/store/orders/{id}/start-preparing", "/store/orders/{id}/prepare"} {
		op := openAPIOperation(t, document, path, http.MethodPost)
		if got := operationRequestSchemaRef(t, op); got != "#/components/schemas/StoreOrderActionReq" {
			t.Errorf("POST %s request schema=%q, want StoreOrderActionReq", path, got)
		}
		requestBody := nestedMap(t, op, "requestBody")
		if required, _ := requestBody["required"].(bool); !required {
			t.Errorf("POST %s requestBody must be required", path)
		}
		responses := nestedMap(t, op, "responses")
		if _, ok := responses["409"]; !ok {
			t.Errorf("POST %s must document stable 409 conflicts", path)
		}
	}
	for _, path := range []string{"/store/orders/{id}/verification", "/orders/{id}/verification"} {
		op := openAPIOperation(t, document, path, http.MethodGet)
		if sensitive, _ := op["x-sensitive-response"].(bool); !sensitive {
			t.Errorf("GET %s must be marked as a sensitive response", path)
		}
		response := nestedMap(t, nestedMap(t, op, "responses"), "200")
		headers := nestedMap(t, response, "headers")
		cacheControl := nestedMap(t, headers, "Cache-Control")
		if required, _ := cacheControl["x-required-response-header"].(bool); !required {
			t.Errorf("GET %s must mark Cache-Control as required", path)
		}
		schema := nestedMap(t, cacheControl, "schema")
		values, _ := schema["enum"].([]any)
		if len(values) != 1 || values[0] != "no-store" {
			t.Errorf("GET %s must require Cache-Control: no-store", path)
		}
	}
	for _, path := range []string{"/delivery/orders/{id}/pickup", "/delivery/orders/{id}/complete"} {
		op := openAPIOperation(t, document, path, http.MethodPost)
		if got, _ := op["x-legacy-permission"].(string); got != "delivery:update_status" {
			t.Errorf("POST %s x-legacy-permission=%q", path, got)
		}
	}

	closedSchemas := []string{
		"Category", "CategoryListEnvelope", "ProductListEnvelope", "ProductDeliveryPolicy", "ProductDeliveryPromise", "ProductDetailServiceShop", "ProductDetailNoServiceShop", "ProductDetailEnvelope", "CartItem", "Cart",
		"ShopSummary", "OrderItemSummary", "OrderSummary", "CustomerOrderPageEnvelope",
		"StoreOrderActionReq", "StoreOrderSummary", "StoreOrderPageEnvelope", "StoreOrderDetail", "StoreOrderItem", "StoreOrderDetailEnvelope", "StoreBusinessStatusEnvelope",
		"ShopProduct", "ShopProductEnvelope", "ShopProductPageEnvelope", "ShopProductMutationEnvelope", "RiderWorkStatus", "RiderHeartbeat", "RiderWorkStatusEnvelope", "RiderHeartbeatEnvelope",
		"Coordinate", "RouteStep", "RoutePlan", "DeliveryOffer", "DeliveryAssignment", "DeliveryPickupSnapshot", "DeliveryRecipientSnapshot", "DeliveryProductSnapshot",
		"DeliveryCandidateSummary", "AssignedDeliverySummary", "DeliveryDetail", "DeliveryDetailItem", "DeliveryOfferPageEnvelope", "DeliveryAssignmentEnvelope", "DeliveryOfferRejectEnvelope", "DeliveryOrderEnvelope", "DeliveryOrderPageEnvelope", "DeliveryDetailEnvelope",
		"PrintSetting", "PrintRenderSummary", "PrintTask", "PrintTaskEnvelope", "PrintTaskPageEnvelope", "TestPrint", "PrintSettingEnvelope", "TestPrintEnvelope",
		"PickupVerification", "DeliveryVerification", "VerificationUnlock", "VerificationUnlockReq", "PickupVerificationEnvelope", "DeliveryVerificationEnvelope", "VerificationUnlockEnvelope",
		"BusinessStatusReq", "ShopProductCreateReq", "ShopProductUpdateReq", "StockAdjustReq",
		"DispatchOfferActionReq", "DispatchOfferRejectReq", "DispatchGrabReq", "ReasonReq",
	}
	schemas := nestedMap(t, nestedMap(t, document, "components"), "schemas")
	for _, name := range closedSchemas {
		schema := nestedMap(t, schemas, name)
		if closed, ok := schema["additionalProperties"].(bool); !ok || closed {
			t.Errorf("schema %s must set additionalProperties=false", name)
		}
	}
	assertPageItemsRef(t, schemas, "CategoryListEnvelope", "#/components/schemas/Category")
	assertPageItemsRef(t, schemas, "ProductListEnvelope", "#/components/schemas/ProductDetail")
	assertPageItemsRef(t, schemas, "CustomerOrderPageEnvelope", "#/components/schemas/OrderSummary")
	assertPageItemsRef(t, schemas, "StoreOrderPageEnvelope", "#/components/schemas/StoreOrderSummary")
	assertPageItemsRef(t, schemas, "ShopProductPageEnvelope", "#/components/schemas/ShopProduct")
	assertPageItemsRef(t, schemas, "DeliveryOrderPageEnvelope", "#/components/schemas/DeliveryOrder")
	assertPageItemsRef(t, schemas, "PrintTaskPageEnvelope", "#/components/schemas/PrintTask")
	assertSchemaRequiredContains(t, schemas, "StoreOrderSummary", []string{
		"shop_summary", "item_summary", "item_kind_count", "total_quantity", "address_summary", "customer_contact_mask", "has_remark", "version", "updated_at",
	})
	assertSchemaRequiredContains(t, schemas, "StoreOrderActionReq", []string{"expected_version"})
	assertEnvelopeDataRequiredContains(t, schemas, "CategoryListEnvelope", []string{"items"})
	assertEnvelopeDataRequiredContains(t, schemas, "ProductListEnvelope", []string{"items", "next_page_token"})
	assertEnvelopeDataRequiredContains(t, schemas, "CustomerOrderPageEnvelope", []string{"items", "next_page_token"})
	assertEnvelopeDataOmitsProperties(t, schemas, "CategoryListEnvelope", []string{"next_page_token"})
	assertSchemaRequiredContains(t, schemas, "OrderSummary", []string{
		"shop_summary", "item_summary", "item_kind_count", "total_quantity", "updated_at",
	})
	assertSchemaRequiredContains(t, schemas, "CartItem", []string{"availability_status", "available", "unavailable_reason"})
	assertSchemaRequiredContains(t, schemas, "ShopProduct", []string{
		"shop_product_id", "available_qty", "reserved_qty", "locked_qty", "total_qty", "low_stock_threshold", "low_stock", "version", "updated_at",
	})
	assertSchemaRequiredContains(t, schemas, "PrintTask", []string{"payload_schema_version"})
	assertSchemaRequiredContains(t, schemas, "PrintRenderSummary", []string{"item_kind_count", "total_quantity", "payable_amount", "paper_width_mm", "content_hash"})
	assertSchemaRequiredContains(t, schemas, "DeliveryCandidateSummary", []string{"view_type"})
	assertSchemaRequiredContains(t, schemas, "AssignedDeliverySummary", []string{"view_type"})
	assertSchemaHasProperties(t, schemas, "PrintTask", []string{"payload_schema_version", "source_task_id", "provider_status", "render_summary"})
	assertSchemaHasProperties(t, schemas, "DeliveryPickupSnapshot", []string{"shop_id", "phone", "latitude", "longitude", "coordinate_system"})
	assertSchemaHasProperties(t, schemas, "DeliveryRecipientSnapshot", []string{"contact_name", "contact_phone", "formatted_address", "latitude", "longitude", "location_source"})
	assertSchemaOmitsProperties(t, schemas, "StoreOrderSummary", []string{"customer_id", "merchant_id", "items", "address_snapshot"})
	assertSchemaOmitsProperties(t, schemas, "StoreOrderDetail", []string{"customer_id", "merchant_id"})
	assertSchemaOmitsProperties(t, schemas, "PrintSetting", []string{"device_id", "device_id_ciphertext", "provider_config_ref"})
	assertSchemaOmitsProperties(t, schemas, "PrintTask", []string{"render_payload", "last_error_safe", "locked_by"})
	assertSchemaOmitsProperties(t, schemas, "VerificationUnlock", []string{"code"})
	assertSchemaOmitsProperties(t, schemas, "DeliveryCandidateSummary", []string{"rider_id", "pickup_snapshot", "recipient_snapshot"})

	assertSchemaEnum(t, schemas, "PickupVerification", "stage", []string{"pickup"})
	assertSchemaEnum(t, schemas, "DeliveryVerification", "stage", []string{"delivery"})
	assertSchemaEnum(t, schemas, "CartItem", "availability_status", []string{"available", "not_on_sale", "out_of_stock", "shop_closed"})
	assertSchemaEnum(t, schemas, "CartItem", "unavailable_reason", []string{"not_on_sale", "out_of_stock", "shop_closed"})
	assertSchemaEnum(t, schemas, "DeliveryCandidateSummary", "view_type", []string{"candidate"})
	assertSchemaEnum(t, schemas, "AssignedDeliverySummary", "view_type", []string{"assigned"})
	deliveryOrder := nestedMap(t, schemas, "DeliveryOrder")
	discriminator := nestedMap(t, deliveryOrder, "discriminator")
	if got, _ := discriminator["propertyName"].(string); got != "view_type" {
		t.Errorf("DeliveryOrder discriminator propertyName=%q, want view_type", got)
	}
	cartItem := nestedMap(t, schemas, "CartItem")
	cartItemProperties := nestedMap(t, cartItem, "properties")
	unavailableReason := nestedMap(t, cartItemProperties, "unavailable_reason")
	if nullable, _ := unavailableReason["nullable"].(bool); !nullable {
		t.Error("CartItem.unavailable_reason must be nullable when available=true")
	}
}

func openAPIOperation(t *testing.T, document map[string]any, path, method string) map[string]any {
	t.Helper()
	paths := nestedMap(t, document, "paths")
	item := nestedMap(t, paths, path)
	return nestedMap(t, item, strings.ToLower(method))
}

func operationResponseSchemaRef(t *testing.T, operation map[string]any) string {
	t.Helper()
	responses := nestedMap(t, operation, "responses")
	response := nestedMap(t, responses, "200")
	content := nestedMap(t, response, "content")
	mediaType := nestedMap(t, content, "application/json")
	schema := nestedMap(t, mediaType, "schema")
	ref, _ := schema["$ref"].(string)
	return ref
}

func operationRequestSchemaRef(t *testing.T, operation map[string]any) string {
	t.Helper()
	requestBody := nestedMap(t, operation, "requestBody")
	content := nestedMap(t, requestBody, "content")
	mediaType := nestedMap(t, content, "application/json")
	schema := nestedMap(t, mediaType, "schema")
	ref, _ := schema["$ref"].(string)
	return ref
}

func assertOperationParameterRefs(t *testing.T, operation map[string]any, want []string) {
	t.Helper()
	parameters, ok := operation["parameters"].([]any)
	if !ok {
		t.Fatal("OpenAPI operation parameters are missing or not an array")
	}
	got := make(map[string]struct{}, len(parameters))
	for _, value := range parameters {
		parameter, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if ref, ok := parameter["$ref"].(string); ok {
			got[ref] = struct{}{}
		}
	}
	for _, ref := range want {
		if _, ok := got[ref]; !ok {
			t.Errorf("operation must declare parameter ref %s", ref)
		}
	}
}

func assertOperationNamedParameters(t *testing.T, operation map[string]any, want []string) {
	t.Helper()
	parameters, ok := operation["parameters"].([]any)
	if !ok {
		t.Fatal("OpenAPI operation parameters are missing or not an array")
	}
	got := make(map[string]struct{}, len(parameters))
	for _, value := range parameters {
		parameter, ok := value.(map[string]any)
		if !ok || parameter["in"] != "query" {
			continue
		}
		if name, ok := parameter["name"].(string); ok {
			got[name] = struct{}{}
		}
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("operation must declare query parameter %s", name)
		}
	}
}

func nestedMap(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := parent[key].(map[string]any)
	if !ok {
		t.Fatalf("OpenAPI key %q is missing or not an object", key)
	}
	return value
}

func assertSchemaEnum(t *testing.T, schemas map[string]any, schemaName, property string, want []string) {
	t.Helper()
	schema := nestedMap(t, schemas, schemaName)
	properties := nestedMap(t, schema, "properties")
	propertySchema := nestedMap(t, properties, property)
	values, _ := propertySchema["enum"].([]any)
	if len(values) != len(want) {
		t.Errorf("schema %s.%s enum=%v, want %v", schemaName, property, values, want)
		return
	}
	for index := range want {
		if values[index] != want[index] {
			t.Errorf("schema %s.%s enum=%v, want %v", schemaName, property, values, want)
			return
		}
	}
}

func assertPageItemsRef(t *testing.T, schemas map[string]any, schemaName, want string) {
	t.Helper()
	schema := nestedMap(t, schemas, schemaName)
	data := nestedMap(t, nestedMap(t, schema, "properties"), "data")
	items := nestedMap(t, nestedMap(t, data, "properties"), "items")
	itemSchema := nestedMap(t, items, "items")
	if got, _ := itemSchema["$ref"].(string); got != want {
		t.Errorf("schema %s page item ref=%q, want %q", schemaName, got, want)
	}
}

func assertSchemaRequiredContains(t *testing.T, schemas map[string]any, schemaName string, want []string) {
	t.Helper()
	schema := nestedMap(t, schemas, schemaName)
	values, _ := schema["required"].([]any)
	got := make(map[string]struct{}, len(values))
	for _, value := range values {
		if field, ok := value.(string); ok {
			got[field] = struct{}{}
		}
	}
	for _, field := range want {
		if _, ok := got[field]; !ok {
			t.Errorf("schema %s must require property %s", schemaName, field)
		}
	}
}

func assertEnvelopeDataRequiredContains(t *testing.T, schemas map[string]any, schemaName string, want []string) {
	t.Helper()
	schema := nestedMap(t, schemas, schemaName)
	data := nestedMap(t, nestedMap(t, schema, "properties"), "data")
	values, _ := data["required"].([]any)
	got := make(map[string]struct{}, len(values))
	for _, value := range values {
		if field, ok := value.(string); ok {
			got[field] = struct{}{}
		}
	}
	for _, field := range want {
		if _, ok := got[field]; !ok {
			t.Errorf("schema %s.data must require property %s", schemaName, field)
		}
	}
}

func assertEnvelopeDataOmitsProperties(t *testing.T, schemas map[string]any, schemaName string, forbidden []string) {
	t.Helper()
	schema := nestedMap(t, schemas, schemaName)
	data := nestedMap(t, nestedMap(t, schema, "properties"), "data")
	properties := nestedMap(t, data, "properties")
	for _, field := range forbidden {
		if _, ok := properties[field]; ok {
			t.Errorf("schema %s.data must not expose property %s", schemaName, field)
		}
	}
}

func assertSchemaHasProperties(t *testing.T, schemas map[string]any, schemaName string, want []string) {
	t.Helper()
	properties := nestedMap(t, nestedMap(t, schemas, schemaName), "properties")
	for _, field := range want {
		if _, ok := properties[field]; !ok {
			t.Errorf("schema %s must expose property %s", schemaName, field)
		}
	}
}

func assertSchemaOmitsProperties(t *testing.T, schemas map[string]any, schemaName string, forbidden []string) {
	t.Helper()
	properties := nestedMap(t, nestedMap(t, schemas, schemaName), "properties")
	for _, field := range forbidden {
		if _, ok := properties[field]; ok {
			t.Errorf("schema %s must not expose property %s", schemaName, field)
		}
	}
}

// assertOpenAPIRefsResolve 防止文档语法合法、但组件引用路径无效。
func assertOpenAPIRefsResolve(t *testing.T, content []byte) {
	t.Helper()
	var document map[string]any
	if err := yaml.Unmarshal(content, &document); err != nil {
		t.Fatalf("parse openapi document: %v", err)
	}

	var walk func(any)
	walk = func(value any) {
		switch current := value.(type) {
		case map[string]any:
			for key, child := range current {
				if key == "$ref" {
					ref, ok := child.(string)
					if !ok || !strings.HasPrefix(ref, "#/components/") || !componentRefExists(document, ref) {
						t.Errorf("unresolved component reference: %v", child)
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range current {
				walk(child)
			}
		}
	}
	walk(document)
}

// componentRefExists 判断component Ref Exists。
func componentRefExists(document map[string]any, ref string) bool {
	current := any(document)
	for _, segment := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		object, ok := current.(map[string]any)
		if !ok {
			return false
		}
		current, ok = object[segment]
		if !ok {
			return false
		}
	}
	return true
}
