package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"

	"github.com/abietic/argus/internal/configrepo"
	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/normalizationeval"
	"github.com/abietic/argus/internal/normalizationpromotion"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/store/local"
)

const evaluationNormalizationUsage = `usage:
  argus evaluation normalization oracle seal --store <absolute-dir> --input <absolute-json> --access <absolute-json> --json
  argus evaluation normalization oracle register --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> --json
  argus evaluation normalization oracle revoke --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> --json
  argus evaluation normalization oracle show --store <absolute-dir> --oracle <id> --access <absolute-json> --json
  argus evaluation normalization oracle list --store <absolute-dir> --access <absolute-json> --json
  argus evaluation normalization run --store <absolute-dir> --input <absolute-json> --access <absolute-json> --json
  argus evaluation normalization show --store <absolute-dir> --ref <absolute-json> --access <absolute-json> --json
  argus evaluation normalization promotion prepare --store <absolute-dir> --config-state-dir <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation normalization promotion gate --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation normalization promotion operational-gate --store <absolute-dir> --config-state-dir <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation normalization promotion canary --store <absolute-dir> --config-state-dir <absolute-dir> --variant <id> --policy-ref <absolute-json> --percentage <1-99> --seed <seed> --mutation <absolute-json> [--json]
  argus evaluation normalization promotion activate --store <absolute-dir> --config-state-dir <absolute-dir> --variant <id> --mutation <absolute-json> [--json]
  argus evaluation normalization promotion rollback --store <absolute-dir> --config-state-dir <absolute-dir> --variant <id> --mutation <absolute-json> [--json]
  argus evaluation normalization promotion show --store <absolute-dir> --variant <id> --access <absolute-json> [--json]

Oracle seal validates an independently adjudicated equivalence partition against
one governed CorpusSnapshot case and a committed succeeded formal ReviewRun.
Run evaluates only the currently implemented deterministic normalization policy;
it performs no provider calls and never creates Finding, Decision, or gold labels.`

type normalizationControlFlags struct {
	store  string
	input  string
	ref    string
	access string
	json   bool
}

type normalizationOracleOutput struct {
	Oracle    evaluation.NormalizationOracle `json:"oracle"`
	OracleRef runmodel.ArtifactRef           `json:"oracle_ref"`
	StorePath string                         `json:"store_path"`
}

type normalizationQualityOutput struct {
	Run       evaluation.NormalizationQualityRun `json:"run"`
	RunRef    runmodel.ArtifactRef               `json:"run_ref"`
	StorePath string                             `json:"store_path"`
}

type normalizationOracleRecordOutput struct {
	Record    evaluation.NormalizationOracleRecord `json:"record"`
	StorePath string                               `json:"store_path"`
}

type normalizationOracleRecordListOutput struct {
	Records   []evaluation.NormalizationOracleRecord `json:"records"`
	StorePath string                                 `json:"store_path"`
}

func runEvaluationNormalization(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(evaluationNormalizationUsage)
	}
	if arguments[0] == "oracle" {
		if len(arguments) < 2 {
			return fmt.Errorf("unknown evaluation normalization oracle command\n%s", evaluationNormalizationUsage)
		}
		switch arguments[1] {
		case "seal":
			options, err := parseNormalizationControlFlags("evaluation normalization oracle seal", arguments[2:], true, false)
			if err != nil {
				return err
			}
			return sealNormalizationOracle(ctx, options, stdout)
		case "register":
			return registerNormalizationOracle(ctx, arguments[2:], stdout)
		case "revoke":
			return revokeNormalizationOracle(ctx, arguments[2:], stdout)
		case "show":
			return showNormalizationOracle(ctx, arguments[2:], stdout)
		case "list":
			return listNormalizationOracles(ctx, arguments[2:], stdout)
		default:
			return fmt.Errorf("unknown evaluation normalization oracle command %q\n%s", arguments[1], evaluationNormalizationUsage)
		}
	}
	if arguments[0] == "promotion" {
		return runNormalizationPromotion(ctx, arguments[1:], stdout)
	}
	switch arguments[0] {
	case "run":
		options, err := parseNormalizationControlFlags("evaluation normalization run", arguments[1:], true, false)
		if err != nil {
			return err
		}
		return executeNormalizationQualityRun(ctx, options, stdout)
	case "show":
		options, err := parseNormalizationControlFlags("evaluation normalization show", arguments[1:], false, true)
		if err != nil {
			return err
		}
		return showNormalizationQualityRun(ctx, options, stdout)
	default:
		return fmt.Errorf("unknown evaluation normalization command %q\n%s", arguments[0], evaluationNormalizationUsage)
	}
}

