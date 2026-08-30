package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"argus.local/argus/internal/controlplane"
	"argus.local/argus/internal/evaluation"
	feedbackdomain "argus.local/argus/internal/feedback"
	"argus.local/argus/internal/findingdecision"
	"argus.local/argus/internal/findinglineage"
	"argus.local/argus/internal/publication"
	"argus.local/argus/internal/publication/githubadapter"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const maxControlPlaneDescriptorBytes = int64(4 << 20)

const candidateUsage = `usage:
  argus candidate list --store <absolute-dir> --run <id> [--json]
  argus candidate show --store <absolute-dir> --run <id> --candidate <id> [--json]`

const findingUsage = `usage:
  argus finding show --store <absolute-dir> --run <id> --finding <id> [--json]`

const decisionUsage = `usage:
  argus decision record --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]`

const publicationUsage = `usage:
  argus publication grant record --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus publication request build --store <absolute-dir> --input <absolute-json> [--json]
  argus publication github dispatch --store <absolute-dir> --input <absolute-json> --single-user-local [--json]
  argus publication github reconcile --store <absolute-dir> --publication <id> --single-user-local [--json]`

const feedbackUsage = `usage:
  argus feedback record --store <absolute-dir> --input <absolute-json> [--json]`

const outcomeUsage = `usage:
  argus outcome record --store <absolute-dir> --input <absolute-json> [--json]`

const evaluationUsage = `usage:
  argus evaluation case create --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation case import --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation case derive --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation case list --store <absolute-dir> --access <absolute-json> [--json]
  argus evaluation case show --store <absolute-dir> --case <id> --access <absolute-json> [--json]
  argus evaluation case correct-label --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation case assign --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation case annotate --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation case adjudicate --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation case activate --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation case reopen --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation trust-key register --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation trust-key revoke --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation trust-key list --store <absolute-dir> --access <absolute-json> [--json]
  argus evaluation governance-batch run --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation governance-batch list --store <absolute-dir> --access <absolute-json> [--json]
  argus evaluation governance-batch show --store <absolute-dir> --batch <id> --access <absolute-json> [--json]
  argus evaluation incident record --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation incident list --store <absolute-dir> --access <absolute-json> [--json]
  argus evaluation incident show --store <absolute-dir> --incident <id> --access <absolute-json> [--json]
  argus evaluation probe record --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation probe list --store <absolute-dir> --access <absolute-json> [--json]
  argus evaluation probe show --store <absolute-dir> --probe <id> --access <absolute-json> [--json]
  argus evaluation exposure record --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation run record --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation run list --store <absolute-dir> --access <absolute-json> [--json]
  argus evaluation run show --store <absolute-dir> --run <id> --access <absolute-json> [--json]
  argus evaluation experiment record --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation experiment show --store <absolute-dir> --experiment <id> --access <absolute-json> [--json]
  argus evaluation repeatability record --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation repeatability list --store <absolute-dir> --access <absolute-json> [--json]
  argus evaluation repeatability show --store <absolute-dir> --repeatability <id> --access <absolute-json> [--json]
  argus evaluation normalization oracle seal --store <absolute-dir> --input <absolute-json> --access <absolute-json> --json
  argus evaluation normalization oracle register --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> --json
  argus evaluation normalization oracle revoke --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> --json
  argus evaluation normalization oracle show --store <absolute-dir> --oracle <id> --access <absolute-json> --json
  argus evaluation normalization oracle list --store <absolute-dir> --access <absolute-json> --json
  argus evaluation normalization run --store <absolute-dir> --input <absolute-json> --access <absolute-json> --json
  argus evaluation normalization show --store <absolute-dir> --ref <absolute-json> --access <absolute-json> --json
  argus evaluation normalization promotion prepare --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation normalization promotion gate --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus evaluation normalization promotion show --store <absolute-dir> --variant <id> --access <absolute-json> [--json]
  argus evaluation repeatability batch run --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json] -- <formal exact replay flags>
  argus evaluation repeatability batch resume --store <absolute-dir> --batch <id> --access <absolute-json> [--json]
  argus evaluation repeatability batch show --store <absolute-dir> --batch <id> --access <absolute-json> [--json]
  argus evaluation corpus run --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json] -- <formal run flags>
  argus evaluation corpus resume --store <absolute-dir> --batch <id> --access <absolute-json> [--json]
  argus evaluation corpus show --store <absolute-dir> --batch <id> --access <absolute-json> [--json]
  argus evaluation batch run --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json] -- <formal replay variant flags>
  argus evaluation batch resume --store <absolute-dir> --batch <id> --access <absolute-json> [--json]
  argus evaluation batch show --store <absolute-dir> --batch <id> --access <absolute-json> [--json]`

const promotionUsage = `usage:
  argus promotion register --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus promotion gate --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus promotion show --store <absolute-dir> --variant <id> --access <absolute-json> [--json]
  argus promotion rollback --store <absolute-dir> --variant <id> --mutation <absolute-json> [--json]`

type findingShowFlags struct {
	store   string
	run     string
	finding string
	json    bool
}

type candidateFlags struct {
	store     string
	run       string
	candidate string
	json      bool
}

type candidateListOutput struct {
	RunID                string                                        `json:"run_id"`
	CandidateSetID       string                                        `json:"candidate_set_id"`
	VerificationLedgerID string                                        `json:"verification_ledger_id"`
	Candidates           []contractsv1alpha1.GovernedReviewCandidate   `json:"candidates"`
	VerificationFacts    []contractsv1alpha1.CandidateVerificationFact `json:"verification_facts"`
	StorePath            string                                        `json:"store_path"`
}

type candidateShowOutput struct {
	RunID          string                                       `json:"run_id"`
	CandidateSetID string                                       `json:"candidate_set_id"`
	Candidate      contractsv1alpha1.GovernedReviewCandidate    `json:"candidate"`
	Verification   *contractsv1alpha1.CandidateVerificationFact `json:"verification,omitempty"`
	StorePath      string                                       `json:"store_path"`
}

type factRecordFlags struct {
	store string
	input string
	json  bool
}

type governedWriteFlags struct {
	store    string
	input    string
	mutation string
	json     bool
}

type governedReadFlags struct {
	store  string
	access string
	id     string
	json   bool
}

type rollbackFlags struct {
	store    string
	variant  string
	mutation string
	json     bool
}

type findingShowOutput struct {
	Run               runmodel.ReviewRun                          `json:"run"`
	Finding           *reviewcore.Finding                         `json:"finding,omitempty"`
	Decisions         []reviewcore.FindingDecision                `json:"decisions"`
	GovernedFinding   *contractsv1alpha1.GovernedReviewFinding    `json:"governed_finding,omitempty"`
	GovernedDecisions []contractsv1alpha1.GovernedFindingDecision `json:"governed_decisions"`
	HumanDecisions    []findingdecision.Decision                  `json:"human_decisions"`
	Feedback          []feedbackdomain.Feedback                   `json:"feedback"`
	Outcomes          []feedbackdomain.Outcome                    `json:"outcomes"`
	StorePath         string                                      `json:"store_path"`
}

type decisionRecordOutput struct {
	Decision  findingdecision.Decision `json:"decision"`
	StorePath string                   `json:"store_path"`
}

type publicationRequestOutput struct {
	Request   publication.Request `json:"request"`
	StorePath string              `json:"store_path"`
}

type publicationDispatchOutput struct {
	Record    publication.Record `json:"record"`
	StorePath string             `json:"store_path"`
}

type githubPublicationFlags struct {
	store           string
	input           string
	publication     string
	singleUserLocal bool
	json            bool
}

var buildGitHubPublicationProvider = func() (publication.Provider, error) {
	return githubadapter.New(
		nil,
		githubadapter.NewEnvironmentCredentialSource(),
		githubadapter.SingleUserLocalPermissionBroker{},
		nil,
	)
}

type publicationGrantOutput struct {
	Grant     publication.Grant `json:"grant"`
	StorePath string            `json:"store_path"`
}

type feedbackRecordOutput struct {
	Feedback    feedbackdomain.Feedback                       `json:"feedback"`
	Eligibility feedbackdomain.EvaluationCandidateEligibility `json:"evaluation_candidate_eligibility"`
	StorePath   string                                        `json:"store_path"`
}

