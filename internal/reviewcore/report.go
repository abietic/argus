package reviewcore

import (
	"context"
	"fmt"
	"strings"
)

func RenderReportMarkdown(ctx context.Context, report Report) (string, error) {
	if err := checkContext(ctx); err != nil {
		return "", err
	}
	if err := report.Validate(); err != nil {
		return "", err
	}
	var builder strings.Builder
	builder.WriteString("# Argus deterministic review\n\n")
	builder.WriteString("- Target digest: `")
	builder.WriteString(report.TargetDigest)
	builder.WriteString("`\n")
	fmt.Fprintf(
		&builder,
		"- Findings: %d (verified %d, rejected %d, inconclusive %d)\n",
		report.Summary.Findings,
		report.Summary.Verified,
		report.Summary.Rejected,
		report.Summary.Inconclusive,
	)
	fmt.Fprintf(
		&builder,
		"- Decisions: publish %d, human review %d\n\n",
		report.Summary.Publish,
		report.Summary.HumanReview,
	)
	if len(report.Findings) == 0 {
		builder.WriteString("No findings.\n")
		return builder.String(), nil
	}
	decisions := make(map[string]FindingDecision, len(report.Decisions))
	for _, decision := range report.Decisions {
		current, exists := decisions[decision.FindingID]
		if !exists || decision.Sequence > current.Sequence {
			decisions[decision.FindingID] = decision
		}
	}
	builder.WriteString("| Status | Decision | Location | Rule | Message |\n")
	builder.WriteString("|---|---|---|---|---|\n")
	for _, finding := range report.Findings {
		if err := checkContext(ctx); err != nil {
			return "", err
		}
		decision := decisions[finding.ID]
		fmt.Fprintf(
			&builder,
			"| %s | %s | `%s:%d` | `%s` | %s |\n",
			finding.Verification.Status,
			decision.Action,
			escapeMarkdownCell(finding.Path),
			finding.StartLine,
			escapeMarkdownCell(finding.RuleID),
			escapeMarkdownCell(finding.Message),
		)
	}
	return builder.String(), nil
}

func escapeMarkdownCell(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "|", `\|`)
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}
