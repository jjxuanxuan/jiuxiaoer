package delivery

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestDeliveryListQueryContractRejectsUndocumentedSortAndFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, target := range []string{
		"/api/v1/delivery/orders?order_by=created_at%20desc",
		"/api/v1/delivery/orders?filter=status:accepted",
		"/api/v1/delivery/orders?unknown=value",
	} {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest("GET", target, nil)
		if _, err := deliveryListStatusFromGin(ctx); problem.FromError(err).ErrorCode != "VALIDATION_INVALID_QUERY" {
			t.Fatalf("undocumented query accepted for %s: %v", target, err)
		}
	}
}

func TestCandidateDeliverySummaryNeverContainsPreciseSnapshots(t *testing.T) {
	distance := uint(123)
	expires := time.Now().Add(time.Minute)
	row := DeliveryOrder{
		ID: 1, OrderID: 2, ShopID: 3, AssignmentVersion: 1, ShopName: "门店", DestinationDistrict: "南山区",
		ItemCount: 2, PickupDistanceM: &distance, GrabExpiresAt: &expires,
		PickupSnapshot:    datatypes.JSON(`{"phone":"13800138000","provider_secret":"secret"}`),
		RecipientSnapshot: datatypes.JSON(`{"contact_phone":"13900139000","delivery_code":"123456"}`),
	}
	payload, err := json.Marshal(candidateDeliverySummaryDTO(row))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"pickup_snapshot", "recipient_snapshot", "13800138000", "13900139000", "provider_secret", "delivery_code"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("candidate summary leaked %q: %s", forbidden, payload)
		}
	}
	for _, required := range []string{"shop_name", "destination_district", "item_count", "pickup_distance_m", "grab_expires_at"} {
		if !strings.Contains(string(payload), `"`+required+`"`) {
			t.Fatalf("candidate summary omitted %q: %s", required, payload)
		}
	}
}

func TestAssignedDeliverySummaryCarriesClosedSafeSnapshots(t *testing.T) {
	riderID := uint64(9)
	row := DeliveryOrder{
		ID: 1, OrderID: 2, ShopID: 3, RiderID: &riderID, AssignmentVersion: 2, Status: "accepted", CreatedAt: time.Now(),
		PickupSnapshot:    datatypes.JSON(`{"name":"门店","phone":"13800138000","pickup_code":"654321"}`),
		RecipientSnapshot: datatypes.JSON(`{"contact_name":"顾客","contact_phone":"13900139000","identity_number":"secret"}`),
	}
	payload, err := json.Marshal(assignedDeliverySummaryDTO(row))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"pickup_snapshot", "recipient_snapshot"} {
		if !strings.Contains(string(payload), `"`+required+`"`) {
			t.Fatalf("assigned summary omitted %q: %s", required, payload)
		}
	}
	for _, forbidden := range []string{"pickup_code", "654321", "identity_number", "secret"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("assigned summary leaked %q: %s", forbidden, payload)
		}
	}
}
