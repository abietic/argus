package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/source/gitadapter"
)

const (
	coverageUnknown = "unknown"
	maxShownReasons = 8
)

type runCoverageSummary struct {
	State           string                       `json:"state"`
	TargetSummary   string                       `json:"target_summary,omitempty"`
	EvidenceSummary string                       `json:"evidence_summary,omitempty"`
	EffectiveRanges []application.SelectionRange `json:"effective_ranges,omitempty"`
	Symbol          *application.SymbolSelector  `json:"symbol,omitempty"`
	Reasons         []string                     `json:"reasons"`
}

func summarizeRunCoverage(
	repository *runrepo.Repository,
	run runmodel.ReviewRun,
) runCoverageSummary {
	summary := runCoverageSummary{
		State:   coverageUnknown,
		Reasons: []string{},
	}
	targetLoaded := false
	if repository != nil && run.TargetSnapshotRef.SHA256 != "" {
		var target application.MaterializedTarget
		if err := repository.ReadJSONArtifact(run.TargetSnapshotRef, &target); err != nil {
			summary.Reasons = appendUnique(
				summary.Reasons,
				"coverage metadata unavailable: "+err.Error(),
			)
		} else {
			targetLoaded = true
			summary.State = string(target.Snapshot.Completeness)
			summary.TargetSummary = summarizeTarget(repository, target)
			if target.Snapshot.Selection != nil {
				selection := target.Snapshot.Selection
				summary.EffectiveRanges = append(
					[]application.SelectionRange(nil),
					selection.EffectiveRanges...,
				)
				if selection.Symbol != nil {
					symbol := *selection.Symbol
					summary.Symbol = &symbol
				}
			}
			for _, reason := range target.Snapshot.CompletenessReason {
				summary.Reasons = appendUnique(
					summary.Reasons,
					formatCoverageReason("", reason),
				)
			}
			for _, file := range target.FileRefs {
				if file.Status == gitadapter.ChangeDeleted ||
					file.Completeness == gitadapter.CompletenessComplete {
					continue
				}
				summary.State = worseCoverage(summary.State, string(file.Completeness))
				for _, reason := range file.Reasons {
					summary.Reasons = appendUnique(
						summary.Reasons,
						formatCoverageReason(file.Path, reason),
					)
				}
			}
			if len(summary.Reasons) > 0 &&
				summary.State == string(gitadapter.CompletenessComplete) {
				summary.State = string(gitadapter.CompletenessPartial)
			}
		}
	}

	completeEvidence := 0
	partialEvidence := 0
	for _, evidence := range run.Evidence {
		switch evidence.Completeness {
		case runmodel.CompletenessComplete:
			completeEvidence++
		case runmodel.CompletenessPartial:
			partialEvidence++
			summary.State = worseCoverage(
				summary.State,
				string(runmodel.CompletenessPartial),
			)
		default:
			summary.State = worseCoverage(summary.State, coverageUnknown)
		}
		for _, note := range evidence.CompletenessNotes {
			summary.Reasons = appendUnique(summary.Reasons, note)
		}
	}
	if len(run.Evidence) > 0 {
		summary.EvidenceSummary = fmt.Sprintf(
			"stages=%d, complete=%d, partial=%d",
			len(run.Evidence),
			completeEvidence,
			partialEvidence,
		)
	}
	if !targetLoaded && len(run.Evidence) > 0 && summary.State == coverageUnknown {
		if partialEvidence > 0 {
			summary.State = string(runmodel.CompletenessPartial)
		} else if completeEvidence == len(run.Evidence) {
			summary.State = string(runmodel.CompletenessComplete)
		}
	}
	if summary.State == string(runmodel.CompletenessPartial) &&
		len(summary.Reasons) == 0 {
		summary.Reasons = append(
			summary.Reasons,
			"partial coverage reported without a structured reason",
		)
	}
	return summary
}

func summarizeTarget(
	repository *runrepo.Repository,
	target application.MaterializedTarget,
) string {
	switch target.Snapshot.Mode {
	case "diff":
		patchBytes := int64(0)
		if target.Snapshot.Diff != nil {
			patchBytes = target.Snapshot.Diff.PatchSizeBytes
		}
		var manifest gitadapter.ChangeManifest
		if err := repository.ReadJSONArtifact(target.ManifestRef, &manifest); err == nil {
			unavailable := 0
			deleted := 0
			for _, file := range target.FileRefs {
				if file.Status == gitadapter.ChangeDeleted {
					deleted++
				} else if file.ContentRef == nil {
					unavailable++
				}
			}
			return fmt.Sprintf(
				"diff patch=%d bytes; files total=%d, included=%d, skipped=%d; "+
					"hunks total=%d, included=%d, skipped=%d; "+
					"exact content unavailable=%d, deleted=%d",
				patchBytes,
				manifest.Coverage.TotalFiles,
				manifest.Coverage.IncludedFiles,
				manifest.Coverage.SkippedFiles,
				manifest.Coverage.TotalHunks,
				manifest.Coverage.IncludedHunks,
				manifest.Coverage.SkippedHunks,
				unavailable,
				deleted,
			)
		}
		return fmt.Sprintf(
			"diff patch=%d bytes; retained file references=%d",
			patchBytes,
			len(target.FileRefs),
		)
	case "selection":
		if target.Snapshot.Selection == nil {
			return "selection coverage metadata is unavailable"
		}
		selection := target.Snapshot.Selection
		selector := "effective ranges=" +
			formatEffectiveRanges(selection.EffectiveRanges)
		if selection.StartLine != 0 {
			selector = fmt.Sprintf(
				"lines=%d-%d",
				selection.StartLine,
				selection.EndLine,
			)
		}
		if selection.Symbol != nil {
			selector = fmt.Sprintf(
				"symbol=%s/%s %s, resolved ranges=%s",
				selection.Symbol.Language,
				selection.Symbol.Kind,
				selection.Symbol.QualifiedName,
				formatEffectiveRanges(selection.EffectiveRanges),
			)
		}
		return fmt.Sprintf(
			"selection path=%s, %s, source=%s, bytes=%d",
			selection.Path,
			selector,
			selection.SourceKind,
			selection.SelectionSizeBytes,
		)
	case "scope":
		if target.Snapshot.Scope == nil {
			return "scope coverage metadata is unavailable"
		}
		scope := target.Snapshot.Scope
		var manifest application.ScopeManifest
		if err := repository.ReadJSONArtifact(target.ManifestRef, &manifest); err == nil {
			return fmt.Sprintf(
				"scope files scanned=%d, matched=%d, included=%d, skipped=%d, excluded=%d",
				manifest.Coverage.ScannedFiles,
				manifest.Coverage.MatchedFiles,
				manifest.Coverage.IncludedFiles,
				manifest.Coverage.SkippedFiles,
				manifest.Coverage.ExcludedFiles,
			)
		}
		return fmt.Sprintf(
			"scope files matched=%d, included=%d, skipped=%d",
			scope.MatchedFiles,
			scope.IncludedFiles,
			scope.SkippedFiles,
		)
	default:
		return fmt.Sprintf("target mode=%s", target.Snapshot.Mode)
	}
}