type normalizationPromotionPreparationOutput struct {
	Preparation normalizationpromotion.Preparation `json:"preparation"`
	StorePath   string                             `json:"store_path"`
}

type normalizationPromotionGateOutput struct {
	Gate      normalizationpromotion.GateOutput `json:"gate"`
	StorePath string                            `json:"store_path"`
}

type normalizationPromotionOperationalGateOutput struct {
	Gate            normalizationpromotion.OperationalGateOutput `json:"gate"`
	StorePath       string                                       `json:"store_path"`
	ConfigStatePath string                                       `json:"config_state_path"`
}

type normalizationPromotionLifecycleOutput struct {
	Lifecycle       normalizationpromotion.LifecycleOutput `json:"lifecycle"`
	StorePath       string                                 `json:"store_path"`
	ConfigStatePath string                                 `json:"config_state_path"`
}

type normalizationPromotionPrepareFlags struct {
	governedWriteFlags
	configState string
}

type normalizationPromotionLifecycleFlags struct {
	store, configState, input, mutation, variant, policyRef, seed string
	percentage                                                    int
	json                                                          bool
}

func parseNormalizationPromotionLifecycleFlags(command string, arguments []string) (normalizationPromotionLifecycleFlags, error) {
	var options normalizationPromotionLifecycleFlags
	flags := newFlagSet("evaluation normalization promotion " + command)
	flags.StringVar(&options.store, "store", "", "explicit local state root")
	flags.StringVar(&options.configState, "config-state-dir", "", "absolute config lifecycle state")
	flags.StringVar(&options.input, "input", "", "absolute operational gate request JSON")
	flags.StringVar(&options.mutation, "mutation", "", "absolute strict JSON mutation descriptor")
	flags.StringVar(&options.variant, "variant", "", "normalization promotion variant ID")
	flags.StringVar(&options.policyRef, "policy-ref", "", "absolute normalization policy artifact ref JSON")
	flags.StringVar(&options.seed, "seed", "", "stable rollout assignment seed")
	flags.IntVar(&options.percentage, "percentage", 0, "canary rollout percentage")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return options, controlFlagError("evaluation normalization promotion "+command, err, evaluationNormalizationUsage)
	}
	if flags.NArg() != 0 {
		return options, fmt.Errorf("evaluation normalization promotion %s accepts flags only", command)
	}
	if _, err := validateExplicitControlStore(options.store); err != nil {
		return options, err
	}
	if _, err := validateExplicitControlStore(options.configState); err != nil {
		return options, fmt.Errorf("config-state-dir: %w", err)
	}
	if err := validateDescriptorPath("mutation", options.mutation); err != nil {
		return options, err
	}
	if command == "operational-gate" {
		if err := validateDescriptorPath("input", options.input); err != nil {
			return options, err
		}
	} else if options.variant == "" {
		return options, fmt.Errorf("--variant is required")
	}
	if command == "canary" {
		if err := validateDescriptorPath("policy-ref", options.policyRef); err != nil {
			return options, err
		}
		if options.percentage < 1 || options.percentage >= 100 || options.seed == "" {
			return options, fmt.Errorf("canary requires --percentage between 1 and 99 and non-empty --seed")
		}
	}
	return options, nil
}

func parseNormalizationPromotionPrepareFlags(arguments []string) (normalizationPromotionPrepareFlags, error) {
	var options normalizationPromotionPrepareFlags
	flags := newFlagSet("evaluation normalization promotion prepare")
	flags.StringVar(&options.store, "store", "", "explicit local state root")
	flags.StringVar(&options.configState, "config-state-dir", "", "absolute config lifecycle state")
	flags.StringVar(&options.input, "input", "", "absolute strict JSON domain descriptor")
	flags.StringVar(&options.mutation, "mutation", "", "absolute strict JSON mutation descriptor")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return normalizationPromotionPrepareFlags{}, controlFlagError(
			"evaluation normalization promotion prepare", err, evaluationNormalizationUsage,
		)
	}
	if flags.NArg() != 0 {
		return normalizationPromotionPrepareFlags{}, controlFlagError(
			"evaluation normalization promotion prepare",
			fmt.Errorf("unexpected argument %q", flags.Arg(0)),
			evaluationNormalizationUsage,
		)
	}
	if _, err := validateExplicitControlStore(options.store); err != nil {
		return normalizationPromotionPrepareFlags{}, err
	}
	if _, err := validateExplicitControlStore(options.configState); err != nil {
		return normalizationPromotionPrepareFlags{}, fmt.Errorf("config-state-dir: %w", err)
	}
	if err := validateDescriptorPath("input", options.input); err != nil {
		return normalizationPromotionPrepareFlags{}, err
	}
	if err := validateDescriptorPath("mutation", options.mutation); err != nil {
		return normalizationPromotionPrepareFlags{}, err
	}
	return options, nil
}

