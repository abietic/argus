package reportreview

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	contract "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

var (
	caseNames        = []string{"configured-costs", "high-friction", "delayed-execution"}
	factsSHA         = regexp.MustCompile(`^[0-9a-f]{64}$`)
	decimal          = regexp.MustCompile(`^-?[0-9]{1,20}(\.[0-9]{1,20})?$`)
	unregisteredLink = regexp.MustCompile(`\]\((?:\./)?delivery-manifest\.json\)`)
)

type reportFacts struct {
	ExperimentInputID string                       `json:"experiment_input_id"`
	ResultSHA256      string                       `json:"result_sha256"`
	CaseCount         int                          `json:"case_count"`
	Cases             map[string]reportCase        `json:"cases"`
	Scenarios         []map[string]json.RawMessage `json:"execution_scenarios"`
	FormalV1SHA256    string                       `json:"formal_v1_sha256,omitempty"`
	Market            json.RawMessage              `json:"market,omitempty"`
}

type reportMarketEvidence struct {
	DatasetID        string   `json:"dataset_id"`
	SourceSnapshotID string   `json:"source_snapshot_id"`
	EvaluationDates  []string `json:"evaluation_dates"`
}

type reportCase struct {
	Label      string `json:"label"`
	TradeCount *uint  `json:"filled_trade_count"`
	SourceSHA  string `json:"source_sha256"`
}

type reportMetrics struct {
	Returns  *big.Rat
	Drawdown *big.Rat
}

// This profile implements the checks of the existing offline evidence pack.
// A validated profile is a prerequisite for issuing any review verdict.
func auditLetsQuant(inputs map[string][]byte, profile string) ([]contract.ReportFinding, error) {
	if !utf8.Valid(inputs[reportName]) || bytes.IndexByte(inputs[reportName], 0) >= 0 {
		return nil, fmt.Errorf("report must be UTF-8 Markdown")
	}
	facts, err := readReportFacts(inputs["facts.json"])
	if err != nil {
		return nil, err
	}
	marketProfile := profile == contract.LetsQuantMarketReportProfile
	if marketProfile != (len(facts.Market) > 0) {
		return nil, fmt.Errorf("market evidence must match the selected review profile")
	}
	var market reportMarketEvidence
	if marketProfile {
		market, err = readReportMarketEvidence(facts.Market)
		if err != nil {
			return nil, err
		}
	}
	report := string(inputs[reportName])
	lines := strings.Split(strings.ReplaceAll(report, "\r\n", "\n"), "\n")
	findings := []contract.ReportFinding{}
	check := func(ok bool, code, summary, anchor string) {
		if ok {
			return
		}
		finding := contract.ReportFinding{Code: code, Summary: summary}
		for index, line := range lines {
			if strings.Contains(line, anchor) {
				finding.Line = uint64(index + 1)
				break
			}
		}
		findings = append(findings, finding)
	}
	check(strings.Contains(report, facts.ExperimentInputID) && strings.Contains(report, facts.ResultSHA256),
		"source-binding", "需要绑定冻结实验输入 ID 和结果 SHA-256。", "可复核证据")
	check(!unregisteredLink.MatchString(report), "unregistered-evidence-link",
		"报告正文的相对清单链接没有对应的已登记产物。改为明确本地交付包信息。", "delivery-manifest.json")
	if marketProfile {
		dates := market.EvaluationDates
		window := fmt.Sprintf("评估窗口 %s 至 %s，共 %d 个交易日", dates[0], dates[len(dates)-1], len(dates))
		check(strings.Contains(report, window), "market-window", "报告评估窗口与冻结交易日历不一致。", "评估窗口")
		check(strings.Contains(report, market.DatasetID) && strings.Contains(report, market.SourceSnapshotID),
			"market-source-binding", "需要列出冻结行情快照和清洗数据集的完整标识。", "可复核证据")
	}
	returns := make(map[string]*big.Rat, len(caseNames))
	for _, name := range caseNames {
		entry := facts.Cases[name]
		nav := inputs[name+".csv"]
		if digest(nav) != entry.SourceSHA {
			return nil, fmt.Errorf("scenario %s CSV does not match its facts digest", name)
		}
		metrics, err := readReportMetrics(nav, market.EvaluationDates)
		if err != nil {
			return nil, fmt.Errorf("scenario %s: %w", name, err)
		}
		returns[name] = metrics.Returns
		anchor := fmt.Sprintf("| %s |", entry.Label)
		want := fmt.Sprintf("%s %s%% | %s%% | %d |", anchor,
			formatReportMetric(metrics.Returns, 100, 3), formatReportMetric(metrics.Drawdown, 100, 3), *entry.TradeCount)
		check(slices.Contains(lines, want), "metric-reconciliation",
			entry.Label+"的表格指标与冻结净值证据不一致。", anchor)
	}
	base := returns["configured-costs"]
	friction := new(big.Rat).Sub(base, returns["high-friction"])
	delay := new(big.Rat).Sub(base, returns["delayed-execution"])
	comparison := fmt.Sprintf("低 %s 和 %s 个基点", formatReportMetric(friction, 10000, 2), formatReportMetric(delay, 10000, 2))
	comparisonValid := friction.Sign() >= 0 && delay.Sign() >= 0
	if marketProfile {
		comparison = fmt.Sprintf("相对配置成本的期末收益差为 %s / %s 个基点",
			formatReportMetric(friction.Neg(friction), 10000, 2),
			formatReportMetric(delay.Neg(delay), 10000, 2))
		comparisonValid = true
	}
	check(comparisonValid && strings.Contains(report, comparison),
		"basis-point-reconciliation", "场景之间的期末差值与净值证据不一致。", "个基点")
	for _, scenario := range facts.Scenarios {
		var name string
		// readReportFacts validates the scenario before returning it.
		_ = json.Unmarshal(scenario["name"], &name)
		if len(scenario) == 1 {
			continue
		}
		label := facts.Cases[name].Label
		anchor := fmt.Sprintf("| %s |", label)
		matched := slices.ContainsFunc(lines, func(line string) bool {
			if !strings.HasPrefix(line, anchor) {
				return false
			}
			for key, value := range scenario {
				if key != "name" && !strings.Contains(line, fmt.Sprintf("`%s=%s`", key, value)) {
					return false
				}
			}
			return true
		})
		check(matched, "omitted-overrides-"+name, label+"缺少完整执行参数覆盖，读者无法复核情景差异。", anchor)
	}
	return findings, nil
}