func writeCoverageMarkdown(
	stdout io.Writer,
	summary runCoverageSummary,
	asList bool,
) (int, error) {
	prefix := ""
	lineEnd := "  \n"
	if asList {
		prefix = "- "
		lineEnd = "\n"
	}
	written := 0
	writeLine := func(label string, value string) error {
		count, err := fmt.Fprintf(
			stdout,
			"%s%s: %s%s",
			prefix,
			label,
			value,
			lineEnd,
		)
		written += count
		return err
	}
	coverage := "`" + markdownCell(summary.State) + "`"
	switch summary.State {
	case string(gitadapter.CompletenessPartial):
		coverage += ` — **INCOMPLETE; "No findings" is not a clean verdict for the requested target**`
	case string(gitadapter.CompletenessSkipped):
		coverage += ` — **NOT REVIEWED; "No findings" is not a clean verdict**`
	case coverageUnknown:
		coverage += ` — **UNKNOWN; "No findings" is not a clean verdict**`
	}
	if err := writeLine("Coverage", coverage); err != nil {
		return written, err
	}
	if summary.TargetSummary != "" {
		if err := writeLine(
			"Coverage summary",
			markdownCell(summary.TargetSummary),
		); err != nil {
			return written, err
		}
	}
	if len(summary.EffectiveRanges) > 0 {
		if err := writeLine(
			"Effective ranges",
			"`"+markdownCell(formatEffectiveRanges(summary.EffectiveRanges))+"`",
		); err != nil {
			return written, err
		}
	}
	if summary.Symbol != nil {
		if err := writeLine(
			"Symbol",
			fmt.Sprintf(
				"`language=%s, kind=%s, qualified_name=%s`",
				markdownCell(summary.Symbol.Language),
				markdownCell(summary.Symbol.Kind),
				markdownCell(summary.Symbol.QualifiedName),
			),
		); err != nil {
			return written, err
		}
	}
	if summary.EvidenceSummary != "" {
		if err := writeLine(
			"Coverage evidence",
			markdownCell(summary.EvidenceSummary),
		); err != nil {
			return written, err
		}
	}
	reasons := "none"
	if len(summary.Reasons) > 0 {
		shown := summary.Reasons
		if len(shown) > maxShownReasons {
			shown = shown[:maxShownReasons]
		}
		escaped := make([]string, 0, len(shown))
		for _, reason := range shown {
			escaped = append(escaped, "`"+markdownCell(reason)+"`")
		}
		reasons = strings.Join(escaped, "; ")
		if remaining := len(summary.Reasons) - len(shown); remaining > 0 {
			reasons += fmt.Sprintf("; and %d more", remaining)
		}
	}
	if err := writeLine("Coverage reasons", reasons); err != nil {
		return written, err
	}
	count, err := fmt.Fprintln(stdout)
	written += count
	return written, err
}

func formatEffectiveRanges(ranges []application.SelectionRange) string {
	formatted := make([]string, 0, len(ranges))
	for _, lineRange := range ranges {
		formatted = append(
			formatted,
			fmt.Sprintf("%d:%d", lineRange.StartLine, lineRange.EndLine),
		)
	}
	return strings.Join(formatted, ",")
}

func formatCoverageReason(path string, reason gitadapter.Reason) string {
	value := string(reason.Code)
	if path != "" {
		value = path + ": " + value
	}
	if reason.Detail != "" {
		value += ": " + reason.Detail
	}
	return value
}

func appendUnique(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func worseCoverage(current string, candidate string) string {
	rank := func(value string) int {
		switch value {
		case string(gitadapter.CompletenessComplete):
			return 0
		case coverageUnknown:
			return 1
		case string(gitadapter.CompletenessPartial):
			return 2
		case string(gitadapter.CompletenessSkipped):
			return 3
		default:
			return 1
		}
	}
	if rank(candidate) > rank(current) {
		return candidate
	}
	return current
}
