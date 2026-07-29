// Command check-alerts 执行 Prometheus 告警门禁中可在仓库本地完成的部分。
// 生产 CI 可以额外运行 promtool，但本命令不依赖外部二进制或网络。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-yaml"
)

type alertFile struct {
	Groups []alertGroup `yaml:"groups"`
}

type alertGroup struct {
	Name  string      `yaml:"name"`
	Rules []alertRule `yaml:"rules"`
}

type alertRule struct {
	Alert       string            `yaml:"alert"`
	Expression  any               `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

var requiredWineTicketAlerts = []string{
	"JiuxiaoerWineTicketSettlementLag",
	"JiuxiaoerWineTicketRefundSettlementLag",
	"JiuxiaoerWineTicketRenewalGuardStalled",
	"JiuxiaoerWineTicketLotInvariantViolation",
	"JiuxiaoerWineTicketReconciliationDifference",
	"JiuxiaoerWineTicketReconciliationDeadlineMissed",
	"JiuxiaoerWineTicketReminderLag",
	"JiuxiaoerWineTicketReminderLagCritical",
}

var requiredAdminActionAlerts = []string{
	"JiuxiaoerAdminOverrideSpike",
	"JiuxiaoerAdminHighRiskActionSpike",
}

func main() {
	if len(os.Args) != 2 {
		fail("usage: go run ./deploy/scripts/check-alerts.go ALERTS_YAML")
	}
	path := os.Args[1]
	content, err := os.ReadFile(path)
	if err != nil {
		fail("read %s: %v", path, err)
	}

	var document alertFile
	if err := yaml.Unmarshal(content, &document); err != nil {
		fail("parse %s: %v", path, err)
	}
	if len(document.Groups) == 0 {
		fail("%s contains no alert groups", path)
	}

	seen := make(map[string]struct{})
	for _, group := range document.Groups {
		if strings.TrimSpace(group.Name) == "" || len(group.Rules) == 0 {
			fail("every alert group must have a name and at least one rule")
		}
		for _, rule := range group.Rules {
			name := strings.TrimSpace(rule.Alert)
			if name == "" || strings.TrimSpace(fmt.Sprint(rule.Expression)) == "" {
				fail("every alert rule must have alert and expr fields")
			}
			if _, duplicate := seen[name]; duplicate {
				fail("duplicate alert name %q", name)
			}
			seen[name] = struct{}{}
			if strings.TrimSpace(rule.Labels["severity"]) == "" {
				fail("alert %s has no severity label", name)
			}
			if strings.TrimSpace(rule.Annotations["summary"]) == "" {
				fail("alert %s has no summary annotation", name)
			}
			if strings.HasPrefix(name, "JiuxiaoerWineTicket") {
				validateRunbook(name, rule.Annotations["runbook"])
			}
			if name == "JiuxiaoerAdminHighRiskActionSpike" {
				validateRunbook(name, rule.Annotations["runbook"])
			}
		}
	}

	for _, required := range requiredWineTicketAlerts {
		if _, ok := seen[required]; !ok {
			fail("required wine-ticket alert %s is missing", required)
		}
	}
	for _, required := range requiredAdminActionAlerts {
		if _, ok := seen[required]; !ok {
			fail("required admin-action alert %s is missing", required)
		}
	}
	fmt.Printf("alerts-check: %d groups, %d unique rules, wine-ticket and admin-action alert contracts OK\n", len(document.Groups), len(seen))
}

func validateRunbook(alertName, reference string) {
	path, anchor, found := strings.Cut(strings.TrimSpace(reference), "#")
	if path == "" || !found || anchor == "" {
		fail("alert %s must link to a repository runbook anchor", alertName)
	}
	content, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		fail("alert %s runbook %s: %v", alertName, path, err)
	}
	heading := strings.ReplaceAll(anchor, "-", " ")
	if !strings.Contains(strings.ToLower(string(content)), strings.ToLower("## "+heading)) {
		fail("alert %s references missing runbook anchor %q", alertName, anchor)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "alerts-check: "+format+"\n", args...)
	os.Exit(1)
}
