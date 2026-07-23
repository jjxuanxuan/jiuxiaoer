package auditlog

import "testing"

func TestSensitiveAuditKeyRejectsCompositeFreeTextAndPIIKeys(t *testing.T) {
	for _, key := range []string{
		"AddressSnapshot", "recipient_snapshot", "contact_phone", "latitude",
		"delivery_code", "api_key", "license_no", "client_ip",
		"review_remark", "failure_detail", "StatusReasonSafe", "last_error_safe",
	} {
		if !sensitiveAuditKey(key) {
			t.Errorf("sensitive audit key %q was not rejected", key)
		}
	}
}

func TestSensitiveAuditKeyKeepsControlledCodeDimensions(t *testing.T) {
	for _, key := range []string{
		"error_code", "reason_code", "failure_code", "policy_code", "return_policy_code",
	} {
		if sensitiveAuditKey(key) {
			t.Errorf("controlled code dimension %q was incorrectly removed", key)
		}
	}
}
