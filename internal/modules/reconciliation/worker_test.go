package reconciliation

import (
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/config"
)

func TestBillWindowKeepsDelayedRunsInsideOfficialNinetyDayRange(t *testing.T) {
	cfg := config.Load()
	cfg.Reconciliation.RunHour = 10
	cfg.Reconciliation.LagDays = 30
	cfg.Reconciliation.StartDate = "2020-01-01"
	worker := &Worker{cfg: cfg}
	now := time.Date(2026, 7, 20, 11, 0, 0, 0, chinaLocation())

	start, end, due, err := worker.billWindow(now, true)
	if err != nil || !due {
		t.Fatalf("window start=%s end=%s due=%v err=%v", start, end, due, err)
	}
	latestDownloadable := normalizeBillDate(now.AddDate(0, 0, -1))
	wantStart := latestDownloadable.AddDate(0, 0, -89)
	wantEnd := latestDownloadable.AddDate(0, 0, -29)
	if !start.Equal(wantStart) || !end.Equal(wantEnd) {
		t.Fatalf("window start=%s end=%s, want start=%s end=%s", start, end, wantStart, wantEnd)
	}
}