type outcomeRecordOutput struct {
	Outcome   feedbackdomain.Outcome `json:"outcome"`
	StorePath string                 `json:"store_path"`
}

type evaluationCaseOutput struct {
	Record    evaluation.CaseRecord `json:"record"`
	StorePath string                `json:"store_path"`
}

type evaluationCaseListOutput struct {
	Cases     []evaluation.CaseRecord `json:"cases"`
	StorePath string                  `json:"store_path"`
}

type evaluationCaseShowOutput struct {
	Record        evaluation.CaseRecord                  `json:"record"`
	LabelHistory  []evaluation.LabelEntry                `json:"label_history"`
	Annotations   []evaluation.CaseAnnotationEntry       `json:"annotations"`
	Adjudications []evaluation.CaseAdjudicationEntry     `json:"adjudications"`
	Assignments   []evaluation.CaseReviewAssignmentEntry `json:"assignments"`
	Agreement     evaluation.CaseReviewAgreement         `json:"agreement"`
	Exposures     []evaluation.ExposureEntry             `json:"exposures"`
	StorePath     string                                 `json:"store_path"`
}

type evaluationAnnotationOutput struct {
	Annotation evaluation.CaseAnnotationEntry `json:"annotation"`
	StorePath  string                         `json:"store_path"`
}

type evaluationAssignmentOutput struct {
	Assignment evaluation.CaseReviewAssignmentEntry `json:"assignment"`
	StorePath  string                               `json:"store_path"`
}

type evaluationTrustKeyOutput struct {
	TrustKey  evaluation.GovernanceTrustKeyRecord `json:"trust_key"`
	StorePath string                              `json:"store_path"`
}

type evaluationTrustKeyListOutput struct {
	TrustKeys []evaluation.GovernanceTrustKeyRecord `json:"trust_keys"`
	StorePath string                                `json:"store_path"`
}

type evaluationGovernanceBatchOutput struct {
	Batch     evaluation.GovernanceBatchRecord `json:"batch"`
	StorePath string                           `json:"store_path"`
}

type evaluationGovernanceBatchListOutput struct {
	Batches   []evaluation.GovernanceBatchRecord `json:"batches"`
	StorePath string                             `json:"store_path"`
}

type evaluationIncidentOutput struct {
	Incident  evaluation.MissedDefectIncidentEntry `json:"incident"`
	StorePath string                               `json:"store_path"`
}

type evaluationIncidentListOutput struct {
	Incidents []evaluation.MissedDefectIncidentEntry `json:"incidents"`
	StorePath string                                 `json:"store_path"`
}

type evaluationProbeOutput struct {
	Probe     evaluation.EvaluationProbeEntry `json:"probe"`
	StorePath string                          `json:"store_path"`
}

type evaluationProbeListOutput struct {
	Probes    []evaluation.EvaluationProbeEntry `json:"probes"`
	StorePath string                            `json:"store_path"`
}

type evaluationRunOutput struct {
	Run       evaluation.EvaluationRun `json:"run"`
	StorePath string                   `json:"store_path"`
}

type evaluationRunListOutput struct {
	Runs      []evaluation.EvaluationRun `json:"runs"`
	StorePath string                     `json:"store_path"`
}

type experimentRunOutput struct {
	Run       evaluation.ExperimentRun `json:"run"`
	StorePath string                   `json:"store_path"`
}

type repeatabilityRunOutput struct {
	Run       evaluation.RepeatabilityRun `json:"run"`
	StorePath string                      `json:"store_path"`
}

type repeatabilityRunListOutput struct {
	Runs      []evaluation.RepeatabilityRun `json:"runs"`
	StorePath string                        `json:"store_path"`
}

type evaluationExposureOutput struct {
	Entry     evaluation.ExposureEntry `json:"entry"`
	StorePath string                   `json:"store_path"`
}

type promotionOutput struct {
	Record    evaluation.PromotionRecord `json:"record"`
	StorePath string                     `json:"store_path"`
}

func runCandidate(arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(candidateUsage)
	}
	if isHelpArgument(arguments) {
		_, err := fmt.Fprintln(stdout, candidateUsage)
		return err
	}
	action := arguments[0]
	if action != "list" && action != "show" {
		return fmt.Errorf("unknown candidate command %q\n%s", action, candidateUsage)
	}
	var options candidateFlags
	flags := newFlagSet("candidate " + action)
	flags.StringVar(&options.store, "store", "", "explicit local state root")
	flags.StringVar(&options.run, "run", "", "formal review run ID")
	flags.StringVar(&options.candidate, "candidate", "", "candidate ID")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments[1:]); err != nil {
		return controlFlagError("candidate "+action, err, candidateUsage)
	}
	if flags.NArg() != 0 {
		return controlFlagError(
			"candidate "+action, fmt.Errorf("unexpected argument %q", flags.Arg(0)), candidateUsage,
		)
	}
	if options.store == "" || options.run == "" ||
		(action == "show" && options.candidate == "") ||
		(action == "list" && options.candidate != "") {
		return controlFlagError(
			"candidate "+action,
			fmt.Errorf("--store and --run are required; --candidate is required only for show"),
			candidateUsage,
		)
	}
	state, err := local.Open(options.store)
	if err != nil {
		return fmt.Errorf("open candidate store: %w", err)
	}
	runs, err := runrepo.New(state)
	if err != nil {
		return err
	}
	run, err := runs.LoadRun(options.run)
	if err != nil {
		return fmt.Errorf("load candidate run %q: %w", options.run, err)
	}
	if run.CandidateSetRef == nil || run.VerificationLedgerRef == nil {
		return fmt.Errorf("run %q has no authoritative candidate or verification ledger", options.run)
	}
	data, err := runs.ReadArtifact(*run.CandidateSetRef)
	if err != nil {
		return fmt.Errorf("read candidate set: %w", err)
	}
	set, err := contractsv1alpha1.DecodeGovernedCandidateSet(data)
	if err != nil {
		return fmt.Errorf("decode candidate set: %w", err)
	}
	if set.ReviewRunID != run.RunID {
		return fmt.Errorf("candidate set escaped committed run")
	}
	verificationData, err := runs.ReadArtifact(*run.VerificationLedgerRef)
	if err != nil {
		return fmt.Errorf("read candidate verification ledger: %w", err)
	}
	verification, err := contractsv1alpha1.DecodeCandidateVerificationLedger(verificationData)
	if err != nil {
		return fmt.Errorf("decode candidate verification ledger: %w", err)
	}
	if err := set.ValidateAgainstVerificationLedger(verification); err != nil {
		return fmt.Errorf("validate candidate verification projection: %w", err)
	}
	if action == "list" {
		output := candidateListOutput{
			RunID: run.RunID, CandidateSetID: set.CandidateSetID,
			VerificationLedgerID: verification.VerificationLedgerID,
			Candidates:           set.Candidates, VerificationFacts: verification.Facts,
			StorePath: state.Root(),
		}
		if options.json {
			return writeJSON(stdout, output)
		}
		_, err = fmt.Fprintf(
			stdout, "run=%s candidate_set=%s verification_ledger=%s candidates=%d verification_facts=%d store=%s\n",
			output.RunID, output.CandidateSetID, output.VerificationLedgerID,
			len(output.Candidates), len(output.VerificationFacts), output.StorePath,
		)
		return err
	}
	for _, candidate := range set.Candidates {
		if candidate.CandidateID != options.candidate {
			continue
		}
		output := candidateShowOutput{
			RunID: run.RunID, CandidateSetID: set.CandidateSetID,
			Candidate: candidate, StorePath: state.Root(),
		}
		if fact, found := verification.Latest(candidate.CandidateID); found {
			output.Verification = &fact
		}
		if options.json {
			return writeJSON(stdout, output)
		}
		_, err = fmt.Fprintf(
			stdout, "run=%s candidate=%s disposition=%s reason=%s store=%s\n",
			output.RunID, candidate.CandidateID, candidate.Disposition,
			candidate.ReasonCode, output.StorePath,
		)
		return err
	}
	return fmt.Errorf("candidate %q does not belong to run %q", options.candidate, options.run)
}

