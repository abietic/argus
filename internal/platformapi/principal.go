package platformapi

import (
	"fmt"
	"os"
	"path/filepath"
)

const maxPrincipalBytes = 64 << 10

func LoadPrincipal(path string) (Principal, error) {
	var principal Principal
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return principal, fmt.Errorf("principal path must be a clean absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return principal, fmt.Errorf("stat principal file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return principal, fmt.Errorf("principal path must name a regular file, not a symlink")
	}
	if info.Size() > maxPrincipalBytes {
		return principal, fmt.Errorf("principal file exceeds %d bytes", maxPrincipalBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return principal, fmt.Errorf("read principal file: %w", err)
	}
	principal, err = DecodePrincipal(data)
	if err != nil {
		return Principal{}, fmt.Errorf("decode principal file: %w", err)
	}
	return principal, nil
}
