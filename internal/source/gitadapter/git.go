package gitadapter

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	maxCommandOutputBytes = 1 << 20
	maxCommandStderrBytes = 64 << 10
)

var stableGitArgs = []string{
	"--no-pager",
	"-c", "color.ui=false",
	"-c", "core.quotePath=false",
	"-c", "core.fsmonitor=false",
	"-c", "core.untrackedCache=false",
	"-c", "core.attributesFile=/dev/null",
	"-c", "diff.algorithm=myers",
	"-c", "diff.indentHeuristic=false",
	"-c", "diff.mnemonicPrefix=false",
	"-c", "diff.noprefix=false",
	"-c", "diff.renames=true",
	"-c", "diff.renameLimit=32767",
}

type limitedBuffer struct {
	limit    int
	data     []byte
	overflow bool
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	originalLength := len(data)
	remaining := buffer.limit - len(buffer.data)
	if remaining > 0 {
		if len(data) > remaining {
			buffer.data = append(buffer.data, data[:remaining]...)
		} else {
			buffer.data = append(buffer.data, data...)
		}
	}
	if originalLength > remaining {
		buffer.overflow = true
	}
	return originalLength, nil
}

type anyOutputWriter struct {
	any bool
}

func (writer *anyOutputWriter) Write(data []byte) (int, error) {
	if len(data) != 0 {
		writer.any = true
	}
	return len(data), nil
}

func (adapter *Adapter) run(
	ctx context.Context,
	directory string,
	args []string,
	stdout io.Writer,
) error {
	commandArgs := make([]string, 0, len(stableGitArgs)+len(args))
	commandArgs = append(commandArgs, stableGitArgs...)
	commandArgs = append(commandArgs, args...)
	command := exec.CommandContext(ctx, adapter.gitPath, commandArgs...)
	command.Dir = directory
	command.Env = stableGitEnvironment()
	command.Stdout = stdout
	stderr := &limitedBuffer{limit: maxCommandStderrBytes}
	command.Stderr = stderr
	err := command.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		message := strings.TrimSuffix(string(stderr.data), "\n")
		if stderr.overflow {
			message += " [stderr omitted after limit]"
		}
		if message == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, message)
	}
	return nil
}

func (adapter *Adapter) runOutput(
	ctx context.Context,
	directory string,
	args ...string,
) ([]byte, error) {
	stdout := &limitedBuffer{limit: maxCommandOutputBytes}
	if err := adapter.run(ctx, directory, args, stdout); err != nil {
		return nil, err
	}
	if stdout.overflow {
		return nil, fmt.Errorf("git command output exceeds %d bytes", maxCommandOutputBytes)
	}
	return bytes.Clone(stdout.data), nil
}

func stableGitEnvironment() []string {
	environment := os.Environ()
	filtered := make([]string, 0, len(environment)+8)
	for _, variable := range environment {
		name, _, _ := strings.Cut(variable, "=")
		if strings.HasPrefix(name, "GIT_") ||
			strings.HasPrefix(name, "LC_") ||
			name == "LANG" ||
			name == "LANGUAGE" {
			continue
		}
		filtered = append(filtered, variable)
	}
	return append(filtered,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"GIT_LITERAL_PATHSPECS=1",
		"GIT_NO_REPLACE_OBJECTS=1",
		"LC_ALL=C",
	)
}

// newIsolatedObjectView creates a minimal bare Git directory that borrows only
// the repository's immutable object database. It deliberately has no
// worktree, index, config, hooks, or info/attributes, so exact tree-to-tree
// diffs cannot drift when mutable repository-local state changes.
func (adapter *Adapter) newIsolatedObjectView(
	ctx context.Context,
	repositoryRoot string,
	objectFormat string,
) (string, func(), error) {
	rawObjectPath, err := adapter.runOutput(
		ctx,
		repositoryRoot,
		"rev-parse",
		"--git-path",
		"objects",
	)
	if err != nil {
		return "", nil, fmt.Errorf("locate Git object database: %w", err)
	}
	objectPath := strings.TrimSpace(string(rawObjectPath))
	if objectPath == "" || strings.ContainsAny(objectPath, "\r\n") {
		return "", nil, fmt.Errorf("Git object database path is invalid")
	}
	if !filepath.IsAbs(objectPath) {
		objectPath = filepath.Join(repositoryRoot, objectPath)
	}
	objectPath, err = filepath.EvalSymlinks(objectPath)
	if err != nil {
		return "", nil, fmt.Errorf("resolve Git object database: %w", err)
	}
	info, err := os.Stat(objectPath)
	if err != nil || !info.IsDir() {
		return "", nil, fmt.Errorf("Git object database is not a directory")
	}

	view, err := os.MkdirTemp("", "argus-git-object-view-")
	if err != nil {
		return "", nil, fmt.Errorf("create isolated Git object view: %w", err)
	}
	cleanup := func() {
		_ = os.RemoveAll(view)
	}
	fail := func(err error) (string, func(), error) {
		cleanup()
		return "", nil, err
	}
	for _, directory := range []string{
		filepath.Join(view, "objects", "info"),
		filepath.Join(view, "objects", "pack"),
		filepath.Join(view, "refs"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fail(fmt.Errorf("initialize isolated Git object view: %w", err))
		}
	}
	repositoryVersion := "0"
	extension := ""
	switch objectFormat {
	case "sha1":
	case "sha256":
		repositoryVersion = "1"
		extension = "[extensions]\n\tobjectFormat = sha256\n"
	default:
		return fail(fmt.Errorf("unsupported Git object format %q", objectFormat))
	}
	config := fmt.Sprintf(
		"[core]\n\trepositoryFormatVersion = %s\n\tbare = true\n%s",
		repositoryVersion,
		extension,
	)
	files := map[string][]byte{
		filepath.Join(view, "HEAD"):                          []byte("ref: refs/heads/__argus_unborn__\n"),
		filepath.Join(view, "config"):                        []byte(config),
		filepath.Join(view, "objects", "info", "alternates"): []byte(objectPath + "\n"),
	}
	for path, data := range files {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return fail(fmt.Errorf("write isolated Git object view: %w", err))
		}
	}
	return view, cleanup, nil
}