func runFinding(arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(findingUsage)
	}
	if isHelpArgument(arguments) {
		_, err := fmt.Fprintln(stdout, findingUsage)
		return err
	}
	if arguments[0] != "show" {
		return fmt.Errorf("unknown finding command %q\n%s", arguments[0], findingUsage)
	}
	var options findingShowFlags
	flags := newFlagSet("finding show")
	flags.StringVar(&options.store, "store", "", "explicit local state root")
	flags.StringVar(&options.run, "run", "", "review run ID")
	flags.StringVar(&options.finding, "finding", "", "finding ID")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments[1:]); err != nil {
		return controlFlagError("finding show", err, findingUsage)
	}
	if flags.NArg() != 0 {
		return controlFlagError(
			"finding show",
			fmt.Errorf("unexpected argument %q", flags.Arg(0)),
			findingUsage,
		)
	}
	if options.run == "" || options.finding == "" {
		return controlFlagError(
			"finding show",
			fmt.Errorf("--run and --finding are required"),
			findingUsage,
		)
	}
	service, storePath, err := openControlPlane(options.store)
	if err != nil {
		return err
	}
	detail, err := service.Finding(options.run, options.finding)
	if err != nil {
		return err
	}
	output := findingShowOutput{
		Run: detail.Run, Finding: detail.Finding, Decisions: detail.Decisions,
		GovernedFinding: detail.GovernedFinding, GovernedDecisions: detail.GovernedDecisions,
		HumanDecisions: detail.HumanDecisions,
		Feedback:       detail.Feedback, Outcomes: detail.Outcomes, StorePath: storePath,
	}
	if options.json {
		return writeJSON(stdout, output)
	}
	_, err = fmt.Fprintf(
		stdout,
		"run=%s finding=%s decisions=%d feedback=%d outcomes=%d store=%s\n",
		output.Run.RunID,
		detail.FindingID(),
		len(output.Decisions)+len(output.GovernedDecisions)+len(output.HumanDecisions),
		len(output.Feedback),
		len(output.Outcomes),
		output.StorePath,
	)
	return err
}

func runDecision(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(decisionUsage)
	}
	if isHelpArgument(arguments) {
		_, err := fmt.Fprintln(stdout, decisionUsage)
		return err
	}
	if arguments[0] != "record" {
		return fmt.Errorf("unknown decision command %q\n%s", arguments[0], decisionUsage)
	}
	options, err := parseGovernedWriteFlags("decision record", arguments[1:], decisionUsage)
	if err != nil {
		return err
	}
	request, err := readStrictDescriptor(
		options.input, "finding decision request", findingdecision.DecodeRequestJSON,
	)
	if err != nil {
		return err
	}
	mutation, err := readStrictDescriptor(
		options.mutation, "finding decision mutation", findingdecision.DecodeMutationJSON,
	)
	if err != nil {
		return err
	}
	service, storePath, err := openControlPlane(options.store)
	if err != nil {
		return err
	}
	decision, err := service.RecordDecision(ctx, request, mutation)
	if err != nil {
		return err
	}
	output := decisionRecordOutput{Decision: decision, StorePath: storePath}
	if options.json {
		return writeJSON(stdout, output)
	}
	_, err = fmt.Fprintf(
		stdout,
		"decision=%s run=%s finding=%s sequence=%d action=%s store=%s\n",
		decision.DecisionID, decision.RunID, decision.FindingID,
		decision.Sequence, decision.Action, storePath,
	)
	return err
}

func runPublication(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(publicationUsage)
	}
	if isHelpArgument(arguments) {
		_, err := fmt.Fprintln(stdout, publicationUsage)
		return err
	}
	if len(arguments) < 2 {
		return fmt.Errorf("unknown publication command\n%s", publicationUsage)
	}
	if arguments[0] == "github" &&
		(arguments[1] == "dispatch" || arguments[1] == "reconcile") {
		return runGitHubPublication(ctx, arguments[1], arguments[2:], stdout)
	}
	if arguments[0] == "grant" && arguments[1] == "record" {
		options, err := parseGovernedWriteFlags(
			"publication grant record", arguments[2:], publicationUsage,
		)
		if err != nil {
			return err
		}
		request, err := readStrictDescriptor(
			options.input, "publication grant request", publication.DecodeGrantRequestJSON,
		)
		if err != nil {
			return err
		}
		mutation, err := readStrictDescriptor(
			options.mutation, "publication grant mutation", publication.DecodeGrantMutationJSON,
		)
		if err != nil {
			return err
		}
		service, storePath, err := openControlPlane(options.store)
		if err != nil {
			return err
		}
		grant, err := service.RecordPublicationGrant(ctx, request, mutation)
		if err != nil {
			return err
		}
		output := publicationGrantOutput{Grant: grant, StorePath: storePath}
		if options.json {
			return writeJSON(stdout, output)
		}
		_, err = fmt.Fprintf(
			stdout, "grant=%s publication=%s run=%s finding=%s expires=%s store=%s\n",
			grant.GrantID, grant.PublicationID, grant.RunID, grant.FindingID,
			grant.ExpiresAt.Format(time.RFC3339Nano), storePath,
		)
		return err
	}
	if arguments[0] != "request" || arguments[1] != "build" {
		return fmt.Errorf("unknown publication command\n%s", publicationUsage)
	}
	options, err := parseFactRecordFlags(
		"publication request build", arguments[2:], publicationUsage,
	)
	if err != nil {
		return err
	}
	intent, err := readStrictDescriptor(
		options.input, "publication intent", publication.DecodeIntentJSON,
	)
	if err != nil {
		return err
	}
	service, storePath, err := openControlPlane(options.store)
	if err != nil {
		return err
	}
	request, err := service.BuildPublicationRequest(intent)
	if err != nil {
		return err
	}
	output := publicationRequestOutput{Request: request, StorePath: storePath}
	if options.json {
		return writeJSON(stdout, output)
	}
	_, err = fmt.Fprintf(
		stdout,
		"publication=%s run=%s finding=%s decision=%s channel=%s store=%s\n",
		request.PublicationID, request.RunID, request.FindingID, request.DecisionID,
		request.Channel, storePath,
	)
	return err
}

func runGitHubPublication(
	ctx context.Context,
	action string,
	arguments []string,
	stdout io.Writer,
) error {
	var options githubPublicationFlags
	flags := newFlagSet("publication github " + action)
	flags.StringVar(&options.store, "store", "", "explicit local state root")
	flags.StringVar(&options.input, "input", "", "absolute PublicationIntent JSON")
	flags.StringVar(&options.publication, "publication", "", "publication ID")
	flags.BoolVar(
		&options.singleUserLocal, "single-user-local", false,
		"explicitly use the local fixed-environment credential profile",
	)
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return controlFlagError("publication github "+action, err, publicationUsage)
	}
	if flags.NArg() != 0 {
		return controlFlagError(
			"publication github "+action,
			fmt.Errorf("unexpected argument %q", flags.Arg(0)), publicationUsage,
		)
	}
	if !options.singleUserLocal {
		return controlFlagError(
			"publication github "+action,
			fmt.Errorf("--single-user-local is required until a production permission broker is configured"),
			publicationUsage,
		)
	}
	if action == "dispatch" {
		if options.input == "" || options.publication != "" {
			return controlFlagError(
				"publication github dispatch",
				fmt.Errorf("--input is required and --publication is forbidden"), publicationUsage,
			)
		}
		if err := validateDescriptorPath("input", options.input); err != nil {
			return controlFlagError("publication github dispatch", err, publicationUsage)
		}
	} else if options.publication == "" || options.input != "" {
		return controlFlagError(
			"publication github reconcile",
			fmt.Errorf("--publication is required and --input is forbidden"), publicationUsage,
		)
	}
	control, storePath, err := openControlPlane(options.store)
	if err != nil {
		return err
	}
	store, err := local.Open(storePath)
	if err != nil {
		return fmt.Errorf("open publication state: %w", err)
	}
	repository, err := publication.NewRepository(store)
	if err != nil {
		return fmt.Errorf("open publication repository: %w", err)
	}
	provider, err := buildGitHubPublicationProvider()
	if err != nil {
		return fmt.Errorf("initialize GitHub publication provider: %w", err)
	}
	service, err := publication.NewService(repository, provider, control, nil)
	if err != nil {
		return fmt.Errorf("initialize publication service: %w", err)
	}
	var request publication.Request
	if action == "dispatch" {
		intent, err := readStrictDescriptor(
			options.input, "publication intent", publication.DecodeIntentJSON,
		)
		if err != nil {
			return err
		}
		request, err = control.BuildPublicationRequest(intent)
		if err != nil {
			return err
		}
	} else {
		existing, err := repository.Get(options.publication)
		if err != nil {
			return err
		}
		switch existing.State {
		case publication.StateDispatching, publication.StateUnknown,
			publication.StatePublished, publication.StateRejected, publication.StateNotFound:
			request = existing.Request
		default:
			return fmt.Errorf(
				"publication %q is %s; reconcile cannot initiate a new remote write",
				options.publication, existing.State,
			)
		}
	}
	record, publishErr := service.Publish(ctx, request)
	if publishErr != nil && record.Request.PublicationID == "" {
		return publishErr
	}
	output := publicationDispatchOutput{Record: record, StorePath: storePath}
	if options.json {
		if err := writeJSON(stdout, output); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(
			stdout, "publication=%s state=%s provider=%s repository=%s change=%s store=%s\n",
			record.Request.PublicationID, record.State, record.Request.Provider,
			record.Request.RepositoryID, record.Request.ChangeID, storePath,
		); err != nil {
			return err
		}
	}
	return publishErr
}

