package gocompile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

var ErrCommandOutputLimit = errors.New("Go compile command output limit exceeded")

type LocalRunner struct {
	goPath   string
	identity Toolchain
}

func NewLocalRunner() (*LocalRunner, error) {
	goPath, err := exec.LookPath("go")
	if err != nil {
		return nil, fmt.Errorf("locate Go toolchain: %w", err)
	}
	goPath, err = filepath.Abs(goPath)
	if err != nil || filepath.Clean(goPath) != goPath {
		return nil, fmt.Errorf("resolve Go toolchain path")
	}
	info, err := os.Lstat(goPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("Go toolchain must be an absolute regular non-symlink file")
	}
	identity := Toolchain{
		GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		CGOEnabled: false, RepositoryCodeExecute: false,
		ModuleNetwork: "disabled_goproxy_off", Authority: "local_host_unattested",
	}
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	return &LocalRunner{goPath: goPath, identity: identity}, nil
}

func (runner *LocalRunner) Identity() Toolchain {
	if runner == nil {
		return Toolchain{}
	}
	return runner.identity
}

func (runner *LocalRunner) Compile(ctx context.Context, request CompileRequest) (CompileResult, error) {
	if runner == nil || runner.goPath == "" || ctx == nil {
		return CompileResult{}, fmt.Errorf("Go compile runner dependencies are required")
	}
	if err := ctx.Err(); err != nil {
		return CompileResult{}, err
	}
	if request.Workspace == "" || !filepath.IsAbs(request.Workspace) ||
		filepath.Clean(request.Workspace) != request.Workspace || !validPackage(request.Package) ||
		request.OutputPath == "" || !filepath.IsAbs(request.OutputPath) ||
		request.MaxOutputBytes <= 0 {
		return CompileResult{}, fmt.Errorf("Go compile request is invalid")
	}
	relativeOutput, err := filepath.Rel(request.Workspace, request.OutputPath)
	if err != nil || relativeOutput == "." || strings.HasPrefix(relativeOutput, ".."+string(filepath.Separator)) {
		return CompileResult{}, fmt.Errorf("Go compile output escaped workspace")
	}
	cacheRoot := filepath.Join(request.Workspace, ".argus-cache")
	for _, directory := range []string{
		cacheRoot, filepath.Join(cacheRoot, "home"), filepath.Join(cacheRoot, "tmp"),
		filepath.Join(cacheRoot, "go-cache"), filepath.Join(cacheRoot, "mod-cache"),
		filepath.Join(cacheRoot, "gopath"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return CompileResult{}, fmt.Errorf("create isolated Go cache: %w", err)
		}
	}
	moduleMode := "readonly"
	if request.VendorMode {
		moduleMode = "vendor"
	}
	commandContext, cancelCommand := context.WithCancel(ctx)
	defer cancelCommand()
	command := exec.CommandContext(
		commandContext, runner.goPath, "test", "-c", "-vet=off", "-mod="+moduleMode,
		"-buildvcs=false", "-trimpath", "-o", request.OutputPath, request.Package,
	)
	command.Dir = request.Workspace
	configureCompileCommand(command)
	command.Env = []string{
		"CGO_ENABLED=0", "GO111MODULE=on", "GOENV=off", "GONOSUMDB=*", "GOPROXY=off",
		"GOSUMDB=off", "GOTOOLCHAIN=local", "GOWORK=off", "HOME=" + filepath.Join(cacheRoot, "home"),
		"LANG=C", "LC_ALL=C", "GOCACHE=" + filepath.Join(cacheRoot, "go-cache"),
		"GOMODCACHE=" + filepath.Join(cacheRoot, "mod-cache"), "GOPATH=" + filepath.Join(cacheRoot, "gopath"),
		"TMPDIR=" + filepath.Join(cacheRoot, "tmp"),
	}
	readPipe, writePipe, err := os.Pipe()
	if err != nil {
		return CompileResult{}, fmt.Errorf("open bounded compile output pipe: %w", err)
	}
	command.Stdout, command.Stderr = writePipe, writePipe
	if err := command.Start(); err != nil {
		_ = readPipe.Close()
		_ = writePipe.Close()
		return CompileResult{}, fmt.Errorf("start Go compile: %w", err)
	}
	_ = writePipe.Close()
	var output bytes.Buffer
	copyDone := make(chan error, 1)
	limitExceeded := make(chan struct{}, 1)
	go func() {
		written, copyErr := io.CopyN(&output, readPipe, request.MaxOutputBytes+1)
		if written > request.MaxOutputBytes {
			output.Truncate(int(request.MaxOutputBytes))
			limitExceeded <- struct{}{}
			cancelCommand()
			_, _ = io.Copy(io.Discard, readPipe)
		}
		copyDone <- copyErr
	}()
	waitErr := command.Wait()
	copyErr := <-copyDone
	_ = readPipe.Close()
	select {
	case <-limitExceeded:
		return CompileResult{}, ErrCommandOutputLimit
	default:
	}
	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		return CompileResult{}, fmt.Errorf("read Go compile output: %w", copyErr)
	}
	if ctx.Err() != nil {
		return CompileResult{}, ctx.Err()
	}
	result := CompileResult{ExitCode: 0, Output: output.Bytes()}
	if waitErr == nil {
		return result, nil
	}
	var exitError *exec.ExitError
	if errors.As(waitErr, &exitError) {
		result.ExitCode = exitError.ExitCode()
		return result, nil
	}
	return CompileResult{}, fmt.Errorf("wait for Go compile: %w", waitErr)
}
