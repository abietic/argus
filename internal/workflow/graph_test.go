package workflow

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDefinitionTopologicalOrder(t *testing.T) {
	definition := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "default", Revision: "1",
		Stages: []Stage{
			testStage("materialize", "materialize", []string{}, "builtin"),
			testStage("detect-a", "detect", []string{"materialize"}, "agent"),
			testStage("detect-b", "detect", []string{"materialize"}, "tool"),
			testStage(
				"adjudicate",
				"adjudicate",
				[]string{"detect-a", "detect-b"},
				"builtin",
			),
		},
	}
	if err := definition.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	order, err := definition.TopologicalOrder()
	if err != nil {
		t.Fatalf("TopologicalOrder() error = %v", err)
	}
	want := []string{"materialize", "detect-a", "detect-b", "adjudicate"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("TopologicalOrder() = %v, want %v", order, want)
	}
}

func TestDefinitionRejectsCycle(t *testing.T) {
	definition := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "cycle", Revision: "1",
		Stages: []Stage{
			testStage("a", "detect", []string{"b"}, "agent"),
			testStage("b", "verify", []string{"a"}, "agent"),
		},
	}
	if err := definition.Validate(); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("Validate() error = %v, want cycle rejection", err)
	}
}

func TestDefinitionRejectsReplayableSideEffect(t *testing.T) {
	definition := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "publish", Revision: "1",
		Stages: []Stage{
			func() Stage {
				stage := testStage("publish", "publish", []string{}, "publisher")
				stage.SideEffect = SideEffectRemotePublish
				return stage
			}(),
		},
	}
	if err := definition.Validate(); err == nil || !strings.Contains(err.Error(), "cannot be replayable") {
		t.Fatalf("Validate() error = %v, want side effect rejection", err)
	}
}

func TestDefinitionRejectsIncompleteStageExecutionPolicy(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Stage)
		want   string
	}{
		{
			name: "missing implementation revision",
			mutate: func(stage *Stage) {
				stage.ImplementationRevision = ""
			},
			want: "implementation revision",
		},
		{
			name: "missing explicit capability array",
			mutate: func(stage *Stage) {
				stage.RequiredCapabilities = nil
			},
			want: "required_capabilities",
		},
		{
			name: "invalid timeout",
			mutate: func(stage *Stage) {
				stage.Budget.TimeoutMS = 0
			},
			want: "budget values",
		},
		{
			name: "unsorted retry codes",
			mutate: func(stage *Stage) {
				stage.Retry.RetryableCodes = []string{"temporary", "stage_timeout"}
			},
			want: "retryable_codes",
		},
		{
			name: "unknown outcome missing",
			mutate: func(stage *Stage) {
				stage.Retry.UnknownOutcome = ""
			},
			want: "unknown_outcome",
		},
		{
			name: "replay policy missing",
			mutate: func(stage *Stage) {
				stage.ReplayPolicy = ""
			},
			want: "replay policy",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stage := testStage("detect", "detect", []string{}, "agent")
			test.mutate(&stage)
			definition := Definition{
				SchemaVersion: DefinitionSchemaVersion,
				ID:            "workflow",
				Revision:      "1",
				Stages:        []Stage{stage},
			}
			if err := definition.Validate(); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDefinitionValidatesAgentAuthorityCeiling(t *testing.T) {
	stage := testStage("agent_hypothesize", "agent_hypothesize", []string{}, "acp-agent")
	stage.OutputContract = "argus.review_hypothesis_set.v1alpha1"
	stage.AuthorityCeiling = &StageAuthorityCeiling{
		ModelEgress: "provider_broker_only", AllowedTools: []string{"read_frozen_input"},
		ToolNetwork: "deny", WorkspaceReads: "frozen_input_only",
		WorkspaceWrites: "deny", RemoteWrites: "deny",
		MaxDelegationDepth: 0, MaxModelCalls: 8, MaxToolCalls: 16,
	}
	definition := Definition{
		SchemaVersion: DefinitionSchemaVersion,
		ID:            "agent-review", Revision: "1", Stages: []Stage{stage},
	}
	if err := definition.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	for name, mutate := range map[string]func(*StageAuthorityCeiling){
		"unbrokered model egress": func(value *StageAuthorityCeiling) {
			value.ModelEgress = "allow"
		},
		"tool network": func(value *StageAuthorityCeiling) {
			value.ToolNetwork = "allow"
		},
		"workspace write": func(value *StageAuthorityCeiling) {
			value.WorkspaceWrites = "allow"
		},
		"unsorted tools": func(value *StageAuthorityCeiling) {
			value.AllowedTools = []string{"search", "read"}
		},
		"missing model budget": func(value *StageAuthorityCeiling) {
			value.MaxModelCalls = 0
		},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := definition
			ceiling := *definition.Stages[0].AuthorityCeiling
			ceiling.AllowedTools = append([]string{}, ceiling.AllowedTools...)
			invalid.Stages = append([]Stage{}, definition.Stages...)
			invalid.Stages[0].AuthorityCeiling = &ceiling
			mutate(&ceiling)
			if err := invalid.Validate(); err == nil {
				t.Fatal("Validate() accepted authority above the v1alpha1 ceiling")
			}
		})
	}
}

