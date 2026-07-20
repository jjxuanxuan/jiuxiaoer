package realtime

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const ProtocolVersion = 1

const (
	FrameHello          = "hello"
	FrameEvent          = "event"
	FrameResume         = "resume"
	FrameSyncComplete   = "sync_complete"
	FrameAck            = "ack"
	FrameAckResult      = "ack_result"
	FrameError          = "error"
	FrameServerShutdown = "server_shutdown"
)

var allowedAckTypes = map[string]bool{
	"displayed": true, "sound_played": true, "sound_disabled": true,
	"sound_error": true, "closed": true,
}

type TicketRequest struct {
	DeviceID        string `json:"device_id"`
	Platform        string `json:"platform"`
	ClientVersion   string `json:"client_version"`
	ProtocolVersion int    `json:"protocol_version"`
}

// Validate 校验实时消息是否合法。
func (r TicketRequest) Validate() error {
	if strings.TrimSpace(r.DeviceID) == "" || len(r.DeviceID) > 128 {
		return fmt.Errorf("device_id is required and must be at most 128 bytes")
	}
	switch r.Platform {
	case "weapp", "h5", "android", "ios", "test":
	default:
		return fmt.Errorf("platform is unsupported")
	}
	if strings.TrimSpace(r.ClientVersion) == "" || len(r.ClientVersion) > 32 {
		return fmt.Errorf("client_version is required and must be at most 32 bytes")
	}
	if r.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("protocol_version must be %d", ProtocolVersion)
	}
	return nil
}

type TicketResponse struct {
	Ticket                   string    `json:"ticket"`
	ExpiresAt                time.Time `json:"expires_at"`
	WSPath                   string    `json:"ws_path"`
	HeartbeatIntervalSeconds int       `json:"heartbeat_interval_seconds"`
	MaxResumeItems           int       `json:"max_resume_items"`
	ProtocolVersion          int       `json:"protocol_version"`
}

type TicketInfo struct {
	RiderID         uint64    `json:"rider_id"`
	AccountType     string    `json:"account_type"`
	AccountID       string    `json:"account_id"`
	SessionID       string    `json:"session_id"`
	AccessJTI       string    `json:"access_jti"`
	AccessExpiresAt time.Time `json:"access_expires_at"`
	DeviceHash      string    `json:"device_hash"`
	Platform        string    `json:"platform"`
	ClientVersion   string    `json:"client_version"`
	ProtocolVersion int       `json:"protocol_version"`
}

type ClientFrame struct {
	ProtocolVersion int        `json:"protocol_version"`
	Type            string     `json:"type"`
	RequestID       string     `json:"request_id,omitempty"`
	AfterDeliveryID string     `json:"after_delivery_id,omitempty"`
	DeliveryID      string     `json:"delivery_id,omitempty"`
	Outcome         string     `json:"outcome,omitempty"`
	ClientAt        *time.Time `json:"client_at,omitempty"`
	ErrorCode       string     `json:"error_code,omitempty"`
}

type ServerFrame struct {
	ProtocolVersion          int             `json:"protocol_version"`
	Type                     string          `json:"type"`
	RequestID                string          `json:"request_id,omitempty"`
	ServerTime               time.Time       `json:"server_time"`
	ConnectionID             string          `json:"connection_id,omitempty"`
	DeliveryID               string          `json:"delivery_id,omitempty"`
	SourceEventID            string          `json:"source_event_id,omitempty"`
	EventType                string          `json:"event_type,omitempty"`
	OccurredAt               *time.Time      `json:"occurred_at,omitempty"`
	ExpiresAt                *time.Time      `json:"expires_at,omitempty"`
	RequiresAck              bool            `json:"requires_ack,omitempty"`
	SoundKey                 string          `json:"sound_key,omitempty"`
	Data                     json.RawMessage `json:"data,omitempty"`
	HeartbeatIntervalSeconds int             `json:"heartbeat_interval_seconds,omitempty"`
	MaxResumeItems           int             `json:"max_resume_items,omitempty"`
	LastDeliveryID           string          `json:"last_delivery_id,omitempty"`
	HasMore                  *bool           `json:"has_more,omitempty"`
	Accepted                 bool            `json:"accepted,omitempty"`
	ErrorCode                string          `json:"error_code,omitempty"`
	Detail                   string          `json:"detail,omitempty"`
}

// frameNow 返回frame Now。
func frameNow(frameType string) ServerFrame {
	return ServerFrame{ProtocolVersion: ProtocolVersion, Type: frameType, ServerTime: time.Now().UTC()}
}

// deliveryFrame 返回配送 Frame。
func deliveryFrame(row Delivery) ServerFrame {
	frame := frameNow(FrameEvent)
	frame.DeliveryID = strconv.FormatUint(row.ID, 10)
	frame.SourceEventID = row.SourceEventID
	frame.EventType = row.ClientEventType
	frame.OccurredAt = &row.OccurredAt
	frame.ExpiresAt = &row.ExpiresAt
	frame.RequiresAck = true
	frame.Data = json.RawMessage(row.PayloadSnapshot)
	if row.SoundKey != nil {
		frame.SoundKey = *row.SoundKey
	}
	return frame
}

// parseID 解析并校验字符串形式的 ID。
func parseID(raw string) (uint64, error) {
	value, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("invalid id")
	}
	return value, nil
}
