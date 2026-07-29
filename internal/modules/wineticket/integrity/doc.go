// Package integrity 校验酒票业务事实、权益流水、履约记录及结算记录之间的一致性。
//
// 本包与负责支付机构账单对账的 internal/modules/reconciliation 有意分离。
// 完整性检查会把差异上报为酒票异常，绝不会自动修复业务台账。
package integrity