func runFeedback(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(feedbackUsage)
	}
	if isHelpArgument(arguments) {
		_, err := fmt.Fprintln(stdout, feedbackUsage)
		return err
	}
	if arguments[0] != "record" {
		return fmt.Errorf("unknown feedback command %q\n%s", arguments[0], feedbackUsage)
	}
	options, err := parseFactRecordFlags("feedback record", arguments[1:], feedbackUsage)
	if err != nil {
		return err
	}
	fact, err := readStrictDescriptor(
		options.input,
		"feedback",
		feedbackdomain.DecodeFeedbackJSON,
	)
	if err != nil {
		return err
	}
	service, storePath, err := openControlPlane(options.store)
	if err != nil {
		return err
	}
	recorded, err := service.RecordFeedback(ctx, fact)
	if err != nil {
		return err
	}
	output := feedbackRecordOutput{
		Feedback:    recorded,
		Eligibility: recorded.EvaluationCandidateEligibility(),
		StorePath:   storePath,
	}
	if options.json {
		return writeJSON(stdout, output)
	}
	_, err = fmt.Fprintf(
		stdout,
		"feedback=%s run=%s finding=%s evaluation_use=%s store=%s\n",
		recorded.FeedbackID,
		recorded.RunID,
		recorded.FindingID,
		output.Eligibility.Use,
		storePath,
	)
	return err
}

func runOutcome(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(outcomeUsage)
	}
	if isHelpArgument(arguments) {
		_, err := fmt.Fprintln(stdout, outcomeUsage)
		return err
	}
	if arguments[0] != "record" {
		return fmt.Errorf("unknown outcome command %q\n%s", arguments[0], outcomeUsage)
	}
	options, err := parseFactRecordFlags("outcome record", arguments[1:], outcomeUsage)
	if err != nil {
		return err
	}
	fact, err := readStrictDescriptor(
		options.input,
		"outcome",
		feedbackdomain.DecodeOutcomeJSON,
	)
	if err != nil {
		return err
	}
	service, storePath, err := openControlPlane(options.store)
	if err != nil {
		return err
	}
	recorded, err := service.RecordOutcome(ctx, fact)
	if err != nil {
		return err
	}
	output := outcomeRecordOutput{Outcome: recorded, StorePath: storePath}
	if options.json {
		return writeJSON(stdout, output)
	}
	_, err = fmt.Fprintf(
		stdout,
		"outcome=%s run=%s finding=%s state=%s store=%s\n",
		recorded.OutcomeID,
		recorded.RunID,
		recorded.FindingID,
		recorded.State,
		storePath,
	)
	return err
}

func runEvaluation(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	if len(arguments) == 0 {
		return errors.New(evaluationUsage)
	}
	if isHelpArgument(arguments) {
		_, err := fmt.Fprintln(stdout, evaluationUsage)
		return err
	}
	switch arguments[0] {
	case "case":
		return runEvaluationCase(ctx, arguments[1:], stdout)
	case "trust-key":
		return runEvaluationTrustKey(ctx, arguments[1:], stdout)
	case "governance-batch":
		return runEvaluationGovernanceBatch(ctx, arguments[1:], stdout)
	case "incident":
		return runEvaluationIncident(ctx, arguments[1:], stdout)
	case "probe":
		return runEvaluationProbe(ctx, arguments[1:], stdout)
	case "exposure":
		return runEvaluationExposure(ctx, arguments[1:], stdout)
	case "run":
		return runEvaluationRun(ctx, arguments[1:], stdout)
	case "experiment":
		return runEvaluationExperiment(ctx, arguments[1:], stdout)
	case "repeatability":
		return runEvaluationRepeatability(ctx, arguments[1:], stdout)
	case "normalization":
		return runEvaluationNormalization(ctx, arguments[1:], stdout)
	case "corpus":
		return runEvaluationCorpus(ctx, arguments[1:], stdout)
	case "batch":
		return runEvaluationBatch(ctx, arguments[1:], stdout)
	default:
		return fmt.Errorf("unknown evaluation command %q\n%s", arguments[0], evaluationUsage)
	}
}

func runEvaluationRepeatability(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(evaluationUsage)
	}
	switch arguments[0] {
	case "batch":
		return runEvaluationRepeatabilityBatch(ctx, arguments[1:], stdout)
	case "record":
		options, err := parseGovernedWriteFlags("evaluation repeatability record", arguments[1:], evaluationUsage)
		if err != nil {
			return err
		}
		request, err := readStrictDescriptor(options.input, "repeatability run request", evaluation.DecodeRepeatabilityRunRequest)
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
		store, err := local.Open(storePath)
		if err != nil {
			return err
		}
		runs, err := runrepo.New(store)
		if err != nil {
			return err
		}
		run, err := repository.RecordRepeatabilityRun(ctx, request, mutation, runs)
		if err != nil {
			return err
		}
		return writeRepeatabilityRunOutput(stdout, run, storePath, options.json)
	case "list":
		options, err := parseGovernedReadFlags("evaluation repeatability list", arguments[1:], evaluationUsage, "", "")
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
		runs, err := repository.ListRepeatabilityRuns(access)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, repeatabilityRunListOutput{Runs: runs, StorePath: storePath})
		}
		_, err = fmt.Fprintf(stdout, "repeatability_runs=%d store=%s\n", len(runs), storePath)
		return err
	case "show":
		options, err := parseGovernedReadFlags("evaluation repeatability show", arguments[1:], evaluationUsage, "repeatability", "repeatability run ID")
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
		run, err := repository.GetRepeatabilityRun(options.id, access)
		if err != nil {
			return err
		}
		return writeRepeatabilityRunOutput(stdout, run, storePath, options.json)
	default:
		return fmt.Errorf("unknown evaluation repeatability command %q\n%s", arguments[0], evaluationUsage)
	}
}

func runEvaluationGovernanceBatch(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(evaluationUsage)
	}
	switch arguments[0] {
	case "run":
		options, err := parseGovernedWriteFlags(
			"evaluation governance-batch run", arguments[1:], evaluationUsage,
		)
		if err != nil {
			return err
		}
		request, err := readStrictDescriptor(
			options.input, "governance batch request", evaluation.DecodeGovernanceBatchRequest,
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
		record, err := repository.RunGovernanceBatch(ctx, request, mutation)
		if err != nil {
			return err
		}
		return writeEvaluationGovernanceBatchOutput(stdout, record, storePath, options.json)
	case "list":
		options, err := parseGovernedReadFlags(
			"evaluation governance-batch list", arguments[1:], evaluationUsage, "", "",
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
		records, err := repository.ListGovernanceBatches(access)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, evaluationGovernanceBatchListOutput{Batches: records, StorePath: storePath})
		}
		_, err = fmt.Fprintf(stdout, "governance_batches=%d store=%s\n", len(records), storePath)
		return err
	case "show":
		options, err := parseGovernedReadFlags(
			"evaluation governance-batch show", arguments[1:], evaluationUsage,
			"batch", "governance batch ID",
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
		record, err := repository.GetGovernanceBatch(options.id, access)
		if err != nil {
			return err
		}
		return writeEvaluationGovernanceBatchOutput(stdout, record, storePath, options.json)
	default:
		return fmt.Errorf("unknown evaluation governance-batch command %q\n%s", arguments[0], evaluationUsage)
	}
}

