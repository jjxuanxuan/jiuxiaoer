package cp1data

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestWineTicketRegistryBackfillsAreResumableAndExplicitlyGated(t *testing.T) {
	db := newBackfillDB(t)
	mustExec(t, db, `CREATE TABLE payments (id INTEGER PRIMARY KEY, order_id INTEGER NOT NULL, biz_type TEXT, biz_id INTEGER)`)
	mustExec(t, db, `CREATE TABLE refunds (id INTEGER PRIMARY KEY, after_sale_id INTEGER NOT NULL, biz_type TEXT, biz_id INTEGER)`)
	mustExec(t, db, `CREATE TABLE delivery_returns (id INTEGER PRIMARY KEY, after_sale_id INTEGER, status TEXT NOT NULL, closed_at DATETIME, settlement_type TEXT, settlement_biz_id INTEGER, settlement_status TEXT, settled_at DATETIME)`)
	mustExec(t, db, `INSERT INTO payments VALUES (1,11,NULL,NULL),(2,12,'retail_order',12)`)
	mustExec(t, db, `INSERT INTO refunds VALUES (3,13,NULL,NULL)`)
	closedAt := time.Now().Truncate(time.Millisecond)
	mustExec(t, db, `INSERT INTO delivery_returns VALUES (4,NULL,'requested',NULL,NULL,NULL,NULL,NULL),(5,15,'closed',?,NULL,NULL,NULL,NULL)`, closedAt)

	base := BackfillOptions{Job: "wine-ticket-payments", Execute: true, AllowWrite: true, BatchSize: 500, RowsPerSecond: 10000, SampleLimit: 20, MaxRetries: 5, CheckpointFile: filepath.Join(t.TempDir(), "payments.json")}
	if err := base.Validate(); err == nil {
		t.Fatal("wine-ticket write accepted without its dedicated confirmation phrase")
	}
	base.Confirmation = WineTicketWriteConfirmation
	runner, err := NewBackfiller(db, base)
	if err != nil {
		t.Fatal(err)
	}
	report, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Progress.Updated != 1 || report.LastID != 1 {
		t.Fatalf("unexpected payment report: %+v", report)
	}

	for _, job := range []string{"wine-ticket-refunds", "wine-ticket-returns"} {
		options := base
		options.Job = job
		options.CheckpointFile = filepath.Join(t.TempDir(), job+".json")
		runner, err := NewBackfiller(db, options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runner.Run(context.Background()); err != nil {
			t.Fatalf("%s: %v", job, err)
		}
	}

	var payment struct {
		BizType string
		BizID   uint64
	}
	if err := db.Table("payments").Where("id=1").Scan(&payment).Error; err != nil {
		t.Fatal(err)
	}
	if payment.BizType != "retail_order" || payment.BizID != 11 {
		t.Fatalf("payment registry not backfilled: %+v", payment)
	}
	var rows []struct {
		ID               uint64
		SettlementStatus string
		SettledAt        *time.Time
	}
	if err := db.Table("delivery_returns").Order("id").Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows[0].SettlementStatus != "not_started" || rows[0].SettledAt != nil ||
		rows[1].SettlementStatus != "succeeded" || rows[1].SettledAt == nil {
		t.Fatalf("return settlement registry not backfilled: %+v", rows)
	}
}

func TestReturnSettlementStatusMapping(t *testing.T) {
	afterSaleID := uint64(9)
	cases := []struct {
		row  wineTicketReturnBackfillRow
		want string
	}{
		{wineTicketReturnBackfillRow{Status: "requested"}, "not_started"},
		{wineTicketReturnBackfillRow{Status: "returning", AfterSaleID: &afterSaleID}, "processing"},
		{wineTicketReturnBackfillRow{Status: "disputed", AfterSaleID: &afterSaleID}, "exception"},
		{wineTicketReturnBackfillRow{Status: "exception", AfterSaleID: &afterSaleID}, "exception"},
		{wineTicketReturnBackfillRow{Status: "closed", AfterSaleID: &afterSaleID}, "succeeded"},
	}
	for _, tc := range cases {
		if got := returnSettlementStatus(tc.row); got != tc.want {
			t.Fatalf("status %q: got %q want %q", tc.row.Status, got, tc.want)
		}
	}
}
