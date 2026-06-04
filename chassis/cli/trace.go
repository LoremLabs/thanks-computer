package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// runTrace fetches one execution trace from a running chassis admin
// endpoint and prints a terminal-friendly summary.
//
//	txco trace [--target NAME] [--addr URL] [--user USER] [--pass PASS]
//	           [--json] [--verbose|-v] [--step <scope>[-name]] <rid>
//
// Default render is a request header + a one-row-per-step table where
// each row leads with `<scope-padded>-<name>` (matching the on-disk
// folder naming under trace_dir/requests/<rid>/steps/). When two
// consecutive same-scope rows actually overlap in wall-clock time, a
// `# parallel ×N` comment line marks the group.
func runTrace(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("trace", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", "", "target name from txco.yaml (default: the config target, or dev)")
	addr := fs.String("addr", "", "chassis admin endpoint (overrides the target's chassis URL)")
	user := fs.String("user", "", "basic auth user (overrides the target's user)")
	pass := fs.String("pass", "", "basic auth password (overrides the target's pass)")
	profile := fs.String("profile", "", fmt.Sprintf("signing profile (TXCO_PROFILE, then %s/active, then \"local\")", auth.HomePathPretty()))
	asJSON := fs.Bool("json", false, "print the aggregate JSON response as-is")
	verbose := fs.BoolP("verbose", "v", false, "include in/out payloads (requires --trace-mode=full)")
	step := fs.String("step", "", "drill into a single step (e.g. \"100\" or \"100-hello\")")
	plain := fs.Bool("plain", false, "force plain text output even on a TTY (skips the curses UI)")
	grep := fs.String("grep", "", "list mode only: filter to traces whose step name or operation contains this substring")
	watch := fs.Int("watch", 0, "list mode only: auto-reload every N seconds (TUI only; uses ETags for cheap polling)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco trace [flags] [<rid>]

With <rid>: fetch and render the execution trace for that request id.
Without <rid>: launch the interactive trace browser (TTY required) —
arrow keys navigate recent traces, Enter inspects one, Esc returns.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}

	// Workspace dir for txco.yaml lookup — walk up to the OPS/ root so trace
	// resolves the workspace target from a subdirectory too.
	dir, err := workspaceDir("")
	if err != nil {
		fmt.Fprintf(stderr, "trace: resolve dir: %v\n", err)
		return 1
	}

	t := resolveTarget(dir, *target, *addr, *user, *pass, *profile)
	c := client.New(t)
	// Trace reads are tenant-scoped by default (the caller's membership caps
	// apply, like every other CLI command). A super-admin should see every
	// tenant, so auto-detect via whoami and switch to the chassis-wide view —
	// no flag. A whoami failure (unsigned/basic-auth target, older chassis)
	// leaves the safe tenant-scoped default.
	if who, werr := c.Whoami(context.Background()); werr == nil && who.SuperAdmin {
		c.SetTraceAllTenants(true)
	}

	// No-rid mode: list the recent traces and let the user pick one.
	if fs.NArg() == 0 {
		return runTraceListMode(c, stdout, stderr, *plain, *asJSON, *grep, *watch)
	}
	rid := fs.Arg(0)

	// Shortcut: `txco trace last` resolves to the most recent trace's
	// RID via the same /traces/requests.json endpoint the list view
	// uses. Saves copy-pasting RIDs from log lines for the common
	// "what did that last request do?" question.
	if rid == "last" {
		list, lerr := c.ListTraces(context.Background(), 1, "")
		if lerr != nil {
			fmt.Fprintf(stderr, "trace last: list recent: %v\n", lerr)
			return 1
		}
		if list == nil || len(list.Traces) == 0 {
			fmt.Fprintln(stderr, "trace last: no traces recorded yet")
			return 1
		}
		rid = list.Traces[0].RID
		fmt.Fprintf(stderr, "trace last → %s\n", rid)
	}

	// Pick TUI when stdout is a terminal and the user hasn't asked for
	// plain output via --json, --step, --verbose, or --plain. The TUI
	// always shows payloads, so it implies include=full on the fetch.
	useTUI := !*plain && !*asJSON && !*verbose && *step == "" && banner.IsTTY(stdout)

	resp, raw, err := c.GetTrace(context.Background(), rid, *verbose || *asJSON || useTUI)
	if err != nil {
		var nf *client.TraceNotFoundError
		if errors.As(err, &nf) {
			fmt.Fprintf(stderr, "trace: %v\n", nf)
			fmt.Fprintf(stderr, "       (is the chassis running with --trace-mode=full or summary?)\n")
			return 1
		}
		fmt.Fprintf(stderr, "trace: %v\n", err)
		return 1
	}

	if *asJSON {
		var buf bytes.Buffer
		if err := json.Indent(&buf, raw, "", "  "); err != nil {
			_, _ = stdout.Write(raw)
		} else {
			_, _ = stdout.Write(buf.Bytes())
			_, _ = stdout.Write([]byte{'\n'})
		}
		return 0
	}

	if *step != "" {
		return printStep(stdout, stderr, resp, *step, *verbose)
	}

	if useTUI {
		if err := runTraceTUI(resp, rid); err != nil {
			fmt.Fprintf(stderr, "trace: %v\n", err)
			return 1
		}
		return 0
	}

	printSummary(stdout, resp)
	printSteps(stdout, resp.Steps)

	if *verbose {
		if resp.TraceMode != "full" {
			fmt.Fprintf(stderr, "trace: --verbose requested but chassis trace_mode=%q (no payload data on disk)\n",
				traceModeOrUnset(resp.TraceMode))
		} else {
			printPayloads(stdout, resp)
		}
	}
	return 0
}

func printSummary(w io.Writer, r *client.TraceResponse) {
	fmt.Fprintf(w, "trace %s\n", r.RID)
	if r.Src != "" {
		fmt.Fprintf(w, "  src      %s\n", r.Src)
	}
	if r.Tenant != "" {
		fmt.Fprintf(w, "  tenant   %s\n", r.Tenant)
	}
	if s := routeOrStack(r.Route, r.Stack); s != "" {
		fmt.Fprintf(w, "  stack    %s\n", s)
	}
	if r.StartedAt != "" {
		fmt.Fprintf(w, "  started  %s\n", r.StartedAt)
	}
	dur := "--"
	if r.DurationMs != nil {
		dur = fmt.Sprintf("%dms", *r.DurationMs)
	}
	fmt.Fprintf(w, "  status   %-12s duration %s\n", r.Status, dur)
	if r.PayloadBytes > 0 {
		trunc := ""
		if r.PayloadTruncated {
			trunc = " (truncated)"
		}
		fmt.Fprintf(w, "  payload  %s%s\n", humanBytes(r.PayloadBytes), trunc)
	}
	if r.BytesIn > 0 || r.BytesOut > 0 {
		fmt.Fprintf(w, "  bytes    %s → %s\n", humanBytes(r.BytesIn), humanBytes(r.BytesOut))
	}
	if r.Fuel > 0 {
		fmt.Fprintf(w, "  fuel     %d\n", r.Fuel)
	}
	fmt.Fprintln(w)
}

func printSteps(w io.Writer, steps []client.TraceStep) {
	fmt.Fprintf(w, "steps (%d):\n", len(steps))
	if len(steps) == 0 {
		fmt.Fprintln(w, "  -")
		return
	}
	fmt.Fprintf(w, "  %-16s %-16s %-40s %-8s %6s   %s\n",
		"step", "name", "operation", "status", "dur", "in→out")
	for _, s := range steps {
		fmt.Fprintf(w, "  %-16s %-16s %-40s %-8s %5dms  %s→%s\n",
			stepLabel(s),
			truncate(s.Name, 16),
			truncate(s.Operation, 40),
			truncate(s.Status, 8),
			s.DurationMs,
			humanBytes(s.InputBytes),
			humanBytes(s.OutputBytes),
		)
		if s.Error != "" {
			fmt.Fprintf(w, "      error: %s\n", s.Error)
		}
	}
}

// stepLabel returns "<scope-padded>-<name>", matching the on-disk
// folder name written by chassis/trace/file.go. Four-digit pad on the
// scope so the column stays aligned across `0000-`..`9999-`. Two rows
// with the same prefix ran in parallel — that visual cue is the only
// signal of concurrency the renderer provides.
func stepLabel(s client.TraceStep) string {
	return fmt.Sprintf("%04d-%s", s.Scope, s.Name)
}

func printPayloads(w io.Writer, r *client.TraceResponse) {
	fmt.Fprintln(w)
	if r.In != nil {
		fmt.Fprintln(w, "in:")
		writePrettyJSON(w, r.In)
	}
	if r.Out != nil {
		fmt.Fprintln(w, "out:")
		writePrettyJSON(w, r.Out)
	}
}

// printStep drills into a single step identified by --step. The
// selector is one of:
//   - "<scope>"           — picks the first step at that scope
//   - "<scope>-<name>"    — picks the step at that scope with that name
//   - "<name>"            — picks the first step with that name
//
// "100-hello" is the canonical form (matches the on-disk folder name).
func printStep(stdout, stderr io.Writer, r *client.TraceResponse, sel string, verbose bool) int {
	step := findStep(r.Steps, sel)
	if step == nil {
		fmt.Fprintf(stderr, "trace: step %q not found (have %d step(s))\n", sel, len(r.Steps))
		return 1
	}

	fmt.Fprintf(stdout, "trace %s  step %s\n", r.RID, stepLabel(*step))
	fmt.Fprintf(stdout, "  name        %s\n", step.Name)
	if step.Operation != "" {
		fmt.Fprintf(stdout, "  operation   %s\n", step.Operation)
	}
	if step.Transport != "" {
		fmt.Fprintf(stdout, "  transport   %s\n", step.Transport)
	}
	if step.Stack != "" {
		fmt.Fprintf(stdout, "  stack/scope %s/%d\n", step.Stack, step.Scope)
	}
	if step.StartedAt != "" {
		fmt.Fprintf(stdout, "  started     %s\n", step.StartedAt)
	}
	fmt.Fprintf(stdout, "  status      %-12s duration %dms\n", step.Status, step.DurationMs)
	fmt.Fprintf(stdout, "  in→out      %s → %s\n", humanBytes(step.InputBytes), humanBytes(step.OutputBytes))
	if step.Error != "" {
		fmt.Fprintf(stdout, "  error       %s\n", step.Error)
	}

	if verbose {
		if r.TraceMode != "full" {
			fmt.Fprintf(stderr, "trace: --verbose requested but chassis trace_mode=%q (no payload data on disk)\n",
				traceModeOrUnset(r.TraceMode))
			return 0
		}
		if step.In != nil {
			fmt.Fprintln(stdout, "\nin:")
			writePrettyJSON(stdout, step.In)
		}
		if step.Out != nil {
			fmt.Fprintln(stdout, "\nout:")
			writePrettyJSON(stdout, step.Out)
		}
	}
	return 0
}

// findStep resolves --step <sel> against the step list. Accepts plain
// scope ("100"), scope-name ("100-hello"), or bare name ("hello").
// Returns the first match in step order so an ambiguous selector picks
// deterministically.
func findStep(steps []client.TraceStep, sel string) *client.TraceStep {
	// Try "<scope>-<name>".
	if dash := strings.IndexByte(sel, '-'); dash > 0 {
		if scope, err := strconv.Atoi(sel[:dash]); err == nil {
			name := sel[dash+1:]
			for i := range steps {
				if steps[i].Scope == scope && steps[i].Name == name {
					return &steps[i]
				}
			}
		}
	}
	// Try plain "<scope>".
	if scope, err := strconv.Atoi(sel); err == nil {
		for i := range steps {
			if steps[i].Scope == scope {
				return &steps[i]
			}
		}
	}
	// Fall back to plain "<name>".
	for i := range steps {
		if steps[i].Name == sel {
			return &steps[i]
		}
	}
	return nil
}

// humanBytes turns a byte count into a short label: "1.2k", "3.4M".
// Single decimal for k/M, plain int for byte-scale.
func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fk", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/(1024*1024))
	}
}