func runEvaluationTrustKey(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(evaluationUsage)
	}
	switch arguments[0] {
	case "register":
		options, err := parseGovernedWriteFlags("evaluation trust-key register", arguments[1:], evaluationUsage)
		if err != nil {
			return err
		}
		registration, err := readStrictDescriptor(
			options.input, "governance trust key registration", evaluation.DecodeGovernanceTrustKeyRegistration,
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
		record, err := repository.RegisterGovernanceTrustKey(ctx, registration, mutation)
		if err != nil {
			return err
		}
		return writeEvaluationTrustKeyOutput(stdout, record, storePath, options.json)
	case "revoke":
		options, err := parseGovernedWriteFlags("evaluation trust-key revoke", arguments[1:], evaluationUsage)
		if err != nil {
			return err
		}
		revocation, err := readStrictDescriptor(
			options.input, "governance trust key revocation", evaluation.DecodeGovernanceTrustKeyRevocation,
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
		record, err := repository.RevokeGovernanceTrustKey(ctx, revocation, mutation)
		if err != nil {
			return err
		}
		return writeEvaluationTrustKeyOutput(stdout, record, storePath, options.json)
	case "list":
		options, err := parseGovernedReadFlags(
			"evaluation trust-key list", arguments[1:], evaluationUsage, "", "",
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
		records, err := repository.ListGovernanceTrustKeys(access)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, evaluationTrustKeyListOutput{TrustKeys: records, StorePath: storePath})
		}
		_, err = fmt.Fprintf(stdout, "governance_trust_keys=%d store=%s\n", len(records), storePath)
		return err
	default:
		return fmt.Errorf("unknown evaluation trust-key command %q\n%s", arguments[0], evaluationUsage)
	}
}

func runEvaluationExperiment(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(evaluationUsage)
	}
	switch arguments[0] {
	case "record":
		options, err := parseGovernedWriteFlags("evaluation experiment record",
			arguments[1:], evaluationUsage)
		if err != nil {
			return err
		}
		request, err := readStrictDescriptor(options.input, "experiment run request",
			evaluation.DecodeExperimentRunRequest)
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
		store, err := local.Open(storePath)
		if err != nil {
			return err
		}
		runs, err := runrepo.New(store)
		if err != nil {
			return err
		}
		run, err := repository.RecordExperimentRun(ctx, request, mutation, runs)
		if err != nil {
			return err
		}
		return writeExperimentRunOutput(stdout, run, storePath, options.json)
	case "show":
		options, err := parseGovernedReadFlags("evaluation experiment show", arguments[1:],
			evaluationUsage, "experiment", "experiment run ID")
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
		run, err := repository.GetExperimentRun(options.id, access)
		if err != nil {
			return err
		}
		return writeExperimentRunOutput(stdout, run, storePath, options.json)
	default:
		return fmt.Errorf("unknown evaluation experiment command %q\n%s", arguments[0], evaluationUsage)
	}
}

func runEvaluationRun(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(evaluationUsage)
	}
	switch arguments[0] {
	case "record":
		options, err := parseGovernedWriteFlags("evaluation run record", arguments[1:], evaluationUsage)
		if err != nil {
			return err
		}
		request, err := readStrictDescriptor(options.input, "evaluation run request",
			evaluation.DecodeEvaluationRunRequest)
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
		store, err := local.Open(storePath)
		if err != nil {
			return err
		}
		runs, err := runrepo.New(store)
		if err != nil {
			return err
		}
		run, err := repository.RecordEvaluationRun(ctx, request, mutation, runs)
		if err != nil {
			return err
		}
		return writeEvaluationRunOutput(stdout, run, storePath, options.json)
	case "list":
		options, err := parseGovernedReadFlags("evaluation run list", arguments[1:],
			evaluationUsage, "", "")
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
		runs, err := repository.ListEvaluationRuns(access)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, evaluationRunListOutput{Runs: runs, StorePath: storePath})
		}
		_, err = fmt.Fprintf(stdout, "evaluation_runs=%d store=%s\n", len(runs), storePath)
		return err
	case "show":
		options, err := parseGovernedReadFlags("evaluation run show", arguments[1:],
			evaluationUsage, "run", "evaluation run ID")
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
		run, err := repository.GetEvaluationRun(options.id, access)
		if err != nil {
			return err
		}
		return writeEvaluationRunOutput(stdout, run, storePath, options.json)
	default:
		return fmt.Errorf("unknown evaluation run command %q\n%s", arguments[0], evaluationUsage)
	}
}

