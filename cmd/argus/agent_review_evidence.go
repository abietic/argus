package main

import (
	"context"
	"fmt"
	"io"

	"argus.local/argus/internal/agentshadow"
	"argus.local/argus/internal/artifactrepo"
)

const agentReviewEvidenceUsage = `usage:
  argus agent-review evidence read --store <absolute-dir> --manifest-id <id> --request-id <id> --actor <id> --purpose <local_debug|evaluation_replay|incident_investigation> --at <RFC3339-UTC> --acknowledge-sensitive-output --json
  argus agent-review evidence revoke --store <absolute-dir> --manifest-id <id> --idempotency-key <id> --actor <id> --reason <text> --at <RFC3339-UTC> [--json]

Task evidence contains exact prompts, code context, tool arguments/results, and model output. Generic run/show JSON never includes it. Read requires an explicit purpose, acknowledgement, and a durable access receipt. Revoke creates an irreversible logical tombstone; shared content-addressed bytes remain until reference-aware garbage collection exists.`

type agentReviewEvidenceReadFlags struct {
	store       string
	manifestID  string
	requestID   string
	actor       string
	purpose     string
	at          string
	acknowledge bool
	json        bool
}

type agentReviewEvidenceRevokeFlags struct {
	store          string
	manifestID     string
	idempotencyKey string
	actor          string
	reason         string
	at             string
	json           bool
}

type agentReviewEvidenceRevokeOutput struct {
	ManifestID string                          `json:"manifest_id"`
	Evidence   agentshadow.TaskEvidenceSummary `json:"task_evidence"`
	StorePath  string                          `json:"store_path"`
}

func runAgentReviewEvidence(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%s", agentReviewEvidenceUsage)
	}
	switch arguments[0] {
	case "read":
		return runAgentReviewEvidenceRead(ctx, arguments[1:], stdout)
	case "revoke":
		return runAgentReviewEvidenceRevoke(ctx, arguments[1:], stdout)
	default:
		return fmt.Errorf("unknown agent-review evidence action %q\n%s", arguments[0], agentReviewEvidenceUsage)
	}
}

func runAgentReviewEvidenceRead(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	var options agentReviewEvidenceReadFlags
	flags := newFlagSet("agent-review evidence read")
	flags.StringVar(&options.store, "store", "", "absolute local Argus store")
	flags.StringVar(&options.manifestID, "manifest-id", "", "shadow result manifest ID")
	flags.StringVar(&options.requestID, "request-id", "", "unique sensitive access request ID")
	flags.StringVar(&options.actor, "actor", "", "human or service actor")
	flags.StringVar(&options.purpose, "purpose", "", "approved sensitive access purpose")
	flags.StringVar(&options.at, "at", "", "RFC3339 UTC authorization time")
	flags.BoolVar(&options.acknowledge, "acknowledge-sensitive-output", false, "acknowledge exact local sensitive bytes will be emitted")
	flags.BoolVar(&options.json, "json", false, "emit exact evidence JSON")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("agent-review evidence read flags: %w\n%s", err, agentReviewEvidenceUsage)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q\n%s", flags.Arg(0), agentReviewEvidenceUsage)
	}
	if err := validateRequiredAbsoluteStore(options.store); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"--manifest-id": options.manifestID,
		"--request-id":  options.requestID,
		"--actor":       options.actor,
		"--purpose":     options.purpose,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if !options.acknowledge || !options.json {
		return fmt.Errorf("sensitive evidence read requires --acknowledge-sensitive-output and --json")
	}
	at, err := parseAgentExecutionTime("at", options.at)
	if err != nil {
		return err
	}
	purpose, err := parseSensitiveAccessPurpose(options.purpose)
	if err != nil {
		return err
	}
	repository, err := openAgentShadowRepository(options.store)
	if err != nil {
		return err
	}
	service, err := agentshadow.NewService(repository)
	if err != nil {
		return err
	}
	result, err := service.ReadTaskEvidence(ctx, agentshadow.TaskEvidenceReadRequest{
		Scope: localAgentReviewScope(), ManifestID: options.manifestID,
		Access: artifactrepo.SensitiveAccessRequest{
			RequestID: options.requestID, Actor: options.actor,
			Purpose: purpose, At: at,
		},
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, result)
}

func runAgentReviewEvidenceRevoke(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	var options agentReviewEvidenceRevokeFlags
	flags := newFlagSet("agent-review evidence revoke")
	flags.StringVar(&options.store, "store", "", "absolute local Argus store")
	flags.StringVar(&options.manifestID, "manifest-id", "", "shadow result manifest ID")
	flags.StringVar(&options.idempotencyKey, "idempotency-key", "", "immutable revocation key")
	flags.StringVar(&options.actor, "actor", "", "revocation actor")
	flags.StringVar(&options.reason, "reason", "", "retention or incident reason")
	flags.StringVar(&options.at, "at", "", "RFC3339 UTC revocation time")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("agent-review evidence revoke flags: %w\n%s", err, agentReviewEvidenceUsage)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q\n%s", flags.Arg(0), agentReviewEvidenceUsage)
	}
	if err := validateRequiredAbsoluteStore(options.store); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"--manifest-id":     options.manifestID,
		"--idempotency-key": options.idempotencyKey,
		"--actor":           options.actor,
		"--reason":          options.reason,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	at, err := parseAgentExecutionTime("at", options.at)
	if err != nil {
		return err
	}
	repository, err := openAgentShadowRepository(options.store)
	if err != nil {
		return err
	}
	service, err := agentshadow.NewService(repository)
	if err != nil {
		return err
	}
	summary, err := service.RevokeTaskEvidence(ctx, agentshadow.TaskEvidenceRevokeRequest{
		Scope: localAgentReviewScope(), ManifestID: options.manifestID,
		IdempotencyKey: options.idempotencyKey, Actor: options.actor,
		Reason: options.reason, At: at,
	})
	if err != nil {
		return err
	}
	output := agentReviewEvidenceRevokeOutput{
		ManifestID: options.manifestID, Evidence: summary, StorePath: options.store,
	}
	if options.json {
		return writeJSON(stdout, output)
	}
	_, err = fmt.Fprintf(
		stdout,
		"agent task evidence revoked manifest=%s state=%s deletion=%s\n",
		options.manifestID,
		summary.State,
		summary.DeletionSemantics,
	)
	return err
}

func parseSensitiveAccessPurpose(value string) (artifactrepo.SensitiveAccessPurpose, error) {
	purpose := artifactrepo.SensitiveAccessPurpose(value)
	switch purpose {
	case artifactrepo.SensitiveAccessLocalDebug,
		artifactrepo.SensitiveAccessEvaluationReplay,
		artifactrepo.SensitiveAccessIncidentInvestigation:
		return purpose, nil
	default:
		return "", fmt.Errorf("--purpose must be local_debug, evaluation_replay, or incident_investigation")
	}
}

func localAgentReviewScope() agentshadow.Scope {
	return agentshadow.Scope{
		TenantID:    agentshadow.LocalTenantID,
		WorkspaceID: agentshadow.LocalWorkspaceID,
	}
}
