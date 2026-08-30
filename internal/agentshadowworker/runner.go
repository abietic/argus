// Package agentshadowworker runs the local Pi review worker over a bounded
// stdio protocol. The worker is deliberately treated as an untrusted local
// subprocess: it receives one frozen request, an explicit environment, and no
// shell expansion.
package agentshadowworker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	DefaultMaxStderrBytes       = 64 << 10
	DefaultTerminationWait      = 2 * time.Second
	DefaultMaxProgressLineBytes = 24 << 20
)

var (
	ErrOutputLimit = errors.New("agent review worker output exceeds its limit")
	ErrWorkerExit  = errors.New("agent review worker exited unsuccessfully")
)

// Request is the transport-level subprocess request. Input is already a
// validated worker protocol envelope. Environment is an explicit allowlist,
// never inherited from the host process.
type Request struct {
	NodePath       string
	ScriptPath     string
	Input          []byte
	Environment    map[string]string
	MaxInputBytes  int64
	MaxOutputBytes int64
	MaxStderrBytes int64
	// ProgressLine receives one stderr JSONL record without the trailing LF.
	// It is used for durable checkpoints; callers must treat the bytes as
	// untrusted and sensitive. A callback error fails the worker execution.
	ProgressLine func([]byte) error
}

type Result struct {
	Stdout       []byte
	StderrSHA256 string
	StderrBytes  int64
	StartedAt    time.Time
	FinishedAt   time.Time
}

// Runner permits deterministic bridge tests without invoking a provider or a
// real subprocess.
type Runner interface {
	Run(context.Context, Request) (Result, error)
}

type SubprocessRunner struct {
	TerminationWait time.Duration
	now             func() time.Time
}

func NewSubprocessRunner() *SubprocessRunner {
	return &SubprocessRunner{
		TerminationWait: DefaultTerminationWait,
		now:             time.Now,
	}
}

