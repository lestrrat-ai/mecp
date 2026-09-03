package main

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// The CLI fills in workspace details from the surrounding Git checkout so that
// a diagnostic run matches what an agent host would report. Every helper fails
// quietly: an absent or broken repository means "unknown", not an error.

func discoverRemote(dir string) string { return gitOutput(dir, "remote", "get-url", "origin") }

func discoverRevision(dir string) string { return gitOutput(dir, "rev-parse", "HEAD") }

func discoverBranch(dir string) string {
	return gitOutput(dir, "rev-parse", "--abbrev-ref", "HEAD")
}

func gitOutput(dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
