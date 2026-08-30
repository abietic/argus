package reviewcore

import (
	"context"
	"fmt"
	"slices"
)

const (
	DetectorFixtureMarkerID       = "argus.fixture-marker"
	DetectorFixtureMarkerRevision = "1"
	DetectorGoASTID               = "argus.go-ast"
	DetectorGoASTRevision         = "1"

	RuleGoContextCancelDiscarded = "go-context-cancel-discarded"
)

// Keep the old package-private names while existing fixture tests migrate.
// Runtime validation no longer treats this identity as the only detector.
const (
	detectorID       = DetectorFixtureMarkerID
	detectorRevision = DetectorFixtureMarkerRevision
)

type DetectorDescriptor struct {
	ID       string
	Revision string
}

type RuleDescriptor struct {
	ID               string
	Revision         string
	DetectorID       string
	DetectorRevision string
	SignalKind       DetectionSignalKind
	Title            string
	Message          string
}

type registeredRule struct {
	RuleDescriptor
	signals []string
}

var orderedRegisteredRules = []registeredRule{
	{
		RuleDescriptor: RuleDescriptor{
			ID: RuleUnfinishedWork, Revision: "1",
			DetectorID:       DetectorFixtureMarkerID,
			DetectorRevision: DetectorFixtureMarkerRevision,
			SignalKind:       DetectionSignalLexicalMarker,
			Title:            "评审目标包含未完成标记",
			Message:          "评审目标中的 Go 代码包含 TODO/FIXME，需确认是否应在交付前完成。",
		},
		signals: []string{"FIXME", "TODO"},
	},
	{
		RuleDescriptor: RuleDescriptor{
			ID: RuleExplicitBugMarker, Revision: "1",
			DetectorID:       DetectorFixtureMarkerID,
			DetectorRevision: DetectorFixtureMarkerRevision,
			SignalKind:       DetectionSignalLexicalMarker,
			Title:            "评审目标包含显式缺陷标记",
			Message:          "评审目标中的 Go 代码包含 ARGUS_BUG 显式缺陷标记。",
		},
		signals: []string{"ARGUS_BUG"},
	},
	{
		RuleDescriptor: RuleDescriptor{
			ID: RuleGoContextCancelDiscarded, Revision: "1",
			DetectorID:       DetectorGoASTID,
			DetectorRevision: DetectorGoASTRevision,
			SignalKind:       DetectionSignalGoASTPattern,
			Title:            "context 取消函数被丢弃",
			Message: "context.WithCancel/WithTimeout/WithDeadline 返回的取消函数被丢弃，" +
				"可能导致计时器、父子上下文引用或关联资源延迟释放。",
		},
		signals: []string{
			"context.WithCancel",
			"context.WithDeadline",
			"context.WithTimeout",
		},
	},
}

func LookupRuleDescriptor(ruleID string) (RuleDescriptor, bool) {
	rule, found := registeredRuleFor(ruleID)
	if !found {
		return RuleDescriptor{}, false
	}
	return rule.RuleDescriptor, true
}

func RegisteredDetectorDescriptors() []DetectorDescriptor {
	return []DetectorDescriptor{
		{ID: DetectorFixtureMarkerID, Revision: DetectorFixtureMarkerRevision},
		{ID: DetectorGoASTID, Revision: DetectorGoASTRevision},
	}
}

func registeredRuleFor(ruleID string) (registeredRule, bool) {
	for _, rule := range orderedRegisteredRules {
		if rule.ID == ruleID {
			return rule, true
		}
	}
	return registeredRule{}, false
}

func validSignal(ruleID string, kind DetectionSignalKind, signal string) bool {
	rule, found := registeredRuleFor(ruleID)
	return found && rule.SignalKind == kind && slices.Contains(rule.signals, signal)
}

type detectorRequest struct {
	input        ReviewInput
	targetDigest string
	policy       RuntimePolicy
	targetLines  []reviewTargetLine
}

type detectorOutput struct {
	candidates []CandidateFinding
	gaps       []DetectionGap
}

type detectorRunner struct {
	descriptor DetectorDescriptor
	run        func(context.Context, detectorRequest) (detectorOutput, error)
}

// detectorRegistry is deliberately closed and ordered. A rule is executable
// only when both its rule revision and this exact detector identity are present
// in the frozen RuntimePolicy.
func detectorRegistry() []detectorRunner {
	return []detectorRunner{
		{
			descriptor: DetectorDescriptor{
				ID: DetectorFixtureMarkerID, Revision: DetectorFixtureMarkerRevision,
			},
			run: detectLexicalMarkers,
		},
		{
			descriptor: DetectorDescriptor{
				ID: DetectorGoASTID, Revision: DetectorGoASTRevision,
			},
			run: detectGoContextCancelDiscarded,
		},
	}
}

func detectorRule(
	policy RuntimePolicy,
	descriptor DetectorDescriptor,
	ruleID string,
) (RuntimeRule, bool, error) {
	rule, configured := runtimeRuleFor(policy, ruleID)
	if !configured || !rule.Enabled {
		return RuntimeRule{}, false, nil
	}
	if rule.DetectorID != descriptor.ID || rule.DetectorRevision != descriptor.Revision {
		return RuntimeRule{}, false, fmt.Errorf(
			"enabled runtime rule %q does not match detector %q@%q",
			ruleID,
			descriptor.ID,
			descriptor.Revision,
		)
	}
	return rule, true, nil
}
