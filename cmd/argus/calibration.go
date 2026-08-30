package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"argus.local/argus/internal/analyticsadapter"
	"argus.local/argus/internal/calibration"
	"argus.local/argus/internal/calibrationpromotion"
	"argus.local/argus/internal/configrepo"
	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/promotionmonitor"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
)

const calibrationUsage = `usage:
  argus calibration fit --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus calibration show --store <absolute-dir> --run <id> --access <absolute-json> [--json]
  argus calibration list --store <absolute-dir> --access <absolute-json> [--json]
  argus calibration promotion prepare --store <absolute-dir> --config-state-dir <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus calibration promotion gate --store <absolute-dir> --config-state-dir <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus calibration promotion activate --store <absolute-dir> --config-state-dir <absolute-dir> --plan <id> --percentage <1-100> [--seed <seed>] --mutation <absolute-json> [--json]
  argus calibration promotion rollback --store <absolute-dir> --config-state-dir <absolute-dir> --plan <id> --mutation <absolute-json> [--json]
  argus calibration promotion show --store <absolute-dir> --config-state-dir <absolute-dir> --plan <id> --access <absolute-json> [--json]
  argus calibration promotion list --store <absolute-dir> --config-state-dir <absolute-dir> --access <absolute-json> [--json]
  argus calibration promotion observe --store <absolute-dir> --config-state-dir <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus calibration promotion observation --store <absolute-dir> --config-state-dir <absolute-dir> --observation <id> --access <absolute-json> [--json]
  argus calibration promotion observations --store <absolute-dir> --config-state-dir <absolute-dir> --access <absolute-json> [--json]`

type calibrationOutput struct {
	Run       calibration.Run `json:"run"`
	StorePath string          `json:"store_path"`
}

type calibrationListOutput struct {
	Runs      []calibration.Run `json:"runs"`
	StorePath string            `json:"store_path"`
}

type calibrationPromotionOutput struct {
	Plan            calibrationpromotion.Plan `json:"plan"`
	StorePath       string                    `json:"store_path"`
	ConfigStatePath string                    `json:"config_state_path"`
}

type calibrationPromotionListOutput struct {
	Plans           []calibrationpromotion.Plan `json:"plans"`
	StorePath       string                      `json:"store_path"`
	ConfigStatePath string                      `json:"config_state_path"`
}

type calibrationPromotionObservationOutput struct {
	Observation     promotionmonitor.Observation `json:"observation"`
	StorePath       string                       `json:"store_path"`
	ConfigStatePath string                       `json:"config_state_path"`
}

type calibrationPromotionObservationListOutput struct {
	Observations    []promotionmonitor.Summary `json:"observations"`
	StorePath       string                     `json:"store_path"`
	ConfigStatePath string                     `json:"config_state_path"`
}

func runCalibration(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(calibrationUsage)
	}
	if isHelpArgument(arguments) {
		_, err := fmt.Fprintln(stdout, calibrationUsage)
		return err
	}
	switch arguments[0] {
	case "promotion":
		return runCalibrationPromotion(ctx, arguments[1:], stdout)
	case "fit":
		options, err := parseGovernedWriteFlags("calibration fit", arguments[1:], calibrationUsage)
		if err != nil {
			return err
		}
		request, err := readStrictDescriptor(options.input, "calibration fit request", calibration.DecodeFitRequest)
		if err != nil {
			return err
		}
		mutation, err := readEvaluationMutation(options.mutation)
		if err != nil {
			return err
		}
		repository, storePath, err := openCalibrationRepository(options.store)
		if err != nil {
			return err
		}
		run, err := repository.Fit(ctx, request, mutation)
		if err != nil {
			return err
		}
		return writeCalibrationRun(stdout, run, storePath, options.json)
	case "show":
		options, err := parseGovernedReadFlags("calibration show", arguments[1:], calibrationUsage, "run", "")
		if err != nil {
			return err
		}
		access, err := readEvaluationAccess(options.access)
		if err != nil {
			return err
		}
		repository, storePath, err := openCalibrationRepository(options.store)
		if err != nil {
			return err
		}
		run, err := repository.Get(options.id, access)
		if err != nil {
			return err
		}
		return writeCalibrationRun(stdout, run, storePath, options.json)
	case "list":
		options, err := parseGovernedReadFlags("calibration list", arguments[1:], calibrationUsage, "", "")
		if err != nil {
			return err
		}
		access, err := readEvaluationAccess(options.access)
		if err != nil {
			return err
		}
		repository, storePath, err := openCalibrationRepository(options.store)
		if err != nil {
			return err
		}
		runs, err := repository.List(access)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, calibrationListOutput{Runs: runs, StorePath: storePath})
		}
		for _, run := range runs {
			if _, err := fmt.Fprintf(stdout, "run=%s profile=%s@%s status=%s report=%s\n", run.RunID, run.ProfileCandidate.Profile.ID, run.ProfileCandidate.Profile.Revision, run.ProfileCandidate.Status, run.Report.ReportID); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(stdout, "store=%s\n", storePath)
		return err
	default:
		return fmt.Errorf("unknown calibration command %q\n%s", arguments[0], calibrationUsage)
	}
}

