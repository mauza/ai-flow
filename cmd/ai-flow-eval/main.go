// ai-flow-eval evaluates exported runs. It never submits work to a server.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/mauza/ai-flow/internal/eval"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("ai-flow-eval", flag.ContinueOnError)
	flags.SetOutput(stderr)
	suite := flags.String("suite", "", "evaluation suite JSON (required; capture paths relative to this file)")
	baseline := flags.String("baseline", "", "prior JSON report with matching case IDs and scoring rules")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: ai-flow-eval -suite suite.json [-baseline report.json] > report.json")
		fmt.Fprintln(stderr, "Offline export scoring. Exit 0: all pass; 1: scoring failure; 2: invalid input/I/O.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *suite == "" || flags.NArg() != 0 {
		flags.Usage()
		return 2
	}
	report, err := eval.EvaluateFile(*suite)
	if err == nil && *baseline != "" {
		err = eval.CompareFile(report, *baseline)
	}
	if err != nil {
		fmt.Fprintln(stderr, "ai-flow-eval:", err)
		return 2
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		fmt.Fprintln(stderr, "ai-flow-eval:", err)
		return 2
	}
	fmt.Fprintf(stderr, "%s (%s): %d/%d eval cases passed; %d/%d runs succeeded\n",
		report.Suite, report.Provenance, report.Summary.Passed, report.Summary.Total, report.Summary.RunSucceeded, report.Summary.Total)
	if report.Summary.Passed != report.Summary.Total {
		return 1
	}
	return 0
}
