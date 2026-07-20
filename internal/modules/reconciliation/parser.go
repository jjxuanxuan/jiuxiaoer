package reconciliation

import (
	"bufio"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

const maxBillLineBytes = 4 << 20

var errBillFormat = errors.New("invalid WeChat bill format")

// parseBill parses the CSV incrementally. It never loads the full bill into
// memory; fn is called for each validated detail row.
func parseBill(r io.Reader, billType string, fn func(parsedEntry) error) (uint64, error) {
	reader := bufio.NewReaderSize(r, 64<<10)
	headerLine, err := readBillLine(reader)
	if err != nil {
		return 0, fmt.Errorf("%w: read header: %v", errBillFormat, err)
	}
	header, err := parseCSVLine(headerLine)
	if err != nil {
		return 0, fmt.Errorf("%w: read header: %v", errBillFormat, err)
	}
	header = normalizedRecord(header)
	indexes := make(map[string]int, len(header))
	for i, field := range header {
		indexes[field] = i
	}
	if err := validateHeaders(billType, indexes); err != nil {
		return 0, err
	}

	var lineNo uint64 = 1
	var rows uint64
	for {
		line, readErr := readBillLine(reader)
		if errors.Is(readErr, io.EOF) && len(line) == 0 {
			break
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return rows, fmt.Errorf("%w: line %d: %v", errBillFormat, lineNo+1, readErr)
		}
		lineNo++
		record, parseErr := parseCSVLine(line)
		if parseErr != nil {
			return rows, fmt.Errorf("%w: line %d: %v", errBillFormat, lineNo, parseErr)
		}
		record = normalizedRecord(record)
		if len(record) == 0 || (len(record) == 1 && record[0] == "") {
			continue
		}
		if isSummaryHeader(record[0]) {
			summaryLine, summaryErr := readBillLine(reader)
			if summaryErr != nil && !errors.Is(summaryErr, io.EOF) {
				return rows, fmt.Errorf("%w: read summary: %v", errBillFormat, summaryErr)
			}
			summary, summaryParseErr := parseCSVLine(summaryLine)
			if summaryParseErr != nil {
				return rows, fmt.Errorf("%w: parse summary: %v", errBillFormat, summaryParseErr)
			}
			summary = normalizedRecord(summary)
			if len(summary) == 0 {
				return rows, fmt.Errorf("%w: missing summary values", errBillFormat)
			}
			summaryCount, countErr := strconv.ParseUint(summary[0], 10, 64)
			if countErr != nil || summaryCount != rows {
				return rows, fmt.Errorf("%w: summary count %q does not match %d detail rows", errBillFormat, summary[0], rows)
			}
			return rows, nil
		}
		if len(record) != len(header) {
			return rows, fmt.Errorf("%w: line %d has %d fields, want %d", errBillFormat, lineNo, len(record), len(header))
		}
		entry, parseErr := parseRecord(billType, indexes, record, lineNo)
		if parseErr != nil {
			return rows, parseErr
		}
		if entry == nil {
			continue
		}
		if err := fn(*entry); err != nil {
			return rows, err
		}
		rows++
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	return rows, fmt.Errorf("%w: missing summary section", errBillFormat)
}

func readBillLine(reader *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0, 256)
	for {
		fragment, prefix, err := reader.ReadLine()
		if len(line)+len(fragment) > maxBillLineBytes {
			return nil, fmt.Errorf("line exceeds %d bytes", maxBillLineBytes)
		}
		line = append(line, fragment...)
		if err != nil {
			return line, err
		}
		if !prefix {
			return line, nil
		}
	}
}

func parseCSVLine(line []byte) ([]string, error) {
	reader := csv.NewReader(strings.NewReader(string(line)))
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	return reader.Read()
}

func validateHeaders(billType string, indexes map[string]int) error {
	var required []string
	switch billType {
	case BillTypeTradeAll:
		required = []string{"交易时间", "微信订单号", "商户订单号", "交易状态", "货币种类", "订单金额", "微信退款单号", "商户退款单号", "退款状态", "申请退款金额"}
	case BillTypeFundflowBase:
		required = []string{"记账时间", "微信支付业务单号", "业务名称", "业务类型", "收支类型", "收支金额(元)", "业务凭证号"}
	default:
		return fmt.Errorf("%w: unsupported bill type %q", errBillFormat, billType)
	}
	for _, name := range required {
		if _, ok := indexes[name]; !ok {
			return fmt.Errorf("%w: missing header %q", errBillFormat, name)
		}
	}
	return nil
}

func parseRecord(billType string, indexes map[string]int, record []string, lineNo uint64) (*parsedEntry, error) {
	value := func(name string) string { return record[indexes[name]] }
	hash := sha256.Sum256([]byte(strings.Join(record, "\x1f")))
	entry := &parsedEntry{LineNo: lineNo, RawHash: hex.EncodeToString(hash[:])}
	var occurredRaw string
	switch billType {
	case BillTypeTradeAll:
		entry.ProviderTradeNo = value("微信订单号")
		entry.BusinessNo = value("商户订单号")
		entry.ProviderStatus = value("交易状态")
		entry.Currency = value("货币种类")
		occurredRaw = value("交易时间")
		switch entry.ProviderStatus {
		case "SUCCESS":
			entry.Kind = "payment"
			amount, err := parseYuan(value("订单金额"))
			if err != nil {
				return nil, lineError(lineNo, "订单金额", err)
			}
			entry.Amount = &amount
		case "REFUND", "REVOKED":
			entry.Kind = "refund"
			entry.ProviderRefundNo = value("微信退款单号")
			entry.BusinessNo = value("商户退款单号")
			entry.ProviderStatus = value("退款状态")
			amount, err := parseYuan(value("申请退款金额"))
			if err != nil {
				return nil, lineError(lineNo, "申请退款金额", err)
			}
			entry.Amount = &amount
		default:
			// WeChat may add new transaction states. Keeping the raw observation is
			// safer than treating an unknown state as a money fact.
			entry.Kind = "unknown"
		}
	case BillTypeFundflowBase:
		entry.Kind = "fund"
		entry.ProviderTradeNo = value("微信支付业务单号")
		entry.BusinessNo = value("业务凭证号")
		entry.ProviderStatus = value("业务名称") + "/" + value("业务类型") + "/" + value("收支类型")
		occurredRaw = value("记账时间")
		amount, err := parseYuan(value("收支金额(元)"))
		if err != nil {
			return nil, lineError(lineNo, "收支金额(元)", err)
		}
		entry.Amount = &amount
	}
	if occurredRaw != "" {
		occurredAt, err := time.ParseInLocation("2006-01-02 15:04:05", occurredRaw, chinaLocation())
		if err != nil {
			return nil, lineError(lineNo, "time", err)
		}
		entry.OccurredAt = &occurredAt
	}
	return entry, nil
}

func normalizedRecord(record []string) []string {
	for i := range record {
		value := strings.TrimSpace(strings.TrimPrefix(record[i], "\ufeff"))
		value = strings.TrimPrefix(value, "`")
		record[i] = strings.TrimSpace(value)
	}
	return record
}

func isSummaryHeader(first string) bool {
	return first == "总交易单数" || first == "资金流水总笔数"
}

func parseYuan(raw string) (int64, error) {
	value := strings.TrimSpace(strings.TrimPrefix(raw, "`"))
	if value == "" {
		return 0, errors.New("empty amount")
	}
	sign := int64(1)
	if value[0] == '-' || value[0] == '+' {
		if value[0] == '-' {
			sign = -1
		}
		value = value[1:]
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" {
		return 0, fmt.Errorf("invalid amount %q", raw)
	}
	yuan, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || yuan < 0 {
		return 0, fmt.Errorf("invalid amount %q", raw)
	}
	fraction := "00"
	if len(parts) == 2 {
		if len(parts[1]) > 2 || parts[1] == "" {
			return 0, fmt.Errorf("invalid amount precision %q", raw)
		}
		fraction = parts[1] + strings.Repeat("0", 2-len(parts[1]))
	}
	cents, err := strconv.ParseInt(fraction, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q", raw)
	}
	if yuan > (1<<63-1-cents)/100 {
		return 0, fmt.Errorf("amount overflows cents %q", raw)
	}
	return sign * (yuan*100 + cents), nil
}

func lineError(line uint64, field string, err error) error {
	return fmt.Errorf("%w: line %d field %s: %v", errBillFormat, line, field, err)
}

func chinaLocation() *time.Location {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*60*60)
	}
	return location
}
