//go:build !darwin && !linux

package main

import (
	"fmt"
	"runtime"
)

func renameDashboardDirectoryNoReplace(source, target string) error {
	return fmt.Errorf(
		"atomic no-replace directory publication is unsupported on %s: %q -> %q",
		runtime.GOOS,
		source,
		target,
	)
}