type calibrationPromotionFlags struct {
	store, configState, input, mutation, access, plan, observation, seed string
	percentage                                                           int
	json                                                                 bool
}

func runCalibrationPromotion(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(calibrationUsage)
	}
	command := arguments[0]
	flags := newFlagSet("calibration promotion " + command)
	var options calibrationPromotionFlags
	flags.StringVar(&options.store, "store", "", "absolute evaluation store")
	flags.StringVar(&options.configState, "config-state-dir", "", "absolute config lifecycle store")
	flags.StringVar(&options.input, "input", "", "absolute request JSON")
	flags.StringVar(&options.mutation, "mutation", "", "absolute mutation JSON")
	flags.StringVar(&options.access, "access", "", "absolute access JSON")
	flags.StringVar(&options.plan, "plan", "", "promotion plan ID")
	flags.StringVar(&options.observation, "observation", "", "promotion observation ID")
	flags.StringVar(&options.seed, "seed", "", "rollout assignment seed")
	flags.IntVar(&options.percentage, "percentage", 100, "rollout percentage")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments[1:]); err != nil {
		return fmt.Errorf("calibration promotion %s flags: %w\n%s", command, err, calibrationUsage)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("calibration promotion %s accepts flags only", command)
	}
	service, storePath, configPath, err := openCalibrationPromotionService(options.store, options.configState)
	if err != nil {
		return err
	}
	write := func(plan calibrationpromotion.Plan) error {
		if options.json {
			return writeJSON(stdout, calibrationPromotionOutput{Plan: plan, StorePath: storePath, ConfigStatePath: configPath})
		}
		_, err := fmt.Fprintf(stdout, "plan=%s status=%s calibration=%s config=%s@%s promotion=%s promotion_status=%s store=%s config_state=%s\n", plan.Request.PlanID, plan.Status, plan.Request.CalibrationRunID, plan.VariantConfig.ID, plan.VariantConfig.Revision, plan.PromotionVariant.VariantID, plan.PromotionStatus, storePath, configPath)
		return err
	}
	switch command {
	case "prepare":
		request, err := readStrictDescriptor(options.input, "calibration promotion prepare request", calibrationpromotion.DecodePrepareRequest)
		if err != nil {
			return err
		}
		mutation, err := readEvaluationMutation(options.mutation)
		if err != nil {
			return err
		}
		plan, err := service.Prepare(ctx, request, mutation)
		if err != nil {
			return err
		}
		return write(plan)
	case "gate":
		request, err := readStrictDescriptor(options.input, "calibration promotion gate request", calibrationpromotion.DecodeGateRequest)
		if err != nil {
			return err
		}
		mutation, err := readEvaluationMutation(options.mutation)
		if err != nil {
			return err
		}
		plan, err := service.RecordGate(ctx, request, mutation)
		if err != nil {
			return err
		}
		return write(plan)
	case "activate":
		if options.plan == "" || options.mutation == "" {
			return errors.New(calibrationUsage)
		}
		mutation, err := readEvaluationMutation(options.mutation)
		if err != nil {
			return err
		}
		plan, err := service.Activate(ctx, options.plan, configrepo.Rollout{Percentage: options.percentage, Seed: options.seed}, mutation)
		if err != nil {
			return err
		}
		return write(plan)
	case "rollback":
		if options.plan == "" || options.mutation == "" {
			return errors.New(calibrationUsage)
		}
		mutation, err := readEvaluationMutation(options.mutation)
		if err != nil {
			return err
		}
		plan, err := service.Rollback(ctx, options.plan, mutation)
		if err != nil {
			return err
		}
		return write(plan)
	case "show":
		if options.plan == "" {
			return errors.New(calibrationUsage)
		}
		access, err := readEvaluationAccess(options.access)
		if err != nil {
			return err
		}
		plan, err := service.Get(options.plan, access)
		if err != nil {
			return err
		}
		return write(plan)
	case "list":
		access, err := readEvaluationAccess(options.access)
		if err != nil {
			return err
		}
		plans, err := service.List(access)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, calibrationPromotionListOutput{Plans: plans, StorePath: storePath, ConfigStatePath: configPath})
		}
		for _, plan := range plans {
			if _, err := fmt.Fprintf(stdout, "plan=%s status=%s promotion=%s\n", plan.Request.PlanID, plan.Status, plan.PromotionVariant.VariantID); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(stdout, "plans=%s store=%s config_state=%s\n", strconv.Itoa(len(plans)), storePath, configPath)
		return err
	case "observe":
		request, err := readStrictDescriptor(options.input, "calibration promotion observation request", promotionmonitor.DecodeBuildRequest)
		if err != nil {
			return err
		}
		mutation, err := readEvaluationMutation(options.mutation)
		if err != nil {
			return err
		}
		monitor, err := openPromotionMonitor(storePath, service)
		if err != nil {
			return err
		}
		observation, err := monitor.Build(ctx, request, mutation)
		if err != nil {
			return err
		}
		return writePromotionObservation(stdout, observation, storePath, configPath, options.json)
	case "observation":
		if options.observation == "" {
			return errors.New(calibrationUsage)
		}
		access, err := readEvaluationAccess(options.access)
		if err != nil {
			return err
		}
		monitor, err := openPromotionMonitorQuery(storePath)
		if err != nil {
			return err
		}
		observation, err := monitor.Get(options.observation, access)
		if err != nil {
			return err
		}
		return writePromotionObservation(stdout, observation, storePath, configPath, options.json)
	case "observations":
		access, err := readEvaluationAccess(options.access)
		if err != nil {
			return err
		}
		monitor, err := openPromotionMonitorQuery(storePath)
		if err != nil {
			return err
		}
		observations, err := monitor.List(access)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, calibrationPromotionObservationListOutput{Observations: observations, StorePath: storePath, ConfigStatePath: configPath})
		}
		for _, observation := range observations {
			if _, err := fmt.Fprintf(stdout, "observation=%s plan=%s status=%s rollback_recommended=%t sha256=%s\n", observation.ObservationID, observation.PlanID, observation.Status, observation.RollbackRecommendation, observation.ObservationSHA256); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(stdout, "observations=%s store=%s config_state=%s\n", strconv.Itoa(len(observations)), storePath, configPath)
		return err
	default:
		return fmt.Errorf("unknown calibration promotion command %q\n%s", command, calibrationUsage)
	}
}

