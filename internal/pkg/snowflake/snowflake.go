package snowflake

import (
	"strconv"
	"sync"
	"time"
)

type Generator struct {
	mu        sync.Mutex
	lastMS    int64
	sequence  int64
	nodeID    int64
	nowMS     func() int64
	rollbacks uint64
}

// ID生成器
func New(nodeID int64) *Generator {
	if nodeID < 0 || nodeID > 1023 {
		panic("snowflake node ID must be between 0 and 1023")
	}
	return &Generator{nodeID: nodeID, nowMS: func() int64 { return time.Now().UnixMilli() }}
}

// Next 生成并返回下一个可用值。
func (g *Generator) Next() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()

	nowMS := g.nowMS()
	if nowMS < g.lastMS {
		g.rollbacks++
		nowMS = g.lastMS
	}
	if nowMS == g.lastMS {
		g.sequence++
		if g.sequence > 0xfff {
			// 当系统时钟落后或 1 毫秒内请求超过 4096 个 ID 时，
			// 推进逻辑时间，而不是自旋或复用序列号。
			nowMS = g.lastMS + 1
			g.sequence = 0
		}
	} else {
		g.sequence = 0
	}
	g.lastMS = nowMS

	return uint64(((nowMS - 1704067200000) << 22) | (g.nodeID << 12) | g.sequence)
}

// ClockRollbackCount 返回已检测到的时钟回拨次数。
func (g *Generator) ClockRollbackCount() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rollbacks
}

// String 返回当前值的字符串表示。
func String(id uint64) string {
	return strconv.FormatUint(id, 10)
}
