package cp1metrics

import (
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestAdminOverrideMetricCountsDirectForceCompleteAudits(t *testing.T) {
	db, err := gorm.Open(
		sqlite.Open("file:cp1metrics-direct-force-complete?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		CREATE TABLE audit_logs (
			id INTEGER PRIMARY KEY,
			actor_type TEXT,
			action TEXT,
			result TEXT
		)
	`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		INSERT INTO audit_logs (id, actor_type, action, result) VALUES
			(1, 'admin', 'delivery.force_complete', 'success'),
			(2, 'admin', 'delivery.force_complete', 'success'),
			(3, 'admin', 'delivery.force_complete', 'failed'),
			(4, 'admin', 'wine_ticket_exception.resolution_executed', 'success'),
			(5, 'rider', 'delivery.force_complete', 'success'),
			(6, 'admin', 'asset_adjustment.execute', 'success'),
			(7, 'admin', 'asset_adjustment.execute', 'failed'),
			(8, 'admin', 'wine_ticket.package.publish', 'success')
	`).Error; err != nil {
		t.Fatal(err)
	}

	highRiskWant := map[string]float64{
		"delivery.force_complete\x00success":                   2,
		"delivery.force_complete\x00failed":                    1,
		"asset_adjustment.execute\x00success":                  1,
		"asset_adjustment.execute\x00failed":                   1,
		"wine_ticket_exception.resolution_executed\x00success": 1,
		"wine_ticket_exception.resolution_executed\x00failed":  0,
		"wine_ticket.package.publish\x00success":               1,
		"wine_ticket.package.publish\x00failed":                0,
	}
	overrideFound := false
	for _, sample := range collect(db) {
		switch sample.Name {
		case "jxe_admin_override_total":
			if sample.Labels["action"] != "delivery.force_complete" {
				t.Fatalf("unexpected override action label: %+v", sample)
			}
			if sample.Value != 2 {
				t.Fatalf("override metric value=%v, want 2", sample.Value)
			}
			overrideFound = true
		case "jxe_admin_high_risk_action_total":
			key := sample.Labels["action"] + "\x00" + sample.Labels["result"]
			want, ok := highRiskWant[key]
			if !ok {
				t.Fatalf("unexpected high-risk metric: %+v", sample)
			}
			if sample.Value != want {
				t.Fatalf("high-risk metric %+v value=%v, want %v", sample.Labels, sample.Value, want)
			}
			delete(highRiskWant, key)
		}
	}
	if !overrideFound {
		t.Fatal("direct force-complete override metric was not emitted")
	}
	if len(highRiskWant) != 0 {
		t.Fatalf("missing high-risk metrics: %+v", highRiskWant)
	}
}

func TestAdminActionMetricsEmitStableZeroBaseline(t *testing.T) {
	db, err := gorm.Open(
		sqlite.Open("file:cp1metrics-admin-zero-baseline?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		CREATE TABLE audit_logs (
			id INTEGER PRIMARY KEY,
			actor_type TEXT,
			action TEXT,
			result TEXT
		)
	`).Error; err != nil {
		t.Fatal(err)
	}

	overrideCount := 0
	highRiskSeen := make(map[adminActionSeries]int)
	for _, sample := range collect(db) {
		switch sample.Name {
		case "jxe_admin_override_total":
			overrideCount++
			if sample.Labels["action"] != "delivery.force_complete" ||
				sample.Value != 0 {
				t.Fatalf("unexpected override zero baseline: %+v", sample)
			}
		case "jxe_admin_high_risk_action_total":
			series := adminActionSeries{
				Action: sample.Labels["action"],
				Result: sample.Labels["result"],
			}
			highRiskSeen[series]++
			if sample.Value != 0 {
				t.Fatalf("unexpected high-risk zero baseline: %+v", sample)
			}
		}
	}
	if overrideCount != 1 {
		t.Fatalf("override zero baseline series=%d, want 1", overrideCount)
	}
	if len(highRiskSeen) != len(adminHighRiskSeries) {
		t.Fatalf(
			"high-risk zero baseline series=%d, want %d: %+v",
			len(highRiskSeen),
			len(adminHighRiskSeries),
			highRiskSeen,
		)
	}
	for _, series := range adminHighRiskSeries {
		if highRiskSeen[series] != 1 {
			t.Fatalf(
				"high-risk zero baseline %+v count=%d, want 1",
				series,
				highRiskSeen[series],
			)
		}
	}
}
