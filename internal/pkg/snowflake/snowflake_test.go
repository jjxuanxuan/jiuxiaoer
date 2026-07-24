package snowflake

import "testing"

// TestNewRejectsOutOfRangeNode 验证构造器拒绝超出范围的节点编号。
func TestNewRejectsOutOfRangeNode(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected invalid node ID to panic")
		}
	}()
	New(1024)
}

// TestClockRollbackKeepsIDsMonotonicAndUnique 验证时钟回拨时 ID 仍保持单调且唯一。
func TestClockRollbackKeepsIDsMonotonicAndUnique(t *testing.T) {
	generator := New(1)
	times := []int64{1704067201000, 1704067201000, 1704067200990, 1704067200991, 1704067201001}
	index := 0
	generator.nowMS = func() int64 {
		value := times[index]
		index++
		return value
	}
	previous := generator.Next()
	seen := map[uint64]bool{previous: true}
	for range times[1:] {
		current := generator.Next()
		if current <= previous || seen[current] {
			t.Fatalf("IDs must remain monotonic and unique: previous=%d current=%d", previous, current)
		}
		seen[current] = true
		previous = current
	}
	if generator.ClockRollbackCount() != 2 {
		t.Fatalf("expected two rollback observations, got %d", generator.ClockRollbackCount())
	}
}

// TestSequenceOverflowAdvancesLogicalMillisecond 验证序列溢出会推进逻辑毫秒。
func TestSequenceOverflowAdvancesLogicalMillisecond(t *testing.T) {
	generator := New(1)
	generator.nowMS = func() int64 { return 1704067201000 }
	seen := make(map[uint64]struct{}, 5000)
	var previous uint64
	for index := 0; index < 5000; index++ {
		id := generator.Next()
		if _, exists := seen[id]; exists || (index > 0 && id <= previous) {
			t.Fatalf("duplicate or non-monotonic ID at %d", index)
		}
		seen[id] = struct{}{}
		previous = id
	}
}

// TestDifferentNodesProduceDifferentIDs 验证不同节点生成不同的 ID。
func TestDifferentNodesProduceDifferentIDs(t *testing.T) {
	first := New(1).Next()
	second := New(2).Next()
	if first == second {
		t.Fatal("different nodes produced the same ID")
	}
}
