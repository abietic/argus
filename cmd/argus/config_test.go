package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abietic/argus/internal/configrepo"
	"github.com/abietic/argus/internal/reviewconfig"
)

func TestConfigCLILifecycleIsExplicitStrictAndIdempotent(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	revisionFile := filepath.Join(t.TempDir(), "revision.json")
	maxFiles := 17
	revision := reviewconfig.Revision{
		SchemaVersion: reviewconfig.RevisionSchemaVersion,
		ID:            "platform",
		Revision:      "1",
		Scope:         reviewconfig.ScopePlatform,
		Selector:      reviewconfig.Selector{},
		Patch: reviewconfig.ConfigPatch{
			Target: &reviewconfig.TargetPatch{MaxFiles: &maxFiles},
		},
	}
	data, err := json.Marshal(revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(revisionFile, data, 0o600); err != nil {
		t.Fatal(err)
	}

	runConfigCommand(t, []string{
		"config", "create",
		"--state-dir", stateDir,
		"--file", revisionFile,
		"--idempotency-key", "create-platform-1",
		"--actor", "tester",
		"--audit", "create baseline",
		"--at", "2026-07-27T16:00:01Z",
		"--json",
	})
	runConfigCommand(t, []string{
		"config", "validate",
		"--state-dir", stateDir,
		"--id", "platform",
		"--revision", "1",
		"--idempotency-key", "validate-platform-1",
		"--actor", "tester",
		"--audit", "validation passed",
		"--at", "2026-07-27T16:00:02Z",
		"--json",
	})
	publish := []string{
		"config", "activate",
		"--state-dir", stateDir,
		"--id", "platform",
		"--revision", "1",
		"--percentage", "100",
		"--idempotency-key", "publish-platform-1",
		"--actor", "tester",
		"--audit", "activate baseline",
		"--at", "2026-07-27T16:00:03Z",
		"--json",
	}
	published := runConfigCommand(t, publish)
	if published.Record.Status != configrepo.StatusPublished {
		t.Fatalf("published status = %q", published.Record.Status)
	}
	// The canonical lifecycle name and activate alias converge on the same
	// idempotent event.
	publish[1] = "publish"
	retried := runConfigCommand(t, publish)
	if retried.Record.SHA256 != published.Record.SHA256 {
		t.Fatal("idempotent publish retry changed revision")
	}

	var show configShowOutput
	decodeConfigCommand(t, []string{
		"config", "show",
		"--state-dir", stateDir,
		"--id", "platform",
		"--revision", "1",
		"--json",
	}, &show)
	canonicalState, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if show.Record.Status != configrepo.StatusPublished ||
		len(show.History) != 3 ||
		show.StateDir != canonicalState {
		t.Fatalf("config show = %+v", show)
	}
	var listed struct {
		Records  []configrepo.Record `json:"records"`
		StateDir string              `json:"state_dir"`
	}
	decodeConfigCommand(t, []string{
		"config", "list", "--state-dir", stateDir, "--json",
	}, &listed)
	if len(listed.Records) != 1 ||
		listed.Records[0].Status != configrepo.StatusPublished ||
		listed.StateDir != canonicalState {
		t.Fatalf("config list = %+v", listed)
	}
}

func TestConfigCLIHasNoImplicitStateAndRejectsUnknownJSONBeforeCreation(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runWithIO(
		context.Background(),
		[]string{"config", "list", "--json"},
		&output,
	)
	if err == nil || !strings.Contains(err.Error(), "no implicit state directory") {
		t.Fatalf("config list without state error = %v", err)
	}

	root := t.TempDir()
	stateDir := filepath.Join(root, "not-created")
	revisionFile := filepath.Join(root, "invalid.json")
	if err := os.WriteFile(
		revisionFile,
		[]byte(`{"schema_version":"argus.config_revision.v1alpha1","unknown":true}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	err = runWithIO(context.Background(), []string{
		"config", "create",
		"--state-dir", stateDir,
		"--file", revisionFile,
		"--idempotency-key", "invalid-create",
		"--actor", "tester",
		"--audit", "must fail",
		"--at", "2026-07-27T16:01:00Z",
	}, &output)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("config create unknown JSON error = %v", err)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("invalid config input created state directory: %v", err)
	}
}

func TestConfigCLIAdvanceWidensExistingCanary(t *testing.T) {
	stateDir := t.TempDir()
	writeRevision := func(id, revision string, maxFiles int) string {
		path := filepath.Join(t.TempDir(), id+"-"+revision+".json")
		data, err := json.Marshal(reviewconfig.Revision{SchemaVersion: reviewconfig.RevisionSchemaVersion, ID: id, Revision: revision, Scope: reviewconfig.ScopePlatform, Selector: reviewconfig.Selector{}, Patch: reviewconfig.ConfigPatch{Target: &reviewconfig.TargetPatch{MaxFiles: &maxFiles}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	mutate := func(action, revision, key, at string, extra ...string) configMutationOutput {
		arguments := []string{"config", action, "--state-dir", stateDir, "--id", "platform", "--revision", revision, "--idempotency-key", key, "--actor", "tester", "--audit", key, "--at", at, "--json"}
		arguments = append(arguments, extra...)
		return runConfigCommand(t, arguments)
	}
	for index, revision := range []string{"1", "2"} {
		runConfigCommand(t, []string{"config", "create", "--state-dir", stateDir, "--file", writeRevision("platform", revision, 100+index), "--idempotency-key", "create-" + revision, "--actor", "tester", "--audit", "create", "--at", "2026-08-27T12:00:0" + revision + "Z", "--json"})
		mutate("validate", revision, "validate-"+revision, "2026-08-27T12:00:1"+revision+"Z")
	}
	mutate("publish", "1", "publish-1", "2026-08-27T12:00:21Z", "--percentage", "100")
	mutate("publish", "2", "publish-2", "2026-08-27T12:00:22Z", "--percentage", "10", "--seed", "stable-seed")
	advanced := mutate("advance", "2", "advance-2", "2026-08-27T12:00:23Z", "--percentage", "40", "--seed", "stable-seed")
	if advanced.Record.Status != configrepo.StatusPublished {
		t.Fatalf("advanced status = %q", advanced.Record.Status)
	}
	var shown configShowOutput
	decodeConfigCommand(t, []string{"config", "show", "--state-dir", stateDir, "--id", "platform", "--revision", "2", "--json"}, &shown)
	last := shown.History[len(shown.History)-1]
	if last.Type != configrepo.EventRolloutAdvanced || last.Rollout == nil || last.Rollout.Percentage != 40 {
		t.Fatalf("advance history = %+v", last)
	}
}

func TestReviewWithExplicitEmptyConfigStateFailsClosed(t *testing.T) {
	t.Parallel()
	repositoryPath := newCLITargetRepository(t)
	writeCLITargetFile(t, repositoryPath, "review.go", "package fixture\n")
	base := commitCLITarget(t, repositoryPath, "base")
	writeCLITargetFile(
		t,
		repositoryPath,
		"review.go",
		"package fixture\n// ARGUS_BUG published policy required\n",
	)
	head := commitCLITarget(t, repositoryPath, "head")
	var output bytes.Buffer
	err := runWithIO(context.Background(), []string{
		"review",
		"--repo", repositoryPath,
		"--mode", "diff",
		"--base", base,
		"--head", head,
		"--store", t.TempDir(),
		"--config-state-dir", t.TempDir(),
		"--json",
	}, &output)
	if !errors.Is(err, configrepo.ErrNoPublishedConfig) {
		t.Fatalf("review with empty config state error = %v", err)
	}
}

func runConfigCommand(
	t *testing.T,
	arguments []string,
) configMutationOutput {
	t.Helper()
	var output configMutationOutput
	decodeConfigCommand(t, arguments, &output)
	return output
}

func decodeConfigCommand(t *testing.T, arguments []string, output any) {
	t.Helper()
	var buffer bytes.Buffer
	if err := runWithIO(context.Background(), arguments, &buffer); err != nil {
		t.Fatalf("runWithIO(%s) error = %v", strings.Join(arguments[:2], " "), err)
	}
	decoder := json.NewDecoder(&buffer)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		t.Fatalf("decode command output: %v\n%s", err, buffer.String())
	}
}
