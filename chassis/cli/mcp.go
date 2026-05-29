package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/jsonrpc"
)

// mcpProtocolVersion is the MCP spec version doctor advertises on
// `initialize`. Kept in sync with chassis/processor/mcphttp.go.
const mcpProtocolVersion = "2025-06-18"

const mcpSessionHeader = "Mcp-Session-Id"

func runMcp(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printMcpUsage(stdout)
		return 0
	}
	switch args[0] {
	case "doctor":
		return runMcpDoctor(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printMcpUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "mcp: unknown subcommand %q\n\n", args[0])
		printMcpUsage(stderr)
		return 2
	}
}

func printMcpUsage(w io.Writer) {
	banner.PrintLogo(w)
	fmt.Fprint(w, `
Usage: txco mcp <command> [flags]

Tools for working with MCP (Model Context Protocol) servers from a txco
workspace.

Commands:
  doctor <url-or-alias>   Probe an MCP-over-HTTP server: run the
                          initialize handshake and print its tool list.

Run 'txco mcp <command> --help' for per-command flags.
`)
}

// runMcpDoctor probes one MCP-over-HTTP endpoint with the full
// session lifecycle a real EXEC would use (initialize →
// notifications/initialized → tools/list), then pretty-prints the
// tools it advertises. Lets an author confirm an endpoint is
// reachable and that a tool name spelt in a rule actually exists,
// before they paste it into a .txcl file.
func runMcpDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", "", "target name from txco.yaml (for resolving op://NAME)")
	timeout := fs.Duration("timeout", 10*time.Second, "deadline for the whole probe")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco mcp doctor [flags] <url-or-alias>

Probe an MCP-over-HTTP server. The target is either:

  - A direct URL:  mcp+https://api.example.com/mcp
                   (or https://... — the mcp+ prefix is optional for doctor)
  - An alias:      op://summarize
                   (resolved against txco.yaml in the current workspace;
                   the alias's url field is used as-is; #tool fragment
                   is stripped since doctor calls tools/list, not
                   tools/call.)

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "mcp doctor: expected exactly one argument (url or op://alias)")
		fs.Usage()
		return 2
	}

	rawArg := fs.Arg(0)
	endpoint, err := resolveDoctorEndpoint(rawArg, *target)
	if err != nil {
		fmt.Fprintf(stderr, "mcp doctor: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	client := &http.Client{Timeout: *timeout}
	if err := doctorProbe(ctx, client, endpoint, stdout, stderr); err != nil {
		// doctorProbe already printed a diagnostic to stderr.
		return 1
	}
	return 0
}

// resolveDoctorEndpoint normalizes the user's argument into a
// concrete http(s):// URL suitable for tools/list. Strips the
// mcp+ prefix and the #tool fragment, since doctor calls
// tools/list rather than tools/call.
func resolveDoctorEndpoint(arg, target string) (string, error) {
	if strings.HasPrefix(arg, "op://") {
		name := strings.TrimPrefix(arg, "op://")
		if name == "" {
			return "", fmt.Errorf("empty op alias")
		}
		dir, err := resolveDir("")
		if err != nil {
			return "", fmt.Errorf("resolve workspace: %w", err)
		}
		resolved := resolveFullTarget(dir, target)
		ops := buildOpRefMap(resolved)
		op, ok := ops[name]
		if !ok || op.URL == "" {
			return "", fmt.Errorf("unresolved op://%s — define it under operations: in txco.yaml", name)
		}
		arg = op.URL
	}

	u, err := url.Parse(arg)
	if err != nil {
		return "", fmt.Errorf("parse URL %q: %w", arg, err)
	}
	u.Scheme = strings.TrimPrefix(u.Scheme, "mcp+")
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("expected http(s) or mcp+http(s) URL, got scheme %q", u.Scheme)
	}
	u.Fragment = ""
	return u.String(), nil
}

