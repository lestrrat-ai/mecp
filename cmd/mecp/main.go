// Command mecp is the single executable for the mecp context broker: the MCP
// server an agent host launches, and the administrative interface the user
// drives.
//
// The two roles share one database, one schema, and one policy configuration,
// which is what makes the design "centralized at the data layer without a
// permanently running daemon".
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "0.1.0-dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := rootCommand().Run(ctx, os.Args); err != nil {
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		// Errors go to stderr unconditionally: in MCP stdio mode, stdout
		// carries protocol messages and anything else there corrupts them.
		fmt.Fprintf(os.Stderr, "mecp: %s\n", err)
		os.Exit(1)
	}
}

func rootCommand() *cli.Command {
	return &cli.Command{
		Name:                  "mecp",
		Usage:                 "personal context broker for coding agents",
		Version:               version,
		Flags:                 globalFlags(),
		EnableShellCompletion: true,
		Description: `mecp stores the user's durable preferences, decisions, constraints, and project
history, and serves the slice of it that applies to a concrete coding task.

Agents reach it over MCP ("mecp mcp"). The user curates it with the record,
review, import, and export commands.`,
		Commands: []*cli.Command{
			initCommand(),
			mcpCommand(),
			prepareCommand(),
			searchCommand(),
			recordCommand(),
			reviewCommand(),
			importCommand(),
			exportCommand(),
			validateCommand(),
			auditCommand(),
		},
	}
}
