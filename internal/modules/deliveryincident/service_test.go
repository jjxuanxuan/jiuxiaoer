package deliveryincident

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/evidencetoken"
	"jiuxiaoer-admin/backend-go/internal/pkg/evidenceview"
	"jiuxiaoer-admin/backend-go/internal/pkg/fixedwindow"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestStageAndTypeRules(t *testing.T) {
	for status, want := range map[string]string{"accepted": StagePickup, "delivering": StageDelivery} {
		got, err := stageFor(status)
		if err != nil || got != want {
			t.Fatalf("stageFor(%q) = %q, %v; want %q", status, got, err, want)
		}
	}
	for _, status := range []string{"pending_assign", "completed", "cancelled"} {
		if _, err := stageFor(status); err == nil {
			t.Fatalf("terminal or unsupported status %q was accepted", status)
		}
	}

	want := map[string]map[string]bool{
		StagePickup: {
			TypeOutOfStock: true, TypeAlcoholDamaged: true,
		},
		StageDelivery: {
			TypeAlcoholDamaged: true, TypeCustomerRefused: true, TypeCustomerUnreachable: true,
		},
	}
	for _, stage := range []string{StagePickup, StageDelivery} {
		for _, incidentType := range []string{TypeOutOfStock, TypeAlcoholDamaged, TypeCustomerRefused, TypeCustomerUnreachable} {
			if got := typeAllowedAtStage(incidentType, stage); got != want[stage][incidentType] {
				t.Errorf("typeAllowedAtStage(%q, %q) = %v", incidentType, stage, got)
			}
		}
	}
}

func TestContactAttemptBoundary(t *testing.T) {
	now := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	accepted := now.Add(-20 * time.Minute)
	first := now.Add(-10 * time.Minute)
	for name, attempts := range map[string]*ContactAttemptsInput{
		"missing":        nil,
		"one attempt":    {Count: 1, FirstAt: first, LastAt: first.Add(3 * time.Minute)},
		"two fifty-nine": {Count: 2, FirstAt: first, LastAt: first.Add(2*time.Minute + 59*time.Second)},
		"before accept":  {Count: 2, FirstAt: accepted.Add(-time.Second), LastAt: accepted.Add(3 * time.Minute)},
		"future":         {Count: 2, FirstAt: now.Add(3 * time.Minute), LastAt: now.Add(6 * time.Minute)},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateContactAttempts(TypeCustomerUnreachable, attempts, &accepted, now)
			assertProblem(t, err, http.StatusUnprocessableEntity, "DELIVERY_INCIDENT_CONTACT_ATTEMPTS_INVALID")
		})
	}
	valid := &ContactAttemptsInput{Count: 2, FirstAt: first, LastAt: first.Add(3 * time.Minute)}
	if err := validateContactAttempts(TypeCustomerUnreachable, valid, &accepted, now); err != nil {
		t.Fatalf("three-minute boundary was rejected: %v", err)
	}
	if err := validateContactAttempts(TypeCustomerRefused, nil, &accepted, now); err != nil {
		t.Fatalf("contact attempts must not be required for other incident types: %v", err)
	}
}

func TestCreateShapeAndTransitionRules(t *testing.T) {
	if err := validateCreateShape(CreateReq{Type: TypeOutOfStock}); err == nil {
		t.Fatal("out-of-stock incident without an item was accepted")
	}
	if err := validateCreateShape(CreateReq{Type: TypeCustomerRefused, Description: "refused"}); err == nil {
		t.Fatal("customer refusal without a reason code was accepted")
	}
	if err := validateCreateShape(CreateReq{Type: TypeCustomerRefused, ReasonCode: "customer_changed_mind"}); err != nil {
		t.Fatalf("valid refusal shape was rejected: %v", err)
	}
	if err := validateCreateShape(CreateReq{Type: "other"}); err == nil {
		t.Fatal("unknown incident type was accepted")
	}

	allowed := map[string]map[string]bool{
		"acknowledged": {StatusOpen: true},
		"resolved":     {StatusOpen: true, StatusAcknowledged: true},
		"rejected":     {StatusEvidenceRequired: true, StatusOpen: true, StatusAcknowledged: true},
	}
	for action, statuses := range allowed {
		for _, status := range []string{StatusEvidenceRequired, StatusOpen, StatusAcknowledged, StatusResolved, StatusRejected} {
			err := validateTransition(status, action)
			if statuses[status] && err != nil {
				t.Errorf("%s -> %s rejected: %v", status, action, err)
			}
			if !statuses[status] && err == nil {
				t.Errorf("%s -> %s unexpectedly accepted", status, action)
			}
		}
	}
}

