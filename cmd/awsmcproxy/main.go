package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/alecthomas/kong"
	"github.com/winebarrel/awsmcproxy"
)

var (
	version string
)

func parseArgs() *awsmcproxy.Options {
	var cli struct {
		awsmcproxy.Options
		Version kong.VersionFlag
	}

	parser := kong.Must(&cli, kong.Vars{
		"version":  version,
		"endpoint": awsmcproxy.DefaultEndpoint,
	})
	parser.Model.HelpFlag.Help = "Show help."
	_, err := parser.Parse(os.Args[1:])
	parser.FatalIfErrorf(err)

	return &cli.Options
}

func main() {
	// Logs must go to stderr; stdout is reserved for the MCP stdio transport.
	log.SetOutput(os.Stderr)

	options := parseArgs()

	proxy, err := awsmcproxy.NewProxy(options, version)

	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := proxy.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
