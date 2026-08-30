package reviewcore

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"sort"
	"strconv"
	"strings"
)

type signalOccurrence struct {
	rule   registeredRule
	signal string
	offset int
}

func detectLexicalMarkers(
	ctx context.Context,
	request detectorRequest,
) (detectorOutput, error) {
	descriptor := DetectorDescriptor{
		ID: DetectorFixtureMarkerID, Revision: DetectorFixtureMarkerRevision,
	}
	candidates := make([]CandidateFinding, 0)
	for _, line := range request.targetLines {
		if err := checkContext(ctx); err != nil {
			return detectorOutput{}, err
		}
		if err := validateRepositoryPath("target.path", line.Path); err != nil {
			continue
		}
		occurrences := make([]signalOccurrence, 0)
		for _, registered := range orderedRegisteredRules {
			if registered.DetectorID != descriptor.ID ||
				registered.DetectorRevision != descriptor.Revision {
				continue
			}
			runtimeRule, enabled, err := detectorRule(
				request.policy,
				descriptor,
				registered.ID,
			)
			if err != nil {
				return detectorOutput{}, err
			}
			if !enabled || !runtimeRuleApplies(runtimeRule, line.Path) {
				continue
			}
			for _, signal := range registered.signals {
				for searchFrom := 0; searchFrom <= len(line.Text)-len(signal); {
					offset := strings.Index(line.Text[searchFrom:], signal)
					if offset < 0 {
						break
					}
					offset += searchFrom
					occurrences = append(occurrences, signalOccurrence{
						rule: registered, signal: signal, offset: offset,
					})
					searchFrom = offset + len(signal)
				}
			}
		}
		sort.Slice(occurrences, func(left, right int) bool {
			if occurrences[left].offset != occurrences[right].offset {
				return occurrences[left].offset < occurrences[right].offset
			}
			if occurrences[left].rule.ID != occurrences[right].rule.ID {
				return occurrences[left].rule.ID < occurrences[right].rule.ID
			}
			return occurrences[left].signal < occurrences[right].signal
		})
		for _, occurrence := range occurrences {
			candidate, err := newDetectorCandidate(
				request.targetDigest,
				occurrence.rule,
				line,
				occurrence.signal,
				occurrence.offset,
			)
			if err != nil {
				return detectorOutput{}, err
			}
			candidates = append(candidates, candidate)
		}
	}
	return detectorOutput{candidates: candidates, gaps: []DetectionGap{}}, nil
}

func detectGoContextCancelDiscarded(
	ctx context.Context,
	request detectorRequest,
) (detectorOutput, error) {
	registered, found := registeredRuleFor(RuleGoContextCancelDiscarded)
	if !found {
		return detectorOutput{}, fmt.Errorf("Go AST rule is absent from detector registry")
	}
	descriptor := DetectorDescriptor{
		ID: DetectorGoASTID, Revision: DetectorGoASTRevision,
	}
	runtimeRule, enabled, err := detectorRule(request.policy, descriptor, registered.ID)
	if err != nil {
		return detectorOutput{}, err
	}
	if !enabled {
		return detectorOutput{candidates: []CandidateFinding{}, gaps: []DetectionGap{}}, nil
	}

	targetByPath := make(map[string]map[uint32]reviewTargetLine)
	orderedPaths := make([]string, 0)
	for _, line := range request.targetLines {
		if validateRepositoryPath("target.path", line.Path) != nil ||
			!runtimeRuleApplies(runtimeRule, line.Path) {
			continue
		}
		if targetByPath[line.Path] == nil {
			targetByPath[line.Path] = make(map[uint32]reviewTargetLine)
			orderedPaths = append(orderedPaths, line.Path)
		}
		targetByPath[line.Path][line.Line] = line
	}
	sort.Strings(orderedPaths)
	files := make(map[string]FileManifestEntry, len(request.input.Files))
	for _, file := range request.input.Files {
		files[file.Path] = file
	}

	candidates := make([]CandidateFinding, 0)
	gaps := make([]DetectionGap, 0)
	for _, repositoryPath := range orderedPaths {
		if err := checkContext(ctx); err != nil {
			return detectorOutput{}, err
		}
		targetLines := targetByPath[repositoryPath]
		firstTarget := firstAuthorizedLine(targetLines)
		file, exists := files[repositoryPath]
		if !exists || file.Content == nil {
			sourceDigest := firstTarget.SourceDigest
			if exists {
				sourceDigest = file.SHA256
			}
			gaps = append(gaps, newDetectionGap(
				request.targetDigest,
				sourceDigest,
				repositoryPath,
				firstTarget.Line,
				registered,
				DetectionGapContentUnavailable,
			))
			continue
		}
		matches, parseErr := discardedContextCancelMatches(repositoryPath, *file.Content)
		if parseErr != nil {
			gaps = append(gaps, newDetectionGap(
				request.targetDigest,
				file.SHA256,
				repositoryPath,
				firstTarget.Line,
				registered,
				DetectionGapGoParseFailed,
			))
			continue
		}
		for _, match := range matches {
			targetLine, authorized := targetLines[match.line]
			if !authorized {
				continue
			}
			candidate, err := newDetectorCandidate(
				request.targetDigest,
				registered,
				targetLine,
				match.signal,
				match.offset,
			)
			if err != nil {
				return detectorOutput{}, err
			}
			candidates = append(candidates, candidate)
		}
	}
	return detectorOutput{candidates: candidates, gaps: gaps}, nil
}

