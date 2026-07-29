package refund

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type refundAcceptanceFixture struct {
	orderID, paymentID, afterSaleID, itemID, refundID uint64
	orderNo, paymentNo, afterSaleNo, refundNo         string
	amount, total                                     int64
}

type acceptanceProvider struct {
	refundState, queryState, callbackState State
	refundErr, queryErr                    error
	refundCalls, queryCalls                atomic.Int64
	delay                                  time.Duration
	mu                                     sync.Mutex
	queryStarted                           chan struct{}
	queryRelease                           chan struct{}
	queryStartOnce                         sync.Once
	refundInputs                           []Input
}

// Code 返回代码。
func (p *acceptanceProvider) Code() string { return "wechat" }

// Refund 返回退款。
func (p *acceptanceProvider) Refund(_ context.Context, input Input) (State, error) {
	p.refundCalls.Add(1)
	p.mu.Lock()
	p.refundInputs = append(p.refundInputs, input)
	p.mu.Unlock()
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	return p.refundState, p.refundErr
}

// QueryRefund 查询退款。
func (p *acceptanceProvider) QueryRefund(_ context.Context, _ string) (State, error) {
	p.queryCalls.Add(1)
	if p.queryStarted != nil {
		p.queryStartOnce.Do(func() { close(p.queryStarted) })
	}
	if p.queryRelease != nil {
		<-p.queryRelease
	}
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	return p.queryState, p.queryErr
}

// ParseRefundCallback 解析退款回调。
func (p *acceptanceProvider) ParseRefundCallback(_ context.Context, request *http.Request) (CallbackEvent, error) {
	return CallbackEvent{EventID: request.Header.Get("X-Event-ID"), MchID: "local-mch", State: p.callbackState}, nil
}
