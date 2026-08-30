package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const DefinitionSchemaVersion = "argus.workflow.v1alpha1"

type SideEffect string

const (
	SideEffectNone          SideEffect = "none"
	SideEffectRemotePublish SideEffect = "remote_publish"
)

type FailurePolicy string

const (
	FailurePolicyFailRun       FailurePolicy = "fail_run"
	FailurePolicyAllowPartial  FailurePolicy = "allow_partial"
	FailurePolicyRecordPending FailurePolicy = "record_pending"
)

type ReplayPolicy string

const (
	ReplayPolicyCheckpoint ReplayPolicy = "checkpoint"
	ReplayPolicyExact      ReplayPolicy = "exact"
	ReplayPolicyForbidden  ReplayPolicy = "forbidden"
)

type UnknownOutcomePolicy string

const (
	UnknownOutcomeFail      UnknownOutcomePolicy = "fail"
	UnknownOutcomeReconcile UnknownOutcomePolicy = "reconcile"
)

// StageBudget is part of the immutable workflow definition. Runtime policy may
// tighten these values, but it cannot enlarge them.
type StageBudget struct {
	TimeoutMS      int64 `json:"timeout_ms"`
	MaxInputBytes  int64 `json:"max_input_bytes"`
	MaxOutputBytes int64 `json:"max_output_bytes"`
	MaxConcurrency int   `json:"max_concurrency"`
}

// RetryPolicy makes retry and unknown-outcome handling explicit. Retryable
// codes are a versioned allow-list; an empty list means no error is retryable.
type RetryPolicy struct {
	MaxAttempts    int                  `json:"max_attempts"`
	BackoffMS      int64                `json:"backoff_ms"`
	Jitter         bool                 `json:"jitter"`
	RetryableCodes []string             `json:"retryable_codes"`
	UnknownOutcome UnknownOutcomePolicy `json:"unknown_outcome"`
}

// StageAuthorityCeiling is an upper bound, not a request for capabilities.
// Runtime/config policy may tighten it, but exceeding any field is a
// configuration error rather than an instruction to silently clamp access.
// Model egress is brokered separately from tool network access.
type StageAuthorityCeiling struct {
	ModelEgress        string   `json:"model_egress"`
	AllowedTools       []string `json:"allowed_tools"`
	ToolNetwork        string   `json:"tool_network"`
	WorkspaceReads     string   `json:"workspace_reads"`
	WorkspaceWrites    string   `json:"workspace_writes"`
	RemoteWrites       string   `json:"remote_writes"`
	MaxDelegationDepth int      `json:"max_delegation_depth"`
	MaxModelCalls      int      `json:"max_model_calls"`
	MaxToolCalls       int      `json:"max_tool_calls"`
}

type Stage struct {
	ID                     string                 `json:"stage_id"`
	Kind                   string                 `json:"kind"`
	ImplementationRevision string                 `json:"implementation_revision"`
	InputContract          string                 `json:"input_contract"`
	OutputContract         string                 `json:"output_contract"`
	DependsOn              []string               `json:"depends_on"`
	Executor               string                 `json:"executor"`
	RequiredCapabilities   []string               `json:"required_capabilities"`
	AuthorityCeiling       *StageAuthorityCeiling `json:"authority_ceiling,omitempty"`
	Budget                 StageBudget            `json:"budget"`
	Retry                  RetryPolicy            `json:"retry"`
	FailurePolicy          FailurePolicy          `json:"failure_policy"`
	SideEffect             SideEffect             `json:"side_effect"`
	ReplayPolicy           ReplayPolicy           `json:"replay_policy"`
}

type Definition struct {
	SchemaVersion string  `json:"schema_version"`
	ID            string  `json:"workflow_id"`
	Revision      string  `json:"revision"`
	Stages        []Stage `json:"stages"`
}

