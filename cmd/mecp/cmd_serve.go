package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-ai/mecp/mcpserver"
	"github.com/urfave/cli/v3"
)

func mcpCommand() *cli.Command {
	return &cli.Command{
		Name:  "mcp",
		Usage: "serve the MCP tools over stdio",
		Description: `Runs the agent-facing MCP server. An agent host launches this as a subprocess.

The database is opened read-only unless the selected client profile holds the
propose capability, so a read-only agent cannot write to the store at all.

Stdout carries MCP protocol messages only; diagnostics go to stderr.`,
		Flags: append(globalFlags(),
			&cli.StringFlag{
				Name:    "client",
				Usage:   "client profile to apply, as named under clients: in the configuration",
				Value:   "default",
				Sources: cli.EnvVars("MECP_CLIENT"),
			},
			&cli.BoolFlag{
				Name:  "stdio",
				Usage: "serve over stdio (the only transport in this release)",
				Value: true,
			},
		),
		Action: runMCP,
	}
}

func runMCP(ctx context.Context, cmd *cli.Command) error {
	if !cmd.Bool("stdio") {
		return fmt.Errorf(`only the stdio transport is available in this release`)
	}

	clientID := cmd.String("client")

	// Load configuration once to decide whether this profile needs write
	// access, then open the store accordingly.
	probe, err := openRuntime(ctx, cmd, true)
	if err != nil {
		return err
	}
	caller := probe.cfg.Caller(clientID)
	needsWrite := caller.Has(mecp.CapPropose)
	if err := probe.Close(); err != nil {
		return err
	}

	rt, err := openRuntime(ctx, cmd, !needsWrite)
	if err != nil {
		return err
	}
	defer rt.Close()

	caller = rt.cfg.Caller(clientID)
	if err := caller.Validate(); err != nil {
		return fmt.Errorf(`client profile %q is not usable: %w`, clientID, err)
	}

	srv, err := mcpserver.New(rt.svc, caller)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "mecp: serving profile %q over stdio from %s\n", clientID, rt.store.Path())
	return srv.RunStdio(ctx)
}
