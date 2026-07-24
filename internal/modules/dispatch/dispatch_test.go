package dispatch

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// TestValidatePolicySnapshot 验证策略快照。
func TestValidatePolicySnapshot(t *testing.T) {
	valid := defaultPolicySnapshot()
	if err := validatePolicySnapshot(valid); err != nil {
		t.Fatalf("default policy must be valid: %v", err)
	}
	invalid := valid
	invalid.ScoreWeights.Distance = .8
	if err := validatePolicySnapshot(invalid); err == nil {
		t.Fatal("weights that do not sum to one were accepted")
	}
	invalid = valid
	invalid.ScoreWeights.Load = -.1
	invalid.ScoreWeights.Distance += .1
	if err := validatePolicySnapshot(invalid); err == nil {
		t.Fatal("negative score weight was accepted")
	}
}

// TestValidatePolicyCandidateLimit 验证策略候选数量限制。
func TestValidatePolicyCandidateLimit(t *testing.T) {
	req := PolicyCreateReq{
		PolicyCode: "test", ScopeType: "global", ScopeID: "0", Mode: "hybrid",
		AutoRounds: 3, OfferTTLSeconds: 10, GrabTTLSeconds: 30,
		CandidateLimit: 2, OfferCandidateLimit: 3, HeartbeatFreshSeconds: 60,
		LocationFreshSeconds: 120, MaxLocationAccuracyM: 200, MaxPickupDistanceM: 5000,
		MaxActiveOrdersDefault: 3, IdleFullScoreSeconds: 1800,
		ScoreWeights: ScoreWeights{Distance: .45, Load: .30, Idle: .20, Freshness: .05},
	}
	if err := validatePolicyRequest(req); err == nil {
		t.Fatal("offer_candidate_limit greater than candidate_limit was accepted")
	}
}

// TestHaversineAndDistanceScore 验证 Haversine 距离和距离评分。
func TestHaversineAndDistanceScore(t *testing.T) {
	meters := haversineMeters(22.541, 113.931, 22.551, 113.931)
	if math.Abs(meters-1112) > 15 {
		t.Fatalf("unexpected haversine distance: %.2f", meters)
	}
	distance := uint(2500)
	if score := scoreDistance(&distance, 5000); math.Abs(score-50) > .0001 {
		t.Fatalf("unexpected distance score: %.4f", score)
	}
	tooFar := uint(6000)
	if scoreDistance(&tooFar, 5000) != 0 {
		t.Fatal("distance score must clamp at zero")
	}
}

// TestCandidateRankingIsDeterministic 验证候选骑手排序结果确定。
func TestCandidateRankingIsDeterministic(t *testing.T) {
	now := time.Now()
	earlier := now.Add(-time.Minute)
	score := func(value float64) *float64 { return &value }
	rows := []scoredCandidate{
		{candidate: Candidate{RiderID: 30, Eligible: false, FinalScore: score(100)}},
		{candidate: Candidate{RiderID: 20, Eligible: true, FinalScore: score(80)}, last: &now},
		{candidate: Candidate{RiderID: 10, Eligible: true, FinalScore: score(80)}, last: &earlier},
		{candidate: Candidate{RiderID: 40, Eligible: true, FinalScore: score(80)}},
	}
	sortScoredCandidates(rows)
	want := []uint64{40, 10, 20, 30}
	for index, riderID := range want {
		if rows[index].candidate.RiderID != riderID {
			t.Fatalf("rank %d rider=%d want=%d", index+1, rows[index].candidate.RiderID, riderID)
		}
	}
	// 使用相同输入再次计算不得改变顺序。
	sortScoredCandidates(rows)
	for index, riderID := range want {
		if rows[index].candidate.RiderID != riderID {
			t.Fatalf("second rank %d rider=%d want=%d", index+1, rows[index].candidate.RiderID, riderID)
		}
	}
}

// TestAssignmentResultUsesStableSnakeCaseStringIDs 验证分配结果使用稳定的蛇形命名字符串 ID。
func TestAssignmentResultUsesStableSnakeCaseStringIDs(t *testing.T) {
	raw, err := json.Marshal(AssignmentResult{DeliveryOrderID: "9007199254740993", OrderID: "2", ShopID: "3", RiderID: "4", Status: "accepted"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "DeliveryOrderID") || !strings.Contains(text, `"delivery_order_id":"9007199254740993"`) {
		t.Fatalf("unstable assignment JSON: %s", text)
	}
}

// TestOfferRejectRequestDoesNotRequireAssignmentVersion 验证拒绝派单请求不要求分配版本。
func TestOfferRejectRequestDoesNotRequireAssignmentVersion(t *testing.T) {
	var req OfferRejectReq
	if err := json.Unmarshal([]byte(`{"expected_offer_version":2,"reason_code":"busy"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.ExpectedOfferVersion != 2 || req.ReasonCode != "busy" {
		t.Fatalf("unexpected reject request: %+v", req)
	}
}

// TestCandidateDTOUsesStringIDsAndDecodedExclusions 验证候选 DTO 使用字符串 ID 和已解码排除项。
func TestCandidateDTOUsesStringIDsAndDecodedExclusions(t *testing.T) {
	dto := candidateDTO(Candidate{ID: 11, RiderID: 22, ExclusionCodes: jsonData([]string{"RIDER_OFFLINE"})})
	if dto.ID != "11" || dto.RiderID != "22" || len(dto.ExclusionCodes) != 1 || dto.ExclusionCodes[0] != "RIDER_OFFLINE" {
		t.Fatalf("unexpected candidate DTO: %+v", dto)
	}
}

// TestHeartbeatSequenceIsScopedToDevice 验证心跳序列按设备隔离。
func TestHeartbeatSequenceIsScopedToDevice(t *testing.T) {
	oldDevice := "old-device-hash"
	if got := heartbeatSequence(&oldDevice, 100, oldDevice, true, 110); got != 110 {
		t.Fatalf("same-device latest sequence=%d want=110", got)
	}
	if got := heartbeatSequence(&oldDevice, 100, "new-device-hash", false, 0); got != 0 {
		t.Fatalf("new device must be allowed to restart its sequence, got=%d", got)
	}
}