func TestAuditActionContract(t *testing.T) {
	for name, test := range map[string]struct {
		action string
		actor  string
		want   string
	}{
		"report":       {action: "reported", actor: "rider", want: "report"},
		"evidence":     {action: "evidence_added", actor: "rider", want: "evidence_add"},
		"acknowledge":  {action: "acknowledged", actor: "admin", want: "acknowledge"},
		"resolve":      {action: "resolved", actor: "admin", want: "resolve"},
		"auto resolve": {action: "resolved", actor: "system", want: "auto_resolve"},
		"reject":       {action: "rejected", actor: "admin", want: "reject"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := auditAction(test.action, test.actor); got != test.want {
				t.Fatalf("auditAction(%q, %q) = %q, want %q", test.action, test.actor, got, test.want)
			}
		})
	}
	if result, _ := auditFailureResult("incident.resolve", problem.Conflict("DELIVERY_INCIDENT_VERSION_CONFLICT", "changed")); result != "conflict" {
		t.Fatalf("version conflict audit result=%s", result)
	}
	if result, _ := auditFailureResult("incident.evidence_add", evidenceInvalid("invalid")); result != "token_invalid" {
		t.Fatalf("evidence audit result=%s", result)
	}
}

func TestMetricStateExposesRequestAndAcknowledgementLatency(t *testing.T) {
	state := newMetricState(nil, nil)
	state.observe("report", TypeOutOfStock, nil, 100*time.Millisecond)
	state.observe("report", TypeOutOfStock, problem.Conflict("TEST_CONFLICT", "conflict"), 900*time.Millisecond)
	state.observeAcknowledge(TypeOutOfStock, 4*time.Minute)
	state.incLocationDistanceSuppressed("accuracy_gt_1000m")
	found := map[string]bool{}
	for _, sample := range state.collect() {
		found[sample.Name] = true
	}
	for _, name := range []string{"jxe_delivery_incident_requests_total", "jxe_delivery_incident_request_duration_seconds",
		"jxe_delivery_incident_ack_latency_seconds", "jxe_delivery_incident_ack_latency_seconds_sum", "jxe_delivery_incident_ack_latency_seconds_count",
		"jxe_delivery_incident_location_distance_suppressed_total"} {
		if !found[name] {
			t.Fatalf("metric %s was not collected: %+v", name, found)
		}
	}
}

func TestWriteRateChecksActorAndAnonymizedIPDimensions(t *testing.T) {
	service := &Service{cfg: config.Config{JWT: config.JWTConfig{AccessSecret: "rate-hmac-secret"}, DeliveryIncident: config.DeliveryIncidentConfig{
		CreateRatePerHour: 10, CreateIPRatePerHour: 1, EvidenceRatePerHour: 10, EvidenceIPRatePerHour: 10,
	}}, limiter: fixedwindow.New(nil), metrics: newMetricState(nil, nil)}
	ctx := requestctx.WithHTTPMeta(t.Context(), "203.0.113.8", "test")
	if err := service.checkWriteRate(ctx, "create", 1); err != nil {
		t.Fatalf("first IP request rejected: %v", err)
	}
	err := service.checkWriteRate(ctx, "create", 2)
	assertProblem(t, err, http.StatusTooManyRequests, "DELIVERY_INCIDENT_RATE_LIMITED")
	foundIPLimit, foundDegraded := false, false
	for _, sample := range service.metrics.collect() {
		if sample.Name == "jxe_delivery_incident_rate_limited_total" && sample.Labels["scope"] == "create_ip" && sample.Value == 1 {
			foundIPLimit = true
		}
		if sample.Name == "jxe_delivery_incident_rate_limiter_degraded_total" && sample.Labels["scope"] == "create_ip" && sample.Value >= 1 {
			foundDegraded = true
		}
	}
	if !foundIPLimit || !foundDegraded {
		t.Fatalf("missing IP/degraded limiter metrics: limited=%v degraded=%v", foundIPLimit, foundDegraded)
	}
}

