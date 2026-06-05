package tcp

import (
	"bufio"
	"context"
	"encoding/base64"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/units"
)

type TCPController struct {
	ctx      context.Context
	pu       *processor.Unit
	shutdown chan bool
	wg       sync.WaitGroup
}

// acceptedConn pairs a freshly Accept()-ed connection with the
// operator-given name of the listener it arrived on, so the
// connection handler can stamp `_txc.tcp.listener` correctly when
// the chassis is bound to multiple listeners.
type acceptedConn struct {
	conn net.Conn
	name string
}

// parseTCPListenSpec splits one `--tcp-listen-addrs` entry into its
// operator-chosen name and the address to bind. Form `name=addr`
// picks the name; bare `addr` falls back to `"default"` so existing
// configs keep stamping `_txc.tcp.listener = "default"` (and any
// ingress YAML keyed on it keeps matching). Empty input returns
// ("", "") so callers can drop blank entries (viper's CSV parsing
// occasionally produces them).
func parseTCPListenSpec(spec string) (name, addr string) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", ""
	}
	if i := strings.Index(spec, "="); i >= 0 {
		name = strings.TrimSpace(spec[:i])
		addr = strings.TrimSpace(spec[i+1:])
		if name == "" {
			name = "default"
		}
		return name, addr
	}
	return "default", spec
}

const MAX_MESSAGE_SIZE = units.MB * 10

func NewController(ctx context.Context, pu *processor.Unit) *TCPController {

	tcp := &TCPController{
		ctx:      ctx,
		pu:       pu,
		shutdown: make(chan bool),
	}

	return tcp
}

