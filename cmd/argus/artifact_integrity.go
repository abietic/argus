package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
)

const artifactUsage = `usage:
  argus artifact integrity inspect --store <absolute-dir> --ref <absolute-json> [--json]
  argus artifact integrity <quarantine|release|tombstone> --store <absolute-dir> --input <absolute-json> [--json]`

type artifactIntegrityFlags struct {
	store string
	input string
	ref   string
	json  bool
}

type artifactIntegrityOutput struct {
	Record    runrepo.ArtifactIntegrityRecord `json:"record"`
	StorePath string                          `json:"store_path"`
}

func runArtifact(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(artifactUsage)
	}
	if isHelpArgument(arguments) {
		_, err := fmt.Fprintln(stdout, artifactUsage)
		return err
	}
	if arguments[0] != "integrity" || len(arguments) < 2 {
		return fmt.Errorf("unknown artifact command\n%s", artifactUsage)
	}
	action := arguments[1]
	if action != "inspect" && action != "quarantine" && action != "release" && action != "tombstone" {
		return fmt.Errorf("unknown artifact integrity action %q\n%s", action, artifactUsage)
	}
	options, err := parseArtifactIntegrityFlags(action, arguments[2:])
	if err != nil {
		return err
	}
	repository, storePath, err := openArtifactIntegrityRepository(options.store)
	if err != nil {
		return err
	}
	var record runrepo.ArtifactIntegrityRecord
	if action == "inspect" {
		ref, readErr := readStrictDescriptor(options.ref, "artifact ref", func(data []byte) (runmodel.ArtifactRef, error) {
			return decodeStrictCommandJSON(data, "ArtifactRef", func(value runmodel.ArtifactRef) error {
				return value.Validate()
			})
		})
		if readErr != nil {
			return readErr
		}
		record, err = repository.InspectArtifactIntegrity(ref)
	} else {
		change, readErr := readStrictDescriptor(options.input, "artifact integrity change", func(data []byte) (runrepo.ArtifactIntegrityChange, error) {
			return decodeStrictCommandJSON(data, "ArtifactIntegrityChange", func(value runrepo.ArtifactIntegrityChange) error {
				return value.Validate()
			})
		})
		if readErr != nil {
			return readErr
		}
		switch action {
		case "quarantine":
			record, err = repository.QuarantineArtifact(ctx, change.Ref, change.Reason, change.Mutation)
		case "release":
			record, err = repository.ReleaseArtifactQuarantine(ctx, change.Ref, change.Reason, change.Mutation)
		case "tombstone":
			record, err = repository.TombstoneArtifact(ctx, change.Ref, change.Reason, change.Mutation)
		}
	}
	if err != nil {
		return err
	}
	output := artifactIntegrityOutput{Record: record, StorePath: storePath}
	if options.json {
		return writeJSON(stdout, output)
	}
	_, err = fmt.Fprintf(
		stdout, "artifact=%s contract=%s state=%s sequence=%d store=%s\n",
		record.Ref.SHA256, record.Ref.Contract, record.State, record.Sequence, storePath,
	)
	return err
}

func parseArtifactIntegrityFlags(action string, arguments []string) (artifactIntegrityFlags, error) {
	var options artifactIntegrityFlags
	flags := newFlagSet("artifact integrity " + action)
	flags.StringVar(&options.store, "store", "", "explicit local state root")
	if action == "inspect" {
		flags.StringVar(&options.ref, "ref", "", "absolute strict JSON ArtifactRef descriptor")
	} else {
		flags.StringVar(&options.input, "input", "", "absolute strict JSON integrity change descriptor")
	}
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return artifactIntegrityFlags{}, controlFlagError("artifact integrity "+action, err, artifactUsage)
	}
	if flags.NArg() != 0 {
		return artifactIntegrityFlags{}, controlFlagError(
			"artifact integrity "+action,
			fmt.Errorf("unexpected argument %q", flags.Arg(0)), artifactUsage,
		)
	}
	if _, err := validateExplicitControlStore(options.store); err != nil {
		return artifactIntegrityFlags{}, err
	}
	if action == "inspect" {
		if err := validateDescriptorPath("ref", options.ref); err != nil {
			return artifactIntegrityFlags{}, err
		}
	} else if err := validateDescriptorPath("input", options.input); err != nil {
		return artifactIntegrityFlags{}, err
	}
	return options, nil
}

func openArtifactIntegrityRepository(
	requestedStore string,
) (*runrepo.Repository, string, error) {
	storePath, err := validateExplicitControlStore(requestedStore)
	if err != nil {
		return nil, "", err
	}
	store, err := local.Open(storePath)
	if err != nil {
		return nil, "", fmt.Errorf("open artifact integrity state %q: %w", storePath, err)
	}
	repository, err := runrepo.New(store)
	if err != nil {
		return nil, "", fmt.Errorf("open run repository: %w", err)
	}
	return repository, store.Root(), nil
}
