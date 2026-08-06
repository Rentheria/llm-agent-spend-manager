// Command llm-agent-spend-manager is the CLI entry point: it serves the local
// dashboard (`serve`) and prints a text summary (`status`). Every $ it shows is
// an estimated-equivalent cost, never money charged — the covered agents run on
// flat-price subscriptions (see internal/aggregate and docs/architecture.md).
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

const version = "0.0.1-dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}

func run(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("llm-agent-spend-manager", flag.ContinueOnError)
	fs.SetOutput(out)
	showVersion := fs.Bool("version", false, "print the version and exit")

	fs.Usage = func() {
		fmt.Fprintln(out, "Usage: llm-agent-spend-manager <command> [flags]")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Commands:")
		fmt.Fprintln(out, "  serve     serve the dashboard + JSON API on localhost (--lan exposes the network, with a token)")
		fmt.Fprintln(out, "  status    print the per-agent estimated-equivalent cost as a table")
		fmt.Fprintln(out, "  quota     how much of the quota window is left, who is eating it, and what stretches it")
		fmt.Fprintln(out, "  advise    where the cost really goes, is efficiency improving, and what to change")
		fmt.Fprintln(out, "  outcome   which real changes were made, and whether a metric changed level after them")
		fmt.Fprintln(out, "  proxy     run the combined-cap enforcement proxy on loopback (Phase 3)")
		fmt.Fprintln(out)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *showVersion {
		fmt.Fprintln(out, "llm-agent-spend-manager", version)
		return 0
	}

	remaining := fs.Args()
	if len(remaining) == 0 {
		fs.Usage()
		return 0
	}

	switch remaining[0] {
	case "serve":
		return cmdServe(remaining[1:], out)
	case "status":
		return cmdStatus(remaining[1:], out)
	case "quota":
		return cmdQuota(remaining[1:], out)
	case "advise":
		return cmdAdvise(remaining[1:], out)
	case "outcome":
		return cmdOutcome(remaining[1:], out)
	case "proxy":
		return cmdProxy(remaining[1:], out)
	default:
		fmt.Fprintf(out, "unknown command: %s\n\n", remaining[0])
		fs.Usage()
		return 2
	}
}