func runNormalizationPromotion(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("unknown evaluation normalization promotion command\n%s", evaluationNormalizationUsage)
	}
	switch arguments[0] {
	case "prepare":
		options, err := parseNormalizationPromotionPrepareFlags(arguments[1:])
		if err != nil {
			return err
		}
		request, err := readStrictDescriptor(
			options.input, "normalization promotion prepare request", normalizationpromotion.DecodePrepareRequest,
		)
		if err != nil {
			return err
		}
		mutation, err := readEvaluationMutation(options.mutation)
		if err != nil {
			return err
		}
		repository, store, runs, err := openNormalizationEvaluation(options.store)
		if err != nil {
			return err
		}
		configRepository, _, err := openConfigRepository(options.configState)
		if err != nil {
			return err
		}
		service, err := normalizationpromotion.New(repository, runs, configRepository)
		if err != nil {
			return err
		}
		preparation, err := service.Prepare(ctx, request, mutation)
		if err != nil {
			return err
		}
		if err := preparation.Validate(); err != nil {
			return fmt.Errorf("validate normalization promotion preparation: %w", err)
		}
		if options.json {
			return writeJSON(stdout, normalizationPromotionPreparationOutput{Preparation: preparation, StorePath: store.Root()})
		}
		_, err = fmt.Fprintf(stdout, "normalization_promotion=%s status=%s next_gate=%s store=%s\n",
			preparation.Promotion.Variant.VariantID, preparation.Promotion.Status,
			promotionNextGate(preparation.Promotion), store.Root())
		return err
	case "gate":
		options, err := parseGovernedWriteFlags(
			"evaluation normalization promotion gate", arguments[1:], evaluationNormalizationUsage,
		)
		if err != nil {
			return err
		}
		request, err := readStrictDescriptor(
			options.input, "normalization promotion gate request", normalizationpromotion.DecodeGateRequest,
		)
		if err != nil {
			return err
		}
		mutation, err := readEvaluationMutation(options.mutation)
		if err != nil {
			return err
		}
		repository, store, runs, err := openNormalizationEvaluation(options.store)
		if err != nil {
			return err
		}
		service, err := normalizationpromotion.New(repository, runs)
		if err != nil {
			return err
		}
		output, err := service.RecordQualityGate(ctx, request, mutation)
		if err != nil {
			return err
		}
		if err := output.Validate(); err != nil {
			return fmt.Errorf("validate normalization promotion gate output: %w", err)
		}
		if options.json {
			return writeJSON(stdout, normalizationPromotionGateOutput{Gate: output, StorePath: store.Root()})
		}
		_, err = fmt.Fprintf(stdout, "normalization_promotion=%s gate=%s outcome=%s status=%s next_gate=%s store=%s\n",
			request.VariantID, request.Gate, output.Decision.Outcome, output.Promotion.Status,
			promotionNextGate(output.Promotion), store.Root())
		return err
	case "operational-gate", "canary", "activate", "rollback":
		return runNormalizationPromotionLifecycle(ctx, arguments[0], arguments[1:], stdout)
	case "show":
		options, err := parseGovernedReadFlags(
			"evaluation normalization promotion show", arguments[1:], evaluationNormalizationUsage,
			"variant", "normalization promotion variant ID",
		)
		if err != nil {
			return err
		}
		access, err := readEvaluationAccess(options.access)
		if err != nil {
			return err
		}
		repository, storePath, err := openEvaluationRepository(options.store)
		if err != nil {
			return err
		}
		record, err := repository.GetPromotion(options.id, access)
		if err != nil {
			return err
		}
		if record.Variant.ManagedBinding == nil ||
			record.Variant.ManagedBinding.Kind != evaluation.PromotionManagementNormalizationPolicy {
			return fmt.Errorf("promotion %q is not a managed normalization policy promotion", options.id)
		}
		return writePromotionOutput(stdout, record, storePath, options.json)
	default:
		return fmt.Errorf("unknown evaluation normalization promotion command %q\n%s", arguments[0], evaluationNormalizationUsage)
	}
}

