package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/abietic/argus/internal/calibration"
	"github.com/abietic/argus/internal/calibrationpromotion"
	goastcontext "github.com/abietic/argus/internal/contextcapture/goast"
	compilecontext "github.com/abietic/argus/internal/contextcapture/gocompile"
	depscontext "github.com/abietic/argus/internal/contextcapture/godeps"
	searchcontext "github.com/abietic/argus/internal/contextcapture/reposearch"
	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/findingdecision"
	"github.com/abietic/argus/internal/normalizationpromotion"
	"github.com/abietic/argus/internal/pireviewmap"
	"github.com/abietic/argus/internal/platformapi"
	"github.com/abietic/argus/internal/promotionmonitor"
	"github.com/abietic/argus/internal/publication"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/training"
	"github.com/abietic/argus/internal/workflow"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

var markdownLink = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "docs-check:", err)
		os.Exit(1)
	}
	fmt.Println("docs-check: required docs, local links, schema, and examples are valid")
}

func run() error {
	required := []string{
		"README.md",
		"REQUIREMENTS.md",
		"PROJECT_DIRECTION.md",
		"TECHNICAL_DESIGN.md",
		"AGENTS.md",
		"docs/contracts/core-protocols.md",
		"docs/contracts/hailix-platform-execution-http-v1alpha1.md",
		"docs/architecture/context-map.md",
		"docs/integration/hailix-eino-agent.md",
		"docs/product/metrics-evaluation-value.md",
		"docs/roadmap/STATUS.md",
		"docs/roadmap/PLAN.md",
		"docs/roadmap/REMAINING.md",
	}
	for _, name := range required {
		if _, err := os.Stat(name); err != nil {
			return fmt.Errorf("required file %s: %w", name, err)
		}
	}
	if err := checkMarkdown(); err != nil {
		return err
	}
	examples := []string{
		"examples/review-spec.diff.json",
		"examples/review-spec.selection.json",
		"examples/review-spec.selection-multi-range.json",
		"examples/review-spec.selection-symbol.json",
		"examples/review-spec.scope.json",
	}
	schemaPath := "api/schema/v1alpha1/review-spec.schema.json"
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	evaluationCaseSchemaData, err := os.ReadFile("api/schema/v1alpha1/evaluation-case.schema.json")
	if err != nil {
		return fmt.Errorf("read EvaluationCase schema: %w", err)
	}
	evaluationCaseSchema, err := jsonschema.UnmarshalJSON(bytes.NewReader(evaluationCaseSchemaData))
	if err != nil {
		return fmt.Errorf("decode EvaluationCase schema: %w", err)
	}
	if err := compiler.AddResource(
		"https://argus.local/schema/v1alpha1/evaluation-case.schema.json", evaluationCaseSchema,
	); err != nil {
		return fmt.Errorf("register EvaluationCase schema: %w", err)
	}
	externalGovernedImportSchemaData, err := os.ReadFile("api/schema/v1alpha1/external-governed-case-import.schema.json")
	if err != nil {
		return fmt.Errorf("read ExternalGovernedCaseImport schema: %w", err)
	}
	externalGovernedImportSchema, err := jsonschema.UnmarshalJSON(bytes.NewReader(externalGovernedImportSchemaData))
	if err != nil {
		return fmt.Errorf("decode ExternalGovernedCaseImport schema: %w", err)
	}
	if err := compiler.AddResource(
		"https://argus.local/schema/v1alpha1/external-governed-case-import.schema.json",
		externalGovernedImportSchema,
	); err != nil {
		return fmt.Errorf("register ExternalGovernedCaseImport schema: %w", err)
	}
	for _, dependency := range []string{
		"normalization-oracle.schema.json",
		"normalization-oracle-attestation.schema.json",
	} {
		path := filepath.Join("api/schema/v1alpha1", dependency)
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read normalization governance dependency %s: %w", dependency, err)
		}
		resource, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("decode normalization governance dependency %s: %w", dependency, err)
		}
		if err := compiler.AddResource("https://argus.local/schema/v1alpha1/"+dependency, resource); err != nil {
			return fmt.Errorf("register normalization governance dependency %s: %w", dependency, err)
		}
	}
	for _, dependency := range []string{
		"evaluation-case-review-assignment.schema.json",
		"evaluation-case-annotation.schema.json",
		"evaluation-case-adjudication.schema.json",
		"evaluation-case-activation.schema.json",
		"evaluation-case-reopen.schema.json",
	} {
		path := filepath.Join("api/schema/v1alpha1", dependency)
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read governance batch dependency %s: %w", dependency, err)
		}
		resource, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("decode governance batch dependency %s: %w", dependency, err)
		}
		if err := compiler.AddResource(
			"https://argus.local/schema/v1alpha1/"+dependency, resource,
		); err != nil {
			return fmt.Errorf("register governance batch dependency %s: %w", dependency, err)
		}
	}
	for _, dependency := range []string{
		"governance-batch-request.schema.json",
		"governance-trust-key-registration.schema.json",
		"governance-trust-key-revocation.schema.json",
		"local-api-mutation.schema.json",
		"config-revision.schema.json",
		"config-bundle.schema.json",
		"workflow-definition.schema.json",
		"experiment-batch-request.schema.json",
		"repeatability-batch-request.schema.json",
		"calibration-fit-request.schema.json",
		"training-materialization-request.schema.json",
		"training-strict-redaction-policy.schema.json",
		"training-export-request.schema.json",
		"training-dataset-manifest.schema.json",
		"training-job-prepare-request.schema.json",
		"training-provider-job-receipt.schema.json",
		"training-job-observation-request.schema.json",
		"calibration-promotion-prepare-request.schema.json",
		"promotion-gate-result.schema.json",
		"calibration-promotion-gate-request.schema.json",
		"calibration-promotion-observation-request.schema.json",
		"normalization-promotion-policy.schema.json",
		"normalization-promotion-prepare-request.schema.json",
		"normalization-promotion-gate-request.schema.json",
		"normalization-promotion-gate-decision.schema.json",
		"normalization-promotion-operational-gate-request.schema.json",
	} {
		path := filepath.Join("api/schema/v1alpha1", dependency)
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read local API dependency %s: %w", dependency, err)
		}
		resource, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("decode local API dependency %s: %w", dependency, err)
		}
		if err := compiler.AddResource(
			"https://argus.local/schema/v1alpha1/"+dependency, resource,
		); err != nil {
			return fmt.Errorf("register local API dependency %s: %w", dependency, err)
		}
	}
	applyTrialSchemaData, err := os.ReadFile("api/schema/v1alpha1/apply-trial.schema.json")
	if err != nil {
		return fmt.Errorf("read ApplyTrial schema: %w", err)
	}
	applyTrialSchema, err := jsonschema.UnmarshalJSON(bytes.NewReader(applyTrialSchemaData))
	if err != nil {
		return fmt.Errorf("decode ApplyTrial schema: %w", err)
	}
	if err := compiler.AddResource(
		"https://argus.local/schema/v1alpha1/apply-trial.schema.json", applyTrialSchema,
	); err != nil {
		return fmt.Errorf("register ApplyTrial schema: %w", err)
	}
	shadowDefinitionsPath := "api/schema/v1alpha1/agent-review-shadow-defs.schema.json"
	shadowDefinitionsData, err := os.ReadFile(shadowDefinitionsPath)
	if err != nil {
		return fmt.Errorf("read agent review shadow schema definitions: %w", err)
	}
	shadowDefinitions, err := jsonschema.UnmarshalJSON(bytes.NewReader(shadowDefinitionsData))
	if err != nil {
		return fmt.Errorf("decode agent review shadow schema definitions: %w", err)
	}
	if err := compiler.AddResource(
		"https://argus.local/schema/v1alpha1/agent-review-shadow-defs.schema.json",
		shadowDefinitions,
	); err != nil {
		return fmt.Errorf("register agent review shadow schema definitions: %w", err)
	}
	hypothesisSchemaPath := "api/schema/v1alpha1/review-hypothesis-set.schema.json"
	hypothesisSchemaData, err := os.ReadFile(hypothesisSchemaPath)
	if err != nil {
		return fmt.Errorf("read ReviewHypothesisSet schema: %w", err)
	}
	hypothesisSchemaResource, err := jsonschema.UnmarshalJSON(bytes.NewReader(hypothesisSchemaData))
	if err != nil {
		return fmt.Errorf("decode ReviewHypothesisSet schema: %w", err)
	}
	if err := compiler.AddResource(
		"https://argus.local/schema/v1alpha1/review-hypothesis-set.schema.json",
		hypothesisSchemaResource,
	); err != nil {
		return fmt.Errorf("register ReviewHypothesisSet schema: %w", err)
	}
	governedReportSchemaPath := "api/schema/v1alpha1/governed-review-report.schema.json"
	governedReportSchemaData, err := os.ReadFile(governedReportSchemaPath)
	if err != nil {
		return fmt.Errorf("read GovernedReviewReport schema: %w", err)
	}
	governedReportSchemaResource, err := jsonschema.UnmarshalJSON(
		bytes.NewReader(governedReportSchemaData),
	)
	if err != nil {
		return fmt.Errorf("decode GovernedReviewReport schema: %w", err)
	}
	if err := compiler.AddResource(
		"https://argus.local/schema/v1alpha1/governed-review-report.schema.json",
		governedReportSchemaResource,
	); err != nil {
		return fmt.Errorf("register GovernedReviewReport schema: %w", err)
	}
	planSchemaPath := "api/schema/v1alpha1/agent-review-plan.schema.json"
	planSchemaData, err := os.ReadFile(planSchemaPath)
	if err != nil {
		return fmt.Errorf("read agent review plan schema: %w", err)
	}
	planSchemaResource, err := jsonschema.UnmarshalJSON(bytes.NewReader(planSchemaData))
	if err != nil {
		return fmt.Errorf("decode agent review plan schema: %w", err)
	}
	if err := compiler.AddResource(
		"https://argus.local/schema/v1alpha1/agent-review-plan.schema.json",
		planSchemaResource,
	); err != nil {
		return fmt.Errorf("register agent review plan schema: %w", err)
	}
	receiptSchemaPath := "api/schema/v1alpha1/agent-execution-receipt.schema.json"
	receiptSchemaData, err := os.ReadFile(receiptSchemaPath)
	if err != nil {
		return fmt.Errorf("read agent execution receipt schema: %w", err)
	}
	receiptSchemaResource, err := jsonschema.UnmarshalJSON(bytes.NewReader(receiptSchemaData))
	if err != nil {
		return fmt.Errorf("decode agent execution receipt schema: %w", err)
	}
	if err := compiler.AddResource(
		"https://argus.local/schema/v1alpha1/agent-execution-receipt.schema.json",
		receiptSchemaResource,
	); err != nil {
		return fmt.Errorf("register agent execution receipt schema: %w", err)
	}
	compiledSchema, err := compiler.Compile(schemaPath)
	if err != nil {
		return fmt.Errorf("compile ReviewSpec schema: %w", err)
	}
	for _, name := range examples {
		data, err := os.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := contractsv1alpha1.DecodeReviewSpec(data); err != nil {
			return fmt.Errorf("validate %s: %w", name, err)
		}
		if err := validateJSONSchema(compiledSchema, data); err != nil {
			return fmt.Errorf("schema validate %s: %w", name, err)
		}
	}
	invalidExamples := []string{
		"api/schema/v1alpha1/testdata/review-spec.invalid.file-uri.json",
		"api/schema/v1alpha1/testdata/review-spec.invalid.artifact-traversal.json",
		"api/schema/v1alpha1/testdata/review-spec.invalid.ambiguous-selection.json",
		"api/schema/v1alpha1/testdata/review-spec.invalid.windows-path.json",
		"api/schema/v1alpha1/testdata/review-spec.invalid.unknown-field.json",
	}
	for _, name := range invalidExamples {
		data, err := os.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := contractsv1alpha1.DecodeReviewSpec(data); err == nil {
			return fmt.Errorf("Go validator accepted negative fixture %s", name)
		}
		if err := validateJSONSchema(compiledSchema, data); err == nil {
			return fmt.Errorf("JSON Schema accepted negative fixture %s", name)
		}
	}
	schemaData, err := os.ReadFile(schemaPath)
	if err != nil {
		return fmt.Errorf("read ReviewSpec schema: %w", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(schemaData, &schema); err != nil {
		return fmt.Errorf("decode ReviewSpec schema: %w", err)
	}
	if schema["title"] != "Argus ReviewSpec v1alpha1" || schema["additionalProperties"] != false {
		return fmt.Errorf("ReviewSpec schema identity or strictness is invalid")
	}
	stageContracts := []struct {
		name       string
		schemaPath string
		example    string
		decode     func([]byte) error
	}{
		{
			name:       "ReviewInput ContextRef",
			schemaPath: "api/schema/v1alpha1/review-input.schema.json",
			example:    "examples/review-input.selection-context-ref.json",
			decode: func(data []byte) error {
				_, err := reviewcore.DecodeReviewInput(data)
				return err
			},
		},
		{
			name:       "ReviewInput ContextGap",
			schemaPath: "api/schema/v1alpha1/review-input.schema.json",
			example:    "examples/review-input.selection-context-gap.json",
			decode: func(data []byte) error {
				_, err := reviewcore.DecodeReviewInput(data)
				return err
			},
		},
		{
			name:       "StageExecutionRequest",
			schemaPath: "api/schema/v1alpha1/stage-execution-request.schema.json",
			example:    "examples/stage-execution.request.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeStageExecutionRequest(data)
				return err
			},
		},
		{
			name:       "StageExecutionResult",
			schemaPath: "api/schema/v1alpha1/stage-execution-result.schema.json",
			example:    "examples/stage-execution.result.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeStageExecutionResult(data)
				return err
			},
		},
		{
			name:       "AgentStagePlan",
			schemaPath: "api/schema/v1alpha1/agent-stage-plan.schema.json",
			example:    "examples/agent-stage-plan.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentStagePlan(data)
				return err
			},
		},
		{
			name:       "WorkflowDefinition",
			schemaPath: "api/schema/v1alpha1/workflow-definition.schema.json",
			example:    "examples/workflow-definition.json",
			decode: func(data []byte) error {
				_, err := workflow.DecodeDefinition(data)
				return err
			},
		},
	}
	for _, contract := range stageContracts {
		schema, err := compiler.Compile(contract.schemaPath)
		if err != nil {
			return fmt.Errorf("compile %s schema: %w", contract.name, err)
		}
		data, err := os.ReadFile(contract.example)
		if err != nil {
			return fmt.Errorf("read %s: %w", contract.example, err)
		}
		if err := contract.decode(data); err != nil {
			return fmt.Errorf("validate %s: %w", contract.example, err)
		}
		if err := validateJSONSchema(schema, data); err != nil {
			return fmt.Errorf("schema validate %s: %w", contract.example, err)
		}
	}
	for _, dependency := range []string{
		"agent-stage-plan.schema.json",
		"stage-execution-request.schema.json",
		"stage-execution-result.schema.json",
	} {
		path := filepath.Join("api/schema/v1alpha1", dependency)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read Hailix HTTP schema dependency %s: %w", dependency, readErr)
		}
		resource, decodeErr := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if decodeErr != nil {
			return fmt.Errorf("decode Hailix HTTP schema dependency %s: %w", dependency, decodeErr)
		}
		if addErr := compiler.AddResource(
			"https://argus.local/schema/v1alpha1/"+dependency,
			resource,
		); addErr != nil {
			return fmt.Errorf("register Hailix HTTP schema dependency %s: %w", dependency, addErr)
		}
	}
	hailixHTTPContract, err := compiler.Compile(
		"api/schema/v1alpha1/hailix-platform-execution-http.schema.json",
	)
	if err != nil {
		return fmt.Errorf("compile Hailix platform-execution HTTP schema: %w", err)
	}
	for _, example := range []string{
		"examples/hailix-platform-execution-http.ensure-request.json",
		"examples/hailix-platform-execution-http.terminal-response.json",
	} {
		data, readErr := os.ReadFile(example)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", example, readErr)
		}
		if validationErr := validateJSONSchema(hailixHTTPContract, data); validationErr != nil {
			return fmt.Errorf("schema validate %s: %w", example, validationErr)
		}
	}
	if err := checkStageExecutionNegativeMutations(compiler); err != nil {
		return err
	}
	if err := checkAgentStagePlanNegativeMutations(compiler); err != nil {
		return err
	}
	reviewInputSchema, err := compiler.Compile(
		"api/schema/v1alpha1/review-input.schema.json",
	)
	if err != nil {
		return fmt.Errorf("compile ReviewInput schema: %w", err)
	}
	invalidReviewInputs := []string{
		"api/schema/v1alpha1/testdata/review-input.invalid.ambiguous-context.json",
	}
	for _, name := range invalidReviewInputs {
		data, err := os.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := reviewcore.DecodeReviewInput(data); err == nil {
			return fmt.Errorf("Go validator accepted negative fixture %s", name)
		}
		if err := validateJSONSchema(reviewInputSchema, data); err == nil {
			return fmt.Errorf("JSON Schema accepted negative fixture %s", name)
		}
	}
	if err := checkConfigContracts(compiler); err != nil {
		return err
	}
	if err := checkAgentReviewShadowContracts(compiler); err != nil {
		return err
	}
	if err := checkAgentReviewPlanWorkerNegativeMutations(compiler); err != nil {
		return err
	}
	return nil
}

