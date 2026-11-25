package reportreview

import (
	"strings"
	"testing"
)

func TestRealMarketReviewUsesExactFrozenDateAxis(t *testing.T) {
	const header = "date,nav,cash,drawdown,risk_frozen,positions\n"
	wanted := []string{"2024-01-02", "2024-01-03", "2024-01-05"}
	valid := header + "2024-01-02,100,100,0,false,{}\n2024-01-03,110,100,0,false,{}\n2024-01-05,105,100,-0.045,false,{}\n"
	metrics, err := readReportMetrics([]byte(valid), wanted)
	if err != nil || metrics.Returns.RatString() != "1/20" {
		t.Fatalf("expected 5 percent over the frozen window, got %v (%v)", metrics.Returns, err)
	}
	tampered := strings.Replace(valid, "2024-01-03", "2024-01-04", 1)
	if _, err := readReportMetrics([]byte(tampered), wanted); err == nil {
		t.Fatal("accepted a changed interior date with matching boundaries and row count")
	}
}