func runNormalizationPromotionLifecycle(ctx context.Context, command string, arguments []string, stdout io.Writer) error {
	options, err := parseNormalizationPromotionLifecycleFlags(command, arguments)
	if err != nil {
		return err
	}
	mutation, err := readEvaluationMutation(options.mutation)
	if err != nil {
		return err
	}
	repository, store, runs, err := openNormalizationEvaluation(options.store)
	if err != nil {
		return err
	}
	configs, configPath, err := openConfigRepository(options.configState)
	if err != nil {
		return err
	}
	service, err := normalizationpromotion.New(repository, runs, configs)
	if err != nil {
		return err
	}
	if command == "operational-gate" {
		request, readErr := readStrictDescriptor(options.input, "normalization operational gate request", normalizationpromotion.DecodeOperationalGateRequest)
		if readErr != nil {
			return readErr
		}
		output, recordErr := service.RecordOperationalGate(ctx, request, mutation)
		if recordErr != nil {
			return recordErr
		}
		if options.json {
			return writeJSON(stdout, normalizationPromotionOperationalGateOutput{Gate: output, StorePath: store.Root(), ConfigStatePath: configPath})
		}
		_, err = fmt.Fprintf(stdout, "normalization_promotion=%s gate=%s outcome=%s status=%s next_gate=%s\n", request.VariantID, request.Gate, output.Result.Outcome, output.Promotion.Status, promotionNextGate(output.Promotion))
		return err
	}
	var output normalizationpromotion.LifecycleOutput
	switch command {
	case "canary":
		policyRef, readErr := readStrictDescriptor(options.policyRef, "normalization policy artifact ref", func(data []byte) (runmodel.ArtifactRef, error) {
			return decodeStrictCommandJSON(data, "NormalizationPromotionPolicyRef", func(value runmodel.ArtifactRef) error {
				if err := value.Validate(); err != nil || value.Contract != normalizationpromotion.PolicyContract {
					return fmt.Errorf("normalization policy artifact ref is invalid")
				}
				return nil
			})
		})
		if readErr != nil {
			return readErr
		}
		output, err = service.StartCanary(ctx, options.variant, policyRef, configrepo.Rollout{Percentage: options.percentage, Seed: options.seed}, mutation)
	case "activate":
		output, err = service.Activate(ctx, options.variant, mutation)
	case "rollback":
		output, err = service.Rollback(ctx, options.variant, mutation)
	}
	if err != nil {
		return err
	}
	if options.json {
		return writeJSON(stdout, normalizationPromotionLifecycleOutput{Lifecycle: output, StorePath: store.Root(), ConfigStatePath: configPath})
	}
	_, err = fmt.Fprintf(stdout, "normalization_promotion=%s status=%s config=%s@%s config_status=%s\n", options.variant, output.Promotion.Status, output.Config.Revision.ID, output.Config.Revision.Revision, output.Config.Status)
	return err
}

func promotionNextGate(record evaluation.PromotionRecord) string {
	if record.NextGate == nil {
		return "none"
	}
	return string(*record.NextGate)
}

func parseNormalizationControlFlags(
	name string,
	arguments []string,
	wantInput bool,
	wantRef bool,
) (normalizationControlFlags, error) {
	var options normalizationControlFlags
	flags := newFlagSet(name)
	flags.StringVar(&options.store, "store", "", "explicit local state root")
	if wantInput {
		flags.StringVar(&options.input, "input", "", "absolute strict JSON descriptor")
	}
	if wantRef {
		flags.StringVar(&options.ref, "ref", "", "absolute strict JSON ArtifactRef")
	}
	flags.StringVar(&options.access, "access", "", "absolute strict JSON evaluation access descriptor")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return normalizationControlFlags{}, controlFlagError(name, err, evaluationNormalizationUsage)
	}
	if flags.NArg() != 0 {
		return normalizationControlFlags{}, controlFlagError(name, fmt.Errorf("unexpected argument %q", flags.Arg(0)), evaluationNormalizationUsage)
	}
	store, err := validateExplicitControlStore(options.store)
	if err != nil {
		return normalizationControlFlags{}, err
	}
	options.store = store
	if err := validateDescriptorPath("access", options.access); err != nil {
		return normalizationControlFlags{}, err
	}
	if wantInput {
		if err := validateDescriptorPath("input", options.input); err != nil {
			return normalizationControlFlags{}, err
		}
	}
	if wantRef {
		if err := validateDescriptorPath("ref", options.ref); err != nil {
			return normalizationControlFlags{}, err
		}
	}
	if !options.json {
		return normalizationControlFlags{}, fmt.Errorf("%s requires --json", name)
	}
	return options, nil
}

