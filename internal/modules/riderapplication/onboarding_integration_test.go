package riderapplication

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestMySQLRiderApplicationRejectEditResubmitApproveFlow 验证 MySQL 骑手申请的拒绝、编辑、重提和批准流程。
func TestMySQLRiderApplicationRejectEditResubmitApproveFlow(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run rider application integration test")
	}
	cfg := config.Load()
	dsn := os.Getenv("JXE_MYSQL_RUNTIME_DSN")
	if dsn == "" {
		dsn = cfg.MySQL.DSN
	}
	if dsn == "" || cfg.Redis.Addr == "" {
		t.Fatal("local MySQL runtime DSN and Redis are required")
	}
	db, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	redisClient := goredis.NewClient(&goredis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping redis: %v", err)
	}
	defer redisClient.Close()

	ids := snowflake.New(811)
	unique := ids.Next()
	phone := fmt.Sprintf("166%08d", unique%100000000)
	adminUserID := ids.Next()
	var shopID uint64
	if err := db.WithContext(ctx).Table("shops").Select("id").Where("status = 'active' AND deleted_at IS NULL").Order("id").Limit(1).Scan(&shopID).Error; err != nil || shopID == 0 {
		t.Fatalf("an active seeded shop is required: shop=%d err=%v", shopID, err)
	}

	cfg.RiderApplication.Enabled = true
	cfg.Feature.SMSMockEnabled = true
	authService := auth.NewService(cfg, db, redisClient, ids)
	service := NewService(cfg, db, redisClient, ids).WithSMSVerifier(authService)
	publicActorID := service.publicActorID(phone)
	var applicationID, accountID, riderID uint64
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if accountID == 0 {
			_ = db.WithContext(cleanupCtx).Table("accounts").Select("id").Where("account_type='rider' AND phone=?", phone).Scan(&accountID).Error
		}
		if applicationID == 0 && accountID != 0 {
			_ = db.WithContext(cleanupCtx).Table("rider_applications").Select("id").Where("account_id=?", accountID).Scan(&applicationID).Error
		}
		if riderID == 0 && applicationID != 0 {
			_ = db.WithContext(cleanupCtx).Table("rider_applications").Select("rider_id").Where("id=?", applicationID).Scan(&riderID).Error
		}
		if riderID != 0 {
			_ = db.WithContext(cleanupCtx).Table("rider_service_shops").Where("rider_id=?", riderID).Delete(nil).Error
			_ = db.WithContext(cleanupCtx).Table("rider_runtime_states").Where("rider_id=?", riderID).Delete(nil).Error
			_ = db.WithContext(cleanupCtx).Table("riders").Where("id=?", riderID).Delete(nil).Error
		}
		if applicationID != 0 {
			_ = db.WithContext(cleanupCtx).Table("rider_application_reviews").Where("application_id=?", applicationID).Delete(nil).Error
			_ = db.WithContext(cleanupCtx).Table("outbox_events").Where("(aggregate_type='rider_application' AND aggregate_id=?) OR (aggregate_type='rider' AND aggregate_id=?)", applicationID, riderID).Delete(nil).Error
			_ = db.WithContext(cleanupCtx).Table("audit_logs").Where("resource_type='rider_application' AND resource_id=?", applicationID).Delete(nil).Error
			_ = db.WithContext(cleanupCtx).Table("rider_applications").Where("id=?", applicationID).Delete(nil).Error
		}
		if accountID != 0 {
			_ = db.WithContext(cleanupCtx).Table("audit_logs").Where("resource_type='account' AND resource_id=?", accountID).Delete(nil).Error
			_ = db.WithContext(cleanupCtx).Table("idempotency_keys").Where("actor_id IN ?", []uint64{publicActorID, accountID, adminUserID}).Delete(nil).Error
			_ = db.WithContext(cleanupCtx).Table("accounts").Where("id=?", accountID).Delete(nil).Error
			deleteRedisPattern(cleanupCtx, redisClient, "session:rider:"+idString(accountID)+":*")
		}
		for scope, subject := range map[string]string{
			"submit_ip": "integration-submit-" + idString(unique), "submit_phone": phone,
			"login_ip": "integration-login-" + idString(unique), "login_phone": phone,
			"write_account": idString(accountID), "resubmit_account": idString(accountID), "review_admin": idString(adminUserID),
		} {
			_ = redisClient.Del(cleanupCtx, "rate:rider_application:"+scope+":"+service.hmacString(subject)).Err()
		}
		_ = redisClient.Del(cleanupCtx, "sms:login:rider:"+phone).Err()
	})

	sendFreshRiderCode(t, ctx, redisClient, authService, phone)
	submitIP := "integration-submit-" + idString(unique)
	submitKey := "integration-submit-" + idString(unique)
	submitReq := SubmitRequest{
		Name: "集成测试骑手", Phone: phone, Code: "123456",
		ServiceScope: ServiceScope{ShopIDs: []string{idString(shopID)}},
	}
	submitted, err := service.Submit(ctx, submitIP, "POST", "/api/v1/rider-applications", submitKey, submitReq)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	applicationID = mustTestID(t, submitted.ID)
	accountID = mustTestID(t, submitted.AccountID)
	if submitted.Status != StatusSubmitted || submitted.SubmissionCount != 1 || submitted.Version != 1 {
		t.Fatalf("unexpected submitted application: %+v", submitted)
	}
	var accountStatus string
	if err := db.WithContext(ctx).Table("accounts").Select("status").Where("id=?", accountID).Scan(&accountStatus).Error; err != nil || accountStatus != "disabled" {
		t.Fatalf("applicant account must be disabled: status=%s err=%v", accountStatus, err)
	}
	var riderCount int64
	if err := db.WithContext(ctx).Table("riders").Where("account_id=?", accountID).Count(&riderCount).Error; err != nil || riderCount != 0 {
		t.Fatalf("submit must not create a rider: count=%d err=%v", riderCount, err)
	}
	replayed, err := service.Submit(ctx, submitIP, "POST", "/api/v1/rider-applications", submitKey, submitReq)
	if err != nil || replayed.ID != submitted.ID || replayed.AccountID != submitted.AccountID {
		t.Fatalf("same idempotency key and request must return the original application: replay=%+v err=%v", replayed, err)
	}
	sendFreshRiderCode(t, ctx, redisClient, authService, phone)
	conflictingReq := submitReq
	conflictingReq.Name = "同手机号重复申请"
	if _, err := service.Submit(ctx, submitIP, "POST", "/api/v1/rider-applications", "integration-conflict-"+idString(unique), conflictingReq); err == nil || problem.FromError(err).ErrorCode != "RIDER_APPLICATION_EXISTS" {
		t.Fatalf("same phone must reuse the original application, got %v", err)
	}

	sendFreshRiderCode(t, ctx, redisClient, authService, phone)
	if _, err := authService.RiderSMSLogin(ctx, auth.SmsLoginReq{Phone: phone, Code: "123456"}); err == nil || problem.FromError(err).ErrorCode != "AUTH_ACCOUNT_DISABLED" {
		t.Fatalf("formal rider login must be disabled before approval, got %v", err)
	}
	loginIP := "integration-login-" + idString(unique)
	sendFreshRiderCode(t, ctx, redisClient, authService, phone)
	applicationLogin, err := service.Login(ctx, loginIP, LoginRequest{Phone: phone, Code: "123456"})
	if err != nil {
		t.Fatalf("application login: %v", err)
	}
	applicationClaims, err := service.VerifyApplicationToken(ctx, applicationLogin.ApplicationAccessToken)
	if err != nil {
		t.Fatalf("verify application token: %v", err)
	}

	adminClaims := &auth.Claims{
		TokenType: "access", AccountType: "admin", AdminUserID: idString(adminUserID),
		Permissions: []string{"rider_application:list", "rider_application:view", "rider_application:view_phone", "rider_application:review"},
	}
	owned, err := service.GetOwn(ctx, applicationClaims)
	if err != nil || owned.ID != submitted.ID || owned.Phone != maskPhone(phone) {
		t.Fatalf("applicant must only see the original application with masked phone: owned=%+v err=%v", owned, err)
	}
	listed, err := service.List(ctx, adminClaims, 20, "", "phone="+phone, defaultApplicationOrder)
	if err != nil || len(listed.Items) != 1 || listed.Items[0].ID != submitted.ID || listed.Items[0].Phone != phone {
		t.Fatalf("admin list/filter/full-phone contract failed: listed=%+v err=%v", listed, err)
	}
	detail, err := service.Detail(ctx, adminClaims, submitted.ID)
	if err != nil || detail.ID != submitted.ID || len(detail.Reviews) != 0 {
		t.Fatalf("admin detail before review failed: detail=%+v err=%v", detail, err)
	}
	rejected, err := service.Review(ctx, adminClaims, "integration-admin", "POST", "/api/v1/admin/rider-applications/:id/review", "integration-reject-"+idString(unique), submitted.ID, ReviewRequest{
		Decision: StatusRejected, Reason: "资料需要修改", ExpectedVersion: 1,
	})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if rejected.Status != StatusRejected || rejected.Version != 2 {
		t.Fatalf("unexpected rejection result: %+v", rejected)
	}
	detail, err = service.Detail(ctx, adminClaims, submitted.ID)
	if err != nil || len(detail.Reviews) != 1 || detail.Reviews[0].Decision != StatusRejected {
		t.Fatalf("review history must be append-only after rejection: detail=%+v err=%v", detail, err)
	}

	updated, err := service.UpdateOwn(ctx, applicationClaims, "PATCH", "/api/v1/rider-applications/me", "integration-update-"+idString(unique), UpdateRequest{
		Name: "集成测试骑手二", ServiceScope: ServiceScope{ShopIDs: []string{idString(shopID)}}, ExpectedVersion: 2,
	})
	if err != nil {
		t.Fatalf("update rejected application: %v", err)
	}
	if updated.Status != StatusRejected || updated.Version != 3 {
		t.Fatalf("unexpected update result: %+v", updated)
	}
	if _, err := service.VerifyApplicationToken(ctx, applicationLogin.ApplicationAccessToken); err != nil {
		t.Fatalf("application token must remain valid after profile edit: %v", err)
	}
	sendFreshRiderCode(t, ctx, redisClient, authService, phone)
	applicationLogin, err = service.Login(ctx, loginIP, LoginRequest{Phone: phone, Code: "123456"})
	if err != nil {
		t.Fatalf("application relogin: %v", err)
	}
	applicationClaims, err = service.VerifyApplicationToken(ctx, applicationLogin.ApplicationAccessToken)
	if err != nil {
		t.Fatalf("verify replacement application token: %v", err)
	}
	resubmitted, err := service.Resubmit(ctx, applicationClaims, "POST", "/api/v1/rider-applications/me/resubmit", "integration-resubmit-"+idString(unique), VersionRequest{ExpectedVersion: 3})
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if resubmitted.ID != submitted.ID || resubmitted.AccountID != submitted.AccountID || resubmitted.Status != StatusSubmitted || resubmitted.SubmissionCount != 2 || resubmitted.Version != 4 {
		t.Fatalf("resubmit must preserve original IDs and increment counters: %+v", resubmitted)
	}

	approved, err := service.Review(ctx, adminClaims, "integration-admin", "POST", "/api/v1/admin/rider-applications/:id/review", "integration-approve-"+idString(unique), submitted.ID, ReviewRequest{
		Decision: StatusApproved, Reason: "资料审核通过", ExpectedVersion: 4,
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	riderID = mustTestID(t, approved.RiderID)
	if approved.Status != StatusApproved || approved.Version != 5 || approved.Phone != phone {
		t.Fatalf("unexpected approval result: %+v", approved)
	}
	if _, err := service.VerifyApplicationToken(ctx, applicationLogin.ApplicationAccessToken); err == nil {
		t.Fatal("application token must be invalid after approval")
	}

	type openedState struct {
		AccountStatus string
		RiderStatus   string
		AccountPhone  string
		RiderPhone    string
	}
	var opened openedState
	if err := db.WithContext(ctx).Table("accounts a").
		Select("a.status AS account_status, r.status AS rider_status, a.phone AS account_phone, r.phone AS rider_phone").
		Joins("JOIN riders r ON r.account_id=a.id").Where("a.id=? AND r.id=?", accountID, riderID).Scan(&opened).Error; err != nil {
		t.Fatal(err)
	}
	if opened.AccountStatus != "active" || opened.RiderStatus != "active" || opened.AccountPhone != phone || opened.RiderPhone != phone {
		t.Fatalf("single phone and activation invariant failed: %+v", opened)
	}
	var runtimeCount, serviceShopCount, reviewCount int64
	_ = db.WithContext(ctx).Table("rider_runtime_states").Where("rider_id=? AND work_status='offline'", riderID).Count(&runtimeCount).Error
	_ = db.WithContext(ctx).Table("rider_service_shops").Where("rider_id=? AND shop_id=? AND status='active'", riderID, shopID).Count(&serviceShopCount).Error
	_ = db.WithContext(ctx).Table("rider_application_reviews").Where("application_id=?", applicationID).Count(&reviewCount).Error
	if runtimeCount != 1 || serviceShopCount != 1 || reviewCount != 2 {
		t.Fatalf("approved rider graph is incomplete: runtime=%d shops=%d reviews=%d", runtimeCount, serviceShopCount, reviewCount)
	}

	sendFreshRiderCode(t, ctx, redisClient, authService, phone)
	formal, err := authService.RiderSMSLogin(ctx, auth.SmsLoginReq{Phone: phone, Code: "123456"})
	if err != nil {
		t.Fatalf("formal rider login after approval: %v", err)
	}
	if formal.RefreshToken == "" || formal.AccountID != idString(accountID) {
		t.Fatalf("formal login must issue access and refresh tokens: %+v", formal)
	}
	verified, err := authService.VerifyAccess(ctx, formal.AccessToken)
	if err != nil || verified.RiderID != idString(riderID) {
		t.Fatalf("formal access token is not usable immediately: claims=%+v err=%v", verified, err)
	}
}

// TestMySQLConcurrentSubmitAndApproveCreateExactlyOneRider 验证 MySQL 并发提交和批准只创建一个骑手。
func TestMySQLConcurrentSubmitAndApproveCreateExactlyOneRider(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run rider application integration test")
	}
	cfg := config.Load()
	dsn := os.Getenv("JXE_MYSQL_RUNTIME_DSN")
	if dsn == "" {
		dsn = cfg.MySQL.DSN
	}
	db, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	redisClient := goredis.NewClient(&goredis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB})
	defer redisClient.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	ids := snowflake.New(812)
	unique := ids.Next()
	phone := fmt.Sprintf("167%08d", unique%100000000)
	var shopID uint64
	if err := db.WithContext(ctx).Table("shops").Select("id").Where("status='active' AND deleted_at IS NULL").Order("id").Limit(1).Scan(&shopID).Error; err != nil || shopID == 0 {
		t.Fatalf("active shop required: %v", err)
	}
	cfg.RiderApplication.Enabled = true
	cfg.Feature.SMSMockEnabled = true
	authService := auth.NewService(cfg, db, redisClient, ids)
	service := NewService(cfg, db, redisClient, ids).WithSMSVerifier(authService)
	publicActorID := service.publicActorID(phone)
	adminIDs := []uint64{ids.Next(), ids.Next()}
	var applicationID, accountID, riderID uint64
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_ = db.WithContext(cleanupCtx).Table("accounts").Select("id").Where("account_type='rider' AND phone=?", phone).Scan(&accountID).Error
		if accountID != 0 {
			_ = db.WithContext(cleanupCtx).Table("rider_applications").Select("id").Where("account_id=?", accountID).Scan(&applicationID).Error
		}
		if applicationID != 0 {
			_ = db.WithContext(cleanupCtx).Table("rider_applications").Select("rider_id").Where("id=?", applicationID).Scan(&riderID).Error
		}
		if riderID != 0 {
			_ = db.WithContext(cleanupCtx).Exec("DELETE FROM rider_service_shops WHERE rider_id=?", riderID).Error
			_ = db.WithContext(cleanupCtx).Exec("DELETE FROM rider_runtime_states WHERE rider_id=?", riderID).Error
			_ = db.WithContext(cleanupCtx).Exec("DELETE FROM riders WHERE id=?", riderID).Error
		}
		if applicationID != 0 {
			_ = db.WithContext(cleanupCtx).Exec("DELETE FROM rider_application_reviews WHERE application_id=?", applicationID).Error
			_ = db.WithContext(cleanupCtx).Exec("DELETE FROM outbox_events WHERE (aggregate_type='rider_application' AND aggregate_id=?) OR (aggregate_type='rider' AND aggregate_id=?)", applicationID, riderID).Error
			_ = db.WithContext(cleanupCtx).Exec("DELETE FROM audit_logs WHERE resource_type='rider_application' AND resource_id=?", applicationID).Error
			_ = db.WithContext(cleanupCtx).Exec("DELETE FROM rider_applications WHERE id=?", applicationID).Error
		}
		_ = db.WithContext(cleanupCtx).Exec("DELETE FROM idempotency_keys WHERE actor_id IN ?", append([]uint64{publicActorID, accountID}, adminIDs...)).Error
		if accountID != 0 {
			_ = db.WithContext(cleanupCtx).Exec("DELETE FROM accounts WHERE id=?", accountID).Error
		}
		_ = redisClient.Del(cleanupCtx, "rate:rider_application:submit_phone:"+service.hmacString(phone)).Err()
		_ = redisClient.Del(cleanupCtx, "sms:login:rider:"+phone).Err()
		for _, adminID := range adminIDs {
			_ = redisClient.Del(cleanupCtx, "rate:rider_application:review_admin:"+service.hmacString(idString(adminID))).Err()
		}
	})
	sendFreshRiderCode(t, ctx, redisClient, authService, phone)

	type submitResult struct {
		dto ApplicationDTO
		err error
	}
	results := make(chan submitResult, 5)
	var wait sync.WaitGroup
	for index := 0; index < 5; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			dto, err := service.Submit(ctx, fmt.Sprintf("concurrent-ip-%d-%d", unique, index), "POST", "/api/v1/rider-applications", fmt.Sprintf("concurrent-submit-%d-%d", unique, index), SubmitRequest{
				Name: "并发测试骑手", Phone: phone, Code: "123456",
				ServiceScope: ServiceScope{ShopIDs: []string{idString(shopID)}},
			})
			results <- submitResult{dto: dto, err: err}
		}()
	}
	wait.Wait()
	close(results)
	successes := 0
	for result := range results {
		if result.err == nil {
			successes++
			applicationID = mustTestID(t, result.dto.ID)
			accountID = mustTestID(t, result.dto.AccountID)
			continue
		}
		code := problem.FromError(result.err).ErrorCode
		if code != "RIDER_APPLICATION_EXISTS" && code != "RATE_LIMITED" && code != "AUTH_INVALID_CODE" {
			t.Fatalf("unexpected concurrent submit error: %s %v", code, result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("exactly one concurrent submit must succeed, got %d", successes)
	}
	var accountCount, applicationCount int64
	_ = db.WithContext(ctx).Table("accounts").Where("account_type='rider' AND phone=?", phone).Count(&accountCount).Error
	_ = db.WithContext(ctx).Table("rider_applications").Where("account_id=?", accountID).Count(&applicationCount).Error
	if accountCount != 1 || applicationCount != 1 {
		t.Fatalf("concurrent submit created duplicates: accounts=%d applications=%d", accountCount, applicationCount)
	}

	type reviewResult struct {
		dto ApplicationDTO
		err error
	}
	reviews := make(chan reviewResult, 2)
	for index, adminUserID := range adminIDs {
		index, adminUserID := index, adminUserID
		wait.Add(1)
		go func() {
			defer wait.Done()
			claims := &auth.Claims{TokenType: "access", AccountType: "admin", AdminUserID: idString(adminUserID), Permissions: []string{"rider_application:review"}}
			dto, err := service.Review(ctx, claims, "concurrent-admin", "POST", "/api/v1/admin/rider-applications/:id/review", fmt.Sprintf("concurrent-review-%d-%d", unique, index), idString(applicationID), ReviewRequest{
				Decision: StatusApproved, Reason: "并发审核通过", ExpectedVersion: 1,
			})
			reviews <- reviewResult{dto: dto, err: err}
		}()
	}
	wait.Wait()
	close(reviews)
	reviewSuccesses, reviewConflicts := 0, 0
	for result := range reviews {
		if result.err == nil {
			reviewSuccesses++
			riderID = mustTestID(t, result.dto.RiderID)
			continue
		}
		if code := problem.FromError(result.err).ErrorCode; code == "RIDER_APPLICATION_STATE_CONFLICT" || code == "RIDER_APPLICATION_VERSION_CONFLICT" {
			reviewConflicts++
			continue
		}
		t.Fatalf("unexpected concurrent review error: %v", result.err)
	}
	if reviewSuccesses != 1 || reviewConflicts != 1 {
		t.Fatalf("concurrent approval must produce one success and one conflict: success=%d conflict=%d", reviewSuccesses, reviewConflicts)
	}
	var riderCount, reviewCount int64
	_ = db.WithContext(ctx).Table("riders").Where("account_id=?", accountID).Count(&riderCount).Error
	_ = db.WithContext(ctx).Table("rider_application_reviews").Where("application_id=?", applicationID).Count(&reviewCount).Error
	if riderCount != 1 || reviewCount != 1 {
		t.Fatalf("concurrent approval created duplicate graph: riders=%d reviews=%d", riderCount, reviewCount)
	}
}

// mustTestID 解析测试 ID，失败时终止测试。
func mustTestID(t *testing.T, raw string) uint64 {
	t.Helper()
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		t.Fatalf("invalid test ID %q", raw)
	}
	return id
}

// deleteRedisPattern 删除Redis Pattern。
func deleteRedisPattern(ctx context.Context, client *goredis.Client, pattern string) {
	var cursor uint64
	for {
		keys, next, err := client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return
		}
		if len(keys) > 0 {
			_ = client.Del(ctx, keys...).Err()
		}
		cursor = next
		if cursor == 0 {
			return
		}
	}
}

// sendFreshRiderCode 只重置每分钟发送冷却时间，使集成场景无需等待即可验证
// 多次独立的一次性验证码消耗。每日计数器保持不变，继续覆盖生产限制。
func sendFreshRiderCode(t *testing.T, ctx context.Context, client *goredis.Client, service *auth.Service, phone string) {
	t.Helper()
	if err := client.Del(ctx, "rate:sms:login:rider:cooldown:"+phone).Err(); err != nil {
		t.Fatalf("reset rider sms cooldown: %v", err)
	}
	if err := service.SendRiderCode(ctx, auth.SendCodeReq{Phone: phone}); err != nil {
		t.Fatalf("send fresh rider sms code: %v", err)
	}
}