func runEvaluationCase(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	if len(arguments) == 0 {
		return errors.New(evaluationUsage)
	}
	switch arguments[0] {
	case "derive":
		options, err := parseGovernedWriteFlags(
			"evaluation case derive", arguments[1:], evaluationUsage,
		)
		if err != nil {
			return err
		}
		request, err := readStrictDescriptor(
			options.input, "evaluation candidate derivation request",
			evaluation.DecodeCandidateDerivationRequest,
		)
		if err != nil {
			return err
		}
		mutation, err := readEvaluationMutation(options.mutation)
		if err != nil {
			return err
		}
		control, storePath, err := openControlPlane(options.store)
		if err != nil {
			return err
		}
		repository, _, err := openEvaluationRepository(storePath)
		if err != nil {
			return err
		}
		control, err = control.WithEvaluationSources(repository)
		if err != nil {
			return err
		}
		candidate, err := control.DeriveEvaluationCandidate(request)
		if err != nil {
			return err
		}
		record, err := repository.CreateCase(ctx, candidate, mutation)
		if err != nil {
			return err
		}
		return writeEvaluationCaseOutput(stdout, record, storePath, options.json)
	case "create":
		options, err := parseGovernedWriteFlags(
			"evaluation case create",
			arguments[1:],
			evaluationUsage,
		)
		if err != nil {
			return err
		}
		evaluationCase, err := readStrictDescriptor(
			options.input,
			"evaluation case",
			evaluation.DecodeEvaluationCase,
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
		record, err := repository.CreateCase(ctx, evaluationCase, mutation)
		if err != nil {
			return err
		}
		return writeEvaluationCaseOutput(stdout, record, storePath, options.json)
	case "import":
		options, err := parseGovernedWriteFlags("evaluation case import", arguments[1:], evaluationUsage)
		if err != nil {
			return err
		}
		request, err := readStrictDescriptor(options.input, "external governed case import", evaluation.DecodeExternalGovernedCaseImport)
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
		record, err := repository.ImportGovernedCase(ctx, request, mutation)
		if err != nil {
			return err
		}
		return writeEvaluationCaseOutput(stdout, record, storePath, options.json)
	case "list":
		options, err := parseGovernedReadFlags(
			"evaluation case list",
			arguments[1:],
			evaluationUsage,
			"",
			"",
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
		records, err := repository.ListCases(access)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, evaluationCaseListOutput{
				Cases: records, StorePath: storePath,
			})
		}
		_, err = fmt.Fprintf(stdout, "cases=%d store=%s\n", len(records), storePath)
		return err
	case "show":
		options, err := parseGovernedReadFlags(
			"evaluation case show",
			arguments[1:],
			evaluationUsage,
			"case",
			"evaluation case ID",
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
		record, err := repository.GetCase(options.id, access)
		if err != nil {
			return err
		}
		labels, err := repository.LabelHistory(options.id, access)
		if err != nil {
			return err
		}
		exposures, err := repository.ExposureHistory(options.id, access)
		if err != nil {
			return err
		}
		annotations, err := repository.AnnotationHistory(options.id, access)
		if err != nil {
			return err
		}
		adjudications, err := repository.AdjudicationHistory(options.id, access)
		if err != nil {
			return err
		}
		assignments, err := repository.AssignmentHistory(options.id, access)
		if err != nil {
			return err
		}
		agreement, err := repository.ReviewAgreement(options.id, access)
		if err != nil {
			return err
		}
		if labels == nil {
			labels = []evaluation.LabelEntry{}
		}
		if exposures == nil {
			exposures = []evaluation.ExposureEntry{}
		}
		if annotations == nil {
			annotations = []evaluation.CaseAnnotationEntry{}
		}
		if adjudications == nil {
			adjudications = []evaluation.CaseAdjudicationEntry{}
		}
		if assignments == nil {
			assignments = []evaluation.CaseReviewAssignmentEntry{}
		}
		output := evaluationCaseShowOutput{
			Record: record, LabelHistory: labels, Annotations: annotations,
			Adjudications: adjudications, Assignments: assignments, Agreement: agreement,
			Exposures: exposures, StorePath: storePath,
		}
		if options.json {
			return writeJSON(stdout, output)
		}
		_, err = fmt.Fprintf(
			stdout,
			"case=%s governance_revision=%d label_revision=%d labels=%d assignments=%d annotations=%d adjudications=%d agreement=%s exposures=%d store=%s\n",
			record.Case.CaseID,
			record.CurrentGovernance.Revision,
			record.CurrentLabelRevision,
			len(labels),
			len(assignments),
			len(annotations),
			len(adjudications),
			agreement.VerdictAgreement,
			len(exposures),
			storePath,
		)
		return err
	case "correct-label":
		options, err := parseGovernedWriteFlags(
			"evaluation case correct-label",
			arguments[1:],
			evaluationUsage,
		)
		if err != nil {
			return err
		}
		correction, err := readStrictDescriptor(
			options.input,
			"label correction",
			evaluation.DecodeLabelCorrection,
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
		record, err := repository.CorrectLabel(ctx, correction, mutation)
		if err != nil {
			return err
		}
		return writeEvaluationCaseOutput(stdout, record, storePath, options.json)
	case "assign":
		options, err := parseGovernedWriteFlags("evaluation case assign", arguments[1:], evaluationUsage)
		if err != nil {
			return err
		}
		assignment, err := readStrictDescriptor(options.input, "case review assignment", evaluation.DecodeCaseReviewAssignment)
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
		entry, err := repository.AssignCaseReview(ctx, assignment, mutation)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, evaluationAssignmentOutput{Assignment: entry, StorePath: storePath})
		}
		_, err = fmt.Fprintf(stdout, "case=%s assignment_event=%s reviewers=%d store=%s\n", assignment.CaseID, entry.EventID, len(assignment.ReviewerIDs), storePath)
		return err
	case "annotate":
		options, err := parseGovernedWriteFlags("evaluation case annotate", arguments[1:], evaluationUsage)
		if err != nil {
			return err
		}
		annotation, err := readStrictDescriptor(options.input, "case annotation", evaluation.DecodeCaseAnnotation)
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
		entry, err := repository.RecordCaseAnnotation(ctx, annotation, mutation)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, evaluationAnnotationOutput{Annotation: entry, StorePath: storePath})
		}
		_, err = fmt.Fprintf(stdout, "case=%s annotation_event=%s reviewer=%s verdict=%s store=%s\n",
			entry.Annotation.CaseID, entry.EventID, entry.Reviewer, entry.Annotation.Verdict, storePath)
		return err
	case "adjudicate":
		options, err := parseGovernedWriteFlags("evaluation case adjudicate", arguments[1:], evaluationUsage)
		if err != nil {
			return err
		}
		adjudication, err := readStrictDescriptor(options.input, "case adjudication", evaluation.DecodeCaseAdjudication)
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
		record, err := repository.AdjudicateCase(ctx, adjudication, mutation)
		if err != nil {
			return err
		}
		return writeEvaluationCaseOutput(stdout, record, storePath, options.json)
	case "activate":
		options, err := parseGovernedWriteFlags("evaluation case activate", arguments[1:], evaluationUsage)
		if err != nil {
			return err
		}
		activation, err := readStrictDescriptor(options.input, "case activation", evaluation.DecodeCaseActivation)
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
		record, err := repository.ActivateCase(ctx, activation, mutation)
		if err != nil {
			return err
		}
		return writeEvaluationCaseOutput(stdout, record, storePath, options.json)
	case "reopen":
		options, err := parseGovernedWriteFlags("evaluation case reopen", arguments[1:], evaluationUsage)
		if err != nil {
			return err
		}
		reopen, err := readStrictDescriptor(options.input, "case reopen", evaluation.DecodeCaseReopen)
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
		record, err := repository.ReopenCase(ctx, reopen, mutation)
		if err != nil {
			return err
		}
		return writeEvaluationCaseOutput(stdout, record, storePath, options.json)
	default:
		return fmt.Errorf("unknown evaluation case command %q\n%s", arguments[0], evaluationUsage)
	}
}

func runEvaluationIncident(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(evaluationUsage)
	}
	switch arguments[0] {
	case "record":
		options, err := parseGovernedWriteFlags("evaluation incident record", arguments[1:], evaluationUsage)
		if err != nil {
			return err
		}
		incident, err := readStrictDescriptor(options.input, "missed-defect incident", evaluation.DecodeMissedDefectIncident)
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
		entry, err := repository.RecordMissedDefectIncident(ctx, incident, mutation)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, evaluationIncidentOutput{Incident: entry, StorePath: storePath})
		}
		_, err = fmt.Fprintf(stdout, "incident=%s run=%s event=%s store=%s\n", incident.IncidentID, incident.ReviewRunID, entry.EventID, storePath)
		return err
	case "list":
		options, err := parseGovernedReadFlags("evaluation incident list", arguments[1:], evaluationUsage, "", "")
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
		entries, err := repository.ListMissedDefectIncidents(access)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, evaluationIncidentListOutput{Incidents: entries, StorePath: storePath})
		}
		_, err = fmt.Fprintf(stdout, "incidents=%d store=%s\n", len(entries), storePath)
		return err
	case "show":
		options, err := parseGovernedReadFlags("evaluation incident show", arguments[1:], evaluationUsage, "incident", "incident ID")
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
		entry, err := repository.GetMissedDefectIncident(options.id, access)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, evaluationIncidentOutput{Incident: entry, StorePath: storePath})
		}
		_, err = fmt.Fprintf(stdout, "incident=%s run=%s event=%s store=%s\n", entry.Incident.IncidentID, entry.Incident.ReviewRunID, entry.EventID, storePath)
		return err
	default:
		return fmt.Errorf("unknown evaluation incident command %q\n%s", arguments[0], evaluationUsage)
	}
}

func runEvaluationProbe(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(evaluationUsage)
	}
	switch arguments[0] {
	case "record":
		options, err := parseGovernedWriteFlags("evaluation probe record", arguments[1:], evaluationUsage)
		if err != nil {
			return err
		}
		probe, err := readStrictDescriptor(options.input, "evaluation probe", evaluation.DecodeEvaluationProbe)
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
		entry, err := repository.RecordEvaluationProbe(ctx, probe, mutation)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, evaluationProbeOutput{Probe: entry, StorePath: storePath})
		}
		_, err = fmt.Fprintf(stdout, "probe=%s kind=%s run=%s event=%s store=%s\n", probe.ProbeID, probe.Kind, probe.ReviewRunID, entry.EventID, storePath)
		return err
	case "list":
		options, err := parseGovernedReadFlags("evaluation probe list", arguments[1:], evaluationUsage, "", "")
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
		entries, err := repository.ListEvaluationProbes(access)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, evaluationProbeListOutput{Probes: entries, StorePath: storePath})
		}
		_, err = fmt.Fprintf(stdout, "probes=%d store=%s\n", len(entries), storePath)
		return err
	case "show":
		options, err := parseGovernedReadFlags("evaluation probe show", arguments[1:], evaluationUsage, "probe", "probe ID")
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
		entry, err := repository.GetEvaluationProbe(options.id, access)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, evaluationProbeOutput{Probe: entry, StorePath: storePath})
		}
		_, err = fmt.Fprintf(stdout, "probe=%s kind=%s run=%s event=%s store=%s\n", entry.Probe.ProbeID, entry.Probe.Kind, entry.Probe.ReviewRunID, entry.EventID, storePath)
		return err
	default:
		return fmt.Errorf("unknown evaluation probe command %q\n%s", arguments[0], evaluationUsage)
	}
}

