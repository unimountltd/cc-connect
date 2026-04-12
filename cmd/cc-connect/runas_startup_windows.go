//go:build windows

package main

import (
	"context"

	"github.com/chenhg5/cc-connect/config"
)

// runRunAsUserStartupChecks is a no-op on Windows — run_as_user isolation is
// not supported.
func runRunAsUserStartupChecks(_ context.Context, _ *config.Config) error {
	return nil
}
