package agentshadowworker

import (
	"crypto/sha256"
	"encoding/hex"
)

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
