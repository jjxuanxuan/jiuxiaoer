package reconciliation

import (
	"errors"
	"strings"
	"testing"
)

const tradeAllHeader = "交易时间,公众账号ID,商户号,特约商户号,设备号,微信订单号,商户订单号,用户标识,交易类型,交易状态,付款银行,货币种类,应结订单金额,代金券金额,微信退款单号,商户退款单号,退款金额,充值券退款金额,退款类型,退款状态,商品名称,商户数据包,手续费,费率,订单金额,申请退款金额,费率备注\n"

func TestParseTradeBillStreamsOfficialAllShape(t *testing.T) {
	payment := "`2026-07-19 11:00:00,`wxapp,`1900000001,`0,`,`420000001,`PAY-1,`openid,`JSAPI,`SUCCESS,`OTHERS,`CNY,`0.01,`0.00,`0,`0,`0.00,`0.00,`,``,`测试,`,`0.00,`0.60%,`0.01,`0.00,`\n"
	refund := "`2026-07-19 12:00:00,`wxapp,`1900000001,`0,`,`420000001,`PAY-1,`openid,`JSAPI,`REFUND,`OTHERS,`CNY,`0.00,`0.00,`503000001,`RF-1,`0.01,`0.00,`ORIGINAL,`SUCCESS,`测试,`,`0.00,`0.60%,`0.00,`0.01,`\n"
	summary := "总交易单数,应结订单总金额,退款总金额,充值券退款总金额,手续费总金额,订单总金额,申请退款总金额\n`2,`0.01,`0.01,`0.00,`0.00,`0.01,`0.01\n"
	var entries []parsedEntry
	rows, err := parseBill(strings.NewReader("\ufeff"+tradeAllHeader+payment+refund+summary), BillTypeTradeAll, func(entry parsedEntry) error {
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if rows != 2 || len(entries) != 2 {
		t.Fatalf("rows=%d entries=%d", rows, len(entries))
	}
	if entries[0].Kind != "payment" || entries[0].BusinessNo != "PAY-1" || entries[0].Amount == nil || *entries[0].Amount != 1 || entries[0].Currency != "CNY" {
		t.Fatalf("unexpected payment: %+v", entries[0])
	}
	if entries[1].Kind != "refund" || entries[1].BusinessNo != "RF-1" || entries[1].ProviderRefundNo != "503000001" || entries[1].Amount == nil || *entries[1].Amount != 1 {
		t.Fatalf("unexpected refund: %+v", entries[1])
	}
}

func TestParseFundFlowBill(t *testing.T) {
	bill := "记账时间,微信支付业务单号,资金流水单号,业务名称,业务类型,收支类型,收支金额(元),账户结余(元),资金变更提交申请人,备注,业务凭证号\n" +
		"`2026-07-19 13:00:00,`420000001,`FUND-1,`交易,`交易,`收入,`0.01,`1.00,`system,`测试,`PAY-1\n" +
		"资金流水总笔数,收入笔数,收入金额,支出笔数,支出金额\n`1,`1,`0.01,`0,`0.00\n"
	var got parsedEntry
	rows, err := parseBill(strings.NewReader(bill), BillTypeFundflowBase, func(entry parsedEntry) error { got = entry; return nil })
	if err != nil || rows != 1 {
		t.Fatalf("rows=%d err=%v", rows, err)
	}
	if got.Kind != "fund" || got.BusinessNo != "PAY-1" || got.ProviderTradeNo != "420000001" || got.Amount == nil || *got.Amount != 1 {
		t.Fatalf("unexpected fund entry: %+v", got)
	}
}

func TestParseBillRejectsMissingRequiredHeader(t *testing.T) {
	_, err := parseBill(strings.NewReader("交易时间,商户订单号\n`2026-07-19 11:00:00,`PAY-1\n"), BillTypeTradeAll, func(parsedEntry) error { return nil })
	if !errors.Is(err, errBillFormat) {
		t.Fatalf("expected format error, got %v", err)
	}
}

func TestParseBillRejectsOverlongPhysicalLine(t *testing.T) {
	_, err := parseBill(strings.NewReader(strings.Repeat("x", maxBillLineBytes+1)), BillTypeTradeAll, func(parsedEntry) error { return nil })
	if !errors.Is(err, errBillFormat) {
		t.Fatalf("expected bounded format error, got %v", err)
	}
}

func TestParseBillRejectsMissingOrMismatchedSummary(t *testing.T) {
	header := "记账时间,微信支付业务单号,业务名称,业务类型,收支类型,收支金额(元),业务凭证号\n"
	detail := "`2026-07-19 13:00:00,`4201,`交易,`交易,`收入,`0.01,`PAY1\n"
	for _, body := range []string{
		header + detail,
		header + detail + "资金流水总笔数,收入笔数,收入金额,支出笔数,支出金额\n`2,`1,`0.01,`0,`0.00\n",
	} {
		if _, err := parseBill(strings.NewReader(body), BillTypeFundflowBase, func(parsedEntry) error { return nil }); !errors.Is(err, errBillFormat) {
			t.Fatalf("expected summary format error, got %v", err)
		}
	}
}

func TestParseYuanIsExactAndRejectsExcessPrecision(t *testing.T) {
	for raw, want := range map[string]int64{"0.01": 1, "8.8": 880, "10": 1000, "-0.10": -10, "`999999.99": 99999999} {
		got, err := parseYuan(raw)
		if err != nil || got != want {
			t.Fatalf("parseYuan(%q)=%d,%v want %d", raw, got, err, want)
		}
	}
	if _, err := parseYuan("0.001"); err == nil {
		t.Fatal("expected excess precision to fail")
	}
}