func (tcp *TCPController) Start() {

	if strings.Contains(tcp.pu.Conf.Personalities, "tcp") {

		go func() {
			newConns := make(chan acceptedConn)
			var listeners []net.Listener
			seenNames := map[string]string{} // name -> first addr (for collision warning)

			// we start a controller per listen address
			for i := range tcp.pu.Conf.TCPListenAddrs {
				name, listen := parseTCPListenSpec(tcp.pu.Conf.TCPListenAddrs[i])
				if listen == "" {
					// Skip blank entries — viper's CSV parsing can yield
					// `[""]` when the flag is set explicitly empty.
					continue
				}
				tcp.wg.Add(1)

				if prev, dup := seenNames[name]; dup {
					// Multiple listeners sharing a name still bind fine, but
					// ingress can't tell their traffic apart. Most likely a
					// config typo; warn loudly rather than failing.
					tcp.pu.Logger.Warn("two tcp listeners share a name; ingress routing cannot distinguish them",
						zap.String("name", name),
						zap.String("first", prev),
						zap.String("second", listen),
						zap.String("hint", "use name=addr form, e.g. webhooks=:5050,iot=:5051"))
				}
				seenNames[name] = listen

				// Pre-bind BEFORE logging "tcp controller started" so a
				// port conflict surfaces with a clear, actionable error
				// before the operator sees anything resembling "ready".
				// (Previously the start-log preceded the bind check, so a
				// failed bind appeared as "started, then died" rather
				// than "couldn't bind".)
				l, err := net.Listen("tcp", listen)
				if err != nil {
					tcp.pu.Logger.Fatal("tcp port already in use (or otherwise unbindable)",
						zap.String("listen", listen),
						zap.String("name", name),
						zap.String("err", err.Error()),
						zap.String("hint", "lsof -iTCP"+listen+" -sTCP:LISTEN"))
				}
				listeners = append(listeners, l)
				defer func() { _ = l.Close() }()

				tcp.pu.Logger.Info("tcp controller started",
					zap.String("listen", listen),
					zap.String("name", name))

				// wait for connections. This is interupted when we terminate the listener
				go func(l net.Listener, listen, name string) {
					for {
						c, err := l.Accept() // blocks
						if err != nil {
							if !strings.Contains(err.Error(), "use of closed") {
								tcp.pu.Logger.Warn("tcp listen error", zap.String("listen", listen), zap.Reflect("err", err.Error()))
							}

							newConns <- acceptedConn{}
							break
						}
						newConns <- acceptedConn{conn: c, name: name}
					}
				}(l, listen, name)
			}

			for {
				select {
				case ac := <-newConns:
					// new connection, or a zero-value struct if an acceptor
					// is down — in the latter case we should do something
					// (respawn, stop when everyone is down or just explode)
					if ac.conn == nil {
						tcp.wg.Done()
						break
					}

					// create a handler for this connection
					go func(conn net.Conn, listenerName string) {
						defer tcp.wg.Done()
						defer func() { _ = conn.Close() }()
						tcp.wg.Add(1)

						now := time.Now()
						rid := hxid.NewTimeSort().String()

						payload, _ := sjson.Set("", "_txc.src", "tcp")
						payload, _ = sjson.Set(payload, "_ts", now.Format(time.RFC3339))
						payload, _ = sjson.Set(payload, "_txc.rid", rid)
						// Listener name is what the ingress router keys on
						// for TCP. Operator names come from `name=addr`
						// entries in --tcp-listen-addrs; bare addresses keep
						// the back-compat "default" name.
						payload, _ = sjson.Set(payload, "_txc.tcp.listener", listenerName)
						// Private-fields plumbing: same pattern as the web
						// inlet — chassis config decides whether to stamp.
						if tcp.pu.Conf.DebugPrivate {
							payload, _ = sjson.Set(payload, "_txc.flag_private", true)
						}

						if addr, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
							payload, _ = sjson.Set(payload, "_txc.client.ip", addr.IP.String())
						}
						// Local addr (port and ip the client connected TO).
						// Rules that want to route on the raw port without
						// operator-side YAML can read these directly.
						if la, ok := conn.LocalAddr().(*net.TCPAddr); ok {
							payload, _ = sjson.Set(payload, "_txc.tcp.local.ip", la.IP.String())
							payload, _ = sjson.Set(payload, "_txc.tcp.local.port", la.Port)
						}

						if tcp.pu.Logger.Core().Enabled(zap.DebugLevel) {
							tcp.pu.Logger.Debug("tcp connection",
								zap.String("payload", payload),
							)
						}

						// on new connections, we notify that we have a new connection
						// set any time constraints
						connectTimeout, err := time.ParseDuration(tcp.pu.Conf.TCPConnectRespTimeout)
						if err != nil {
							tcp.pu.Logger.Warn("Unable to parse TCPConnectRespTimeout",
								zap.Reflect("err", err.Error()),
							)

							connectTimeout = time.Duration(1) * time.Second
						}

						// Inherit from tcp.ctx so chassis shutdown cancels any in-flight
						// connect timer; this matches the response handler at line ~217.
						ctx, cancel := context.WithTimeout(tcp.ctx, connectTimeout)
						defer cancel()

						// set rid
						ctx = context.WithValue(ctx, config.CtxKeyRid, rid)

						// send event for processing
						var resCh = make(chan event.Payload) // response channel
						var envelope = event.PackageJSON(ctx, payload, resCh, "tcp")

						tcp.pu.Bus <- envelope

						if tcp.pu.Logger.Core().Enabled(zap.DebugLevel) {
							tcp.pu.Logger.Debug("sent connect to processors",
								zap.String("payload", payload),
							)
						}

						// wait for conenct response
						select {
						case res := <-resCh:
							if tcp.pu.Logger.Core().Enabled(zap.DebugLevel) {
								tcp.pu.Logger.Debug("tcp connect res", zap.String("response", res.Raw))
							}
						case <-ctx.Done():
							tcp.pu.Logger.Info("tcp connect response timeout")
							_ = conn.Close()
							cancel()
							return
						case <-tcp.ctx.Done():
							tcp.pu.Logger.Info("tcp response shutdown")
							cancel() // shut down the request
							return
						}

						// and then see how we should handle reading from the connection

						// via a buffer

						// or a string reader

						for {
							idleTimeout, err := time.ParseDuration(tcp.pu.Conf.TCPMaxIdleTimeout)
							if err != nil {
								tcp.pu.Logger.Warn("Unable to parse TCPMaxIdleTimeout",
									zap.Reflect("err", err.Error()),
								)

								idleTimeout = time.Duration(5) * time.Second
							}

							s := make(chan string)
							e := make(chan error)
							go func() {
								message, err := bufio.NewReader(conn).ReadString('\n')
								if err != nil {
									e <- err
								} else {
									s <- message
								}
								close(s)
								close(e)
							}()

							var message string
							select {
							case received := <-s:
								tcp.pu.Logger.Debug("tcp read message", zap.String("rid", rid))
								message = received
							case err := <-e:
								tcp.pu.Logger.Warn("tcp read error", zap.String("rid", rid), zap.Reflect("err", err.Error()))
								_ = conn.Close()
								return
							case <-time.After(idleTimeout):
								tcp.pu.Logger.Warn("tcp read error", zap.String("rid", rid), zap.String("err", "timeout"))
								_ = conn.Close()
								return
							}

							var pl string
							if (len(message) > 0) && (len(message) < MAX_MESSAGE_SIZE) {
								body := base64.StdEncoding.EncodeToString([]byte(message))
								pl, _ = sjson.Set(payload, "_txc.client.body", body)
							}

							respTimeout, err := time.ParseDuration(tcp.pu.Conf.TCPRespTimeout)
							if err != nil {
								tcp.pu.Logger.Warn("Unable to parse TCPRespTimeout",
									zap.Reflect("err", err.Error()),
								)

								respTimeout = time.Duration(10) * time.Second
							}

							ctx, cancel := context.WithTimeout(tcp.ctx, respTimeout)
							defer cancel()

							envelope = event.PackageJSON(ctx, pl, resCh, "tcp")

							tcp.pu.Bus <- envelope

							if tcp.pu.Logger.Core().Enabled(zap.DebugLevel) {
								tcp.pu.Logger.Debug("sent message to processors",
									zap.String("payload", pl),
								)
							}

							// wait for conenct response
							var output string
							select {
							case res := <-resCh:
								if tcp.pu.Logger.Core().Enabled(zap.DebugLevel) {
									tcp.pu.Logger.Debug("tcp res", zap.String("response", res.Raw))
								}
								output = res.Raw
							case <-ctx.Done():
								tcp.pu.Logger.Info("tcp response timeout")
								_ = conn.Close()
								cancel()
								return
							case <-tcp.ctx.Done():
								tcp.pu.Logger.Info("tcp shutdown")
								cancel() // shut down the request
								return
							}

							// should return a list of commands
							// gjson.Get(output, "_txc.server.commands").ForEach(func(key, value gjson.Result) bool {
							// 	return true
							// })

							// Shared admission gate denial: TCP has no standard
							// rejection, so write a short "<status> <reason>"
							// line and close the connection.
							if status, reason, ok := admission.Denied(output); ok {
								_, _ = conn.Write([]byte(strconv.Itoa(status) + " " + reason + "\n"))
								_ = conn.Close()
								return
							}

							doHangup := gjson.Get(output, "_txc.server.hangup").Bool()
							if doHangup {
								_ = conn.Close()
								return
							}

							// if body, then return body
							// if no body, then return json
							hidePrivate := !strings.Contains(tcp.pu.Conf.WebDebug, "SHOW_PRIVATE_VARS")

							outputBytes, err := getOutput(output, hidePrivate)
							if err != nil {
								tcp.pu.Logger.Warn("error getting output", zap.Reflect("err", err))

								// TODO: ineffassign - who will use this outputBytes variable?
								//outputBytes = []byte("") // stay silent on error
								err := conn.Close()
								if err != nil {
									// TODO: error handling
									tcp.pu.Logger.Error("conn.Close error", zap.String("err", err.Error()))
								}
								return
							}

							_, err = conn.Write(outputBytes)
							if err != nil {
								// TODO: error handling
								tcp.pu.Logger.Error("write error", zap.String("err", err.Error()))
							}

						}
						// TODO: unreachable code
						//tcp.pu.Logger.Info("tcp handler closing")
					}(ac.conn, ac.name)

				// case <-time.After(time.Minute):
				// 		// timeout branch, no connection for a minute
				case doshutdown := <-tcp.shutdown:
					if doshutdown {
						tcp.pu.Logger.Info("tcp shutdown received")
						for i := range listeners {
							err := listeners[i].Close()
							if err != nil {
								// TODO: error handling
								tcp.pu.Logger.Error("listeners close error", zap.String("err", err.Error()))
								return
							}
						}
						// Don't return: accept goroutines below will see the
						// closed listener and send a zero-value acceptedConn
						// into newConns; the newConns case calls wg.Done()
						// per listener. Exiting here would block those sends.
						// The outer goroutine effectively leaks until process
						// exit, which is fine since Stop() blocks on wg, not
						// on this goroutine.
						//nolint:staticcheck // SA4011: break is dead but the alternative leaks accept goroutines
						break
					}
				}
			}
			// TODO: unreachable code
			//tcp.pu.Logger.Info("tcp closed listeners")
		}()
	}
}

func (tcp *TCPController) Stop() {
	if strings.Contains(tcp.pu.Conf.Personalities, "tcp") {
		tcp.pu.Logger.Info("calling tcp controller stop")

		// shut down workers
		tcp.shutdown <- true
		tcp.wg.Wait()
		tcp.pu.Logger.Info("tcp controller stopped")
	}
}