func sealNormalizationOracle(ctx context.Context, options normalizationControlFlags, stdout io.Writer) error {
	oracle, err := readStrictDescriptor(options.input, "normalization oracle", evaluation.DecodeNormalizationOracle)
	if err != nil {
		return err
	}
	access, err := readEvaluationAccess(options.access)
	if err != nil {
		return err
	}
	repository, store, runs, err := openNormalizationEvaluation(options.store)
	if err != nil {
		return err
	}
	service, err := normalizationeval.New(repository, runs)
	if err != nil {
		return err
	}
	if err := service.ValidateOracleClosure(ctx, oracle, access); err != nil {
		return err
	}
	ref, err := runs.PutJSONArtifact(evaluation.NormalizationOracleContract, oracle)
	if err != nil {
		return fmt.Errorf("persist normalization oracle: %w", err)
	}
	return writeJSON(stdout, normalizationOracleOutput{Oracle: oracle, OracleRef: ref, StorePath: store.Root()})
}

func registerNormalizationOracle(ctx context.Context, arguments []string, stdout io.Writer) error {
	options, err := parseGovernedWriteFlags(
		"evaluation normalization oracle register", arguments, evaluationNormalizationUsage,
	)
	if err != nil {
		return err
	}
	registration, err := readStrictDescriptor(
		options.input, "normalization oracle registration", evaluation.DecodeNormalizationOracleRegistration,
	)
	if err != nil {
		return err
	}
	mutation, err := readEvaluationMutation(options.mutation)
	if err != nil {
		return err
	}
	repository, store, runs, err := openNormalizationEvaluation(options.store)
	if err != nil {
		return err
	}
	data, err := runs.ReadArtifact(registration.OracleRef)
	if err != nil {
		return fmt.Errorf("read normalization oracle artifact: %w", err)
	}
	stored, err := evaluation.DecodeNormalizationOracle(data)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(stored, registration.Oracle) {
		return fmt.Errorf("normalization oracle registration payload differs from exact stored artifact")
	}
	service, err := normalizationeval.New(repository, runs)
	if err != nil {
		return err
	}
	access := evaluation.Access{Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles)}
	if err := service.ValidateOracleClosure(ctx, registration.Oracle, access); err != nil {
		return err
	}
	record, err := repository.RegisterNormalizationOracle(ctx, registration, mutation)
	if err != nil {
		return err
	}
	if options.json {
		return writeJSON(stdout, normalizationOracleRecordOutput{Record: record, StorePath: store.Root()})
	}
	_, err = fmt.Fprintf(stdout, "normalization_oracle=%s revision=%d state=active store=%s\n", record.Binding.OracleID, record.Binding.Revision, store.Root())
	return err
}

func revokeNormalizationOracle(ctx context.Context, arguments []string, stdout io.Writer) error {
	options, err := parseGovernedWriteFlags(
		"evaluation normalization oracle revoke", arguments, evaluationNormalizationUsage,
	)
	if err != nil {
		return err
	}
	revocation, err := readStrictDescriptor(
		options.input, "normalization oracle revocation", evaluation.DecodeNormalizationOracleRevocation,
	)
	if err != nil {
		return err
	}
	mutation, err := readEvaluationMutation(options.mutation)
	if err != nil {
		return err
	}
	repository, storePath, err := openEvaluationRepository(options.store)
	if err != nil {
		return err
	}
	record, err := repository.RevokeNormalizationOracle(ctx, revocation, mutation)
	if err != nil {
		return err
	}
	if options.json {
		return writeJSON(stdout, normalizationOracleRecordOutput{Record: record, StorePath: storePath})
	}
	_, err = fmt.Fprintf(stdout, "normalization_oracle=%s revision=%d state=revoked store=%s\n", record.Binding.OracleID, record.Binding.Revision, storePath)
	return err
}