func runEvaluationExposure(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	if len(arguments) == 0 {
		return errors.New(evaluationUsage)
	}
	if arguments[0] != "record" {
		return fmt.Errorf(
			"unknown evaluation exposure command %q\n%s",
			arguments[0],
			evaluationUsage,
		)
	}
	options, err := parseGovernedWriteFlags(
		"evaluation exposure record",
		arguments[1:],
		evaluationUsage,
	)
	if err != nil {
		return err
	}
	exposure, err := readStrictDescriptor(
		options.input,
		"evaluation exposure",
		evaluation.DecodeExposure,
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
	entry, err := repository.RecordExposure(ctx, exposure, mutation)
	if err != nil {
		return err
	}
	if options.json {
		return writeJSON(stdout, evaluationExposureOutput{
			Entry: entry, StorePath: storePath,
		})
	}
	_, err = fmt.Fprintf(
		stdout,
		"evaluation_run=%s case=%s event=%s store=%s\n",
		entry.Exposure.EvaluationRunID,
		entry.Exposure.CaseID,
		entry.EventID,
		storePath,
	)
	return err
}

func runPromotion(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	if len(arguments) == 0 {
		return errors.New(promotionUsage)
	}
	if isHelpArgument(arguments) {
		_, err := fmt.Fprintln(stdout, promotionUsage)
		return err
	}
	switch arguments[0] {
	case "register":
		options, err := parseGovernedWriteFlags(
			"promotion register",
			arguments[1:],
			promotionUsage,
		)
		if err != nil {
			return err
		}
		variant, err := readStrictDescriptor(
			options.input,
			"promotion variant",
			evaluation.DecodePromotionVariant,
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
		record, err := repository.RegisterPromotion(ctx, variant, mutation)
		if err != nil {
			return err
		}
		return writePromotionOutput(stdout, record, storePath, options.json)
	case "gate":
		options, err := parseGovernedWriteFlags(
			"promotion gate",
			arguments[1:],
			promotionUsage,
		)
		if err != nil {
			return err
		}
		result, err := readStrictDescriptor(
			options.input,
			"promotion gate result",
			evaluation.DecodeGateResult,
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
		record, err := repository.RecordGate(ctx, result, mutation)
		if err != nil {
			return err
		}
		return writePromotionOutput(stdout, record, storePath, options.json)
	case "show":
		options, err := parseGovernedReadFlags(
			"promotion show",
			arguments[1:],
			promotionUsage,
			"variant",
			"promotion variant ID",
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
		return writePromotionOutput(stdout, record, storePath, options.json)
	case "rollback":
		options, err := parseRollbackFlags(arguments[1:])
		if err != nil {
			return controlFlagError("promotion rollback", err, promotionUsage)
		}
		mutation, err := readEvaluationMutation(options.mutation)
		if err != nil {
			return err
		}
		repository, storePath, err := openEvaluationRepository(options.store)
		if err != nil {
			return err
		}
		record, err := repository.RollbackPromotion(ctx, options.variant, mutation)
		if err != nil {
			return err
		}
		return writePromotionOutput(stdout, record, storePath, options.json)
	default:
		return fmt.Errorf("unknown promotion command %q\n%s", arguments[0], promotionUsage)
	}
}

func parseFactRecordFlags(
	command string,
	arguments []string,
	commandUsage string,
) (factRecordFlags, error) {
	var options factRecordFlags
	flags := newFlagSet(command)
	flags.StringVar(&options.store, "store", "", "explicit local state root")
	flags.StringVar(&options.input, "input", "", "absolute strict JSON descriptor")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return factRecordFlags{}, controlFlagError(command, err, commandUsage)
	}
	if flags.NArg() != 0 {
		return factRecordFlags{}, controlFlagError(
			command,
			fmt.Errorf("unexpected argument %q", flags.Arg(0)),
			commandUsage,
		)
	}
	if _, err := validateExplicitControlStore(options.store); err != nil {
		return factRecordFlags{}, err
	}
	if err := validateDescriptorPath("input", options.input); err != nil {
		return factRecordFlags{}, err
	}
	return options, nil
}

func parseGovernedWriteFlags(
	command string,
	arguments []string,
	commandUsage string,
) (governedWriteFlags, error) {
	var options governedWriteFlags
	flags := newFlagSet(command)
	flags.StringVar(&options.store, "store", "", "explicit local state root")
	flags.StringVar(&options.input, "input", "", "absolute strict JSON domain descriptor")
	flags.StringVar(
		&options.mutation,
		"mutation",
		"",
		"absolute strict JSON mutation descriptor",
	)
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return governedWriteFlags{}, controlFlagError(command, err, commandUsage)
	}
	if flags.NArg() != 0 {
		return governedWriteFlags{}, controlFlagError(
			command,
			fmt.Errorf("unexpected argument %q", flags.Arg(0)),
			commandUsage,
		)
	}
	if _, err := validateExplicitControlStore(options.store); err != nil {
		return governedWriteFlags{}, err
	}
	if err := validateDescriptorPath("input", options.input); err != nil {
		return governedWriteFlags{}, err
	}
	if err := validateDescriptorPath("mutation", options.mutation); err != nil {
		return governedWriteFlags{}, err
	}
	return options, nil
}

func parseGovernedReadFlags(
	command string,
	arguments []string,
	commandUsage string,
	idFlag string,
	idDescription string,
) (governedReadFlags, error) {
	var options governedReadFlags
	flags := newFlagSet(command)
	flags.StringVar(&options.store, "store", "", "explicit local state root")
	flags.StringVar(&options.access, "access", "", "absolute strict JSON access descriptor")
	if idFlag != "" {
		flags.StringVar(&options.id, idFlag, "", idDescription)
	}
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return governedReadFlags{}, controlFlagError(command, err, commandUsage)
	}
	if flags.NArg() != 0 {
		return governedReadFlags{}, controlFlagError(
			command,
			fmt.Errorf("unexpected argument %q", flags.Arg(0)),
			commandUsage,
		)
	}
	if _, err := validateExplicitControlStore(options.store); err != nil {
		return governedReadFlags{}, err
	}
	if err := validateDescriptorPath("access", options.access); err != nil {
		return governedReadFlags{}, err
	}
	if idFlag != "" && options.id == "" {
		return governedReadFlags{}, fmt.Errorf("--%s is required", idFlag)
	}
	return options, nil
}

func parseRollbackFlags(arguments []string) (rollbackFlags, error) {
	var options rollbackFlags
	flags := newFlagSet("promotion rollback")
	flags.StringVar(&options.store, "store", "", "explicit local state root")
	flags.StringVar(&options.variant, "variant", "", "promotion variant ID")
	flags.StringVar(
		&options.mutation,
		"mutation",
		"",
		"absolute strict JSON mutation descriptor",
	)
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return rollbackFlags{}, err
	}
	if flags.NArg() != 0 {
		return rollbackFlags{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if _, err := validateExplicitControlStore(options.store); err != nil {
		return rollbackFlags{}, err
	}
	if options.variant == "" {
		return rollbackFlags{}, fmt.Errorf("--variant is required")
	}
	if err := validateDescriptorPath("mutation", options.mutation); err != nil {
		return rollbackFlags{}, err
	}
	return options, nil
}

func readEvaluationMutation(path string) (evaluation.Mutation, error) {
	return readStrictDescriptor(path, "evaluation mutation", func(data []byte) (evaluation.Mutation, error) {
		return decodeStrictCommandJSON(
			data,
			"EvaluationMutation",
			func(value evaluation.Mutation) error { return value.Validate() },
		)
	})
}

func readEvaluationAccess(path string) (evaluation.Access, error) {
	return readStrictDescriptor(path, "evaluation access", func(data []byte) (evaluation.Access, error) {
		return decodeStrictCommandJSON(
			data,
			"EvaluationAccess",
			func(value evaluation.Access) error { return value.Validate() },
		)
	})
}

func readStrictDescriptor[T any](
	path string,
	label string,
	decode func([]byte) (T, error),
) (T, error) {
	var zero T
	if err := validateDescriptorPath(label, path); err != nil {
		return zero, err
	}
	data, err := readRegularFileLimit(path, maxControlPlaneDescriptorBytes)
	if err != nil {
		return zero, fmt.Errorf("read %s descriptor: %w", label, err)
	}
	value, err := decode(data)
	if err != nil {
		return zero, fmt.Errorf("decode %s descriptor: %w", label, err)
	}
	return value, nil
}

func decodeStrictCommandJSON[T any](
	data []byte,
	label string,
	validate func(T) error,
) (T, error) {
	var value T
	if err := rejectDuplicateCommandJSONFields(data); err != nil {
		return value, fmt.Errorf("decode %s: %w", label, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decode %s: %w", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return value, fmt.Errorf("decode %s: multiple JSON values are not allowed", label)
		}
		return value, fmt.Errorf("decode %s: invalid trailing JSON: %w", label, err)
	}
	if err := validate(value); err != nil {
		return value, fmt.Errorf("validate %s: %w", label, err)
	}
	return value, nil
}

func rejectDuplicateCommandJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := inspectCommandJSONValue(decoder); err != nil {
		return err
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		return fmt.Errorf("unexpected trailing JSON token %v", token)
	}
	return nil
}

func inspectCommandJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("read JSON object key: %w", err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := inspectCommandJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return fmt.Errorf("JSON object has invalid closing token")
		}
	case '[':
		for decoder.More() {
			if err := inspectCommandJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return fmt.Errorf("JSON array has invalid closing token")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}

func validateExplicitControlStore(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("--store is required; control-plane commands have no implicit state root")
	}
	if !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return "", fmt.Errorf("--store must be a clean absolute path")
	}
	return value, nil
}