// truncate cuts s to max runes, appending "…" when truncation occurs.
// Operating on runes (not bytes) so multi-byte chars don't get split.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(rs[:max-1]) + "…"
}

func writePrettyJSON(w io.Writer, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(w, "  <unencodable>")
		return
	}
	_, _ = w.Write(b)
	if !strings.HasSuffix(string(b), "\n") {
		_, _ = w.Write([]byte{'\n'})
	}
}

func traceModeOrUnset(s string) string {
	if s == "" {
		return "off"
	}
	return s
}

// runTraceListMode is the no-rid path: fetch a list of recent traces
// and either launch the interactive browser (TTY) or print a plain
// table (piped / --plain). grep, when non-empty, is forwarded to the
// server's substring filter. watchSec, when > 0, enables periodic
// auto-reload in the TUI (ignored in plain/JSON modes).
func runTraceListMode(c *client.Client, stdout, stderr io.Writer, plain, asJSON bool, grep string, watchSec int) int {
	list, err := c.ListTraces(context.Background(), 0, grep)
	if err != nil {
		fmt.Fprintf(stderr, "trace: %v\n", err)
		return 1
	}

	if asJSON {
		// Re-marshal indented for piping into jq etc.
		b, err := json.MarshalIndent(list, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "trace: %v\n", err)
			return 1
		}
		_, _ = stdout.Write(b)
		_, _ = stdout.Write([]byte{'\n'})
		return 0
	}

	if !plain && banner.IsTTY(stdout) {
		watchInterval := time.Duration(watchSec) * time.Second
		if err := runTraceListTUI(c, list, grep, watchInterval); err != nil {
			fmt.Fprintf(stderr, "trace: %v\n", err)
			return 1
		}
		return 0
	}
	if watchSec > 0 {
		fmt.Fprintln(stderr, "trace: --watch is only supported in the interactive TUI (drop --plain or run on a TTY)")
	}

	// Plain text fallback.
	if len(list.Traces) == 0 {
		if grep != "" {
			fmt.Fprintf(stdout, "no traces matching %q\n", grep)
		} else {
			fmt.Fprintln(stdout, "no traces found")
		}
		return 0
	}
	if grep != "" {
		fmt.Fprintf(stdout, "traces matching %q (%d of %d):\n", grep, len(list.Traces), list.Total)
	} else {
		fmt.Fprintf(stdout, "traces (%d of %d):\n", len(list.Traces), list.Total)
	}
	fmt.Fprintf(stdout, "  %-25s %-15s %-25s %-30s %6s   %s\n",
		"rid", "src", "stack", "started", "dur", "status")
	for _, t := range list.Traces {
		dur := "--"
		if t.DurationMs != nil {
			dur = fmt.Sprintf("%dms", *t.DurationMs)
		}
		fmt.Fprintf(stdout, "  %-25s %-15s %-25s %-30s %5s    %s\n",
			t.RID,
			truncate(t.Src, 15),
			truncate(routeOrStack(t.Route, t.Stack), 25),
			t.StartedAt,
			dur,
			t.Status,
		)
	}
	return 0
}

// routeOrStack returns the more informative stack identifier for
// display: the destination of the first stage.jump (`route`) when
// recorded, otherwise the entry trampoline (`stack`). Every request
// starts at the same trampoline (typically "boot/%/0"), so showing
// just that conveys nothing — `route` tells you where the request
// actually went.
func routeOrStack(route, stack string) string {
	if route != "" {
		return route
	}
	return stack
}
