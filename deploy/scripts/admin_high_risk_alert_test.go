package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

func TestAdminHighRiskAlertIsBoundedAndDoesNotDuplicateDeliveryAlert(t *testing.T) {
	content, err := os.ReadFile("../prometheus/alerts.yml")
	if err != nil {
		t.Fatal(err)
	}

	var document alertFile
	if err := yaml.Unmarshal(content, &document); err != nil {
		t.Fatal(err)
	}

	rules := make(map[string]alertRule)
	for _, group := range document.Groups {
		for _, rule := range group.Rules {
			rules[rule.Alert] = rule
		}
	}

	delivery, ok := rules["JiuxiaoerAdminOverrideSpike"]
	if !ok {
		t.Fatal("existing delivery force-complete alert must be retained")
	}
	deliveryExpr := fmt.Sprint(delivery.Expression)
	normalizedDeliveryExpr := strings.Join(strings.Fields(deliveryExpr), " ")
	if !strings.Contains(deliveryExpr, `action="delivery.force_complete"`) ||
		!strings.Contains(
			normalizedDeliveryExpr,
			"delta(( max by (action) (",
		) ||
		!strings.Contains(normalizedDeliveryExpr, ")[15m:1m])") ||
		strings.Contains(deliveryExpr, "sum(") {
		t.Fatalf("delivery alert lost its force-complete scope: %s", deliveryExpr)
	}

	highRisk, ok := rules["JiuxiaoerAdminHighRiskActionSpike"]
	if !ok {
		t.Fatal("non-delivery high-risk action alert is missing")
	}
	expr := fmt.Sprint(highRisk.Expression)
	normalizedExpr := strings.Join(strings.Fields(expr), " ")
	for _, required := range []string{
		"delta(( max by (action, result) (",
		")[15m:1m])",
		`action=~"asset_adjustment.execute|wine_ticket_exception.resolution_executed|wine_ticket.package.publish"`,
		`result="success"`,
		`action="asset_adjustment.execute",result="failed"`,
		"> 10",
		"> 3",
	} {
		if !strings.Contains(normalizedExpr, required) {
			t.Errorf("high-risk alert expression must contain %q: %s", required, expr)
		}
	}
	if strings.Contains(expr, "delivery.force_complete") {
		t.Error("high-risk action alert must not duplicate the dedicated delivery alert")
	}
	for _, unsupported := range []string{
		`action="wine_ticket_exception.resolution_executed",result="failed"`,
		`action="wine_ticket.package.publish",result="failed"`,
	} {
		if strings.Contains(expr, unsupported) {
			t.Errorf("high-risk alert must not claim an unavailable failed-audit series %q", unsupported)
		}
	}
	if strings.Contains(strings.ToLower(expr), "actor") {
		t.Error("Prometheus expression must not add a high-cardinality actor label")
	}
	if strings.Contains(expr, "sum by") {
		t.Error("global database gauges must not be summed across API replicas")
	}
	if highRisk.For != "2m" || highRisk.Labels["severity"] != "warning" {
		t.Fatalf("unexpected anti-noise policy: for=%q labels=%v", highRisk.For, highRisk.Labels)
	}
	if got := highRisk.Annotations["runbook"]; got != "docs/runbooks/admin-high-risk-actions.md#high-risk-admin-action-spike" {
		t.Fatalf("unexpected runbook reference: %q", got)
	}
}