func firstAuthorizedLine(lines map[uint32]reviewTargetLine) reviewTargetLine {
	numbers := make([]uint32, 0, len(lines))
	for number := range lines {
		numbers = append(numbers, number)
	}
	slices.Sort(numbers)
	return lines[numbers[0]]
}

func newDetectorCandidate(
	targetDigest string,
	rule registeredRule,
	line reviewTargetLine,
	signal string,
	offset int,
) (CandidateFinding, error) {
	if offset < 0 || offset > int(^uint32(0)) {
		return CandidateFinding{}, fmt.Errorf("detector signal offset is out of range")
	}
	signalOffset := uint32(offset)
	candidateID := stableID(
		"candidate",
		rule.DetectorID,
		rule.DetectorRevision,
		targetDigest,
		rule.ID,
		string(rule.SignalKind),
		line.Path,
		strconv.FormatUint(uint64(line.Line), 10),
		strconv.FormatUint(uint64(signalOffset), 10),
		line.Text,
	)
	evidence := Evidence{
		ID:           stableID("evidence", string(line.EvidenceKind), candidateID),
		Kind:         line.EvidenceKind,
		SourceDigest: line.SourceDigest,
		Path:         line.Path,
		Line:         line.Line,
		Excerpt:      line.Text,
		Claim:        targetEvidenceClaim(rule.SignalKind, line.EvidenceKind),
	}
	candidate := CandidateFinding{
		ID:               candidateID,
		DetectorID:       rule.DetectorID,
		DetectorRevision: rule.DetectorRevision,
		RuleID:           rule.ID,
		TargetDigest:     targetDigest,
		Path:             line.Path,
		StartLine:        line.Line,
		EndLine:          line.Line,
		SignalKind:       rule.SignalKind,
		Signal:           signal,
		SignalOffset:     signalOffset,
		Excerpt:          line.Text,
		Evidence:         []Evidence{evidence},
	}
	if err := candidate.Validate(); err != nil {
		return CandidateFinding{}, fmt.Errorf("validate detector candidate: %w", err)
	}
	return candidate, nil
}

func targetEvidenceClaim(kind DetectionSignalKind, evidenceKind EvidenceKind) string {
	switch {
	case kind == DetectionSignalLexicalMarker && evidenceKind == EvidencePatchLine:
		return "marker_occurs_on_added_line"
	case kind == DetectionSignalLexicalMarker && evidenceKind == EvidenceTargetLine:
		return "marker_occurs_on_target_line"
	case kind == DetectionSignalGoASTPattern && evidenceKind == EvidencePatchLine:
		return "ast_pattern_occurs_on_added_line"
	case kind == DetectionSignalGoASTPattern && evidenceKind == EvidenceTargetLine:
		return "ast_pattern_occurs_on_target_line"
	default:
		return ""
	}
}