func doctorProbe(ctx context.Context, c *http.Client, endpoint string, stdout, stderr io.Writer) error {
	// Phase 1: initialize.
	initParams, _ := json.Marshal(map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "txco-doctor", "version": "0"},
	})
	initReq, _ := jsonrpc.Call(1, "initialize", initParams)
	body, sessionID, err := doctorPost(ctx, c, endpoint, initReq, "")
	if err != nil {
		fmt.Fprintf(stderr, "init: dial-failed: %v\n", err)
		return err
	}
	initResp, perr := jsonrpc.Parse(body)
	if perr != nil {
		fmt.Fprintf(stderr, "init: rpc-error: %v\n", perr)
		return perr
	}
	serverName := gjson.GetBytes(initResp.Result, "serverInfo.name").String()
	serverVersion := gjson.GetBytes(initResp.Result, "serverInfo.version").String()
	protoVersion := gjson.GetBytes(initResp.Result, "protocolVersion").String()
	fmt.Fprintf(stdout, "server: %s %s  (protocol %s)\n", strDefault(serverName, "?"), strDefault(serverVersion, "?"), strDefault(protoVersion, "?"))
	if sessionID != "" {
		fmt.Fprintf(stdout, "session: %s\n", sessionID)
	} else {
		fmt.Fprintln(stdout, "session: (stateless)")
	}

	// Phase 2: notifications/initialized.
	notifReq, _ := jsonrpc.Notify("notifications/initialized", nil)
	if _, _, err := doctorPost(ctx, c, endpoint, notifReq, sessionID); err != nil {
		fmt.Fprintf(stderr, "initialized: %v\n", err)
		return err
	}

	// Phase 3: tools/list.
	listReq, _ := jsonrpc.Call(2, "tools/list", nil)
	body, _, err = doctorPost(ctx, c, endpoint, listReq, sessionID)
	if err != nil {
		// Server gating tools/list on a session id (with a 400)
		// surfaces as a distinct diagnostic.
		if strings.Contains(err.Error(), "400") {
			fmt.Fprintf(stderr, "tools/list: session-required: %v\n", err)
		} else {
			fmt.Fprintf(stderr, "tools/list: dial-failed: %v\n", err)
		}
		return err
	}
	listResp, perr := jsonrpc.Parse(body)
	if perr != nil {
		fmt.Fprintf(stderr, "tools/list: rpc-error: %v\n", perr)
		return perr
	}

	tools := gjson.GetBytes(listResp.Result, "tools")
	if !tools.IsArray() || len(tools.Array()) == 0 {
		fmt.Fprintln(stdout, "\nno tools advertised.")
		return nil
	}
	fmt.Fprintf(stdout, "\ntools (%d):\n", len(tools.Array()))
	for _, t := range tools.Array() {
		name := t.Get("name").String()
		desc := firstLine(t.Get("description").String())
		fmt.Fprintf(stdout, "  - %s", name)
		if desc != "" {
			fmt.Fprintf(stdout, "  — %s", desc)
		}
		fmt.Fprintln(stdout)
		props := t.Get("inputSchema.properties")
		if props.IsObject() {
			var names []string
			props.ForEach(func(k, _ gjson.Result) bool {
				names = append(names, k.String())
				return true
			})
			if len(names) > 0 {
				fmt.Fprintf(stdout, "      inputs: %s\n", strings.Join(names, ", "))
			}
		}
	}
	return nil
}

// doctorPost posts body to endpoint with MCP / JSON conventions and
// returns the response body plus any Mcp-Session-Id the server
// returned.
func doctorPost(ctx context.Context, c *http.Client, endpoint string, body []byte, sessionID string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "txco-doctor")
	req.Header.Set("Content-Type", "application/json")
	// Streamable-HTTP requires accepting both framings; some
	// servers 406 without them (e.g. DeepWiki).
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set(mcpSessionHeader, sessionID)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	sid := resp.Header.Get(mcpSessionHeader)
	if sid == "" {
		sid = sessionID
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		if extracted := jsonrpc.ExtractSSE(respBody); extracted != nil {
			respBody = extracted
		}
	}
	if resp.StatusCode >= 400 {
		snippet := string(respBody)
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		return respBody, sid, fmt.Errorf("mcp+http %d: %s", resp.StatusCode, snippet)
	}
	return respBody, sid, nil
}

func strDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}