func readReportFacts(data []byte) (reportFacts, error) {
	var facts reportFacts
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if !utf8.Valid(data) || decoder.Decode(&facts) != nil || decoder.Decode(new(any)) != io.EOF {
		return reportFacts{}, fmt.Errorf("invalid lets-quant facts JSON")
	}
	if !factsSHA.MatchString(facts.ExperimentInputID) || !factsSHA.MatchString(facts.ResultSHA256) ||
		(facts.FormalV1SHA256 != "" && !factsSHA.MatchString(facts.FormalV1SHA256)) ||
		facts.CaseCount < len(caseNames) || len(facts.Cases) != len(caseNames) || len(facts.Scenarios) != len(caseNames) {
		return reportFacts{}, fmt.Errorf("incomplete lets-quant experiment facts")
	}
	labels := make(map[string]bool)
	for _, name := range caseNames {
		entry := facts.Cases[name]
		if len(entry.Label) == 0 || len(entry.Label) > 100 || strings.ContainsAny(entry.Label, "|\r\n\x00") ||
			labels[entry.Label] || entry.TradeCount == nil || !factsSHA.MatchString(entry.SourceSHA) {
			return reportFacts{}, fmt.Errorf("invalid facts for scenario %s", name)
		}
		labels[entry.Label] = true
	}
	allowedOverrides := map[string][]string{
		"configured-costs":  {},
		"high-friction":     {"commission_rate", "minimum_commission", "sell_tax_rate", "slippage_bps"},
		"delayed-execution": {"execution_delay_trading_days", "slippage_bps"},
	}
	seen := make(map[string]bool)
	for _, scenario := range facts.Scenarios {
		var name string
		if err := json.Unmarshal(scenario["name"], &name); err != nil {
			return reportFacts{}, fmt.Errorf("invalid scenario name")
		}
		keys, supported := allowedOverrides[name]
		if !supported || seen[name] || len(scenario) != len(keys)+1 {
			return reportFacts{}, fmt.Errorf("unexpected or incomplete execution scenario")
		}
		seen[name] = true
		for _, key := range keys {
			value := scenario[key]
			if !decimal.Match(value) || value[0] == '-' {
				return reportFacts{}, fmt.Errorf("invalid numeric scenario parameter %s", key)
			}
		}
	}
	return facts, nil
}