func TestDefaultReviewDefinitionIsValidAndStable(t *testing.T) {
	definition := DefaultReviewDefinition()
	if err := definition.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	first, err := DigestDefinition(definition)
	if err != nil {
		t.Fatalf("DigestDefinition() error = %v", err)
	}
	second, err := DigestDefinition(DefaultReviewDefinition())
	if err != nil {
		t.Fatalf("DigestDefinition() error = %v", err)
	}
	if first != second {
		t.Fatalf("digest changed: %q != %q", first, second)
	}
	if got, want := len(definition.Stages), 10; got != want {
		t.Fatalf("stage count = %d, want %d", got, want)
	}
	wantStages := []string{
		"materialize_target",
		"plan_context",
		"detect",
		"normalize",
		"verify",
		"adjudicate",
		"report",
		"publish",
		"capture_feedback",
		"export_evaluation",
	}
	for index, stage := range definition.Stages {
		if stage.ID != wantStages[index] || stage.Kind != wantStages[index] {
			t.Fatalf("stage %d = %s/%s, want %s", index, stage.ID, stage.Kind, wantStages[index])
		}
		if index == 0 {
			if stage.InputContract != "argus.review_input.v1alpha1" ||
				len(stage.DependsOn) != 0 {
				t.Fatalf("first stage contract/dependencies = %+v", stage)
			}
		} else if stage.InputContract != "argus.reviewcore_artifact.v1alpha1" ||
			!reflect.DeepEqual(stage.DependsOn, []string{wantStages[index-1]}) {
			t.Fatalf("stage %s contract/dependencies = %+v", stage.ID, stage)
		}
	}
}

func TestFormalAgentReviewDefinitionIsValid(t *testing.T) {
	definition := FormalAgentReviewDefinition()
	if err := definition.Validate(); err != nil {
		t.Fatalf("FormalAgentReviewDefinition().Validate() error = %v", err)
	}
	if len(definition.Stages) != 1 ||
		definition.Stages[0].Kind != "agent_hypothesize" ||
		definition.Stages[0].SideEffect != SideEffectNone {
		t.Fatalf("formal workflow = %+v", definition)
	}
}

func TestDecodeDefinitionIsStrict(t *testing.T) {
	data, err := json.Marshal(DefaultReviewDefinition())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeDefinition(data)
	if err != nil {
		t.Fatalf("DecodeDefinition() error = %v", err)
	}
	if !reflect.DeepEqual(decoded, DefaultReviewDefinition()) {
		t.Fatalf("decoded definition = %+v", decoded)
	}
	for name, invalid := range map[string][]byte{
		"duplicate": bytesReplaceOnce(
			data,
			[]byte(`"schema_version":`),
			[]byte(`"schema_version":"argus.workflow.v1alpha1","schema_version":`),
		),
		"unknown":  append(data[:len(data)-1], []byte(`,"unknown":true}`)...),
		"trailing": append(data, []byte(`{}`)...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeDefinition(invalid); err == nil {
				t.Fatal("DecodeDefinition() accepted invalid JSON")
			}
		})
	}
}

func bytesReplaceOnce(data []byte, old []byte, replacement []byte) []byte {
	return []byte(strings.Replace(string(data), string(old), string(replacement), 1))
}

func testStage(id string, kind string, dependencies []string, executor string) Stage {
	stage := defaultStage(id, kind, dependencies, ReplayPolicyCheckpoint)
	stage.Executor = executor
	return stage
}
