package realtime

import (
	"testing"
)

// TestTicketRequestProtocolValidation 验证Ticket 请求协议校验的预期行为。
func TestTicketRequestProtocolValidation(t *testing.T) {
	valid := TicketRequest{DeviceID: "device", Platform: "weapp", ClientVersion: "1.2.3", ProtocolVersion: 1}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	invalid := []TicketRequest{
		{Platform: "weapp", ClientVersion: "1", ProtocolVersion: 1},
		{DeviceID: "device", Platform: "desktop", ClientVersion: "1", ProtocolVersion: 1},
		{DeviceID: "device", Platform: "weapp", ProtocolVersion: 1},
		{DeviceID: "device", Platform: "weapp", ClientVersion: "1", ProtocolVersion: 2},
	}
	for _, request := range invalid {
		if err := request.Validate(); err == nil {
			t.Fatalf("expected invalid request: %+v", request)
		}
	}
}

// TestProtocolUsesFrozenFrameNames 验证协议 Uses Frozen Frame Names的预期行为。
func TestProtocolUsesFrozenFrameNames(t *testing.T) {
	if FrameHello != "hello" || FrameSyncComplete != "sync_complete" || FrameAckResult != "ack_result" {
		t.Fatalf("unexpected protocol frame names: %s %s %s", FrameHello, FrameSyncComplete, FrameAckResult)
	}
	for _, outcome := range []string{"displayed", "sound_played", "sound_disabled", "sound_error", "closed"} {
		if !allowedAckTypes[outcome] {
			t.Fatalf("missing ACK outcome %s", outcome)
		}
	}
}
