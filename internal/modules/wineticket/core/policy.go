package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// RefundPolicy、RenewalPolicy 和 DeliveryPolicy 是共享业务策略值对象。
// 购买、核销、续期和退款都依赖同一份不可变套餐快照契约，因此它们归属 core。
type RefundPolicy struct {
	SchemaVersion    int   `json:"schema_version"`
	Enabled          bool  `json:"enabled"`
	WindowHours      int   `json:"window_hours"`
	RequireNeverUsed bool  `json:"require_never_used"`
	FeeAmount        int64 `json:"fee_amount"`
}

type RenewalPolicy struct {
	SchemaVersion int   `json:"schema_version"`
	Enabled       bool  `json:"enabled"`
	ExtensionDays int   `json:"extension_days"`
	MaxCount      int   `json:"max_count"`
	GraceDays     int   `json:"grace_days"`
	FeeAmount     int64 `json:"fee_amount"`
}

type DeliveryPolicy struct {
	SchemaVersion       int  `json:"schema_version"`
	DeliveryFeeIncluded bool `json:"delivery_fee_included"`
	DispatchLeadMinutes int  `json:"dispatch_lead_minutes"`
}

func DecodePolicyJSON(raw []byte, out any, required ...string) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("policy must be a JSON object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return err
	}
	for _, name := range required {
		value, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("required policy field %s is missing or null", name)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("policy must contain exactly one JSON object")
	}
	return nil
}

func RefundPolicySummary(policy RefundPolicy) string {
	if !policy.Enabled {
		return "不支持退款"
	}
	return fmt.Sprintf(
		"支付后%d小时内且酒票全部未使用可退款，手续费0元",
		policy.WindowHours,
	)
}

func RenewalPolicySummary(policy RenewalPolicy) string {
	if !policy.Enabled {
		return "不支持续期"
	}
	return fmt.Sprintf(
		"每次延长%d天，最多%d次，费用%.2f元",
		policy.ExtensionDays,
		policy.MaxCount,
		float64(policy.FeeAmount)/100,
	)
}

func DeliveryPolicySummary(policy DeliveryPolicy) string {
	return fmt.Sprintf(
		"配送费已包含，最晚提前%d分钟调度",
		policy.DispatchLeadMinutes,
	)
}
