package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// defaultRoom is the room used when --room is omitted. A single, predictable
// landing room keeps `thanks "…"` working before anyone names a room.
const defaultRoom = "general"

// runRoom routes `txco room ...` — and `thanks ...`, which is the same binary
// dispatched by argv[0] (see chassis/app.roomAlias).
//
// A room is a durable shared context; a message sent to it becomes a normal
// TxCo event (`@src == "room"`) that enters the same rule engine as web, mail,
// and cron — there is no privileged "assistant path." v1 is one-shot send; the
// live SSE feed + interactive REPL land in a later stage.
func runRoom(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		printRoomUsage(stdout)
		return 0
	}

	fs := flag.NewFlagSet("room", flag.ContinueOnError)
	fs.SetOutput(stderr)
	roomName := fs.String("room", "", "room name (e.g. support, dns, billing)")
	rf := registerRoomFlags(fs)
	fs.Usage = func() { printRoomUsage(stderr) }
	if err := fs.Parse(args); err != nil {
		return 2
	}

	room := strings.TrimSpace(*roomName)
	if room == "" {
		room = defaultRoom
	}
	// Flags come first; everything after them is the message (`thanks --room
	// support "why did ticket 184 fail?"`).
	msg := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if msg == "" {
		// No message → the interactive feed (render room activity + read
		// stdin). Requires a terminal; a non-TTY (piped) stdin with no message
		// is a usage error rather than a hung read.
		if !isTerminal(os.Stdin) {
			fmt.Fprintln(stderr, `room: no message and stdin is not a terminal — pass a message, e.g. `+
				"`thanks --room support \"why did ticket 184 fail?\"`")
			return 2
		}
		return runRoomInteractive(rf, room, stdout, stderr)
	}
	return roomSend(rf, room, msg, stdout, stderr)
}

// roomFlags bundles the common target/auth flags (mirrors cronFlags); a room
// message is tenant-scoped, so --tenant selects which tenant it lands in.
type roomFlags struct {
	target, addr, user, pass, profile, tenant *string
}

func registerRoomFlags(fs *flag.FlagSet) roomFlags {
	return roomFlags{
		target:  fs.String("target", "", "target name from txco.yaml"),
		addr:    fs.String("addr", "", "chassis admin endpoint"),
		user:    fs.String("user", "", "basic auth user"),
		pass:    fs.String("pass", "", "basic auth password"),
		profile: fs.String("profile", "", fmt.Sprintf("signing profile (defaults to TXCO_PROFILE, then %s/active)", auth.HomePathPretty())),
		tenant:  fs.String("tenant", "", "tenant slug"),
	}
}

func (f roomFlags) client() *client.Client {
	t := resolveTarget(".", *f.target, *f.addr, *f.user, *f.pass, *f.profile)
	t.Tenant = resolveTenant(*f.tenant, effectiveProfile(*f.target, *f.profile))
	return client.New(t)
}

// roomMessageReq is the body POSTed to the room inlet. The actor is taken from
// the signed request server-side — never from the client — so it isn't here.
type roomMessageReq struct {
	Text string `json:"text"`
}

// roomMessageResp is the inlet's synchronous reply: the message as recorded,
// plus whatever the room's stack answered (empty when nothing responded).
type roomMessageResp struct {
	MessageID string `json:"message_id,omitempty"`
	Room      string `json:"room,omitempty"`
	Actor     string `json:"actor,omitempty"`
	Text      string `json:"text,omitempty"`
}

// roomSend posts one message to the room inlet and renders any reply. The inlet
// endpoint lands in the next stage; the request shape is stable.
func roomSend(f roomFlags, room, text string, stdout, stderr io.Writer) int {
	suffix := "/rooms/" + url.PathEscape(room) + "/messages"
	var resp roomMessageResp
	if err := f.client().DoScoped(context.Background(), "POST", suffix, roomMessageReq{Text: text}, &resp); err != nil {
		fmt.Fprintf(stderr, "room: %v\n", err)
		return 1
	}
	if t := strings.TrimSpace(resp.Text); t != "" {
		fmt.Fprintln(stdout, t)
	}
	return 0
}

// runRoomInteractive opens the room's live SSE feed and a stdin loop: the feed
// renders messages from every participant as they arrive; each line typed is
// posted to the room. The stack's reply returns over the feed (not the POST
// response), so sends here are fire-and-forget.
func runRoomInteractive(rf roomFlags, room string, stdout, stderr io.Writer) int {
	c := rf.client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	body, err := c.OpenStream(ctx, "/rooms/"+url.PathEscape(room)+"/stream")
	if err != nil {
		fmt.Fprintf(stderr, "room: stream: %v\n", err)
		return 1
	}
	defer body.Close()
	go renderRoomStream(body, stdout)

	fmt.Fprintf(stdout, "joined #%s — type a message; Ctrl-D to leave\n> ", room)
	sendSuffix := "/rooms/" + url.PathEscape(room) + "/messages"
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			var resp roomMessageResp
			if err := c.DoScoped(context.Background(), "POST", sendSuffix, roomMessageReq{Text: line}, &resp); err != nil {
				fmt.Fprintf(stderr, "\rroom: %v\n", err)
			}
		}
		fmt.Fprint(stdout, "> ")
	}
	fmt.Fprintln(stdout)
	return 0
}

// renderRoomStream reads an SSE body and prints each room event as
// "<actor>: <text>", reprinting the input prompt after each so a typed line
// isn't visually clobbered (a deliberately simple terminal UX for v1).
func renderRoomStream(body io.Reader, w io.Writer) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") { // skip SSE comments + id: lines
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var ev struct {
			Actor string `json:"actor"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		fmt.Fprintf(w, "\r%s: %s\n> ", ev.Actor, ev.Text)
	}
}

// isTerminal reports whether f is an interactive terminal (vs a pipe/file).
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func printRoomUsage(w io.Writer) {
	banner.PrintLogo(w)
	fmt.Fprint(w, `
Usage: thanks [--room <name>] <message>
   or: txco room [--room <name>] <message>

Send a message into a room — a durable shared context. The message becomes a
normal event (@src == "room") that your installed stacks can resonate on, with
the same tenant, capability, audit, and fuel checks as any other inlet.

  thanks --room support "why did ticket 184 fail?"   # one-shot send
  txco room --room dns "acme.com is not propagating"

Flags:
  --room <name>     Room to post to (default: general)
  --tenant <slug>   Tenant (defaults to your profile's tenant)
  --profile <name>  Signing profile
  --addr <url>      Chassis admin endpoint (else your profile's chassis)

The live feed + interactive REPL land in a later release.
`)
}
