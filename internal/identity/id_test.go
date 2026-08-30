package identity

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestGeneratorNewIsStableWithInjectedDependencies(t *testing.T) {
	generator := Generator{
		Now: func() time.Time {
			return time.Date(2026, time.July, 26, 12, 30, 45, 123, time.UTC)
		},
		Random: bytes.NewReader([]byte{0, 1, 2, 3, 4, 5, 6, 7}),
	}
	id, err := generator.New("run")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	const want = "run-20260726t123045.000000123z-0001020304050607"
	if id != want {
		t.Fatalf("New() = %q, want %q", id, want)
	}
}

func TestGeneratorRejectsUnsafePrefixAndShortEntropy(t *testing.T) {
	tests := []struct {
		name      string
		prefix    string
		random    []byte
		wantError string
	}{
		{name: "path", prefix: "../run", random: make([]byte, 8), wantError: "prefix"},
		{name: "short entropy", prefix: "run", random: []byte{1}, wantError: "random"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			generator := Generator{
				Now:    func() time.Time { return time.Now().UTC() },
				Random: bytes.NewReader(test.random),
			}
			_, err := generator.New(test.prefix)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("New() error = %v, want %q", err, test.wantError)
			}
		})
	}
}