func newDetectionGap(
	targetDigest string,
	sourceDigest string,
	repositoryPath string,
	line uint32,
	rule registeredRule,
	reason DetectionGapReason,
) DetectionGap {
	gap := DetectionGap{
		DetectorID:       rule.DetectorID,
		DetectorRevision: rule.DetectorRevision,
		RuleID:           rule.ID,
		TargetDigest:     targetDigest,
		SourceDigest:     sourceDigest,
		Path:             repositoryPath,
		Line:             line,
		ReasonCode:       reason,
	}
	gap.ID = stableID(
		"detection-gap",
		gap.DetectorID,
		gap.DetectorRevision,
		gap.RuleID,
		gap.TargetDigest,
		gap.SourceDigest,
		gap.Path,
		strconv.FormatUint(uint64(gap.Line), 10),
		string(gap.ReasonCode),
	)
	return gap
}

type goASTMatch struct {
	line   uint32
	offset int
	signal string
}

func discardedContextCancelMatches(
	repositoryPath string,
	content string,
) ([]goASTMatch, error) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, repositoryPath, content, parser.AllErrors)
	if err != nil {
		return nil, err
	}
	if !importsDefaultContext(file) {
		return []goASTMatch{}, nil
	}
	matches := make([]goASTMatch, 0)
	ast.Inspect(file, func(node ast.Node) bool {
		var left []ast.Expr
		var right []ast.Expr
		switch value := node.(type) {
		case *ast.AssignStmt:
			left = value.Lhs
			right = value.Rhs
		case *ast.ValueSpec:
			left = make([]ast.Expr, len(value.Names))
			for index, name := range value.Names {
				left[index] = name
			}
			right = value.Values
		default:
			return true
		}
		if len(left) != 2 || len(right) != 1 || !isBlankIdentifier(left[1]) {
			return true
		}
		call, ok := right[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		signal, position, ok := contextConstructorSignal(call)
		if !ok {
			return true
		}
		tokenPosition := fileSet.Position(position)
		line, exists := lineAt(content, uint32(tokenPosition.Line))
		if !exists {
			return true
		}
		offset := tokenPosition.Column - 1
		if offset < 0 || offset+len(signal) > len(line) ||
			line[offset:offset+len(signal)] != signal {
			return true
		}
		matches = append(matches, goASTMatch{
			line: uint32(tokenPosition.Line), offset: offset, signal: signal,
		})
		return true
	})
	sort.Slice(matches, func(left, right int) bool {
		if matches[left].line != matches[right].line {
			return matches[left].line < matches[right].line
		}
		if matches[left].offset != matches[right].offset {
			return matches[left].offset < matches[right].offset
		}
		return matches[left].signal < matches[right].signal
	})
	return matches, nil
}

func importsDefaultContext(file *ast.File) bool {
	for _, imported := range file.Imports {
		if imported.Path == nil || imported.Path.Value != `"context"` {
			continue
		}
		return imported.Name == nil || imported.Name.Name == "context"
	}
	return false
}

func isBlankIdentifier(expression ast.Expr) bool {
	identifier, ok := expression.(*ast.Ident)
	return ok && identifier.Name == "_"
}

func contextConstructorSignal(
	call *ast.CallExpr,
) (string, token.Pos, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", token.NoPos, false
	}
	packageIdentifier, ok := selector.X.(*ast.Ident)
	if !ok || packageIdentifier.Name != "context" || packageIdentifier.Obj != nil {
		return "", token.NoPos, false
	}
	switch selector.Sel.Name {
	case "WithCancel", "WithTimeout", "WithDeadline":
		return "context." + selector.Sel.Name, packageIdentifier.Pos(), true
	default:
		return "", token.NoPos, false
	}
}

func goASTSignalAt(content, repositoryPath string, line uint32, signal string) (bool, error) {
	matches, err := discardedContextCancelMatches(repositoryPath, content)
	if err != nil {
		return false, err
	}
	for _, match := range matches {
		if match.line == line && match.signal == signal {
			return true, nil
		}
	}
	return false, nil
}
