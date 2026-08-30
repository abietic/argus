package main

import (
	"context"
	"fmt"
	"io"

	"argus.local/argus/internal/artifactrepo"
	"argus.local/argus/internal/formalevidence"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
)

type formalEvidenceReadFlags struct {
	store       string
	formalRun   string
	requestID   string
	actor       string
	purpose     string
	at          string
	acknowledge bool
	json        bool
}

type formalEvidenceRevokeFlags struct {
	store          string
	formalRun      string
	idempotencyKey string
	actor          string
	reason         string
	at             string
	json           bool
}

type formalEvidenceRevokeOutput struct {
	FormalRunID string                 `json:"formal_run_id"`
	Evidence    formalevidence.Summary `json:"task_evidence"`
	StorePath   string                 `json:"store_path"`
}

func runFormalAgentEvidence(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%s", formalAgentReviewUsage)
	}
	switch arguments[0] {
	case "read":
		return runFormalAgentEvidenceRead(ctx, arguments[1:], stdout)
	case "revoke":
		return runFormalAgentEvidenceRevoke(ctx, arguments[1:], stdout)
	default:
		return fmt.Errorf("unknown agent-review formal evidence action %q\n%s", arguments[0], formalAgentReviewUsage)
	}
}

func runFormalAgentEvidenceRead(ctx context.Context, arguments []string, stdout io.Writer) error {
	var options formalEvidenceReadFlags
	flags := newFlagSet("agent-review formal evidence read")
	flags.StringVar(&options.store, "store", "", "absolute local Argus store")
	flags.StringVar(&options.formalRun, "formal-run", "", "committed formal ReviewRun ID")
	flags.StringVar(&options.requestID, "request-id", "", "unique sensitive access request ID")
	flags.StringVar(&options.actor, "actor", "", "human or service actor")
	flags.StringVar(&options.purpose, "purpose", "", "approved sensitive access purpose")
	flags.StringVar(&options.at, "at", "", "RFC3339 UTC authorization time")
	flags.BoolVar(&options.acknowledge, "acknowledge-sensitive-output", false, "acknowledge exact local sensitive bytes will be emitted")
	flags.BoolVar(&options.json, "json", false, "emit exact evidence JSON")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("agent-review formal evidence read flags: %w\n%s", err, formalAgentReviewUsage)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q\n%s", flags.Arg(0), formalAgentReviewUsage)
	}
	if err := validateRequiredAbsoluteStore(options.store); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"--formal-run": options.formalRun, "--request-id": options.requestID,
		"--actor": options.actor, "--purpose": options.purpose,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if !options.acknowledge || !options.json {
		return fmt.Errorf("formal sensitive evidence read requires --acknowledge-sensitive-output and --json")
	}
	at, err := parseAgentExecutionTime("at", options.at)
	if err != nil {
		return err
	}
	purpose, err := parseSensitiveAccessPurpose(options.purpose)
	if err != nil {
		return err
	}
	service, err := openFormalEvidenceService(options.store)
	if err != nil {
		return err
	}
	result, err := service.Read(ctx, formalevidence.ReadRequest{
		FormalRunID: options.formalRun,
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

func runFormalAgentEvidenceRevoke(ctx context.Context, arguments []string, stdout io.Writer) error {
	var options formalEvidenceRevokeFlags
	flags := newFlagSet("agent-review formal evidence revoke")
	flags.StringVar(&options.store, "store", "", "absolute local Argus store")
	flags.StringVar(&options.formalRun, "formal-run", "", "committed formal ReviewRun ID")
	flags.StringVar(&options.idempotencyKey, "idempotency-key", "", "immutable revocation key")
	flags.StringVar(&options.actor, "actor", "", "revocation actor")
	flags.StringVar(&options.reason, "reason", "", "retention or incident reason")
	flags.StringVar(&options.at, "at", "", "RFC3339 UTC revocation time")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("agent-review formal evidence revoke flags: %w\n%s", err, formalAgentReviewUsage)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q\n%s", flags.Arg(0), formalAgentReviewUsage)
	}
	if err := validateRequiredAbsoluteStore(options.store); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"--formal-run": options.formalRun, "--idempotency-key": options.idempotencyKey,
		"--actor": options.actor, "--reason": options.reason,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	at, err := parseAgentExecutionTime("at", options.at)
	if err != nil {
		return err
	}
	service, err := openFormalEvidenceService(options.store)
	if err != nil {
		return err
	}
	summary, err := service.Revoke(ctx, formalevidence.RevokeRequest{
		FormalRunID: options.formalRun, IdempotencyKey: options.idempotencyKey,
		Actor: options.actor, Reason: options.reason, At: at,
	})
	if err != nil {
		return err
	}
	output := formalEvidenceRevokeOutput{
		FormalRunID: options.formalRun, Evidence: summary, StorePath: options.store,
	}
	if options.json {
		return writeJSON(stdout, output)
	}
	_, err = fmt.Fprintf(
		stdout,
		"formal agent task evidence revoked run=%s state=%s deletion=%s\n",
		options.formalRun,
		summary.State,
		summary.DeletionSemantics,
	)
	return err
}

func openFormalEvidenceService(storePath string) (*formalevidence.Service, error) {
	store, err := local.Open(storePath)
	if err != nil {
		return nil, fmt.Errorf("open Argus store: %w", err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		return nil, err
	}
	artifacts, err := artifactrepo.Open(store, nil)
	if err != nil {
		return nil, err
	}
	return formalevidence.New(runs, artifacts)
}
