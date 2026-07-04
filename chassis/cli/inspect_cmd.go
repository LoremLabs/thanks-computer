package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/pflag"
	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// runInspectRuntime routes `txco inspect <stack> [noun] [id]` — ask a tenant's
// own ops what their current state is. Where `txco trace` answers "what just
// happened?", inspect answers "what is the current state, and why?": the
// request becomes a normal `@src == "inspect"` event routed to the tenant's
// `_inspect` stack, and whichever inspector op matches the stack/noun answers
// with a structured card that this command renders.
//
// (Package manifests have their own verb: `txco package inspect <source>`.)
func runInspectRuntime(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		printInspectUsage(stdout)
		return 0
	}

	fs := pflag.NewFlagSet("inspect", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", "", "target name from txco.yaml")
	addr := fs.String("addr", "", "chassis admin endpoint")
	user := fs.String("user", "", "basic auth user")
	pass := fs.String("pass", "", "basic auth password")
	profile := fs.String("profile", "", fmt.Sprintf("signing profile (defaults to TXCO_PROFILE, then %s/active)", auth.HomePathPretty()))
	tenant := fs.String("tenant", "", "tenant slug")
	asJSON := fs.Bool("json", false, "emit the card document as machine-readable JSON")
	argPairs := fs.StringArray("arg", nil, "extra inspector argument as k=v (repeatable)")
	fs.Usage = func() { printInspectUsage(stderr) }
	if err := fs.Parse(args); err != nil {
		return 2
	}

	rest := fs.Args()
	if len(rest) < 1 || len(rest) > 3 {
		printInspectUsage(stderr)
		return 2
	}
	req := inspectReq{Stack: strings.TrimSpace(rest[0])}
	if len(rest) > 1 {
		req.Noun = strings.TrimSpace(rest[1])
	}
	if len(rest) > 2 {
		req.ID = strings.TrimSpace(rest[2])
	}
	for _, kv := range *argPairs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" {
			fmt.Fprintf(stderr, "inspect: --arg wants k=v, got %q\n", kv)
			return 2
		}
		if req.Args == nil {
			req.Args = map[string]any{}
		}
		req.Args[strings.TrimSpace(k)] = v
	}

	t := resolveTarget(".", *target, *addr, *user, *pass, *profile)
	t.Tenant = resolveTenant(*tenant, effectiveProfile(*target, *profile))
	c := client.New(t)

	var resp inspectResp
	if err := c.DoScoped(context.Background(), "POST", "/inspect", req, &resp); err != nil {
		if strings.Contains(err.Error(), "no_inspector") {
			fmt.Fprintf(stderr, "inspect: no inspector answered for stack %q noun %q — does the tenant have an _inspect stack with a matching op?\n", req.Stack, req.Noun)
			return 1
		}
		fmt.Fprintf(stderr, "inspect: %v\n", err)
		return 1
	}

	if *asJSON {
		// --json: stdout carries only the card document.
		fmt.Fprintln(stdout, strings.TrimSpace(string(resp.Card)))
		return 0
	}
	renderInspectCard(stdout, gjson.ParseBytes(resp.Card))
	return 0
}

// inspectReq is the body POSTed to the inspect inlet. The tenant is taken from
// the signed request server-side — never from the client — so it isn't here.
type inspectReq struct {
	Stack string         `json:"stack"`
	Noun  string         `json:"noun,omitempty"`
	ID    string         `json:"id,omitempty"`
	Args  map[string]any `json:"args,omitempty"`
}

// inspectResp is the inlet's synchronous reply: the inspector's card verbatim.
type inspectResp struct {
	Card json.RawMessage `json:"card"`
}

// renderInspectCard prints a card as aligned text sections. The card is
// stack-authored data; the CLI owns presentation:
//
//	Marketing Profile
//	  Signals
//	    Last activity  2026-07-02
//	    Books finished 4
func renderInspectCard(w io.Writer, card gjson.Result) {
	if title := card.Get("title").String(); title != "" {
		fmt.Fprintf(w, "\n  %s\n", title)
	}
	card.Get("sections").ForEach(func(_, sec gjson.Result) bool {
		if st := sec.Get("title").String(); st != "" {
			fmt.Fprintf(w, "    %s\n", st)
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		sec.Get("rows").ForEach(func(_, row gjson.Result) bool {
			fmt.Fprintf(tw, "      %s\t%s\n", row.Get("0").String(), cardCell(row.Get("1")))
			return true
		})
		_ = tw.Flush()
		return true
	})
	fmt.Fprintln(w)
}

// cardCell renders one row value: strings bare, everything else (numbers,
// bools, nested JSON) as its raw JSON text.
func cardCell(v gjson.Result) string {
	if v.Type == gjson.String {
		return v.String()
	}
	if !v.Exists() {
		return "-"
	}
	return v.Raw
}

func printInspectUsage(w io.Writer) {
	banner.PrintLogo(w)
	fmt.Fprint(w, `
Usage: txco inspect <stack> [noun] [id] [--arg k=v]... [--json]

Ask a tenant's own ops to explain their current state. The request becomes a
normal event (@src == "inspect") routed to the tenant's _inspect stack; the
matching inspector op answers with a structured card. trace answers "what just
happened?" — inspect answers "what is the current state, and why?".

  txco inspect marketing user matt@example.com
  txco inspect driplit reader matt@example.com --json
  txco inspect crm company openai --arg window=30d

Flags:
  --arg k=v         Extra inspector argument (repeatable → @inspect.args.k)
  --json            Print the raw card document instead of rendering it
  --tenant <slug>   Tenant (defaults to your profile's tenant)
  --profile <name>  Signing profile
  --addr <url>      Chassis admin endpoint (else your profile's chassis)

Package manifests are a different verb: txco package inspect <source>.
`)
}