func validateDescriptorPath(flagName string, value string) error {
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return fmt.Errorf("--%s must be a clean absolute path", flagName)
	}
	return nil
}

func openControlPlane(
	requestedStore string,
) (*controlplane.Service, string, error) {
	storePath, err := validateExplicitControlStore(requestedStore)
	if err != nil {
		return nil, "", err
	}
	store, err := local.Open(storePath)
	if err != nil {
		return nil, "", fmt.Errorf("open control-plane state %q: %w", storePath, err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		return nil, "", fmt.Errorf("open run repository: %w", err)
	}
	ledger, err := feedbackdomain.New(store)
	if err != nil {
		return nil, "", fmt.Errorf("open feedback repository: %w", err)
	}
	publications, err := publication.NewRepository(store)
	if err != nil {
		return nil, "", fmt.Errorf("open publication repository: %w", err)
	}
	decisions, err := findingdecision.New(store)
	if err != nil {
		return nil, "", fmt.Errorf("open finding decision repository: %w", err)
	}
	grants, err := publication.NewGrantRepository(store)
	if err != nil {
		return nil, "", fmt.Errorf("open publication grant repository: %w", err)
	}
	service, err := controlplane.NewWithPublicationGrants(
		runs, ledger, publications, decisions, grants,
	)
	if err != nil {
		return nil, "", fmt.Errorf("initialize control plane: %w", err)
	}
	lineages, err := findinglineage.New(store, runs, nil)
	if err != nil {
		return nil, "", fmt.Errorf("open finding lineage repository: %w", err)
	}
	service, err = service.WithFindingLineages(lineages)
	if err != nil {
		return nil, "", fmt.Errorf("configure finding lineage evaluation evidence: %w", err)
	}
	return service, store.Root(), nil
}

func openEvaluationRepository(
	requestedStore string,
) (*evaluation.Repository, string, error) {
	storePath, err := validateExplicitControlStore(requestedStore)
	if err != nil {
		return nil, "", err
	}
	store, err := local.Open(storePath)
	if err != nil {
		return nil, "", fmt.Errorf("open evaluation state %q: %w", storePath, err)
	}
	repository, err := evaluation.New(store)
	if err != nil {
		return nil, "", fmt.Errorf("open evaluation repository: %w", err)
	}
	return repository, store.Root(), nil
}

func writeEvaluationCaseOutput(
	stdout io.Writer,
	record evaluation.CaseRecord,
	storePath string,
	asJSON bool,
) error {
	if asJSON {
		return writeJSON(stdout, evaluationCaseOutput{
			Record: record, StorePath: storePath,
		})
	}
	_, err := fmt.Fprintf(
		stdout,
		"case=%s dataset_state=%s split=%s governance_revision=%d label_revision=%d store=%s\n",
		record.Case.CaseID,
		record.CurrentGovernance.DatasetState,
		record.CurrentGovernance.Split,
		record.CurrentGovernance.Revision,
		record.CurrentLabelRevision,
		storePath,
	)
	return err
}

func writeEvaluationTrustKeyOutput(
	stdout io.Writer,
	record evaluation.GovernanceTrustKeyRecord,
	storePath string,
	asJSON bool,
) error {
	if asJSON {
		return writeJSON(stdout, evaluationTrustKeyOutput{TrustKey: record, StorePath: storePath})
	}
	state := "active"
	if record.RevokedAt != nil {
		state = "revoked"
	}
	_, err := fmt.Fprintf(
		stdout,
		"governance_key=%s/%s/%s state=%s store=%s\n",
		record.Key.Authority,
		record.Key.KeyID,
		record.Key.Revision,
		state,
		storePath,
	)
	return err
}

func writeEvaluationGovernanceBatchOutput(
	stdout io.Writer,
	record evaluation.GovernanceBatchRecord,
	storePath string,
	asJSON bool,
) error {
	if asJSON {
		return writeJSON(stdout, evaluationGovernanceBatchOutput{Batch: record, StorePath: storePath})
	}
	completed := 0
	if record.Result != nil {
		for _, item := range record.Result.Items {
			if item.State == evaluation.GovernanceBatchItemSucceeded {
				completed++
			}
		}
	}
	_, err := fmt.Fprintf(
		stdout,
		"governance_batch=%s operation=%s status=%s completed=%d/%d store=%s\n",
		record.Request.BatchID,
		record.Request.Operation,
		record.Status,
		completed,
		len(record.Request.Items),
		storePath,
	)
	return err
}

func writeEvaluationRunOutput(
	stdout io.Writer,
	run evaluation.EvaluationRun,
	storePath string,
	asJSON bool,
) error {
	if asJSON {
		return writeJSON(stdout, evaluationRunOutput{Run: run, StorePath: storePath})
	}
	_, err := fmt.Fprintf(stdout,
		"evaluation_run=%s cases=%d passed=%d failed=%d inconclusive=%d store=%s\n",
		run.EvaluationRunID, run.Summary.Cases, run.Summary.Passed, run.Summary.Failed,
		run.Summary.Inconclusive, storePath)
	return err
}

func writeExperimentRunOutput(
	stdout io.Writer,
	run evaluation.ExperimentRun,
	storePath string,
	asJSON bool,
) error {
	if asJSON {
		return writeJSON(stdout, experimentRunOutput{Run: run, StorePath: storePath})
	}
	_, err := fmt.Fprintf(stdout,
		"experiment=%s cases=%d improved=%d regressed=%d inconclusive=%d store=%s\n",
		run.ExperimentRunID, run.Summary.Cases, run.Summary.Improved,
		run.Summary.Regressed, run.Summary.Inconclusive, storePath)
	return err
}

func writeRepeatabilityRunOutput(
	stdout io.Writer,
	run evaluation.RepeatabilityRun,
	storePath string,
	asJSON bool,
) error {
	if asJSON {
		return writeJSON(stdout, repeatabilityRunOutput{Run: run, StorePath: storePath})
	}
	_, err := fmt.Fprintf(stdout,
		"repeatability=%s cases=%d stable=%d unstable=%d indeterminate=%d verdict_flip_ppm=%d store=%s\n",
		run.RepeatabilityRunID, run.Summary.Cases, run.Summary.StableCases,
		run.Summary.UnstableCases, run.Summary.IndeterminateCases,
		run.Summary.VerdictFlipPairRatePPM, storePath)
	return err
}

func writePromotionOutput(
	stdout io.Writer,
	record evaluation.PromotionRecord,
	storePath string,
	asJSON bool,
) error {
	if asJSON {
		return writeJSON(stdout, promotionOutput{
			Record: record, StorePath: storePath,
		})
	}
	nextGate := "-"
	if record.NextGate != nil {
		nextGate = string(*record.NextGate)
	}
	_, err := fmt.Fprintf(
		stdout,
		"variant=%s status=%s next_gate=%s gates=%d store=%s\n",
		record.Variant.VariantID,
		record.Status,
		nextGate,
		len(record.Gates),
		storePath,
	)
	return err
}

func controlFlagError(command string, err error, commandUsage string) error {
	return fmt.Errorf("%s flags: %w\n%s", command, err, commandUsage)
}

func isHelpArgument(arguments []string) bool {
	return len(arguments) == 1 &&
		(arguments[0] == "-h" || arguments[0] == "--help")
}