func checkStageExecutionNegativeMutations(compiler *jsonschema.Compiler) error {
	requestSchema, err := compiler.Compile(
		"api/schema/v1alpha1/stage-execution-request.schema.json",
	)
	if err != nil {
		return fmt.Errorf("compile StageExecutionRequest schema for negative checks: %w", err)
	}
	requestData, err := os.ReadFile("examples/stage-execution.request.json")
	if err != nil {
		return fmt.Errorf("read StageExecutionRequest negative-check source: %w", err)
	}
	var request map[string]any
	if err := json.Unmarshal(requestData, &request); err != nil {
		return fmt.Errorf("decode StageExecutionRequest negative-check source: %w", err)
	}
	requestTests := []struct {
		name   string
		mutate func(map[string]any) error
	}{
		{
			name: "unsafe attempt",
			mutate: func(candidate map[string]any) error {
				candidate["attempt"] = float64(9007199254740992)
				return nil
			},
		},
		{
			name: "unsafe generation",
			mutate: func(candidate map[string]any) error {
				candidate["generation"] = float64(9007199254740992)
				return nil
			},
		},
		{
			name: "unsafe fencing token",
			mutate: func(candidate map[string]any) error {
				candidate["fencing_token"] = float64(9007199254740992)
				return nil
			},
		},
		{
			name: "unsafe artifact size",
			mutate: func(candidate map[string]any) error {
				binding, ok := candidate["plan"].(map[string]any)
				if !ok {
					return fmt.Errorf("plan is not an object")
				}
				ref, ok := binding["ref"].(map[string]any)
				if !ok {
					return fmt.Errorf("plan.ref is not an object")
				}
				ref["size_bytes"] = float64(9007199254740992)
				return nil
			},
		},
		{
			name: "missing workload authority",
			mutate: func(candidate map[string]any) error {
				delete(candidate, "workload_id")
				return nil
			},
		},
		{
			name: "missing lease authority",
			mutate: func(candidate map[string]any) error {
				delete(candidate, "lease_id")
				return nil
			},
		},
		{
			name: "missing lease worker authority",
			mutate: func(candidate map[string]any) error {
				delete(candidate, "lease_worker_id")
				return nil
			},
		},
		{
			name: "wrong execution snapshot contract",
			mutate: func(candidate map[string]any) error {
				binding, ok := candidate["execution_snapshot"].(map[string]any)
				if !ok {
					return fmt.Errorf("execution_snapshot is not an object")
				}
				binding["contract"] = "argus.other_snapshot.v1alpha1"
				return nil
			},
		},
		{
			name: "wrong review input contract",
			mutate: func(candidate map[string]any) error {
				binding, ok := candidate["review_input"].(map[string]any)
				if !ok {
					return fmt.Errorf("review_input is not an object")
				}
				binding["contract"] = "argus.other_input.v1alpha1"
				return nil
			},
		},
		{
			name: "missing runtime revision",
			mutate: func(candidate map[string]any) error {
				capability, ok := candidate["capability"].(map[string]any)
				if !ok {
					return fmt.Errorf("capability is not an object")
				}
				delete(capability, "runtime_revision")
				return nil
			},
		},
	}
	for _, test := range requestTests {
		candidateData, err := json.Marshal(request)
		if err != nil {
			return err
		}
		var candidate map[string]any
		if err := json.Unmarshal(candidateData, &candidate); err != nil {
			return err
		}
		if err := test.mutate(candidate); err != nil {
			return fmt.Errorf("mutate StageExecutionRequest %s: %w", test.name, err)
		}
		candidateData, err = json.Marshal(candidate)
		if err != nil {
			return err
		}
		if _, err := contractsv1alpha1.DecodeStageExecutionRequest(candidateData); err == nil {
			return fmt.Errorf("Go validator accepted StageExecutionRequest %s", test.name)
		}
		if err := validateJSONSchema(requestSchema, candidateData); err == nil {
			return fmt.Errorf("JSON Schema accepted StageExecutionRequest %s", test.name)
		}
	}

	resultSchema, err := compiler.Compile(
		"api/schema/v1alpha1/stage-execution-result.schema.json",
	)
	if err != nil {
		return fmt.Errorf("compile StageExecutionResult schema for negative checks: %w", err)
	}
	resultData, err := os.ReadFile("examples/stage-execution.result.json")
	if err != nil {
		return fmt.Errorf("read StageExecutionResult negative-check source: %w", err)
	}
	var result map[string]any
	if err := json.Unmarshal(resultData, &result); err != nil {
		return fmt.Errorf("decode StageExecutionResult negative-check source: %w", err)
	}
	resultTests := []struct {
		name   string
		mutate func(map[string]any) error
	}{
		{
			name: "unsafe attempt",
			mutate: func(candidate map[string]any) error {
				candidate["attempt"] = float64(9007199254740992)
				return nil
			},
		},
		{
			name: "unsafe generation",
			mutate: func(candidate map[string]any) error {
				candidate["generation"] = float64(9007199254740992)
				return nil
			},
		},
		{
			name: "unsafe fencing token",
			mutate: func(candidate map[string]any) error {
				candidate["fencing_token"] = float64(9007199254740992)
				return nil
			},
		},
		{
			name: "unsafe artifact size",
			mutate: func(candidate map[string]any) error {
				binding, ok := candidate["output"].(map[string]any)
				if !ok {
					return fmt.Errorf("output is not an object")
				}
				ref, ok := binding["ref"].(map[string]any)
				if !ok {
					return fmt.Errorf("output.ref is not an object")
				}
				ref["size_bytes"] = float64(9007199254740992)
				return nil
			},
		},
		{
			name: "empty output artifact",
			mutate: func(candidate map[string]any) error {
				binding, ok := candidate["output"].(map[string]any)
				if !ok {
					return fmt.Errorf("output is not an object")
				}
				ref, ok := binding["ref"].(map[string]any)
				if !ok {
					return fmt.Errorf("output.ref is not an object")
				}
				ref["size_bytes"] = float64(0)
				return nil
			},
		},
		{
			name: "wrong trace contract",
			mutate: func(candidate map[string]any) error {
				trace, ok := candidate["trace_manifest"].(map[string]any)
				if !ok {
					return fmt.Errorf("trace_manifest is not an object")
				}
				trace["contract"] = "hailix.other_trace.v1alpha1"
				return nil
			},
		},
		{
			name: "empty completeness note",
			mutate: func(candidate map[string]any) error {
				candidate["completeness"] = "partial"
				candidate["completeness_notes"] = []any{""}
				return nil
			},
		},
		{
			name: "too many completeness notes",
			mutate: func(candidate map[string]any) error {
				candidate["completeness"] = "partial"
				notes := make([]any, 33)
				for index := range notes {
					notes[index] = "bounded-note"
				}
				candidate["completeness_notes"] = notes
				return nil
			},
		},
	}
	for _, test := range resultTests {
		candidateData, err := json.Marshal(result)
		if err != nil {
			return err
		}
		var candidate map[string]any
		if err := json.Unmarshal(candidateData, &candidate); err != nil {
			return err
		}
		if err := test.mutate(candidate); err != nil {
			return fmt.Errorf("mutate StageExecutionResult %s: %w", test.name, err)
		}
		candidateData, err = json.Marshal(candidate)
		if err != nil {
			return err
		}
		if _, err := contractsv1alpha1.DecodeStageExecutionResult(candidateData); err == nil {
			return fmt.Errorf("Go validator accepted StageExecutionResult %s", test.name)
		}
		if err := validateJSONSchema(resultSchema, candidateData); err == nil {
			return fmt.Errorf("JSON Schema accepted StageExecutionResult %s", test.name)
		}
	}
	return nil
}