func (runner *SubprocessRunner) Run(
	ctx context.Context,
	request Request,
) (Result, error) {
	if ctx == nil {
		return Result{}, fmt.Errorf("worker context is required")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if runner == nil {
		return Result{}, fmt.Errorf("worker runner is required")
	}
	if err := validateRegularAbsolute("node", request.NodePath); err != nil {
		return Result{}, err
	}
	if err := validateRegularAbsolute("worker script", request.ScriptPath); err != nil {
		return Result{}, err
	}
	if request.MaxInputBytes <= 0 || request.MaxOutputBytes <= 0 {
		return Result{}, fmt.Errorf("worker input/output limits must be positive")
	}
	if int64(len(request.Input)) > request.MaxInputBytes {
		return Result{}, fmt.Errorf("worker input exceeds %d bytes", request.MaxInputBytes)
	}
	if request.MaxStderrBytes == 0 {
		request.MaxStderrBytes = DefaultMaxStderrBytes
	}
	if request.MaxStderrBytes < 0 {
		return Result{}, fmt.Errorf("worker stderr limit must be positive")
	}
	environment, err := sanitizedEnvironment(request.Environment)
	if err != nil {
		return Result{}, err
	}

	stdout := newLimitedBuffer(request.MaxOutputBytes, true)
	// stderr carries progress from the worker. Keep bounded diagnostic bytes,
	// but continue draining after truncation so tool-heavy valid executions do
	// not become unknown-outcome failures merely due to progress volume.
	stderr := newLimitedBuffer(request.MaxStderrBytes, false)
	command := exec.Command(request.NodePath, request.ScriptPath)
	command.Args = []string{request.NodePath, request.ScriptPath}
	command.Dir = string(os.PathSeparator)
	command.Env = environment
	command.Stdin = bytes.NewReader(request.Input)
	command.Stdout = stdout
	progress := newProgressLineWriter(stderr, request.ProgressLine, DefaultMaxProgressLineBytes)
	command.Stderr = progress
	configureProcessGroup(command)

	now := runner.now
	if now == nil {
		now = time.Now
	}
	startedAt := normalizedTime(now())
	if err := command.Start(); err != nil {
		return Result{}, fmt.Errorf("start agent review worker: %w", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()

	var waitErr error
	select {
	case waitErr = <-wait:
	case <-ctx.Done():
		terminateProcessGroup(command.Process.Pid)
		terminationWait := runner.TerminationWait
		if terminationWait <= 0 {
			terminationWait = DefaultTerminationWait
		}
		timer := time.NewTimer(terminationWait)
		select {
		case waitErr = <-wait:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			killProcessGroup(command.Process.Pid)
			waitErr = <-wait
		}
		_ = waitErr
		return Result{}, ctx.Err()
	}
	finishedAt := normalizedTime(now())
	result := Result{
		Stdout:       stdout.Bytes(),
		StderrSHA256: stderr.SHA256(),
		StderrBytes:  stderr.TotalBytes(),
		StartedAt:    startedAt,
		FinishedAt:   finishedAt,
	}
	if progressErr := progress.Err(); progressErr != nil {
		return result, fmt.Errorf("persist agent review worker progress: %w", progressErr)
	}
	if stdout.Exceeded() || errors.Is(waitErr, errLimitExceeded) {
		return result, ErrOutputLimit
	}
	if waitErr != nil {
		// Worker stderr is intentionally not reflected into the error: it is
		// untrusted and may contain credentials. Its bounded digest/size remain
		// available to trusted diagnostics.
		return result, fmt.Errorf("%w: %v", ErrWorkerExit, waitErr)
	}
	return result, nil
}

type progressLineWriter struct {
	mu          sync.Mutex
	destination io.Writer
	callback    func([]byte) error
	maximum     int
	pending     []byte
	err         error
}

func newProgressLineWriter(
	destination io.Writer,
	callback func([]byte) error,
	maximum int,
) *progressLineWriter {
	return &progressLineWriter{
		destination: destination, callback: callback, maximum: maximum,
		pending: []byte{},
	}
}

func (writer *progressLineWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.err != nil {
		return 0, writer.err
	}
	if writer.destination != nil {
		if _, err := writer.destination.Write(data); err != nil {
			writer.err = err
			return 0, err
		}
	}
	if writer.callback == nil {
		return len(data), nil
	}
	writer.pending = append(writer.pending, data...)
	for {
		newline := bytes.IndexByte(writer.pending, '\n')
		if newline < 0 {
			if len(writer.pending) > writer.maximum {
				writer.err = fmt.Errorf("worker progress line exceeds %d bytes", writer.maximum)
				return 0, writer.err
			}
			return len(data), nil
		}
		line := bytes.Clone(writer.pending[:newline])
		writer.pending = append(writer.pending[:0], writer.pending[newline+1:]...)
		if len(line) > writer.maximum {
			writer.err = fmt.Errorf("worker progress line exceeds %d bytes", writer.maximum)
			return 0, writer.err
		}
		if err := writer.callback(line); err != nil {
			writer.err = err
			return 0, err
		}
	}
}

func (writer *progressLineWriter) Err() error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.err
}

var allowedEnvironment = map[string]struct{}{
	"LANG": {}, "LC_ALL": {}, "PATH": {},
	"ANTHROPIC_API_KEY": {}, "ANTHROPIC_BASE_URL": {},
}

func sanitizedEnvironment(values map[string]string) ([]string, error) {
	keys := make([]string, 0, len(values))
	for key, value := range values {
		if _, ok := allowedEnvironment[key]; !ok {
			return nil, fmt.Errorf("worker environment variable %q is not allowed", key)
		}
		if strings.ContainsRune(key, '=') || strings.ContainsRune(key, '\x00') ||
			strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("worker environment variable %q is invalid", key)
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment, nil
}

func validateRegularAbsolute(label, path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("%s path must be clean and absolute", label)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s path: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%s path must be a regular non-symlink file", label)
	}
	return nil
}

var errLimitExceeded = errors.New("bounded worker stream exceeded")

type limitedBuffer struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	maximum  int64
	total    int64
	exceeded bool
	hard     bool
}

func newLimitedBuffer(maximum int64, hard bool) *limitedBuffer {
	return &limitedBuffer{maximum: maximum, hard: hard}
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	if int64(len(data)) > int64(^uint64(0)>>1)-buffer.total {
		buffer.total = int64(^uint64(0) >> 1)
	} else {
		buffer.total += int64(len(data))
	}
	remaining := buffer.maximum - int64(buffer.buffer.Len())
	if remaining <= 0 {
		buffer.exceeded = true
		if !buffer.hard {
			return len(data), nil
		}
		return 0, errLimitExceeded
	}
	toWrite := data
	if int64(len(toWrite)) > remaining {
		toWrite = toWrite[:remaining]
		buffer.exceeded = true
	}
	written, err := buffer.buffer.Write(toWrite)
	if err != nil {
		return written, err
	}
	if !buffer.hard && written != len(data) {
		return len(data), nil
	}
	if written != len(data) {
		return written, errLimitExceeded
	}
	return written, nil
}

func (buffer *limitedBuffer) Bytes() []byte {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return bytes.Clone(buffer.buffer.Bytes())
}

func (buffer *limitedBuffer) TotalBytes() int64 {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.total
}

func (buffer *limitedBuffer) Exceeded() bool {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.exceeded
}

func (buffer *limitedBuffer) SHA256() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return digestBytes(buffer.buffer.Bytes())
}

func normalizedTime(value time.Time) time.Time {
	return time.Unix(0, value.UnixNano()).UTC()
}

// Ensure limitedBuffer continues to satisfy the exact os/exec sink contract.
var _ io.Writer = (*limitedBuffer)(nil)