func TestTextLocationAndResponsePrivacy(t *testing.T) {
	cleaned, err := cleanText(" \n事实\t说明\x00 ", true)
	if err != nil || cleaned != "事实说明" {
		t.Fatalf("unexpected sanitized text %q, %v", cleaned, err)
	}
	if _, err := cleanText(strings.Repeat("酒", 1001), true); err == nil {
		t.Fatal("1001-rune text was accepted")
	}
	now := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	location := &LocationInput{Latitude: 22.543096, Longitude: 114.057865, AccuracyM: 12.5, CapturedAt: now}
	recipient := datatypes.JSON(`{"latitude":22.543096,"longitude":114.057865,"phone":"13800000000"}`)
	summary := summarizeLocation(location, recipient, now)
	if summary.distance == nil || *summary.distance != 0 || summary.accuracy == nil || *summary.accuracy != 12.5 {
		t.Fatalf("unexpected location summary: %+v", summary)
	}
	lowQuality := summarizeLocation(&LocationInput{Latitude: location.Latitude, Longitude: location.Longitude, AccuracyM: 1000.01, CapturedAt: now}, recipient, now)
	if lowQuality.distance != nil || lowQuality.distanceSuppressedReason != "accuracy_gt_1000m" {
		t.Fatalf("low-quality location was not marked for observation: %+v", lowQuality)
	}

	dto := aggregateDTO(Aggregate{Incident: Incident{ID: 1, DeliveryOrderID: 2, OrderID: 3, ShopID: 4, RiderID: 5,
		Type: TypeAlcoholDamaged, Stage: StageDelivery, Status: StatusOpen, Priority: "urgent", Description: "bottle damaged",
		ReportedAt: now, CreatedAt: now, UpdatedAt: now, Version: 1, DistanceToDestinationM: summary.distance, LocationAccuracyM: summary.accuracy},
		Evidence: []Evidence{{ID: 6, TokenID: "secret-token-id", ObjectKey: "riders/5/private.jpg", MimeType: "image/jpeg", SizeBytes: 10,
			SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", ScanStatus: "clean", CreatedAt: now}}}, false)
	payload, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, forbidden := range []string{"secret-token-id", "private.jpg", "114.057865", "22.543096", "13800000000"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, `"sha256_suffix":"89abcdef"`) {
		t.Fatalf("response did not expose the safe checksum suffix: %s", text)
	}
}

func TestDeliveryIncidentEvidencePolicy(t *testing.T) {
	now := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	service := &Service{cfg: config.Config{AfterSale: config.AfterSaleConfig{EvidenceTokenSecret: "secret"}}, now: func() time.Time { return now }}
	sign := func(modify func(*evidencetoken.Claims)) string {
		claims := evidencetoken.Claims{Purpose: "delivery_incident_evidence", ObjectKey: "riders/42/evidence.jpg", MimeType: "image/jpeg", SizeBytes: 1024,
			SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", ScanStatus: "clean",
			RegisteredClaims: jwt.RegisteredClaims{Issuer: "jxe-upload", Audience: jwt.ClaimStrings{"delivery-incident"}, Subject: "rider:42", ID: "token-1", IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}}
		if modify != nil {
			modify(&claims)
		}
		raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("secret"))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	rows, err := service.buildEvidence(42, []string{sign(nil)})
	if err != nil || len(rows) != 1 || rows[0].ObjectKey == "" {
		t.Fatalf("valid evidence token rejected: rows=%+v err=%v", rows, err)
	}
	for name, modify := range map[string]func(*evidencetoken.Claims){
		"audience":   func(c *evidencetoken.Claims) { c.Audience = jwt.ClaimStrings{"after-sale"} },
		"subject":    func(c *evidencetoken.Claims) { c.Subject = "rider:43" },
		"purpose":    func(c *evidencetoken.Claims) { c.Purpose = "after_sale_evidence" },
		"object key": func(c *evidencetoken.Claims) { c.ObjectKey = "riders/43/evidence.jpg" },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := service.buildEvidence(42, []string{sign(modify)})
			assertProblem(t, err, http.StatusUnprocessableEntity, "DELIVERY_INCIDENT_EVIDENCE_INVALID")
		})
	}
	_, err = service.buildEvidence(42, []string{sign(func(c *evidencetoken.Claims) { c.ScanStatus = "pending" })})
	assertProblem(t, err, http.StatusTooEarly, "DELIVERY_INCIDENT_EVIDENCE_SCAN_PENDING")
}

func TestStoreEvidenceViewRechecksScopeAndReturnsOpaqueTemporaryURL(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Incident{}, &Item{}, &Evidence{}, &History{}, &AuditLog{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	incident := Incident{ID: 10, IncidentNo: "DI10", DeliveryOrderID: 20, OrderID: 30, ShopID: 40, RiderID: 50,
		Type: TypeAlcoholDamaged, Stage: StageDelivery, Status: StatusOpen, Priority: "urgent", Description: "bottle damaged",
		Version: 1, ReportedAt: now, CreatedAt: now, UpdatedAt: now}
	evidence := Evidence{ID: 60, IncidentID: incident.ID, TokenID: "token-id", ObjectKey: "riders/50/private.jpg", MimeType: "image/jpeg",
		SizeBytes: 1024, SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", ScanStatus: "clean", CreatedAt: now}
	if err := db.Create(&incident).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&evidence).Error; err != nil {
		t.Fatal(err)
	}
	secret := "evidence-view-secret-at-least-thirty-two-characters"
	service := NewService(config.Config{DeliveryIncident: config.DeliveryIncidentConfig{Enabled: true, ShopAllowlist: []string{"40"},
		EvidenceViewBaseURL: "https://media.example.test/private/evidence", EvidenceViewSecret: secret, EvidenceViewTTL: 5 * time.Minute}}, db, snowflake.New(998), nil)
	service.now = func() time.Time { return now }
	service.views, err = evidenceview.New(service.cfg.DeliveryIncident.EvidenceViewBaseURL, secret, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	claims := &auth.Claims{AccountType: "merchant", MerchantUserID: "70", AuthorizedShopIDs: []string{"40"}, Permissions: []string{"delivery_incident:view_shop"}}
	view, err := service.StoreEvidenceView(t.Context(), claims, "10", "60")
	if err != nil {
		t.Fatal(err)
	}
	expiresAt, parseErr := time.Parse(time.RFC3339, view.ExpiresAt)
	if strings.Contains(view.URL, evidence.ObjectKey) || parseErr != nil || expiresAt.Before(now.Add(5*time.Minute-time.Second)) || expiresAt.After(now.Add(5*time.Minute+2*time.Second)) {
		t.Fatalf("unsafe or wrong view response: %+v", view)
	}
	parsed, err := url.Parse(view.URL)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := evidenceview.Open(secret, parsed.Query().Get("token"), time.Now())
	if err != nil || opened.ObjectKey != evidence.ObjectKey || opened.ActorType != "merchant" || opened.ActorID != "70" {
		t.Fatalf("claims=%+v err=%v", opened, err)
	}
	var audit AuditLog
	if err := db.Where("action=? AND resource_id=?", "incident.evidence_view", evidence.ID).First(&audit).Error; err != nil {
		t.Fatal(err)
	}
	if audit.Result != "success" || audit.ActorID != 70 {
		t.Fatalf("audit=%+v", audit)
	}

	foreign := &auth.Claims{AccountType: "merchant", MerchantUserID: "71", AuthorizedShopIDs: []string{"41"}, Permissions: []string{"delivery_incident:view_shop"}}
	_, err = service.StoreEvidenceView(t.Context(), foreign, "10", "60")
	assertProblem(t, err, http.StatusNotFound, "DELIVERY_INCIDENT_NOT_FOUND")
}

func TestDeniedAndInvalidRequestsLeaveCoarseAudit(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&AuditLog{}); err != nil {
		t.Fatal(err)
	}
	service := NewService(config.Config{DeliveryIncident: config.DeliveryIncidentConfig{Enabled: true}}, db, snowflake.New(996), nil)
	claims := &auth.Claims{AccountType: "admin", AdminUserID: "77", Permissions: []string{"delivery_incident:view_all"}}
	_, err = service.Acknowledge(t.Context(), claims, "POST", "/api/v1/admin/delivery-incidents/:id/acknowledge", "valid-key", "123", AcknowledgeReq{ExpectedVersion: 1})
	assertProblem(t, err, http.StatusForbidden, "PERM_FORBIDDEN")
	service.AuditInvalidRequest(t.Context(), claims, "POST", "/api/v1/admin/delivery-incidents/:id/resolve", "incident.resolve", "delivery_incident", "123")

	var audits []AuditLog
	if err := db.Order("id").Find(&audits).Error; err != nil {
		t.Fatal(err)
	}
	if len(audits) != 2 {
		t.Fatalf("audits=%+v", audits)
	}
	if audits[0].Action != "incident.acknowledge" || audits[0].ActorID != 77 || audits[0].ResourceID != 123 || audits[0].Result != "denied" {
		t.Fatalf("unexpected denied write audit: %+v", audits[0])
	}
	if audits[1].Action != "incident.resolve" || audits[1].Result != "invalid" {
		t.Fatalf("unexpected invalid audit: %+v", audits[1])
	}
	for _, audit := range audits {
		var data map[string]any
		if err := json.Unmarshal(audit.AfterData, &data); err != nil {
			t.Fatal(err)
		}
		if data["route"] == "" || data["error_code"] == "" {
			t.Fatalf("missing coarse audit data: %s", audit.AfterData)
		}
	}

	deniedClaims := &auth.Claims{AccountType: "admin", AdminUserID: "78"}
	_, err = service.AdminDetail(t.Context(), deniedClaims, "123")
	assertProblem(t, err, http.StatusForbidden, "PERM_FORBIDDEN")
	var denied AuditLog
	if err := db.Where("actor_id=? AND action=?", 78, "incident.detail_view").First(&denied).Error; err != nil {
		t.Fatal(err)
	}
	if denied.Result != "denied" {
		t.Fatalf("denied audit=%+v", denied)
	}
}

func TestActorAndRolloutScopes(t *testing.T) {
	rider := &auth.Claims{AccountType: "rider", RiderID: "42", Permissions: []string{"delivery_incident:view_own"}}
	if id, err := riderActor(rider, "delivery_incident:view_own"); err != nil || id != 42 {
		t.Fatalf("valid rider scope rejected: %d %v", id, err)
	}
	if _, err := riderActor(rider, "delivery_incident:create"); err == nil {
		t.Fatal("missing rider permission was accepted")
	}
	merchant := &auth.Claims{AccountType: "merchant", MerchantUserID: "8", AuthorizedShopIDs: []string{"10", "20"}, Permissions: []string{"delivery_incident:view_shop"}}
	_, shops, err := storeActor(merchant, "delivery_incident:view_shop")
	if err != nil || len(shops) != 2 {
		t.Fatalf("valid store scope rejected: %+v %v", shops, err)
	}
	rolled := rolloutShops(shops, []string{"20", "30"})
	if len(rolled) != 1 || rolled[0] != 20 {
		t.Fatalf("unexpected rollout intersection: %+v", rolled)
	}
	if allowedID(nil, 42) || len(rolloutShops(shops, nil)) != 0 {
		t.Fatal("empty rollout allowlists must mean no rollout")
	}
	if !allowedID([]string{"*"}, 999) || !allowedID([]string{"ALL"}, 999) {
		t.Fatal("full rollout markers must allow every positive actor ID")
	}
	allShops := rolloutShops(shops, []string{"*"})
	if len(allShops) != len(shops) || allShops[0] != shops[0] || allShops[1] != shops[1] {
		t.Fatalf("full shop rollout did not preserve authorized scope: %+v", allShops)
	}
}

func TestNaturalClosureIsStageScopedAndIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Incident{}, &History{}, &AuditLog{}, &OutboxEvent{}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	rows := []Incident{
		{ID: 1, IncidentNo: "DI1", DeliveryOrderID: 10, OrderID: 20, ShopID: 30, RiderID: 40, Type: TypeOutOfStock, Stage: StagePickup, Status: StatusOpen, Priority: "high", Description: "missing", Version: 1, ReportedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: 2, IncidentNo: "DI2", DeliveryOrderID: 10, OrderID: 20, ShopID: 30, RiderID: 40, Type: TypeCustomerRefused, Stage: StageDelivery, Status: StatusAcknowledged, Priority: "high", Description: "refused", Version: 2, ReportedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: 3, IncidentNo: "DI3", DeliveryOrderID: 10, OrderID: 20, ShopID: 30, RiderID: 40, Type: TypeAlcoholDamaged, Stage: StagePickup, Status: StatusRejected, Priority: "urgent", Description: "rejected", Version: 2, ReportedAt: now, CreatedAt: now, UpdatedAt: now},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	service := NewService(config.Config{DeliveryIncident: config.DeliveryIncidentConfig{Enabled: true, AutoResolveEnabled: true}}, db, snowflake.New(997), nil)
	service.now = func() time.Time { return now.Add(time.Minute) }
	resolve := func(stage, code string) {
		t.Helper()
		if err := db.Transaction(func(tx *gorm.DB) error {
			return service.ResolveActiveLocked(t.Context(), tx, 10, stage, code)
		}); err != nil {
			t.Fatal(err)
		}
	}
	resolve(StagePickup, "pickup_resumed")
	var pickup, delivery, terminal Incident
	if err := db.First(&pickup, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&delivery, 2).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&terminal, 3).Error; err != nil {
		t.Fatal(err)
	}
	if pickup.Status != StatusResolved || pickup.Version != 2 || valueOf(pickup.ResolutionCode) != "pickup_resumed" {
		t.Fatalf("pickup incident was not naturally resolved: %+v", pickup)
	}
	if delivery.Status != StatusAcknowledged || terminal.Status != StatusRejected {
		t.Fatalf("stage scoping changed unrelated incidents: delivery=%s terminal=%s", delivery.Status, terminal.Status)
	}
	var historyCount, auditCount, outboxCount int64
	db.Model(&History{}).Count(&historyCount)
	db.Model(&AuditLog{}).Count(&auditCount)
	db.Model(&OutboxEvent{}).Count(&outboxCount)
	if historyCount != 1 || auditCount != 1 || outboxCount != 1 {
		t.Fatalf("closure facts were not atomic: history=%d audit=%d outbox=%d", historyCount, auditCount, outboxCount)
	}
	var event OutboxEvent
	if err := db.First(&event).Error; err != nil {
		t.Fatal(err)
	}
	var eventPayload map[string]any
	if err := json.Unmarshal(event.Payload, &eventPayload); err != nil {
		t.Fatal(err)
	}
	allowedEventFields := map[string]bool{"incident_id": true, "incident_no": true, "delivery_order_id": true, "order_id": true, "shop_id": true,
		"type": true, "stage": true, "from_status": true, "to_status": true, "actor_type": true}
	for field := range eventPayload {
		if !allowedEventFields[field] {
			t.Fatalf("incident event leaked non-contract field %q: %s", field, event.Payload)
		}
	}
	if event.EventType != "delivery.incident.resolved" || len(eventPayload) != len(allowedEventFields) {
		t.Fatalf("unexpected incident event contract: type=%s payload=%s", event.EventType, event.Payload)
	}
	resolve(StagePickup, "pickup_resumed")
	db.Model(&History{}).Count(&historyCount)
	if historyCount != 1 {
		t.Fatalf("idempotent natural closure duplicated history: %d", historyCount)
	}
	resolve("", "order_cancelled")
	if err := db.First(&delivery, 2).Error; err != nil {
		t.Fatal(err)
	}
	if delivery.Status != StatusResolved || valueOf(delivery.ResolutionCode) != "order_cancelled" {
		t.Fatalf("order cancellation did not resolve all remaining active incidents: %+v", delivery)
	}
}

func assertProblem(t *testing.T, err error, status int, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s", code)
	}
	var details *problem.Details
	if !errors.As(err, &details) || details.Status != status || details.ErrorCode != code {
		t.Fatalf("error = %#v; want status=%d code=%s", err, status, code)
	}
}