func checkAgentStagePlanNegativeMutations(compiler *jsonschema.Compiler) error {
	schema, err := compiler.Compile("api/schema/v1alpha1/agent-stage-plan.schema.json")
	if err != nil {
		return fmt.Errorf("compile AgentStagePlan schema for negative checks: %w", err)
	}
	data, err := os.ReadFile("examples/agent-stage-plan.json")
	if err != nil {
		return fmt.Errorf("read AgentStagePlan example for negative checks: %w", err)
	}
	var canonical map[string]any
	if err := json.Unmarshal(data, &canonical); err != nil {
		return fmt.Errorf("decode AgentStagePlan example for negative checks: %w", err)
	}
	tests := []struct {
		name   string
		mutate func(map[string]any) error
	}{
		{
			name: "empty skills",
			mutate: func(candidate map[string]any) error {
				candidate["skills"] = []any{}
				return nil
			},
		},
		{
			name: "missing review phase skill",
			mutate: func(candidate map[string]any) error {
				skills, ok := candidate["skills"].([]any)
				if !ok || len(skills) == 0 {
					return fmt.Errorf("canonical AgentStagePlan skills are invalid")
				}
				for index, raw := range skills {
					skill, ok := raw.(map[string]any)
					if !ok {
						return fmt.Errorf("canonical AgentStagePlan skill %d is invalid", index)
					}
					skill["phase"] = "context"
				}
				return nil
			},
		},
		{
			name: "unsafe timeout budget",
			mutate: func(candidate map[string]any) error {
				budget, ok := candidate["budget"].(map[string]any)
				if !ok {
					return fmt.Errorf("canonical AgentStagePlan budget is invalid")
				}
				budget["timeout_ms"] = float64(9007199254740992)
				return nil
			},
		},
		{
			name: "unsafe artifact size",
			mutate: func(candidate map[string]any) error {
				binding, ok := candidate["execution_snapshot"].(map[string]any)
				if !ok {
					return fmt.Errorf("canonical AgentStagePlan execution_snapshot is invalid")
				}
				ref, ok := binding["ref"].(map[string]any)
				if !ok {
					return fmt.Errorf("canonical AgentStagePlan execution_snapshot.ref is invalid")
				}
				ref["size_bytes"] = float64(9007199254740992)
				return nil
			},
		},
	}
	for _, test := range tests {
		candidateData, err := json.Marshal(canonical)
		if err != nil {
			return err
		}
		var candidate map[string]any
		if err := json.Unmarshal(candidateData, &candidate); err != nil {
			return err
		}
		if err := test.mutate(candidate); err != nil {
			return fmt.Errorf("mutate AgentStagePlan %s: %w", test.name, err)
		}
		candidateData, err = json.Marshal(candidate)
		if err != nil {
			return err
		}
		if _, err := contractsv1alpha1.DecodeAgentStagePlan(candidateData); err == nil {
			return fmt.Errorf("Go validator accepted AgentStagePlan %s", test.name)
		}
		if err := validateJSONSchema(schema, candidateData); err == nil {
			return fmt.Errorf("JSON Schema accepted AgentStagePlan %s", test.name)
		}
	}
	return nil
}

func checkAgentReviewPlanWorkerNegativeMutations(compiler *jsonschema.Compiler) error {
	schema, err := compiler.Compile("api/schema/v1alpha1/agent-review-plan.schema.json")
	if err != nil {
		return fmt.Errorf("compile AgentReviewPlan schema for worker checks: %w", err)
	}
	data, err := os.ReadFile("examples/agent-review-plan.json")
	if err != nil {
		return fmt.Errorf("read AgentReviewPlan worker-check source: %w", err)
	}
	var canonical map[string]any
	if err := json.Unmarshal(data, &canonical); err != nil {
		return fmt.Errorf("decode AgentReviewPlan worker-check source: %w", err)
	}
	tests := []struct {
		name   string
		mutate func(map[string]any) error
	}{
		{
			name: "unsupported timeout",
			mutate: func(candidate map[string]any) error {
				budget, ok := candidate["budget"].(map[string]any)
				if !ok {
					return fmt.Errorf("AgentReviewPlan budget is not an object")
				}
				budget["timeout_ms"] = float64(3_600_001)
				return nil
			},
		},
		{
			name: "unsupported artifact size",
			mutate: func(candidate map[string]any) error {
				binding, ok := candidate["execution_snapshot_ref"].(map[string]any)
				if !ok {
					return fmt.Errorf("execution_snapshot_ref is not an object")
				}
				ref, ok := binding["ref"].(map[string]any)
				if !ok {
					return fmt.Errorf("execution_snapshot_ref.ref is not an object")
				}
				ref["size_bytes"] = float64(67_108_865)
				return nil
			},
		},
		{
			name: "multiple context dimensions",
			mutate: func(candidate map[string]any) error {
				dimensions, ok := candidate["context_dimensions"].([]any)
				if !ok {
					return fmt.Errorf("AgentReviewPlan context_dimensions is not an array")
				}
				candidate["context_dimensions"] = append(dimensions, map[string]any{
					"id":       "type-context",
					"revision": "1",
					"sha256":   strings.Repeat("b", 64),
				})
				return nil
			},
		},
		{
			name: "too many review dimensions",
			mutate: func(candidate map[string]any) error {
				dimensions := make([]any, contractsv1alpha1.AgentReviewWorkerMaxSkillCount+1)
				for index := range dimensions {
					dimensions[index] = map[string]any{
						"id":       fmt.Sprintf("review-%02d", index),
						"revision": "1",
						"sha256":   strings.Repeat("b", 64),
					}
				}
				candidate["review_dimensions"] = dimensions
				return nil
			},
		},
		{
			name: "too many knowledge refs",
			mutate: func(candidate map[string]any) error {
				knowledge := make([]any, contractsv1alpha1.AgentReviewWorkerMaxKnowledgeCount+1)
				for index := range knowledge {
					knowledge[index] = map[string]any{
						"id":       fmt.Sprintf("knowledge-%d", index),
						"revision": "1",
						"sha256":   strings.Repeat("b", 64),
					}
				}
				candidate["knowledge"] = knowledge
				return nil
			},
		},
		{
			name: "missing repository tool",
			mutate: func(candidate map[string]any) error {
				policy, ok := candidate["tool_policy"].(map[string]any)
				if !ok {
					return fmt.Errorf("AgentReviewPlan tool_policy is not an object")
				}
				policy["allowed_tools"] = []any{"list_files", "read_file"}
				return nil
			},
		},
		{
			name: "extra repository tool",
			mutate: func(candidate map[string]any) error {
				policy, ok := candidate["tool_policy"].(map[string]any)
				if !ok {
					return fmt.Errorf("AgentReviewPlan tool_policy is not an object")
				}
				policy["allowed_tools"] = []any{
					"list_files", "query_codegraph", "read_file", "search_code",
				}
				return nil
			},
		},
		{
			name: "reordered repository tools",
			mutate: func(candidate map[string]any) error {
				policy, ok := candidate["tool_policy"].(map[string]any)
				if !ok {
					return fmt.Errorf("AgentReviewPlan tool_policy is not an object")
				}
				policy["allowed_tools"] = []any{"read_file", "list_files", "search_code"}
				return nil
			},
		},
	}
	for _, test := range tests {
		candidateData, err := json.Marshal(canonical)
		if err != nil {
			return err
		}
		var candidate map[string]any
		if err := json.Unmarshal(candidateData, &candidate); err != nil {
			return err
		}
		if err := test.mutate(candidate); err != nil {
			return fmt.Errorf("mutate AgentReviewPlan %s: %w", test.name, err)
		}
		candidateData, err = json.Marshal(candidate)
		if err != nil {
			return err
		}
		if _, err := contractsv1alpha1.DecodeAgentReviewPlan(candidateData); err == nil {
			return fmt.Errorf("Go validator accepted AgentReviewPlan %s", test.name)
		}
		if err := validateJSONSchema(schema, candidateData); err == nil {
			return fmt.Errorf("JSON Schema accepted AgentReviewPlan %s", test.name)
		}
	}
	return nil
}