func DefaultReviewDefinition() Definition {
	return Definition{
		SchemaVersion: DefinitionSchemaVersion,
		ID:            "local-deterministic-review",
		Revision:      "2",
		Stages: []Stage{
			defaultStage(
				"materialize_target",
				"materialize_target",
				[]string{},
				ReplayPolicyCheckpoint,
			),
			defaultStage(
				"plan_context",
				"plan_context",
				[]string{"materialize_target"},
				ReplayPolicyCheckpoint,
			),
			defaultStage(
				"detect",
				"detect",
				[]string{"plan_context"},
				ReplayPolicyCheckpoint,
			),
			defaultStage("normalize", "normalize", []string{"detect"}, ReplayPolicyCheckpoint),
			defaultStage("verify", "verify", []string{"normalize"}, ReplayPolicyCheckpoint),
			defaultStage(
				"adjudicate",
				"adjudicate",
				[]string{"verify"},
				ReplayPolicyCheckpoint,
			),
			defaultStage("report", "report", []string{"adjudicate"}, ReplayPolicyCheckpoint),
			defaultStage("publish", "publish", []string{"report"}, ReplayPolicyCheckpoint),
			defaultStage(
				"capture_feedback",
				"capture_feedback",
				[]string{"publish"},
				ReplayPolicyCheckpoint,
			),
			defaultStage(
				"export_evaluation",
				"export_evaluation",
				[]string{"capture_feedback"},
				ReplayPolicyCheckpoint,
			),
		},
	}
}

// FormalAgentReviewDefinition is the frozen one-stage workflow executed by
// the local Pi platform adapter. Target capture remains a separate committed
// source run; this definition owns only hypothesis generation and therefore
// cannot be mistaken for the deterministic finding/publication pipeline.
func FormalAgentReviewDefinition() Definition {
	return Definition{
		SchemaVersion: DefinitionSchemaVersion,
		ID:            "formal-pi-agent-review",
		Revision:      "1",
		Stages: []Stage{{
			ID:                     "agent_hypothesize",
			Kind:                   "agent_hypothesize",
			ImplementationRevision: "1",
			InputContract:          "argus.review_input.v1alpha1",
			OutputContract:         "argus.review_hypothesis_set.v1alpha1",
			DependsOn:              []string{},
			Executor:               "formal-local-pi",
			RequiredCapabilities:   []string{"brokered-model", "frozen-review-input"},
			AuthorityCeiling: &StageAuthorityCeiling{
				ModelEgress: "provider_broker_only",
				AllowedTools: []string{
					"list_files",
					"read_file",
					"search_code",
				},
				ToolNetwork: "deny", WorkspaceReads: "frozen_input_only",
				WorkspaceWrites: "deny", RemoteWrites: "deny",
				MaxDelegationDepth: 0, MaxModelCalls: 2_000, MaxToolCalls: 100,
			},
			Budget: StageBudget{
				TimeoutMS:      3_600_000,
				MaxInputBytes:  64 << 20,
				MaxOutputBytes: 1 << 20,
				MaxConcurrency: 16,
			},
			Retry: RetryPolicy{
				MaxAttempts: 2, BackoffMS: 0, Jitter: false,
				RetryableCodes: []string{"deadline_exceeded", "provider_error"},
				UnknownOutcome: UnknownOutcomeReconcile,
			},
			FailurePolicy: FailurePolicyFailRun,
			SideEffect:    SideEffectNone,
			ReplayPolicy:  ReplayPolicyExact,
		}},
	}
}

func defaultStage(
	id string,
	kind string,
	dependsOn []string,
	replayPolicy ReplayPolicy,
) Stage {
	inputContract := "argus.reviewcore_artifact.v1alpha1"
	if id == "materialize_target" {
		inputContract = "argus.review_input.v1alpha1"
	}
	return Stage{
		ID:                     id,
		Kind:                   kind,
		ImplementationRevision: "1",
		InputContract:          inputContract,
		OutputContract:         "argus.reviewcore_artifact.v1alpha1",
		DependsOn:              dependsOn,
		Executor:               "deterministic-local",
		RequiredCapabilities:   []string{},
		Budget: StageBudget{
			TimeoutMS:      30_000,
			MaxInputBytes:  16 << 20,
			MaxOutputBytes: 16 << 20,
			MaxConcurrency: 1,
		},
		Retry: RetryPolicy{
			MaxAttempts:    2,
			BackoffMS:      0,
			Jitter:         false,
			RetryableCodes: []string{"stage_timeout", "temporary"},
			UnknownOutcome: UnknownOutcomeFail,
		},
		FailurePolicy: FailurePolicyFailRun,
		SideEffect:    SideEffectNone,
		ReplayPolicy:  replayPolicy,
	}
}

