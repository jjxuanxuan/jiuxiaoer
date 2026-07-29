package cp1data

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	CheckpointVersion           = 1
	WriteConfirmation           = "APPLY_CP1_DATA_BACKFILL"
	WineTicketWriteConfirmation = "APPLY_WINE_TICKET_REGISTRY_BACKFILL"
	maxBackfillID               = uint64(1<<63 - 1)
)

var checkDescriptions = map[string]string{
	"DQ-001": "orders 与 delivery_orders 状态组合合法",
	"DQ-002": "库存数量非负且库存流水连续、自洽",
	"DQ-003": "订单商品金额和等于 goods_amount",
	"DQ-004": "goods-discount+delivery_fee 等于 payable",
	"DQ-005": "receipt.v1 与订单及商品快照一致",
	"DQ-006": "同一打印事件的首次任务不重复",
	"DQ-007": "切流后的配送完成具备 enforce 核销或受控管理员直执审计",
	"DQ-008": "同一配送单最多一个 active assignment",
	"DQ-009": "启用的打印设置引用已发布且兼容的模板",
	"DQ-010": "一期精确权限及默认商家角色授权符合矩阵",
}

// DefaultCheckIDs 返回 PRD 使用的发布门禁顺序。
func DefaultCheckIDs() []string {
	result := make([]string, 0, len(checkDescriptions))
	for id := range checkDescriptions {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

type Finding struct {
	ObjectType string         `json:"object_type"`
	ObjectID   string         `json:"object_id,omitempty"`
	Code       string         `json:"code"`
	Detail     string         `json:"detail"`
	Data       map[string]any `json:"data,omitempty"`
}

type CheckResult struct {
	CheckID     string    `json:"check_id"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	Violations  int64     `json:"violations"`
	Samples     []Finding `json:"samples,omitempty"`
	Notes       []string  `json:"notes,omitempty"`
}

type DQReport struct {
	SchemaVersion string        `json:"schema_version"`
	GeneratedAt   time.Time     `json:"generated_at"`
	CutoverAt     *time.Time    `json:"verification_cutover_at,omitempty"`
	Passed        bool          `json:"passed"`
	Results       []CheckResult `json:"results"`
}

type DQOptions struct {
	CheckIDs              []string
	SampleLimit           int
	BatchSize             int
	VerificationCutoverAt *time.Time
	VerificationAudit     *VerificationAudit
}

type IDRange struct {
	Min uint64 `json:"min"`
	Max uint64 `json:"max"`
}

type BackfillOptions struct {
	Job                       string
	Execute                   bool
	AllowWrite                bool
	Confirmation              string
	BatchSize                 int
	RowsPerSecond             int
	Range                     IDRange
	TemplateMap               map[uint64]uint64
	FallbackTemplateID        uint64
	VerificationCutoverAt     *time.Time
	VerificationMappingReason string
	SampleLimit               int
	CheckpointFile            string
	Resume                    bool
	MaxRetries                int
}

func (o BackfillOptions) Validate() error {
	switch o.Job {
	case "print-tasks", "print-settings", "verification-history",
		"wine-ticket-payments", "wine-ticket-refunds", "wine-ticket-returns":
	default:
		return fmt.Errorf("unsupported backfill job %q", o.Job)
	}
	if o.BatchSize < 500 || o.BatchSize > 2000 {
		return fmt.Errorf("batch size must be between 500 and 2000")
	}
	if o.RowsPerSecond < 1 || o.RowsPerSecond > 10000 {
		return fmt.Errorf("rows per second must be between 1 and 10000")
	}
	if o.Range.Max != 0 && o.Range.Min > o.Range.Max {
		return fmt.Errorf("minimum id cannot be greater than maximum id")
	}
	if o.SampleLimit < 1 || o.SampleLimit > 1000 {
		return fmt.Errorf("sample limit must be between 1 and 1000")
	}
	if o.MaxRetries < 1 || o.MaxRetries > 5 {
		return fmt.Errorf("max retries must be between 1 and 5")
	}
	if o.Resume && strings.TrimSpace(o.CheckpointFile) == "" {
		return fmt.Errorf("resume requires a checkpoint file")
	}
	if o.Execute {
		confirmation := WriteConfirmation
		if strings.HasPrefix(o.Job, "wine-ticket-") {
			confirmation = WineTicketWriteConfirmation
		}
		if !o.AllowWrite || o.Confirmation != confirmation {
			return fmt.Errorf("write refused: --execute requires the explicit write gate and --confirm=%s", confirmation)
		}
		if strings.TrimSpace(o.CheckpointFile) == "" {
			return fmt.Errorf("write mode requires a checkpoint file")
		}
	}
	if o.Job == "verification-history" {
		if o.VerificationCutoverAt == nil || o.VerificationCutoverAt.IsZero() {
			return fmt.Errorf("verification-history requires an explicit cutover time")
		}
		if strings.TrimSpace(o.VerificationMappingReason) == "" {
			return fmt.Errorf("verification-history requires a mapping reason")
		}
	}
	return nil
}

func (o BackfillOptions) fingerprint() string {
	maxID := o.Range.Max
	if maxID == 0 {
		maxID = maxBackfillID
	}
	templatePairs := make([]string, 0, len(o.TemplateMap))
	for from, to := range o.TemplateMap {
		templatePairs = append(templatePairs, fmt.Sprintf("%d:%d", from, to))
	}
	sort.Strings(templatePairs)
	cutover := ""
	if o.VerificationCutoverAt != nil {
		cutover = o.VerificationCutoverAt.UTC().Format(time.RFC3339Nano)
	}
	payload := strings.Join([]string{
		o.Job,
		fmt.Sprintf("execute=%t", o.Execute),
		fmt.Sprintf("%d", o.Range.Min),
		fmt.Sprintf("%d", maxID),
		fmt.Sprintf("%d", o.FallbackTemplateID),
		strings.Join(templatePairs, ","),
		cutover,
		strings.TrimSpace(o.VerificationMappingReason),
	}, "\x00")
	digest := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(digest[:])
}

type VerificationMapping struct {
	VerificationID  uint64    `json:"verification_id"`
	DeliveryOrderID uint64    `json:"delivery_order_id"`
	Stage           string    `json:"stage"`
	CreatedAt       time.Time `json:"created_at"`
	Status          string    `json:"status"`
	PreviousMode    string    `json:"previous_mode"`
	ResultingMode   string    `json:"resulting_mode"`
	Action          string    `json:"action"`
}

type BackfillProgress struct {
	Scanned              int64                 `json:"scanned"`
	Planned              int64                 `json:"planned"`
	Updated              int64                 `json:"updated"`
	Skipped              int64                 `json:"skipped"`
	Manual               []Finding             `json:"manual,omitempty"`
	VerificationMappings []VerificationMapping `json:"verification_mappings,omitempty"`
}

type BackfillReport struct {
	SchemaVersion string           `json:"schema_version"`
	Job           string           `json:"job"`
	DryRun        bool             `json:"dry_run"`
	StartedAt     time.Time        `json:"started_at"`
	FinishedAt    time.Time        `json:"finished_at"`
	Range         IDRange          `json:"id_range"`
	LastID        uint64           `json:"last_id"`
	Completed     bool             `json:"completed"`
	Progress      BackfillProgress `json:"progress"`
}

type Checkpoint struct {
	Version     int              `json:"version"`
	Job         string           `json:"job"`
	Fingerprint string           `json:"fingerprint"`
	LastID      uint64           `json:"last_id"`
	Completed   bool             `json:"completed"`
	Progress    BackfillProgress `json:"progress"`
	UpdatedAt   time.Time        `json:"updated_at"`
}

type VerificationAudit struct {
	SchemaVersion string                `json:"schema_version"`
	GeneratedAt   time.Time             `json:"generated_at"`
	DryRun        bool                  `json:"dry_run"`
	Completed     bool                  `json:"completed"`
	CutoverAt     time.Time             `json:"cutover_at"`
	MappingReason string                `json:"mapping_reason"`
	IDRange       IDRange               `json:"id_range"`
	Mappings      []VerificationMapping `json:"mappings"`
	Manual        []Finding             `json:"manual,omitempty"`
}

func (a *VerificationAudit) containsDelivery(deliveryID uint64) bool {
	if a == nil || a.DryRun || !a.Completed {
		return false
	}
	for _, mapping := range a.Mappings {
		if mapping.DeliveryOrderID == deliveryID && mapping.Stage == "delivery" && mapping.Action != "manual_active_credential" {
			return true
		}
	}
	return false
}

// LoadCheckpoint 读取操作人员控制的检查点。它会拒绝不完整或跨任务文件，
// 而不是悄然从错误游标开始。
func LoadCheckpoint(path string) (Checkpoint, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Checkpoint{}, err
	}
	var checkpoint Checkpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		return Checkpoint{}, fmt.Errorf("decode checkpoint: %w", err)
	}
	if checkpoint.Version != CheckpointVersion {
		return Checkpoint{}, fmt.Errorf("unsupported checkpoint version %d", checkpoint.Version)
	}
	return checkpoint, nil
}

func saveJSONAtomic(path string, value any) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".cp1-data-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(raw, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func SaveReport(path string, value any) error { return saveJSONAtomic(path, value) }