func checkAgentReviewShadowContracts(compiler *jsonschema.Compiler) error {
	contracts := []struct {
		name         string
		title        string
		schemaPath   string
		example      string
		decode       func([]byte) error
		closedField  string
		invalidValue string
	}{
		{
			name: "EvaluationCandidateDerivationRequest", title: "Argus EvaluationCandidateDerivationRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/evaluation-candidate-derivation-request.schema.json",
			example:    "examples/evaluation-candidate-derivation-request.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeCandidateDerivationRequest(data)
				return err
			},
			closedField: "source", invalidValue: "direct_gold",
		},
		{
			name: "ArtifactIntegrityChange", title: "Argus ArtifactIntegrityChange v1alpha1",
			schemaPath: "api/schema/v1alpha1/artifact-integrity-change.schema.json",
			example:    "examples/artifact-integrity-change.json",
			decode: func(data []byte) error {
				_, err := runrepo.DecodeArtifactIntegrityChangeJSON(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.artifact_integrity_change.v2",
		},
		{
			name: "PublicationGrantRequest", title: "Argus PublicationGrantRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/publication-grant-request.schema.json",
			example:    "examples/publication-grant-request.json",
			decode: func(data []byte) error {
				_, err := publication.DecodeGrantRequestJSON(data)
				return err
			},
			closedField: "channel", invalidValue: "issue_comment",
		},
		{
			name: "PublicationGrantMutation", title: "Argus PublicationGrantMutation v1alpha1",
			schemaPath: "api/schema/v1alpha1/publication-grant-mutation.schema.json",
			example:    "examples/publication-grant-mutation.json",
			decode: func(data []byte) error {
				_, err := publication.DecodeGrantMutationJSON(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.publication_grant_mutation.v2",
		},
		{
			name: "PublicationGrant", title: "Argus PublicationGrant v1alpha1",
			schemaPath: "api/schema/v1alpha1/publication-grant.schema.json",
			example:    "examples/publication-grant.json",
			decode: func(data []byte) error {
				_, err := publication.DecodeGrantJSON(data)
				return err
			},
			closedField: "channel", invalidValue: "issue_comment",
		},
		{
			name: "PublicationGrantReservation", title: "Argus PublicationGrantReservation v1alpha1",
			schemaPath: "api/schema/v1alpha1/publication-grant-reservation.schema.json",
			example:    "examples/publication-grant-reservation.json",
			decode: func(data []byte) error {
				_, err := publication.DecodeGrantReservationJSON(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.publication_grant_reservation.v2",
		},
		{
			name: "PublicationIntent", title: "Argus PublicationIntent v1alpha1",
			schemaPath: "api/schema/v1alpha1/publication-intent.schema.json",
			example:    "examples/publication-intent.json",
			decode: func(data []byte) error {
				_, err := publication.DecodeIntentJSON(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.publication_intent.v2",
		},
		{
			name: "PublicationRequest", title: "Argus PublicationRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/publication-request.schema.json",
			example:    "examples/publication-request.json",
			decode: func(data []byte) error {
				_, err := publication.DecodeRequestJSON(data)
				return err
			},
			closedField: "remote_writes", invalidValue: "deny",
		},
		{
			name: "FindingDecisionRequest", title: "Argus FindingDecisionRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/finding-decision-request.schema.json",
			example:    "examples/finding-decision-request.json",
			decode: func(data []byte) error {
				_, err := findingdecision.DecodeRequestJSON(data)
				return err
			},
			closedField: "action", invalidValue: "auto_publish",
		},
		{
			name: "FindingDecisionMutation", title: "Argus FindingDecisionMutation v1alpha1",
			schemaPath: "api/schema/v1alpha1/finding-decision-mutation.schema.json",
			example:    "examples/finding-decision-mutation.json",
			decode: func(data []byte) error {
				_, err := findingdecision.DecodeMutationJSON(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.finding_decision_mutation.v2",
		},
		{
			name: "FindingDecision", title: "Argus FindingDecision v1alpha1",
			schemaPath: "api/schema/v1alpha1/finding-decision.schema.json",
			example:    "examples/finding-decision.json",
			decode: func(data []byte) error {
				_, err := findingdecision.DecodeDecisionJSON(data)
				return err
			},
			closedField: "action", invalidValue: "auto_publish",
		},
		{
			name: "WorkloadPressureSnapshot", title: "Argus WorkloadPressureSnapshot v1alpha1",
			schemaPath: "api/schema/v1alpha1/workload-pressure-snapshot.schema.json",
			example:    "examples/workload-pressure-snapshot.json",
			decode: func(data []byte) error {
				_, err := scheduling.DecodePressureSnapshot(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.workload_pressure_snapshot.v2",
		},
		{
			name: "RepositorySearchContext", title: "Argus RepositorySearchContext v1alpha1",
			schemaPath: "api/schema/v1alpha1/repository-search-context.schema.json",
			example:    "examples/repository-search-context.json",
			decode: func(data []byte) error {
				_, err := searchcontext.DecodeArtifact(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.context.repository_search.v2",
		},
		{
			name: "GoASTContext", title: "Argus GoASTContext v1alpha1",
			schemaPath: "api/schema/v1alpha1/go-ast-context.schema.json",
			example:    "examples/go-ast-context.json",
			decode: func(data []byte) error {
				_, err := goastcontext.DecodeArtifact(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.context.go_ast.v2",
		},
		{
			name: "GoDependenciesContext", title: "Argus GoDependenciesContext v1alpha1",
			schemaPath: "api/schema/v1alpha1/go-dependencies-context.schema.json",
			example:    "examples/go-dependencies-context.json",
			decode: func(data []byte) error {
				_, err := depscontext.DecodeArtifact(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.context.go_dependencies.v2",
		},
		{
			name: "GoCompileContext", title: "Go Compile Context v1alpha1",
			schemaPath: "api/schema/v1alpha1/go-compile-context.schema.json",
			example:    "examples/go-compile-context.json",
			decode: func(data []byte) error {
				_, err := compilecontext.DecodeArtifact(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.context.go_compile.v2",
		},
		{
			name: "ApplyTrial", title: "Argus ApplyTrial v1alpha1",
			schemaPath: "api/schema/v1alpha1/apply-trial.schema.json",
			example:    "examples/apply-trial.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeApplyTrial(data)
				return err
			},
			closedField: "authority", invalidValue: "platform_attested",
		},
		{
			name: "EvaluationCase", title: "Argus EvaluationCase v1alpha1",
			schemaPath: "api/schema/v1alpha1/evaluation-case.schema.json",
			example:    "examples/evaluation-case.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeEvaluationCase(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.evaluation_case.v2",
		},
		{
			name: "CalibrationFitRequest", title: "Argus CalibrationFitRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/calibration-fit-request.schema.json",
			example:    "examples/calibration-fit-request.json",
			decode: func(data []byte) error {
				_, err := calibration.DecodeFitRequest(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.calibration_fit_request.v2",
		},
		{
			name: "LocalAPICalibrationFitCommand", title: "Argus LocalAPICalibrationFitCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-calibration-fit-command.schema.json",
			example:    "examples/local-api-calibration-fit-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeCalibrationFitCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_calibration_fit_command.v2",
		},
		{
			name: "TrainingMaterializationRequest", title: "Argus TrainingMaterializationRequest v1alpha1",
			schemaPath:  "api/schema/v1alpha1/training-materialization-request.schema.json",
			example:     "examples/training-materialization-request.json",
			decode:      func(data []byte) error { _, err := training.DecodeMaterializationRequest(data); return err },
			closedField: "schema_version", invalidValue: "argus.training_materialization_request.v2",
		},
		{
			name: "TrainingDatasetManifest", title: "Argus TrainingDatasetManifest v1alpha1",
			schemaPath:  "api/schema/v1alpha1/training-dataset-manifest.schema.json",
			example:     "examples/training-dataset-manifest.json",
			decode:      func(data []byte) error { _, err := training.DecodeDatasetManifest(data); return err },
			closedField: "content_mode", invalidValue: "source_bytes",
		},
		{
			name: "LocalAPITrainingMaterializeCommand", title: "Argus LocalAPITrainingMaterializeCommand v1alpha1",
			schemaPath:  "api/schema/v1alpha1/local-api-training-materialize-command.schema.json",
			example:     "examples/local-api-training-materialize-command.json",
			decode:      func(data []byte) error { _, err := platformapi.DecodeTrainingMaterializeCommand(data); return err },
			closedField: "schema_version", invalidValue: "argus.local_api_training_materialize_command.v2",
		},
		{
			name: "TrainingStrictRedactionPolicy", title: "Argus TrainingStrictRedactionPolicy v1alpha1",
			schemaPath:  "api/schema/v1alpha1/training-strict-redaction-policy.schema.json",
			example:     "examples/training-strict-redaction-policy.json",
			decode:      func(data []byte) error { _, err := training.DecodeStrictRedactionPolicy(data); return err },
			closedField: "revision", invalidValue: "2",
		},
		{
			name: "TrainingExportRequest", title: "Argus TrainingExportRequest v1alpha1",
			schemaPath:  "api/schema/v1alpha1/training-export-request.schema.json",
			example:     "examples/training-export-request.json",
			decode:      func(data []byte) error { _, err := training.DecodeExportRequest(data); return err },
			closedField: "schema_version", invalidValue: "argus.training_export_request.v2",
		},
		{
			name: "TrainingExportBundle", title: "Argus TrainingExportBundle v1alpha1",
			schemaPath:  "api/schema/v1alpha1/training-export-bundle.schema.json",
			example:     "examples/training-export-bundle.json",
			decode:      func(data []byte) error { _, err := training.DecodeExportBundle(data); return err },
			closedField: "portable_format", invalidValue: "raw_source_jsonl",
		},
		{
			name: "LocalAPITrainingExportBuildCommand", title: "Argus LocalAPITrainingExportBuildCommand v1alpha1",
			schemaPath:  "api/schema/v1alpha1/local-api-training-export-build-command.schema.json",
			example:     "examples/local-api-training-export-build-command.json",
			decode:      func(data []byte) error { _, err := platformapi.DecodeTrainingExportBuildCommand(data); return err },
			closedField: "schema_version", invalidValue: "argus.local_api_training_export_build_command.v2",
		},
		{
			name: "TrainingJobPrepareRequest", title: "Argus TrainingJobPrepareRequest v1alpha1",
			schemaPath:  "api/schema/v1alpha1/training-job-prepare-request.schema.json",
			example:     "examples/training-job-prepare-request.json",
			decode:      func(data []byte) error { _, err := training.DecodeJobPrepareRequest(data); return err },
			closedField: "objective", invalidValue: "arbitrary_training",
		},
		{
			name: "TrainingProviderJobReceipt", title: "Argus TrainingProviderJobReceipt v1alpha1",
			schemaPath:  "api/schema/v1alpha1/training-provider-job-receipt.schema.json",
			example:     "examples/training-provider-job-receipt.json",
			decode:      func(data []byte) error { _, err := training.DecodeProviderJobReceipt(data); return err },
			closedField: "authority", invalidValue: "provider_attested",
		},
		{
			name: "TrainingJobObservationRequest", title: "Argus TrainingJobObservationRequest v1alpha1",
			schemaPath:  "api/schema/v1alpha1/training-job-observation-request.schema.json",
			example:     "examples/training-job-observation-request.json",
			decode:      func(data []byte) error { _, err := training.DecodeJobObservationRequest(data); return err },
			closedField: "schema_version", invalidValue: "argus.training_job_observation_request.v2",
		},
		{
			name: "TrainingJobPlan", title: "Argus TrainingJobPlan v1alpha1",
			schemaPath:  "api/schema/v1alpha1/training-job-plan.schema.json",
			example:     "examples/training-job-plan.json",
			decode:      func(data []byte) error { _, err := training.DecodeJobPlan(data); return err },
			closedField: "receipt_authority", invalidValue: "provider_attested",
		},
		{
			name: "LocalAPITrainingJobPrepareCommand", title: "Argus LocalAPITrainingJobPrepareCommand v1alpha1",
			schemaPath:  "api/schema/v1alpha1/local-api-training-job-prepare-command.schema.json",
			example:     "examples/local-api-training-job-prepare-command.json",
			decode:      func(data []byte) error { _, err := platformapi.DecodeTrainingJobPrepareCommand(data); return err },
			closedField: "schema_version", invalidValue: "argus.local_api_training_job_prepare_command.v2",
		},
		{
			name: "LocalAPITrainingJobObserveCommand", title: "Argus LocalAPITrainingJobObserveCommand v1alpha1",
			schemaPath:  "api/schema/v1alpha1/local-api-training-job-observe-command.schema.json",
			example:     "examples/local-api-training-job-observe-command.json",
			decode:      func(data []byte) error { _, err := platformapi.DecodeTrainingJobObserveCommand(data); return err },
			closedField: "schema_version", invalidValue: "argus.local_api_training_job_observe_command.v2",
		},
		{
			name: "CalibrationPromotionPrepareRequest", title: "Argus CalibrationPromotionPrepareRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/calibration-promotion-prepare-request.schema.json", example: "examples/calibration-promotion-prepare-request.json",
			decode:      func(data []byte) error { _, err := calibrationpromotion.DecodePrepareRequest(data); return err },
			closedField: "schema_version", invalidValue: "argus.calibration_promotion_prepare_request.v2",
		},
		{
			name: "CalibrationPromotionGateRequest", title: "Argus CalibrationPromotionGateRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/calibration-promotion-gate-request.schema.json", example: "examples/calibration-promotion-gate-request.json",
			decode:      func(data []byte) error { _, err := calibrationpromotion.DecodeGateRequest(data); return err },
			closedField: "schema_version", invalidValue: "argus.calibration_promotion_gate_request.v2",
		},
		{
			name: "CalibrationPromotionObservationRequest", title: "Argus CalibrationPromotionObservationRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/calibration-promotion-observation-request.schema.json", example: "examples/calibration-promotion-observation-request.json",
			decode:      func(data []byte) error { _, err := promotionmonitor.DecodeBuildRequest(data); return err },
			closedField: "schema_version", invalidValue: "argus.calibration_promotion_observation_request.v2",
		},
		{
			name: "CalibrationPromotionObservation", title: "Argus CalibrationPromotionObservation v1alpha1",
			schemaPath: "api/schema/v1alpha1/calibration-promotion-observation.schema.json", example: "examples/calibration-promotion-observation.json",
			decode:      func(data []byte) error { _, err := promotionmonitor.DecodeObservation(data); return err },
			closedField: "schema_version", invalidValue: "argus.calibration_promotion_observation.v2",
		},
		{
			name: "LocalAPICalibrationPromotionPrepareCommand", title: "Argus LocalAPICalibrationPromotionPrepareCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-calibration-promotion-prepare-command.schema.json", example: "examples/local-api-calibration-promotion-prepare-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeCalibrationPromotionPrepareCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_calibration_promotion_prepare_command.v2",
		},
		{
			name: "LocalAPICalibrationPromotionGateCommand", title: "Argus LocalAPICalibrationPromotionGateCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-calibration-promotion-gate-command.schema.json", example: "examples/local-api-calibration-promotion-gate-command.json",
			decode:      func(data []byte) error { _, err := platformapi.DecodeCalibrationPromotionGateCommand(data); return err },
			closedField: "schema_version", invalidValue: "argus.local_api_calibration_promotion_gate_command.v2",
		},
		{
			name: "LocalAPINormalizationPromotionPrepareCommand", title: "Argus LocalAPINormalizationPromotionPrepareCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-normalization-promotion-prepare-command.schema.json", example: "examples/local-api-normalization-promotion-prepare-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeNormalizationPromotionPrepareCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_normalization_promotion_prepare_command.v2",
		},
		{
			name: "LocalAPINormalizationPromotionGateCommand", title: "Argus LocalAPINormalizationPromotionGateCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-normalization-promotion-gate-command.schema.json", example: "examples/local-api-normalization-promotion-gate-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeNormalizationPromotionGateCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_normalization_promotion_gate_command.v2",
		},
		{
			name: "LocalAPINormalizationPromotionOperationalGateCommand", title: "Argus LocalAPINormalizationPromotionOperationalGateCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-normalization-promotion-operational-gate-command.schema.json", example: "examples/local-api-normalization-promotion-operational-gate-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeNormalizationPromotionOperationalGateCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_normalization_promotion_operational_gate_command.v2",
		},
		{
			name: "LocalAPINormalizationPromotionCanaryCommand", title: "Argus LocalAPINormalizationPromotionCanaryCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-normalization-promotion-canary-command.schema.json", example: "examples/local-api-normalization-promotion-canary-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeNormalizationPromotionCanaryCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_normalization_promotion_canary_command.v2",
		},
		{
			name: "LocalAPINormalizationPromotionLifecycleCommand", title: "Argus LocalAPINormalizationPromotionLifecycleCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-normalization-promotion-lifecycle-command.schema.json", example: "examples/local-api-normalization-promotion-lifecycle-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeNormalizationPromotionLifecycleCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_normalization_promotion_lifecycle_command.v2",
		},
		{
			name: "LocalAPICalibrationPromotionActivateCommand", title: "Argus LocalAPICalibrationPromotionActivateCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-calibration-promotion-activate-command.schema.json", example: "examples/local-api-calibration-promotion-activate-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeCalibrationPromotionActivateCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_calibration_promotion_activate_command.v2",
		},
		{
			name: "LocalAPICalibrationPromotionRollbackCommand", title: "Argus LocalAPICalibrationPromotionRollbackCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-calibration-promotion-rollback-command.schema.json", example: "examples/local-api-calibration-promotion-rollback-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeCalibrationPromotionRollbackCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_calibration_promotion_rollback_command.v2",
		},
		{
			name: "LocalAPICalibrationPromotionObserveCommand", title: "Argus LocalAPICalibrationPromotionObserveCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-calibration-promotion-observe-command.schema.json", example: "examples/local-api-calibration-promotion-observe-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeCalibrationPromotionObserveCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_calibration_promotion_observe_command.v2",
		},
		{
			name: "ExternalGovernedCaseImport", title: "Argus ExternalGovernedCaseImport v1alpha1",
			schemaPath: "api/schema/v1alpha1/external-governed-case-import.schema.json",
			example:    "examples/external-governed-case-import.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeExternalGovernedCaseImport(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.external_governed_case_import.v2",
		},
		{
			name: "GovernanceTrustKeyRegistration", title: "Argus GovernanceTrustKeyRegistration v1alpha1",
			schemaPath: "api/schema/v1alpha1/governance-trust-key-registration.schema.json",
			example:    "examples/governance-trust-key-registration.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeGovernanceTrustKeyRegistration(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.governance_trust_key_registration.v2",
		},
		{
			name: "GovernanceTrustKeyRevocation", title: "Argus GovernanceTrustKeyRevocation v1alpha1",
			schemaPath: "api/schema/v1alpha1/governance-trust-key-revocation.schema.json",
			example:    "examples/governance-trust-key-revocation.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeGovernanceTrustKeyRevocation(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.governance_trust_key_revocation.v2",
		},
		{
			name: "GovernanceBatchRequest", title: "Argus GovernanceBatchRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/governance-batch-request.schema.json",
			example:    "examples/governance-batch-request.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeGovernanceBatchRequest(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.governance_batch_request.v2",
		},
		{
			name: "GovernanceBatchResult", title: "Argus GovernanceBatchResult v1alpha1",
			schemaPath: "api/schema/v1alpha1/governance-batch-result.schema.json",
			example:    "examples/governance-batch-result.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeGovernanceBatchResult(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.governance_batch_result.v2",
		},
		{
			name: "LocalAPIPrincipal", title: "Argus LocalAPIPrincipal v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-principal.schema.json",
			example:    "examples/local-api-principal.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodePrincipal(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_principal.v2",
		},
		{
			name: "LocalAPIMutation", title: "Argus LocalAPIMutation v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-mutation.schema.json",
			example:    "examples/local-api-mutation.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeMutationInput(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_mutation.v2",
		},
		{
			name: "LocalAPIGovernanceBatchCommand", title: "Argus LocalAPIGovernanceBatchCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-governance-batch-command.schema.json",
			example:    "examples/local-api-governance-batch-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeGovernanceBatchCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_governance_batch_command.v2",
		},
		{
			name: "LocalAPIEvaluationBatchResumeCommand", title: "Argus LocalAPIEvaluationBatchResumeCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-evaluation-batch-resume-command.schema.json",
			example:    "examples/local-api-evaluation-batch-resume-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeEvaluationBatchResumeCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_evaluation_batch_resume_command.v2",
		},
		{
			name: "LocalAPIExperimentBatchSubmitCommand", title: "Argus LocalAPIExperimentBatchSubmitCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-experiment-batch-submit-command.schema.json",
			example:    "examples/local-api-experiment-batch-submit-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeExperimentBatchSubmitCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_experiment_batch_submit_command.v2",
		},
		{
			name: "LocalAPIPromptExperimentBatchSubmitCommand", title: "Argus LocalAPIExperimentBatchSubmitCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-experiment-batch-submit-command.schema.json",
			example:    "examples/local-api-prompt-experiment-batch-submit-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeExperimentBatchSubmitCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_experiment_batch_submit_command.v2",
		},
		{
			name: "LocalAPIModelExperimentBatchSubmitCommand", title: "Argus LocalAPIExperimentBatchSubmitCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-experiment-batch-submit-command.schema.json",
			example:    "examples/local-api-model-experiment-batch-submit-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeExperimentBatchSubmitCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_experiment_batch_submit_command.v2",
		},
		{
			name: "LocalAPIIndexExperimentBatchSubmitCommand", title: "Argus LocalAPIExperimentBatchSubmitCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-experiment-batch-submit-command.schema.json",
			example:    "examples/local-api-index-experiment-batch-submit-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeExperimentBatchSubmitCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_experiment_batch_submit_command.v2",
		},
		{
			name: "LocalAPIRulePackExperimentBatchSubmitCommand", title: "Argus LocalAPIExperimentBatchSubmitCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-experiment-batch-submit-command.schema.json",
			example:    "examples/local-api-rule-pack-experiment-batch-submit-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeExperimentBatchSubmitCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_experiment_batch_submit_command.v2",
		},
		{
			name: "LocalAPIWorkflowExperimentBatchSubmitCommand", title: "Argus LocalAPIExperimentBatchSubmitCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-experiment-batch-submit-command.schema.json",
			example:    "examples/local-api-workflow-experiment-batch-submit-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeExperimentBatchSubmitCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_experiment_batch_submit_command.v2",
		},
		{
			name: "LocalAPIFindingGovernanceExperimentBatchSubmitCommand", title: "Argus LocalAPIExperimentBatchSubmitCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-experiment-batch-submit-command.schema.json",
			example:    "examples/local-api-finding-governance-experiment-batch-submit-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeExperimentBatchSubmitCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_experiment_batch_submit_command.v2",
		},
		{
			name: "LocalAPIAgentComponentPublishCommand", title: "Argus LocalAPIAgentComponentPublishCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-agent-component-publish-command.schema.json",
			example:    "examples/local-api-agent-component-publish-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeAgentComponentPublishCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_agent_component_publish_command.v2",
		},
		{
			name: "LocalAPIRepeatabilityBatchSubmitCommand", title: "Argus LocalAPIRepeatabilityBatchSubmitCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-repeatability-batch-submit-command.schema.json",
			example:    "examples/local-api-repeatability-batch-submit-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeRepeatabilityBatchSubmitCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_repeatability_batch_submit_command.v2",
		},
		{
			name: "LocalAPICaseImportCommand", title: "Argus LocalAPICaseImportCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-case-import-command.schema.json",
			example:    "examples/local-api-case-import-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeCaseImportCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_case_import_command.v2",
		},
		{
			name: "LocalAPITrustKeyCommand", title: "Argus LocalAPITrustKeyCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-trust-key-command.schema.json",
			example:    "examples/local-api-trust-key-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeTrustKeyCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_trust_key_command.v2",
		},
		{
			name: "LocalAPITrustKeyRevocationCommand", title: "Argus LocalAPITrustKeyRevocationCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-trust-key-revocation-command.schema.json",
			example:    "examples/local-api-trust-key-revocation-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeTrustKeyRevocationCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_trust_key_revocation_command.v2",
		},
		{
			name: "LocalAPIConfigCreateCommand", title: "Argus LocalAPIConfigCreateCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-config-create-command.schema.json",
			example:    "examples/local-api-config-create-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeConfigCreateCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_config_create_command.v2",
		},
		{
			name: "LocalAPIConfigResolutionQuery", title: "Argus LocalAPIConfigResolutionQuery v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-config-resolution-query.schema.json",
			example:    "examples/local-api-config-resolution-query.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeConfigResolutionQuery(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_config_resolution_query.v2",
		},
		{
			name: "LocalAPIConfigTransitionCommand", title: "Argus LocalAPIConfigTransitionCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-config-transition-command.schema.json",
			example:    "examples/local-api-config-transition-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeConfigTransitionCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_config_transition_command.v2",
		},
		{
			name: "LocalAPIFindingDecisionWriteCommand", title: "Argus LocalAPIFindingDecisionWriteCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-finding-decision-write-command.schema.json",
			example:    "examples/local-api-finding-decision-write-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeFindingDecisionWriteCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_finding_decision_write_command.v2",
		},
		{
			name: "LocalAPIFeedbackWriteCommand", title: "Argus LocalAPIFeedbackWriteCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-feedback-write-command.schema.json",
			example:    "examples/local-api-feedback-write-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeFeedbackWriteCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_feedback_write_command.v2",
		},
		{
			name: "LocalAPIOutcomeWriteCommand", title: "Argus LocalAPIOutcomeWriteCommand v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-api-outcome-write-command.schema.json",
			example:    "examples/local-api-outcome-write-command.json",
			decode: func(data []byte) error {
				_, err := platformapi.DecodeOutcomeWriteCommand(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.local_api_outcome_write_command.v2",
		},
		{
			name: "EvaluationLabelCorrection", title: "Argus EvaluationLabelCorrection v1alpha1",
			schemaPath: "api/schema/v1alpha1/evaluation-label-correction.schema.json",
			example:    "examples/evaluation-label-correction.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeLabelCorrection(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.evaluation_label_correction.v2",
		},
		{
			name: "EvaluationCaseAnnotation", title: "Argus EvaluationCaseAnnotation v1alpha1",
			schemaPath: "api/schema/v1alpha1/evaluation-case-annotation.schema.json",
			example:    "examples/evaluation-case-annotation.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeCaseAnnotation(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.evaluation_case_annotation.v2",
		},
		{
			name: "EvaluationCaseReviewAssignment", title: "Argus EvaluationCaseReviewAssignment v1alpha1",
			schemaPath: "api/schema/v1alpha1/evaluation-case-review-assignment.schema.json",
			example:    "examples/evaluation-case-review-assignment.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeCaseReviewAssignment(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.evaluation_case_review_assignment.v2",
		},
		{
			name: "EvaluationCaseReopen", title: "Argus EvaluationCaseReopen v1alpha1",
			schemaPath: "api/schema/v1alpha1/evaluation-case-reopen.schema.json",
			example:    "examples/evaluation-case-reopen.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeCaseReopen(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.evaluation_case_reopen.v2",
		},
		{
			name: "MissedDefectIncident", title: "Argus MissedDefectIncident v1alpha1",
			schemaPath: "api/schema/v1alpha1/missed-defect-incident.schema.json",
			example:    "examples/missed-defect-incident.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeMissedDefectIncident(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.missed_defect_incident.v2",
		},
		{
			name: "EvaluationProbe", title: "Argus EvaluationProbe v1alpha1",
			schemaPath: "api/schema/v1alpha1/evaluation-probe.schema.json",
			example:    "examples/evaluation-probe.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeEvaluationProbe(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.evaluation_probe.v2",
		},
		{
			name: "EvaluationProbeReceipt", title: "Argus EvaluationProbeReceipt v1alpha1",
			schemaPath: "api/schema/v1alpha1/evaluation-probe-receipt.schema.json",
			example:    "examples/evaluation-probe-receipt.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeEvaluationProbeReceipt(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.evaluation_probe_receipt.v2",
		},
		{
			name: "EvaluationCaseAdjudication", title: "Argus EvaluationCaseAdjudication v1alpha1",
			schemaPath: "api/schema/v1alpha1/evaluation-case-adjudication.schema.json",
			example:    "examples/evaluation-case-adjudication.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeCaseAdjudication(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.evaluation_case_adjudication.v2",
		},
		{
			name: "EvaluationCaseActivation", title: "Argus EvaluationCaseActivation v1alpha1",
			schemaPath: "api/schema/v1alpha1/evaluation-case-activation.schema.json",
			example:    "examples/evaluation-case-activation.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeCaseActivation(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.evaluation_case_activation.v2",
		},
		{
			name: "EvaluationRunRequest", title: "Argus EvaluationRunRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/evaluation-run-request.schema.json",
			example:    "examples/evaluation-run-request.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeEvaluationRunRequest(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.evaluation_run_request.v2",
		},
		{
			name: "EvaluationRun", title: "Argus EvaluationRun v1alpha1",
			schemaPath: "api/schema/v1alpha1/evaluation-run.schema.json",
			example:    "examples/evaluation-run.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeEvaluationRun(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.evaluation_run.v2",
		},
		{
			name: "ExperimentRunRequest", title: "Argus ExperimentRunRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/experiment-run-request.schema.json",
			example:    "examples/experiment-run-request.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeExperimentRunRequest(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.experiment_run_request.v2",
		},
		{
			name: "ExperimentRun", title: "Argus ExperimentRun v1alpha1",
			schemaPath: "api/schema/v1alpha1/experiment-run.schema.json",
			example:    "examples/experiment-run.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeExperimentRun(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.experiment_run.v2",
		},
		{
			name: "NormalizationPolicyComparison", title: "Argus NormalizationPolicyComparison v1alpha1",
			schemaPath: "api/schema/v1alpha1/normalization-policy-comparison.schema.json",
			example:    "examples/normalization-policy-comparison.json",
			decode: func(data []byte) error {
				_, err := pireviewmap.DecodeNormalizationPolicyComparison(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.normalization_policy_comparison.v2",
		},
		{
			name: "NormalizationOracle", title: "Argus NormalizationOracle v1alpha1",
			schemaPath: "api/schema/v1alpha1/normalization-oracle.schema.json",
			example:    "examples/normalization-oracle.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeNormalizationOracle(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.normalization_oracle.v2",
		},
		{
			name: "NormalizationOracleAttestation", title: "Argus NormalizationOracleAttestation v1alpha1",
			schemaPath: "api/schema/v1alpha1/normalization-oracle-attestation.schema.json",
			example:    "examples/normalization-oracle-attestation.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeNormalizationOracleAttestation(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.normalization_oracle_attestation.v2",
		},
		{
			name: "NormalizationOracleRegistration", title: "Argus NormalizationOracleRegistration v1alpha1",
			schemaPath: "api/schema/v1alpha1/normalization-oracle-registration.schema.json",
			example:    "examples/normalization-oracle-registration.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeNormalizationOracleRegistration(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.normalization_oracle_registration.v2",
		},
		{
			name: "NormalizationOracleRevocation", title: "Argus NormalizationOracleRevocation v1alpha1",
			schemaPath: "api/schema/v1alpha1/normalization-oracle-revocation.schema.json",
			example:    "examples/normalization-oracle-revocation.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeNormalizationOracleRevocation(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.normalization_oracle_revocation.v2",
		},
		{
			name: "NormalizationQualityRunRequest", title: "Argus NormalizationQualityRunRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/normalization-quality-run-request.schema.json",
			example:    "examples/normalization-quality-run-request.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeNormalizationQualityRunRequest(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.normalization_quality_run_request.v2",
		},
		{
			name: "NormalizationQualityRun", title: "Argus NormalizationQualityRun v1alpha1",
			schemaPath: "api/schema/v1alpha1/normalization-quality-run.schema.json",
			example:    "examples/normalization-quality-run.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeNormalizationQualityRun(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.normalization_quality_run.v2",
		},
		{
			name: "NormalizationPromotionPolicy", title: "Argus NormalizationPromotionPolicy v1alpha1",
			schemaPath: "api/schema/v1alpha1/normalization-promotion-policy.schema.json",
			example:    "examples/normalization-promotion-policy.json",
			decode: func(data []byte) error {
				_, err := normalizationpromotion.DecodePolicy(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.normalization_promotion_policy.v2",
		},
		{
			name: "NormalizationPromotionPrepareRequest", title: "Argus NormalizationPromotionPrepareRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/normalization-promotion-prepare-request.schema.json",
			example:    "examples/normalization-promotion-prepare-request.json",
			decode: func(data []byte) error {
				_, err := normalizationpromotion.DecodePrepareRequest(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.normalization_promotion_prepare_request.v2",
		},
		{
			name: "NormalizationPromotionGateRequest", title: "Argus NormalizationPromotionGateRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/normalization-promotion-gate-request.schema.json",
			example:    "examples/normalization-promotion-gate-request.json",
			decode: func(data []byte) error {
				_, err := normalizationpromotion.DecodeGateRequest(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.normalization_promotion_gate_request.v2",
		},
		{
			name: "NormalizationPromotionGateDecision", title: "Argus NormalizationPromotionGateDecision v1alpha1",
			schemaPath: "api/schema/v1alpha1/normalization-promotion-gate-decision.schema.json",
			example:    "examples/normalization-promotion-gate-decision.json",
			decode: func(data []byte) error {
				_, err := normalizationpromotion.DecodeGateDecision(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.normalization_promotion_gate_decision.v2",
		},
		{
			name: "NormalizationPromotionOperationalGateRequest", title: "Argus NormalizationPromotionOperationalGateRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/normalization-promotion-operational-gate-request.schema.json",
			example:    "examples/normalization-promotion-operational-gate-request.json",
			decode: func(data []byte) error {
				_, err := normalizationpromotion.DecodeOperationalGateRequest(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.normalization_promotion_operational_gate_request.v2",
		},
		{
			name: "RepeatabilityRunRequest", title: "Argus RepeatabilityRunRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/repeatability-run-request.schema.json",
			example:    "examples/repeatability-run-request.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeRepeatabilityRunRequest(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.repeatability_run_request.v2",
		},
		{
			name: "RepeatabilityRun", title: "Argus RepeatabilityRun v1alpha1",
			schemaPath: "api/schema/v1alpha1/repeatability-run.schema.json",
			example:    "examples/repeatability-run.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeRepeatabilityRun(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.repeatability_run.v2",
		},
		{
			name: "RepeatabilityBatchRequest", title: "Argus RepeatabilityBatchRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/repeatability-batch-request.schema.json",
			example:    "examples/repeatability-batch-request.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeRepeatabilityBatchRequest(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.repeatability_batch_request.v2",
		},
		{
			name: "RepeatabilityBatchResult", title: "Argus RepeatabilityBatchResult v1alpha1",
			schemaPath: "api/schema/v1alpha1/repeatability-batch-result.schema.json",
			example:    "examples/repeatability-batch-result.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeRepeatabilityBatchResult(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.repeatability_batch_result.v2",
		},
		{
			name: "ExperimentBatchRequest", title: "Argus ExperimentBatchRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/experiment-batch-request.schema.json",
			example:    "examples/experiment-batch-request.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeExperimentBatchRequest(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.experiment_batch_request.v2",
		},
		{
			name: "ExperimentBatchResult", title: "Argus ExperimentBatchResult v1alpha1",
			schemaPath: "api/schema/v1alpha1/experiment-batch-result.schema.json",
			example:    "examples/experiment-batch-result.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeExperimentBatchResult(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.experiment_batch_result.v2",
		},
		{
			name: "CorpusSnapshotRequest", title: "Argus CorpusSnapshotRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/corpus-snapshot-request.schema.json",
			example:    "examples/corpus-snapshot-request.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeCorpusSnapshotRequest(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.corpus_snapshot_request.v2",
		},
		{
			name: "CorpusSnapshot", title: "Argus CorpusSnapshot v1alpha1",
			schemaPath: "api/schema/v1alpha1/corpus-snapshot.schema.json",
			example:    "examples/corpus-snapshot.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeCorpusSnapshot(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.corpus_snapshot.v2",
		},
		{
			name: "FormalCorpusBatchRequest", title: "Argus FormalCorpusBatchRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/formal-corpus-batch-request.schema.json",
			example:    "examples/formal-corpus-batch-request.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeFormalCorpusBatchRequest(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.formal_corpus_batch_request.v2",
		},
		{
			name: "FormalCorpusBatchResult", title: "Argus FormalCorpusBatchResult v1alpha1",
			schemaPath: "api/schema/v1alpha1/formal-corpus-batch-result.schema.json",
			example:    "examples/formal-corpus-batch-result.json",
			decode: func(data []byte) error {
				_, err := evaluation.DecodeFormalCorpusBatchResult(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.formal_corpus_batch_result.v2",
		},
		{
			name: "ReviewRunImpactIndexFact", title: "Argus ReviewRunImpactIndexFact v1alpha1",
			schemaPath: "api/schema/v1alpha1/review-run-impact-index-fact.schema.json",
			example:    "examples/review-run-impact-index-fact.json",
			decode: func(data []byte) error {
				_, err := runrepo.DecodeImpactIndexFact(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.review_run_impact_index_fact.v2",
		},
		{
			name: "ContextProviderExecutionReceipt", title: "Argus ContextProviderExecutionReceipt v1alpha1",
			schemaPath: "api/schema/v1alpha1/context-provider-execution-receipt.schema.json",
			example:    "examples/context-provider-execution-receipt.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeContextProviderExecutionReceipt(data)
				return err
			},
			closedField: "authority", invalidValue: "platform_attested",
		},
		{
			name: "AgentReviewPlan", title: "Argus AgentReviewPlan v1alpha1",
			schemaPath: "api/schema/v1alpha1/agent-review-plan.schema.json",
			example:    "examples/agent-review-plan.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentReviewPlan(data)
				return err
			},
			closedField: "side_effects", invalidValue: "allow",
		},
		{
			name: "ReviewHypothesisSet", title: "Argus ReviewHypothesisSet v1alpha1",
			schemaPath: "api/schema/v1alpha1/review-hypothesis-set.schema.json",
			example:    "examples/review-hypothesis-set.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeReviewHypothesisSet(data)
				return err
			},
			closedField: "completeness", invalidValue: "unknown",
		},
		{
			name: "GovernedReviewReport", title: "Argus GovernedReviewReport v1alpha1",
			schemaPath: "api/schema/v1alpha1/governed-review-report.schema.json",
			example:    "examples/governed-review-report.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeGovernedReviewReport(data)
				return err
			},
			closedField: "completeness", invalidValue: "unknown",
		},
		{
			name: "GovernedCandidateSet", title: "Argus GovernedCandidateSet v1alpha1",
			schemaPath: "api/schema/v1alpha1/governed-candidate-set.schema.json",
			example:    "examples/governed-candidate-set.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeGovernedCandidateSet(data)
				return err
			},
			closedField: "completeness", invalidValue: "unknown",
		},
		{
			name: "CandidateVerificationLedger", title: "Argus CandidateVerificationLedger v1alpha1",
			schemaPath: "api/schema/v1alpha1/candidate-verification-ledger.schema.json",
			example:    "examples/candidate-verification-ledger.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeCandidateVerificationLedger(data)
				return err
			},
			closedField: "completeness", invalidValue: "unknown",
		},
		{
			name: "FindingCalibrationLedger", title: "Argus FindingCalibrationLedger v1alpha1",
			schemaPath: "api/schema/v1alpha1/finding-calibration-ledger.schema.json",
			example:    "examples/finding-calibration-ledger.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeFindingCalibrationLedger(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.finding_calibration_ledger.v2",
		},
		{
			name: "FindingSuppressionLedger", title: "Argus FindingSuppressionLedger v1alpha1",
			schemaPath: "api/schema/v1alpha1/finding-suppression-ledger.schema.json",
			example:    "examples/finding-suppression-ledger.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeFindingSuppressionLedger(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.finding_suppression_ledger.v2",
		},
		{
			name: "FindingLineage", title: "Argus FindingLineage v1alpha1",
			schemaPath: "api/schema/v1alpha1/finding-lineage.schema.json",
			example:    "examples/finding-lineage.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeFindingLineage(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.finding_lineage.v2",
		},
		{
			name:       "AgentReviewRawCandidateCollection",
			title:      "Argus AgentReviewRawCandidateCollection v1alpha1",
			schemaPath: "api/schema/v1alpha1/agent-review-raw-candidate-collection.schema.json",
			example:    "examples/agent-review-raw-candidate-collection.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentReviewRawCandidateCollection(data)
				return err
			},
			closedField: "authority", invalidValue: "platform_attested",
		},
		{
			name: "LocalRuntimeFileManifest", title: "LocalRuntimeFileManifest v1alpha1",
			schemaPath: "api/schema/v1alpha1/local-runtime-file-manifest.schema.json",
			example:    "examples/local-runtime-file-manifest.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeLocalRuntimeFileManifest(data)
				return err
			},
			closedField: "authority", invalidValue: "platform_attested",
		},
		{
			name: "AgentExecutionReceipt", title: "Argus AgentExecutionReceipt v1alpha1",
			schemaPath: "api/schema/v1alpha1/agent-execution-receipt.schema.json",
			example:    "examples/agent-execution-receipt.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentExecutionReceipt(data)
				return err
			},
			closedField: "provenance_class", invalidValue: "platform_attested",
		},
		{
			name:       "AgentExecutionReceiptCollection",
			title:      "Argus AgentExecutionReceiptCollection v1alpha1",
			schemaPath: "api/schema/v1alpha1/agent-execution-receipt-collection.schema.json",
			example:    "examples/agent-execution-receipt-collection.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentExecutionReceiptCollection(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.agent_execution_receipt_collection.v2",
		},
		{
			name:       "AgentReviewTaskEvidenceCollection",
			title:      "Argus AgentReviewTaskEvidenceCollection v1alpha1",
			schemaPath: "api/schema/v1alpha1/agent-review-task-evidence-collection.schema.json",
			example:    "examples/agent-review-task-evidence-collection.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentReviewTaskEvidenceCollection(data)
				return err
			},
			closedField: "content_policy", invalidValue: "redacted",
		},
		{
			name: "AgentReviewResultManifest", title: "Argus AgentReviewResultManifest v1alpha1",
			schemaPath: "api/schema/v1alpha1/agent-review-result-manifest.schema.json",
			example:    "examples/agent-review-result-manifest.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentReviewResultManifest(data)
				return err
			},
			closedField: "disposition", invalidValue: "publishable",
		},
		{
			name: "AgentReviewObservation", title: "Argus AgentReviewObservation v1alpha1",
			schemaPath: "api/schema/v1alpha1/agent-review-observation.schema.json",
			example:    "examples/agent-review-observation.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentReviewObservation(data)
				return err
			},
			closedField: "disposition", invalidValue: "publishable",
		},
		{
			name: "ModelProfile", title: "Argus ModelProfile v1alpha1",
			schemaPath: "api/schema/v1alpha1/model-profile.schema.json",
			example:    "examples/model-profile.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeModelProfile(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.model_profile.v2",
		},
		{
			name: "AgentReviewPromptBundle", title: "Argus AgentReviewPromptBundle v1alpha1",
			schemaPath: "api/schema/v1alpha1/agent-review-prompt-bundle.schema.json",
			example:    "examples/agent-review-prompt-bundle.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentReviewPromptBundle(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.agent_review_prompt_bundle.v2",
		},
		{
			name: "AgentReviewWorkerRequest", title: "Argus AgentReviewWorkerRequest v1alpha1",
			schemaPath: "api/schema/v1alpha1/agent-review-worker-request.schema.json",
			example:    "examples/agent-review-worker-request.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentReviewWorkerRequest(data)
				return err
			},
			closedField: "schema_version", invalidValue: "argus.agent_review_worker_request.v2",
		},
		{
			name: "AgentReviewWorkerResult", title: "Argus AgentReviewWorkerResult v1alpha1",
			schemaPath: "api/schema/v1alpha1/agent-review-worker-result.schema.json",
			example:    "examples/agent-review-worker-result.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentReviewWorkerResult(data)
				return err
			},
			closedField: "status", invalidValue: "unknown",
		},
	}
	for _, contract := range contracts {
		schema, err := compiler.Compile(contract.schemaPath)
		if err != nil {
			return fmt.Errorf("compile %s schema: %w", contract.name, err)
		}
		data, err := os.ReadFile(contract.example)
		if err != nil {
			return fmt.Errorf("read %s: %w", contract.example, err)
		}
		if err := contract.decode(data); err != nil {
			return fmt.Errorf("validate %s: %w", contract.example, err)
		}
		if err := validateJSONSchema(schema, data); err != nil {
			return fmt.Errorf("schema validate %s: %w", contract.example, err)
		}
		schemaData, err := os.ReadFile(contract.schemaPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", contract.schemaPath, err)
		}
		var identity map[string]any
		if err := json.Unmarshal(schemaData, &identity); err != nil {
			return fmt.Errorf("decode %s: %w", contract.schemaPath, err)
		}
		if identity["title"] != contract.title || identity["additionalProperties"] != false {
			return fmt.Errorf("%s schema identity or strictness is invalid", contract.name)
		}
		var object map[string]any
		if err := json.Unmarshal(data, &object); err != nil {
			return fmt.Errorf("decode %s for negative checks: %w", contract.example, err)
		}
		object["raw_prompt"] = "must never be accepted"
		unknownData, err := json.Marshal(object)
		if err != nil {
			return err
		}
		if err := contract.decode(unknownData); err == nil {
			return fmt.Errorf("Go validator accepted unknown/raw field for %s", contract.name)
		}
		if err := validateJSONSchema(schema, unknownData); err == nil {
			return fmt.Errorf("JSON Schema accepted unknown/raw field for %s", contract.name)
		}
		delete(object, "raw_prompt")
		object[contract.closedField] = contract.invalidValue
		invalidData, err := json.Marshal(object)
		if err != nil {
			return err
		}
		if err := contract.decode(invalidData); err == nil {
			return fmt.Errorf("Go validator accepted open enum for %s", contract.name)
		}
		if err := validateJSONSchema(schema, invalidData); err == nil {
			return fmt.Errorf("JSON Schema accepted open enum for %s", contract.name)
		}
	}
	if err := checkAgentReviewShadowNegativeMutations(compiler); err != nil {
		return err
	}

	planData, err := os.ReadFile("examples/agent-review-plan.json")
	if err != nil {
		return err
	}
	plan, err := contractsv1alpha1.DecodeAgentReviewPlan(planData)
	if err != nil {
		return err
	}
	setData, err := os.ReadFile("examples/review-hypothesis-set.json")
	if err != nil {
		return err
	}
	set, err := contractsv1alpha1.DecodeReviewHypothesisSet(setData)
	if err != nil {
		return err
	}
	receiptData, err := os.ReadFile("examples/agent-execution-receipt-collection.json")
	if err != nil {
		return err
	}
	receiptCollection, err := contractsv1alpha1.DecodeAgentExecutionReceiptCollection(receiptData)
	if err != nil {
		return err
	}
	taskEvidenceData, err := os.ReadFile("examples/agent-review-task-evidence-collection.json")
	if err != nil {
		return err
	}
	taskEvidence, err := contractsv1alpha1.DecodeAgentReviewTaskEvidenceCollection(taskEvidenceData)
	if err != nil {
		return err
	}
	manifestData, err := os.ReadFile("examples/agent-review-result-manifest.json")
	if err != nil {
		return err
	}
	manifest, err := contractsv1alpha1.DecodeAgentReviewResultManifest(manifestData)
	if err != nil {
		return err
	}
	if err := contractsv1alpha1.ValidateAgentReviewResultManifestBindings(
		manifest,
		plan,
		set,
		receiptCollection,
	); err != nil {
		return fmt.Errorf("validate canonical shadow manifest closure: %w", err)
	}
	if err := contractsv1alpha1.ValidateAgentReviewTaskEvidenceBindings(taskEvidence, plan, receiptCollection); err != nil {
		return fmt.Errorf("validate canonical task evidence closure: %w", err)
	}
	observationData, err := os.ReadFile("examples/agent-review-observation.json")
	if err != nil {
		return err
	}
	observation, err := contractsv1alpha1.DecodeAgentReviewObservation(observationData)
	if err != nil {
		return err
	}
	if err := contractsv1alpha1.ValidateAgentReviewObservationBinding(
		observation,
		manifest,
		plan,
	); err != nil {
		return fmt.Errorf("validate canonical shadow observation closure: %w", err)
	}
	workerRequestData, err := os.ReadFile("examples/agent-review-worker-request.json")
	if err != nil {
		return err
	}
	workerRequest, err := contractsv1alpha1.DecodeAgentReviewWorkerRequest(workerRequestData)
	if err != nil {
		return err
	}
	promptExampleData, err := os.ReadFile("examples/agent-review-prompt-bundle.json")
	if err != nil {
		return err
	}
	_, promptBytes, err := contractsv1alpha1.DecodeAgentReviewWorkerPromptBundle(
		workerRequest.PromptBundle,
	)
	if err != nil {
		return fmt.Errorf("decode canonical worker prompt bundle: %w", err)
	}
	compactPrompt := &bytes.Buffer{}
	if err := json.Compact(compactPrompt, promptExampleData); err != nil {
		return fmt.Errorf("compact canonical prompt example: %w", err)
	}
	if !bytes.Equal(promptBytes, compactPrompt.Bytes()) {
		return fmt.Errorf("canonical worker prompt bytes do not equal prompt bundle example")
	}
	reviewInputData, err := base64.StdEncoding.Strict().DecodeString(
		workerRequest.ReviewInputBase64,
	)
	if err != nil {
		return fmt.Errorf("decode canonical worker review input: %w", err)
	}
	reviewInput, err := reviewcore.DecodeReviewInput(reviewInputData)
	if err != nil {
		return fmt.Errorf("validate canonical worker ReviewInput: %w", err)
	}
	targetDigest, err := reviewcore.DigestReviewInput(reviewInput)
	if err != nil {
		return fmt.Errorf("digest canonical worker ReviewInput: %w", err)
	}
	if targetDigest != workerRequest.Plan.TargetDigest {
		return fmt.Errorf("canonical worker ReviewInput does not match plan target_digest")
	}
	workerResultData, err := os.ReadFile("examples/agent-review-worker-result.json")
	if err != nil {
		return err
	}
	workerResult, err := contractsv1alpha1.DecodeAgentReviewWorkerResult(workerResultData)
	if err != nil {
		return err
	}
	if err := contractsv1alpha1.ValidateAgentReviewWorkerResultBinding(
		workerRequest,
		workerResult,
	); err != nil {
		return fmt.Errorf("validate canonical agent review worker closure: %w", err)
	}
	return nil
}

func checkAgentReviewShadowNegativeMutations(compiler *jsonschema.Compiler) error {
	tests := []struct {
		name       string
		schemaPath string
		example    string
		decode     func([]byte) error
		mutate     func(map[string]any) error
		// JSON Schema cannot express invocation_count - failure_count == 1.
		// Keep arithmetic cross-field mutations in the strict Go decoder corpus.
		goOnly bool
	}{
		{
			name:       "plan execution snapshot contract",
			schemaPath: "api/schema/v1alpha1/agent-review-plan.schema.json",
			example:    "examples/agent-review-plan.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentReviewPlan(data)
				return err
			},
			mutate: func(object map[string]any) error {
				return mutateNestedString(
					object,
					"execution_snapshot_ref",
					"contract",
					"argus.unknown_snapshot.v1alpha1",
				)
			},
		},
		{
			name:       "plan terminal submit in allowed repository tools",
			schemaPath: "api/schema/v1alpha1/agent-review-plan.schema.json",
			example:    "examples/agent-review-plan.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentReviewPlan(data)
				return err
			},
			mutate: func(object map[string]any) error {
				policy, ok := object["tool_policy"].(map[string]any)
				if !ok {
					return fmt.Errorf("plan tool_policy example is not an object")
				}
				policy["allowed_tools"] = []any{"submit_context"}
				return nil
			},
		},
		{
			name:       "rejected normalization occurrence lineage",
			schemaPath: "api/schema/v1alpha1/review-hypothesis-set.schema.json",
			example:    "examples/review-hypothesis-set.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeReviewHypothesisSet(data)
				return err
			},
			mutate: func(object map[string]any) error {
				decisions, ok := object["normalization_decisions"].([]any)
				if !ok || len(decisions) == 0 {
					return fmt.Errorf("normalization_decisions example is not an array")
				}
				decision, ok := decisions[0].(map[string]any)
				if !ok {
					return fmt.Errorf("normalization decision example is not an object")
				}
				decision["action"] = "rejected_invalid"
				return nil
			},
		},
		{
			name:       "evidence excerpt below exact evidence floor",
			schemaPath: "api/schema/v1alpha1/review-hypothesis-set.schema.json",
			example:    "examples/review-hypothesis-set.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeReviewHypothesisSet(data)
				return err
			},
			mutate: func(object map[string]any) error {
				hypotheses, ok := object["hypotheses"].([]any)
				if !ok || len(hypotheses) == 0 {
					return fmt.Errorf("hypotheses example is not an array")
				}
				hypothesis, ok := hypotheses[0].(map[string]any)
				if !ok {
					return fmt.Errorf("hypothesis example is not an object")
				}
				evidenceItems, ok := hypothesis["evidence"].([]any)
				if !ok || len(evidenceItems) == 0 {
					return fmt.Errorf("hypothesis evidence example is not an array")
				}
				evidenceObject, ok := evidenceItems[0].(map[string]any)
				if !ok {
					return fmt.Errorf("hypothesis evidence example is not an object")
				}
				evidenceObject["excerpt"] = "1234567"
				evidenceData, err := json.Marshal(evidenceObject)
				if err != nil {
					return err
				}
				var evidence contractsv1alpha1.HypothesisEvidence
				if err := json.Unmarshal(evidenceData, &evidence); err != nil {
					return err
				}
				digest, err := contractsv1alpha1.DigestHypothesisEvidence(evidence)
				if err != nil {
					return err
				}
				evidenceObject["evidence_digest"] = digest
				return nil
			},
		},
		{
			name:       "unavailable usage with fabricated counters",
			schemaPath: "api/schema/v1alpha1/agent-execution-receipt.schema.json",
			example:    "examples/agent-execution-receipt.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentExecutionReceipt(data)
				return err
			},
			mutate: func(object map[string]any) error {
				usage, ok := object["usage"].(map[string]any)
				if !ok {
					return fmt.Errorf("receipt usage example is not an object")
				}
				usage["completeness"] = "unavailable"
				usage["unavailable_reason_code"] = "provider_usage_missing"
				return nil
			},
		},
		{
			name:       "partial usage without missing-portion reason",
			schemaPath: "api/schema/v1alpha1/agent-execution-receipt.schema.json",
			example:    "examples/agent-execution-receipt.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentExecutionReceipt(data)
				return err
			},
			mutate: func(object map[string]any) error {
				usage, ok := object["usage"].(map[string]any)
				if !ok {
					return fmt.Errorf("receipt usage example is not an object")
				}
				usage["completeness"] = "partial"
				return nil
			},
		},
		{
			name:       "repeated typed terminal submit",
			schemaPath: "api/schema/v1alpha1/agent-execution-receipt.schema.json",
			example:    "examples/agent-execution-receipt.json",
			goOnly:     true,
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentExecutionReceipt(data)
				return err
			},
			mutate: func(object map[string]any) error {
				toolUsage, ok := object["tool_usage"].([]any)
				if !ok || len(toolUsage) == 0 {
					return fmt.Errorf("receipt tool_usage example is not an array")
				}
				for _, item := range toolUsage {
					usage, ok := item.(map[string]any)
					if !ok {
						return fmt.Errorf("receipt tool_usage item is not an object")
					}
					if usage["tool_id"] == "submit_candidates" {
						usage["invocation_count"] = float64(2)
						return nil
					}
				}
				return fmt.Errorf("receipt example has no submit_candidates usage")
			},
		},
		{
			name:       "succeeded receipt without prompt digest",
			schemaPath: "api/schema/v1alpha1/agent-execution-receipt.schema.json",
			example:    "examples/agent-execution-receipt.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentExecutionReceipt(data)
				return err
			},
			mutate: func(object map[string]any) error {
				delete(object, "prompt_digest")
				return nil
			},
		},
		{
			name:       "manifest review input contract",
			schemaPath: "api/schema/v1alpha1/agent-review-result-manifest.schema.json",
			example:    "examples/agent-review-result-manifest.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentReviewResultManifest(data)
				return err
			},
			mutate: func(object map[string]any) error {
				return mutateNestedString(
					object,
					"review_input_ref",
					"contract",
					"argus.unknown_input.v1alpha1",
				)
			},
		},
		{
			name:       "worker request open protocol",
			schemaPath: "api/schema/v1alpha1/agent-review-worker-request.schema.json",
			example:    "examples/agent-review-worker-request.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentReviewWorkerRequest(data)
				return err
			},
			mutate: func(object map[string]any) error {
				return mutateNestedString(object, "capability", "protocol", "shell_v0")
			},
		},
		{
			name:       "failed worker result with report",
			schemaPath: "api/schema/v1alpha1/agent-review-worker-result.schema.json",
			example:    "examples/agent-review-worker-result.json",
			decode: func(data []byte) error {
				_, err := contractsv1alpha1.DecodeAgentReviewWorkerResult(data)
				return err
			},
			mutate: func(object map[string]any) error {
				object["status"] = "failed"
				object["failure"] = map[string]any{
					"code": "provider_error", "message": "provider failed", "retryable": true,
				}
				return nil
			},
		},
	}
	for _, test := range tests {
		data, err := os.ReadFile(test.example)
		if err != nil {
			return err
		}
		var object map[string]any
		if err := json.Unmarshal(data, &object); err != nil {
			return err
		}
		if err := test.mutate(object); err != nil {
			return fmt.Errorf("mutate %s: %w", test.name, err)
		}
		mutated, err := json.Marshal(object)
		if err != nil {
			return err
		}
		if err := test.decode(mutated); err == nil {
			return fmt.Errorf("Go validator accepted %s", test.name)
		}
		schema, err := compiler.Compile(test.schemaPath)
		if err != nil {
			return err
		}
		if !test.goOnly {
			if err := validateJSONSchema(schema, mutated); err == nil {
				return fmt.Errorf("JSON Schema accepted %s", test.name)
			}
		}
	}
	return nil
}

func mutateNestedString(
	object map[string]any,
	parent string,
	field string,
	value string,
) error {
	nested, ok := object[parent].(map[string]any)
	if !ok {
		return fmt.Errorf("%s example is not an object", parent)
	}
	nested[field] = value
	return nil
}

func checkConfigContracts(compiler *jsonschema.Compiler) error {
	contracts := []struct {
		name       string
		title      string
		schemaPath string
		example    string
		decode     func([]byte) error
		invalid    []struct {
			path            string
			goErrorContains string
		}
	}{
		{
			name:       "ConfigRevision",
			title:      "Argus ConfigRevision v1alpha1",
			schemaPath: "api/schema/v1alpha1/config-revision.schema.json",
			example:    "examples/config-revision.json",
			decode: func(data []byte) error {
				_, err := reviewconfig.DecodeRevision(data)
				return err
			},
			invalid: []struct {
				path            string
				goErrorContains string
			}{
				{
					path: "api/schema/v1alpha1/testdata/" +
						"config-revision.invalid.unknown-field.json",
					goErrorContains: "unknown field",
				},
				{
					path: "api/schema/v1alpha1/testdata/" +
						"config-revision.invalid.selector-scope.json",
					goErrorContains: "must not contain repository_id",
				},
			},
		},
		{
			name:       "FindingGovernanceConfigRevision",
			title:      "Argus ConfigRevision v1alpha1",
			schemaPath: "api/schema/v1alpha1/config-revision.schema.json",
			example:    "examples/config-revision.finding-governance.json",
			decode: func(data []byte) error {
				_, err := reviewconfig.DecodeRevision(data)
				return err
			},
		},
		{
			name:       "ConfigBundle",
			title:      "Argus ConfigBundle v1alpha1",
			schemaPath: "api/schema/v1alpha1/config-bundle.schema.json",
			example:    "examples/config-bundle.json",
			decode: func(data []byte) error {
				_, err := reviewconfig.DecodeBundle(data)
				return err
			},
			invalid: []struct {
				path            string
				goErrorContains string
			}{
				{
					path: "api/schema/v1alpha1/testdata/" +
						"config-bundle.invalid.unknown-field.json",
					goErrorContains: "unknown field",
				},
				{
					path: "api/schema/v1alpha1/testdata/" +
						"config-bundle.invalid.publication-policy.json",
					goErrorContains: "automatic publication",
				},
			},
		},
		{
			name:       "ConfigResolutionReceipt",
			title:      "Argus ConfigResolutionReceipt v1alpha1",
			schemaPath: "api/schema/v1alpha1/config-resolution-receipt.schema.json",
			example:    "examples/config-resolution-receipt.json",
			decode: func(data []byte) error {
				_, err := reviewconfig.DecodeConfigResolutionReceipt(data)
				return err
			},
		},
		{
			name:       "ReplayBudgetConfigResolutionReceipt",
			title:      "Argus ConfigResolutionReceipt v1alpha1",
			schemaPath: "api/schema/v1alpha1/config-resolution-receipt.schema.json",
			example:    "examples/config-resolution-receipt.replay-budget.json",
			decode: func(data []byte) error {
				_, err := reviewconfig.DecodeConfigResolutionReceipt(data)
				return err
			},
		},
	}
	for _, contract := range contracts {
		schema, err := compiler.Compile(contract.schemaPath)
		if err != nil {
			return fmt.Errorf("compile %s schema: %w", contract.name, err)
		}
		data, err := os.ReadFile(contract.example)
		if err != nil {
			return fmt.Errorf("read %s: %w", contract.example, err)
		}
		if err := contract.decode(data); err != nil {
			return fmt.Errorf("validate %s: %w", contract.example, err)
		}
		if err := validateJSONSchema(schema, data); err != nil {
			return fmt.Errorf("schema validate %s: %w", contract.example, err)
		}
		schemaData, err := os.ReadFile(contract.schemaPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", contract.schemaPath, err)
		}
		var identity map[string]any
		if err := json.Unmarshal(schemaData, &identity); err != nil {
			return fmt.Errorf("decode %s: %w", contract.schemaPath, err)
		}
		if identity["title"] != contract.title ||
			identity["additionalProperties"] != false {
			return fmt.Errorf("%s schema identity or strictness is invalid", contract.name)
		}
		for _, invalid := range contract.invalid {
			data, err := os.ReadFile(invalid.path)
			if err != nil {
				return fmt.Errorf("read %s: %w", invalid.path, err)
			}
			goErr := contract.decode(data)
			if goErr == nil {
				return fmt.Errorf(
					"Go validator accepted negative fixture %s",
					invalid.path,
				)
			}
			if !strings.Contains(goErr.Error(), invalid.goErrorContains) {
				return fmt.Errorf(
					"Go validator rejected %s for the wrong reason: %w",
					invalid.path,
					goErr,
				)
			}
			if err := validateJSONSchema(schema, data); err == nil {
				return fmt.Errorf(
					"JSON Schema accepted negative fixture %s",
					invalid.path,
				)
			}
		}
	}

	revisionData, err := os.ReadFile("examples/config-revision.json")
	if err != nil {
		return fmt.Errorf("read ConfigRevision example for resolution: %w", err)
	}
	revision, err := reviewconfig.DecodeRevision(revisionData)
	if err != nil {
		return fmt.Errorf("decode ConfigRevision example for resolution: %w", err)
	}
	bundleData, err := os.ReadFile("examples/config-bundle.json")
	if err != nil {
		return fmt.Errorf("read ConfigBundle example for resolution: %w", err)
	}
	bundle, err := reviewconfig.DecodeBundle(bundleData)
	if err != nil {
		return fmt.Errorf("decode ConfigBundle example for resolution: %w", err)
	}
	resolved, err := reviewconfig.Resolve(
		bundle.Context,
		[]reviewconfig.Revision{revision},
	)
	if err != nil {
		return fmt.Errorf("resolve normative ConfigBundle example: %w", err)
	}
	if !reflect.DeepEqual(bundle, resolved) {
		return fmt.Errorf(
			"ConfigBundle example is not the exact output of resolving ConfigRevision example",
		)
	}
	agentBundleData, err := os.ReadFile("examples/config-bundle.agent-review.json")
	if err != nil {
		return fmt.Errorf("read agent ConfigBundle example for receipt validation: %w", err)
	}
	agentBundle, err := reviewconfig.DecodeBundle(agentBundleData)
	if err != nil {
		return fmt.Errorf("decode agent ConfigBundle example for receipt validation: %w", err)
	}
	receiptData, err := os.ReadFile("examples/config-resolution-receipt.json")
	if err != nil {
		return fmt.Errorf("read ConfigResolutionReceipt example: %w", err)
	}
	receipt, err := reviewconfig.DecodeConfigResolutionReceipt(receiptData)
	if err != nil {
		return fmt.Errorf("decode ConfigResolutionReceipt example: %w", err)
	}
	if err := receipt.ValidateAgainst(agentBundle); err != nil {
		return fmt.Errorf("validate ConfigResolutionReceipt example against ConfigBundle: %w", err)
	}
	budgetReceiptData, err := os.ReadFile("examples/config-resolution-receipt.replay-budget.json")
	if err != nil {
		return fmt.Errorf("read replay budget ConfigResolutionReceipt example: %w", err)
	}
	budgetReceipt, err := reviewconfig.DecodeConfigResolutionReceipt(budgetReceiptData)
	if err != nil {
		return fmt.Errorf("decode replay budget ConfigResolutionReceipt example: %w", err)
	}
	if budgetReceipt.Origin != reviewconfig.ConfigResolutionOriginReplayVariant ||
		budgetReceipt.ReplayVariant == nil ||
		budgetReceipt.ReplayVariant.SourceReceiptID != receipt.ReceiptID ||
		budgetReceipt.ReplayVariant.SourceReceiptSHA256 != receipt.SHA256 ||
		budgetReceipt.ReplayVariant.BaselineBundleID != agentBundle.BundleID ||
		budgetReceipt.ReplayVariant.BaselineSHA256 != agentBundle.SHA256 {
		return fmt.Errorf("replay budget ConfigResolutionReceipt does not bind normative baseline")
	}
	return nil
}

func validateJSONSchema(schema *jsonschema.Schema, data []byte) error {
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("decode JSON instance: %w", err)
	}
	return schema.Validate(instance)
}

func checkMarkdown() error {
	return checkMarkdownRoot(".")
}

func checkMarkdownRoot(root string) error {
	return filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "build", "coverage", "dist", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(name) != ".md" {
			return nil
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		content := string(data)
		if strings.Contains(content, "/Users/") {
			return fmt.Errorf("%s contains a machine-specific absolute path", name)
		}
		for _, match := range markdownLink.FindAllStringSubmatch(content, -1) {
			target := strings.Trim(match[1], "<>")
			if target == "" || strings.HasPrefix(target, "#") ||
				strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "http://") ||
				strings.HasPrefix(target, "mailto:") {
				continue
			}
			if index := strings.IndexByte(target, '#'); index >= 0 {
				target = target[:index]
			}
			if target == "" {
				continue
			}
			if filepath.IsAbs(target) {
				return fmt.Errorf("%s contains absolute local link %q", name, target)
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(name), target))
			if _, err := os.Stat(resolved); err != nil {
				return fmt.Errorf("%s contains broken link %q: %w", name, match[1], err)
			}
		}
		return nil
	})
}
