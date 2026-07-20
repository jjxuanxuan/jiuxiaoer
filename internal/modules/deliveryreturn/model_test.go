package deliveryreturn

import "testing"

func TestReturnStateMachine(t *testing.T) {
	t.Parallel()
	allowed := [][2]string{
		{StatusRequested, StatusReturning}, {StatusRequested, StatusCancelled}, {StatusRequested, StatusDisputed},
		{StatusReturning, StatusArrived}, {StatusReturning, StatusDisputed}, {StatusReturning, StatusException},
		{StatusArrived, StatusReceived}, {StatusArrived, StatusDisputed}, {StatusArrived, StatusException},
		{StatusReceived, StatusClosed}, {StatusReceived, StatusException},
		{StatusDisputed, StatusReturning}, {StatusDisputed, StatusCancelled},
		{StatusException, StatusArrived}, {StatusException, StatusReceived},
	}
	for _, edge := range allowed {
		if !validTransition(edge[0], edge[1]) {
			t.Errorf("expected transition %s -> %s to be allowed", edge[0], edge[1])
		}
	}
	for _, edge := range [][2]string{
		{StatusRequested, StatusReceived}, {StatusReturning, StatusClosed}, {StatusArrived, StatusClosed},
		{StatusClosed, StatusReturning}, {StatusCancelled, StatusRequested}, {"unknown", StatusRequested},
	} {
		if validTransition(edge[0], edge[1]) {
			t.Errorf("expected transition %s -> %s to be rejected", edge[0], edge[1])
		}
	}
}

func TestReturnStatusClassification(t *testing.T) {
	t.Parallel()
	for _, status := range []string{StatusRequested, StatusReturning, StatusArrived, StatusReceived, StatusDisputed, StatusException} {
		if !isActiveStatus(status) || isTerminalStatus(status) {
			t.Errorf("status %s should be active only", status)
		}
	}
	for _, status := range []string{StatusClosed, StatusCancelled} {
		if isActiveStatus(status) || !isTerminalStatus(status) {
			t.Errorf("status %s should be terminal only", status)
		}
	}
}

func TestReturnReasonsAreClosedEnumeration(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{ReasonCustomerUnreachable, ReasonCustomerRefused, ReasonAddressWrong, ReasonDamagedInTransit, ReasonOther} {
		if !validReason(reason) {
			t.Errorf("reason %s should be valid", reason)
		}
	}
	for _, reason := range []string{"", "customer_cancelled", " CUSTOMER_REFUSED "} {
		if validReason(reason) {
			t.Errorf("reason %q should be invalid", reason)
		}
	}
}
