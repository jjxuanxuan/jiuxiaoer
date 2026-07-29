package reminder

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestReminderWorkerRepositoryExpiresOnlyMatchingAvailableConsents(
	t *testing.T,
) {
	db := newReminderTestDB(t)
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, shanghaiLocation)
	expiredAt := now.Add(-time.Minute)
	futureAt := now.Add(time.Hour)
	rows := []NotificationSubscriptionConsent{
		reminderTestConsent(801, 901, "expired-target", now.Add(-time.Hour)),
		reminderTestConsent(802, 902, "other-customer", now.Add(-time.Hour)),
		reminderTestConsent(803, 901, "other-scene", now.Add(-time.Hour)),
		reminderTestConsent(804, 901, "future-target", now.Add(-time.Hour)),
	}
	rows[0].ExpiresAt = &expiredAt
	rows[1].ExpiresAt = &expiredAt
	rows[2].Scene = "other_scene"
	rows[2].ExpiresAt = &expiredAt
	rows[3].ExpiresAt = &futureAt
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	repo := &reminderWorkerRepository{}
	if err := repo.expireAvailableConsents(
		context.Background(),
		db,
		901,
		expiryReminderScene,
		now,
	); err != nil {
		t.Fatal(err)
	}

	var stored []NotificationSubscriptionConsent
	if err := db.Order("id ASC").Find(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if len(stored) != len(rows) {
		t.Fatalf("stored consents=%d, want %d", len(stored), len(rows))
	}
	wantStatuses := []string{"expired", "available", "available", "available"}
	for index, want := range wantStatuses {
		if stored[index].Status != want {
			t.Fatalf(
				"consent %d status=%q, want %q",
				stored[index].ID,
				stored[index].Status,
				want,
			)
		}
	}
}

func TestReminderWorkerPropagatesProductLookupFailure(t *testing.T) {
	db := newReminderTestDB(t)
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, shanghaiLocation)
	lot := reminderTestLot(811, 911, now.Add(24*time.Hour), now.Add(-24*time.Hour))
	reminder := Reminder{
		ID:              812,
		LotID:           lot.ID,
		OwnerCustomerID: lot.OwnerCustomerID,
		ExpiresAt:       lot.ExpiresAt,
		RemindDays:      expiryReminderDays,
		Channel:         "wechat_subscription",
		Status:          "pending",
		ScheduledAt:     now.Add(-time.Hour),
		CreatedAt:       now.Add(-time.Hour),
		UpdatedAt:       now.Add(-time.Hour),
	}
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&reminder).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("DROP TABLE products").Error; err != nil {
		t.Fatal(err)
	}

	worker := NewExpiryReminderWorker(
		db,
		nil,
		&reminderCountingProvider{},
		"product-query-error",
		nil,
	).WithNow(func() time.Time { return now })
	if _, err := worker.DispatchRemindersOnce(context.Background()); err == nil {
		t.Fatal("product lookup failure must be propagated")
	}

	var stored Reminder
	if err := db.First(&stored, reminder.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != "pending" {
		t.Fatalf(
			"product lookup failure changed reminder status to %q",
			stored.Status,
		)
	}
}

func TestReminderWorkerClaimLocksLotBeforeReminder(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test file")
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(
		fset,
		filepath.Join(filepath.Dir(testFile), "worker.go"),
		nil,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	var function *ast.FuncDecl
	for _, declaration := range file.Decls {
		candidate, ok := declaration.(*ast.FuncDecl)
		if ok && candidate.Name.Name == "claimReminderForSend" {
			function = candidate
			break
		}
	}
	if function == nil {
		t.Fatal("claimReminderForSend not found")
	}

	var lotLock, reminderLock token.Pos
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch selector.Sel.Name {
		case "lockLot":
			if lotLock == token.NoPos {
				lotLock = call.Pos()
			}
		case "lockReminder":
			if reminderLock == token.NoPos {
				reminderLock = call.Pos()
			}
		}
		return true
	})
	if lotLock == token.NoPos || reminderLock == token.NoPos {
		t.Fatalf(
			"missing lock calls: lot=%s reminder=%s",
			fset.Position(lotLock),
			fset.Position(reminderLock),
		)
	}
	if lotLock >= reminderLock {
		t.Fatalf(
			"claim lock order regressed: lot=%s reminder=%s",
			fset.Position(lotLock),
			fset.Position(reminderLock),
		)
	}
}
