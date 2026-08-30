package identity

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"
)

type Generator struct {
	Now    func() time.Time
	Random io.Reader
}

func NewGenerator() Generator {
	return Generator{Now: time.Now, Random: rand.Reader}
}

func (generator Generator) New(prefix string) (string, error) {
	if prefix == "" || strings.ContainsAny(prefix, `/\:`) ||
		prefix != strings.TrimSpace(prefix) {
		return "", fmt.Errorf("invalid id prefix %q", prefix)
	}
	if generator.Now == nil || generator.Random == nil {
		return "", fmt.Errorf("id generator dependencies are required")
	}
	random := make([]byte, 8)
	if _, err := io.ReadFull(generator.Random, random); err != nil {
		return "", fmt.Errorf("read random id bytes: %w", err)
	}
	now := generator.Now().UTC()
	if now.IsZero() {
		return "", fmt.Errorf("id clock returned zero time")
	}
	return fmt.Sprintf("%s-%s-%s", prefix, now.Format("20060102t150405.000000000z"), hex.EncodeToString(random)), nil
}