func openPromotionMonitor(storePath string, plans *calibrationpromotion.Service) (*promotionmonitor.Service, error) {
	store, err := local.Open(storePath)
	if err != nil {
		return nil, err
	}
	snapshots, err := analyticsadapter.Open(store)
	if err != nil {
		return nil, err
	}
	return promotionmonitor.New(store, plans, snapshots)
}

func openPromotionMonitorQuery(storePath string) (*promotionmonitor.Service, error) {
	store, err := local.Open(storePath)
	if err != nil {
		return nil, err
	}
	return promotionmonitor.Open(store)
}

func writePromotionObservation(stdout io.Writer, observation promotionmonitor.Observation, storePath, configPath string, asJSON bool) error {
	if asJSON {
		return writeJSON(stdout, calibrationPromotionObservationOutput{Observation: observation, StorePath: storePath, ConfigStatePath: configPath})
	}
	_, err := fmt.Fprintf(stdout, "observation=%s plan=%s status=%s rollback_recommended=%t store=%s config_state=%s\n", observation.Request.ObservationID, observation.Request.PlanID, observation.Status, observation.RollbackRecommendation, storePath, configPath)
	return err
}

func openCalibrationPromotionService(storeInput, configInput string) (*calibrationpromotion.Service, string, string, error) {
	storePath, err := validateExplicitControlStore(storeInput)
	if err != nil {
		return nil, "", "", err
	}
	configPath, err := validateExplicitControlStore(configInput)
	if err != nil {
		return nil, "", "", fmt.Errorf("config-state-dir: %w", err)
	}
	store, err := local.Open(storePath)
	if err != nil {
		return nil, "", "", err
	}
	evaluationRepository, err := evaluation.New(store)
	if err != nil {
		return nil, "", "", err
	}
	runs, err := runrepo.New(store)
	if err != nil {
		return nil, "", "", err
	}
	calibrationRepository, err := calibration.New(store, evaluationRepository, runs)
	if err != nil {
		return nil, "", "", err
	}
	configStore, err := local.Open(configPath)
	if err != nil {
		return nil, "", "", err
	}
	configRepository, err := configrepo.New(configStore)
	if err != nil {
		return nil, "", "", err
	}
	service, err := calibrationpromotion.New(store, calibrationRepository, configRepository, evaluationRepository, runs)
	if err != nil {
		return nil, "", "", err
	}
	return service, storePath, configPath, nil
}

func openCalibrationRepository(path string) (*calibration.Repository, string, error) {
	storePath, err := validateExplicitControlStore(path)
	if err != nil {
		return nil, "", err
	}
	store, err := local.Open(storePath)
	if err != nil {
		return nil, "", err
	}
	cases, err := evaluation.New(store)
	if err != nil {
		return nil, "", err
	}
	runs, err := runrepo.New(store)
	if err != nil {
		return nil, "", err
	}
	repository, err := calibration.New(store, cases, runs)
	if err != nil {
		return nil, "", err
	}
	return repository, storePath, nil
}

func writeCalibrationRun(stdout io.Writer, run calibration.Run, storePath string, asJSON bool) error {
	if asJSON {
		return writeJSON(stdout, calibrationOutput{Run: run, StorePath: storePath})
	}
	_, err := fmt.Fprintf(stdout, "run=%s profile=%s@%s candidate=%s status=%s report=%s auto_published=%t store=%s\n", run.RunID, run.ProfileCandidate.Profile.ID, run.ProfileCandidate.Profile.Revision, run.ProfileCandidate.CandidateID, run.ProfileCandidate.Status, run.Report.ReportID, run.ProfileCandidate.AutoPublished, storePath)
	return err
}