func showNormalizationOracle(ctx context.Context, arguments []string, stdout io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	options, err := parseGovernedReadFlags(
		"evaluation normalization oracle show", arguments, evaluationNormalizationUsage,
		"oracle", "normalization oracle ID",
	)
	if err != nil {
		return err
	}
	access, err := readEvaluationAccess(options.access)
	if err != nil {
		return err
	}
	repository, storePath, err := openEvaluationRepository(options.store)
	if err != nil {
		return err
	}
	record, err := repository.GetNormalizationOracle(options.id, access)
	if err != nil {
		return err
	}
	if options.json {
		return writeJSON(stdout, normalizationOracleRecordOutput{Record: record, StorePath: storePath})
	}
	state := "active"
	if record.RevokedAt != nil {
		state = "revoked"
	}
	_, err = fmt.Fprintf(stdout, "normalization_oracle=%s revision=%d state=%s store=%s\n", record.Binding.OracleID, record.Binding.Revision, state, storePath)
	return err
}

func listNormalizationOracles(ctx context.Context, arguments []string, stdout io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	options, err := parseGovernedReadFlags(
		"evaluation normalization oracle list", arguments, evaluationNormalizationUsage, "", "",
	)
	if err != nil {
		return err
	}
	access, err := readEvaluationAccess(options.access)
	if err != nil {
		return err
	}
	repository, storePath, err := openEvaluationRepository(options.store)
	if err != nil {
		return err
	}
	records, err := repository.ListNormalizationOracles(access)
	if err != nil {
		return err
	}
	if options.json {
		return writeJSON(stdout, normalizationOracleRecordListOutput{Records: records, StorePath: storePath})
	}
	_, err = fmt.Fprintf(stdout, "normalization_oracles=%d store=%s\n", len(records), storePath)
	return err
}

func executeNormalizationQualityRun(ctx context.Context, options normalizationControlFlags, stdout io.Writer) error {
	request, err := readStrictDescriptor(options.input, "normalization quality run request", evaluation.DecodeNormalizationQualityRunRequest)
	if err != nil {
		return err
	}
	access, err := readEvaluationAccess(options.access)
	if err != nil {
		return err
	}
	repository, store, runs, err := openNormalizationEvaluation(options.store)
	if err != nil {
		return err
	}
	service, err := normalizationeval.New(repository, runs)
	if err != nil {
		return err
	}
	quality, err := service.BuildQuality(ctx, request, access)
	if err != nil {
		return err
	}
	ref, err := runs.PutJSONArtifact(evaluation.NormalizationQualityRunContract, quality)
	if err != nil {
		return fmt.Errorf("persist normalization quality run: %w", err)
	}
	return writeJSON(stdout, normalizationQualityOutput{Run: quality, RunRef: ref, StorePath: store.Root()})
}

func showNormalizationQualityRun(ctx context.Context, options normalizationControlFlags, stdout io.Writer) error {
	ref, err := readStrictDescriptor(options.ref, "normalization quality run ref", func(data []byte) (runmodel.ArtifactRef, error) {
		return decodeStrictCommandJSON(data, "NormalizationQualityRunRef", func(value runmodel.ArtifactRef) error {
			if err := value.Validate(); err != nil {
				return err
			}
			if value.Contract != evaluation.NormalizationQualityRunContract {
				return fmt.Errorf("normalization quality ref has contract %q", value.Contract)
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	access, err := readEvaluationAccess(options.access)
	if err != nil {
		return err
	}
	repository, store, runs, err := openNormalizationEvaluation(options.store)
	if err != nil {
		return err
	}
	service, err := normalizationeval.New(repository, runs)
	if err != nil {
		return err
	}
	quality, err := service.VerifyQuality(ctx, ref, access)
	if err != nil {
		return err
	}
	return writeJSON(stdout, normalizationQualityOutput{Run: quality, RunRef: ref, StorePath: store.Root()})
}

func openNormalizationEvaluation(storePath string) (*evaluation.Repository, *local.Store, *runrepo.Repository, error) {
	store, err := local.Open(storePath)
	if err != nil {
		return nil, nil, nil, err
	}
	repository, err := evaluation.New(store)
	if err != nil {
		return nil, nil, nil, err
	}
	runs, err := runrepo.New(store)
	if err != nil {
		return nil, nil, nil, err
	}
	return repository, store, runs, nil
}
