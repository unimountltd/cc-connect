//go:build windows

package main

import "fmt"

// runDoctor is a no-op on Windows — run_as_user isolation is not supported.
func runDoctor(args []string) {
	fmt.Println("doctor: run_as_user isolation checks are not supported on Windows")
}