func DigestDefinition(definition Definition) (string, error) {
	if err := definition.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(definition)
	if err != nil {
		return "", fmt.Errorf("marshal WorkflowDefinition: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (definition Definition) Validate() error {
	if definition.SchemaVersion != DefinitionSchemaVersion {
		return fmt.Errorf("unsupported WorkflowDefinition schema %q", definition.SchemaVersion)
	}
	if !trimmedNonEmpty(definition.ID) || !trimmedNonEmpty(definition.Revision) {
		return fmt.Errorf("workflow id and revision are required")
	}
	if len(definition.Stages) == 0 {
		return fmt.Errorf("workflow requires at least one stage")
	}
	stages := make(map[string]Stage, len(definition.Stages))
	for index, stage := range definition.Stages {
		if !trimmedNonEmpty(stage.ID) || !trimmedNonEmpty(stage.Kind) ||
			!trimmedNonEmpty(stage.ImplementationRevision) ||
			!trimmedNonEmpty(stage.InputContract) ||
			!trimmedNonEmpty(stage.OutputContract) ||
			!trimmedNonEmpty(stage.Executor) {
			return fmt.Errorf(
				"stage %d requires id, kind, implementation revision, contracts, and executor",
				index,
			)
		}
		if _, exists := stages[stage.ID]; exists {
			return fmt.Errorf("duplicate stage id %q", stage.ID)
		}
		if stage.DependsOn == nil {
			return fmt.Errorf("stage %q depends_on must be an explicit array", stage.ID)
		}
		if err := stage.validatePolicy(); err != nil {
			return fmt.Errorf("stage %q: %w", stage.ID, err)
		}
		switch stage.SideEffect {
		case SideEffectNone:
		case SideEffectRemotePublish:
			if stage.ReplayPolicy != ReplayPolicyForbidden {
				return fmt.Errorf("side-effecting stage %q cannot be replayable", stage.ID)
			}
		default:
			return fmt.Errorf("stage %q has unsupported side effect %q", stage.ID, stage.SideEffect)
		}
		stages[stage.ID] = stage
	}
	for _, stage := range definition.Stages {
		dependencies := make(map[string]struct{}, len(stage.DependsOn))
		for _, dependency := range stage.DependsOn {
			if !trimmedNonEmpty(dependency) {
				return fmt.Errorf("stage %q contains an invalid dependency id", stage.ID)
			}
			if dependency == stage.ID {
				return fmt.Errorf("stage %q cannot depend on itself", stage.ID)
			}
			if _, exists := stages[dependency]; !exists {
				return fmt.Errorf("stage %q depends on unknown stage %q", stage.ID, dependency)
			}
			if _, duplicate := dependencies[dependency]; duplicate {
				return fmt.Errorf("stage %q contains duplicate dependency %q", stage.ID, dependency)
			}
			dependencies[dependency] = struct{}{}
		}
	}
	if _, err := definition.TopologicalOrder(); err != nil {
		return err
	}
	return nil
}

func (stage Stage) validatePolicy() error {
	if stage.RequiredCapabilities == nil ||
		!sortedUniqueNonEmpty(stage.RequiredCapabilities) {
		return fmt.Errorf("required_capabilities must be an explicit sorted unique array")
	}
	if stage.Budget.TimeoutMS < 1 ||
		stage.Budget.MaxInputBytes < 1 ||
		stage.Budget.MaxOutputBytes < 1 ||
		stage.Budget.MaxConcurrency < 1 {
		return fmt.Errorf("budget values must be positive")
	}
	if stage.AuthorityCeiling != nil {
		ceiling := stage.AuthorityCeiling
		if ceiling.ModelEgress != "provider_broker_only" ||
			ceiling.ToolNetwork != "deny" ||
			ceiling.WorkspaceReads != "frozen_input_only" ||
			ceiling.WorkspaceWrites != "deny" ||
			ceiling.RemoteWrites != "deny" {
			return fmt.Errorf("authority_ceiling must use brokered model access and deny tool network/writes")
		}
		if ceiling.AllowedTools == nil ||
			!sortedUniqueNonEmpty(ceiling.AllowedTools) {
			return fmt.Errorf("authority_ceiling.allowed_tools must be an explicit sorted unique array")
		}
		if ceiling.MaxDelegationDepth < 0 || ceiling.MaxModelCalls < 1 ||
			ceiling.MaxToolCalls < 0 {
			return fmt.Errorf("authority_ceiling call/delegation limits are invalid")
		}
	}
	if stage.Retry.MaxAttempts < 1 || stage.Retry.BackoffMS < 0 {
		return fmt.Errorf("retry max_attempts must be positive and backoff_ms non-negative")
	}
	if stage.Retry.RetryableCodes == nil ||
		!sortedUniqueNonEmpty(stage.Retry.RetryableCodes) {
		return fmt.Errorf("retryable_codes must be an explicit sorted unique array")
	}
	switch stage.Retry.UnknownOutcome {
	case UnknownOutcomeFail, UnknownOutcomeReconcile:
	default:
		return fmt.Errorf(
			"unsupported retry unknown_outcome policy %q",
			stage.Retry.UnknownOutcome,
		)
	}
	switch stage.FailurePolicy {
	case FailurePolicyFailRun, FailurePolicyAllowPartial, FailurePolicyRecordPending:
	default:
		return fmt.Errorf("unsupported failure policy %q", stage.FailurePolicy)
	}
	switch stage.ReplayPolicy {
	case ReplayPolicyCheckpoint, ReplayPolicyExact, ReplayPolicyForbidden:
	default:
		return fmt.Errorf("unsupported replay policy %q", stage.ReplayPolicy)
	}
	return nil
}

// TopologicalOrder returns a deterministic order. Stages that become ready at
// the same time keep their declaration order.
func (definition Definition) TopologicalOrder() ([]string, error) {
	indexByID := make(map[string]int, len(definition.Stages))
	indegree := make(map[string]int, len(definition.Stages))
	dependants := make(map[string][]string, len(definition.Stages))
	for index, stage := range definition.Stages {
		if _, exists := indexByID[stage.ID]; exists {
			return nil, fmt.Errorf("duplicate stage id %q", stage.ID)
		}
		indexByID[stage.ID] = index
		indegree[stage.ID] = len(stage.DependsOn)
	}
	for _, stage := range definition.Stages {
		for _, dependency := range stage.DependsOn {
			if _, exists := indexByID[dependency]; !exists {
				return nil, fmt.Errorf("stage %q depends on unknown stage %q", stage.ID, dependency)
			}
			dependants[dependency] = append(dependants[dependency], stage.ID)
		}
	}
	order := make([]string, 0, len(definition.Stages))
	emitted := make(map[string]bool, len(definition.Stages))
	for len(order) < len(definition.Stages) {
		progress := false
		for _, stage := range definition.Stages {
			if emitted[stage.ID] || indegree[stage.ID] != 0 {
				continue
			}
			emitted[stage.ID] = true
			order = append(order, stage.ID)
			for _, dependant := range dependants[stage.ID] {
				indegree[dependant]--
			}
			progress = true
		}
		if !progress {
			return nil, fmt.Errorf("workflow contains a dependency cycle")
		}
	}
	return order, nil
}

func trimmedNonEmpty(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}

func sortedUniqueNonEmpty(values []string) bool {
	for index, value := range values {
		if !trimmedNonEmpty(value) {
			return false
		}
		if index > 0 && values[index-1] >= value {
			return false
		}
	}
	return true
}