func readReportMarketEvidence(raw []byte) (reportMarketEvidence, error) {
	var market reportMarketEvidence
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&market) != nil || decoder.Decode(new(any)) != io.EOF ||
		!factsSHA.MatchString(market.DatasetID) || !factsSHA.MatchString(market.SourceSnapshotID) ||
		len(market.EvaluationDates) < 2 || len(market.EvaluationDates) > 5000 {
		return reportMarketEvidence{}, fmt.Errorf("invalid frozen market evidence")
	}
	var previous string
	for _, date := range market.EvaluationDates {
		if _, err := time.Parse(time.DateOnly, date); err != nil || date <= previous {
			return reportMarketEvidence{}, fmt.Errorf("invalid frozen market trading date")
		}
		previous = date
	}
	return market, nil
}

func readReportMetrics(data []byte, marketDates []string) (reportMetrics, error) {
	reader := csv.NewReader(bytes.NewReader(data))
	header, err := reader.Read()
	if err != nil || !slices.Equal(header, []string{"date", "nav", "cash", "drawdown", "risk_frozen", "positions"}) {
		return reportMetrics{}, fmt.Errorf("invalid NAV CSV header")
	}
	var firstNAV, finalNAV *big.Rat
	minimumDrawdown := new(big.Rat)
	firstDate, lastDate, count := "2024-01-01", "2024-12-27", 260
	if len(marketDates) > 0 {
		firstDate, lastDate, count = marketDates[0], marketDates[len(marketDates)-1], len(marketDates)
	}
	prevDate := ""
	for index := 0; index < count; index++ {
		row, err := reader.Read()
		if err != nil {
			return reportMetrics{}, fmt.Errorf("read NAV CSV row %d: %w", index+1, err)
		}
		date := row[0]
		if _, err := time.Parse(time.DateOnly, date); err != nil || date <= prevDate ||
			(index == 0 && date != firstDate) || (index == count-1 && date != lastDate) ||
			(len(marketDates) > 0 && date != marketDates[index]) {
			return reportMetrics{}, fmt.Errorf("invalid date in NAV CSV row %d", index+1)
		}
		prevDate = date
		if !decimal.MatchString(row[1]) || !decimal.MatchString(row[3]) {
			return reportMetrics{}, fmt.Errorf("invalid decimal in NAV CSV row %d", index+1)
		}
		nav, _ := new(big.Rat).SetString(row[1])
		drawdown, _ := new(big.Rat).SetString(row[3])
		if nav.Sign() <= 0 || drawdown.Sign() > 0 || drawdown.Cmp(big.NewRat(-1, 1)) < 0 {
			return reportMetrics{}, fmt.Errorf("out-of-range NAV or drawdown in row %d", index+1)
		}
		if firstNAV == nil {
			firstNAV = nav
		}
		finalNAV = nav
		if drawdown.Cmp(minimumDrawdown) < 0 {
			minimumDrawdown = drawdown
		}
	}
	if _, err := reader.Read(); err != io.EOF {
		return reportMetrics{}, fmt.Errorf("NAV CSV must contain exactly %d data rows", count)
	}
	returns := new(big.Rat).Sub(new(big.Rat).Quo(finalNAV, firstNAV), big.NewRat(1, 1))
	return reportMetrics{Returns: returns, Drawdown: minimumDrawdown}, nil
}

// Match the reference Decimal formatting, including ties to even.
func formatReportMetric(value *big.Rat, units int64, places int) string {
	scale := int64(1)
	for i := 0; i < places; i++ {
		scale *= 10
	}
	numerator := new(big.Int).Mul(new(big.Int).Abs(value.Num()), big.NewInt(units*scale))
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, value.Denom(), remainder)
	comparison := remainder.Lsh(remainder, 1).Cmp(value.Denom())
	if comparison > 0 || (comparison == 0 && quotient.Bit(0) == 1) {
		quotient.Add(quotient, big.NewInt(1))
	}
	whole, fractional := new(big.Int), new(big.Int)
	whole.QuoRem(quotient, big.NewInt(scale), fractional)
	sign := ""
	if value.Sign() < 0 {
		sign = "-"
	}
	return fmt.Sprintf("%s%s.%0*d", sign, whole, places, fractional.Int64())
}
