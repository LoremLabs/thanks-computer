package tcp

import (
	"bufio"
	"context"
	"encoding/base64"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

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
			newConns := make(chan net.Conn)
			var listeners []net.Listener

			// we start a controller per listen address
			for i := range tcp.pu.Conf.TCPListenAddrs {
				tcp.wg.Add(1)

				listen := tcp.pu.Conf.TCPListenAddrs[i]

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
						zap.String("err", err.Error()),
						zap.String("hint", "lsof -iTCP"+listen+" -sTCP:LISTEN"))
				}
				listeners = append(listeners, l)
				defer func() { _ = l.Close() }()

				tcp.pu.Logger.Info("tcp controller started", zap.String("listen", listen))

				// wait for connections. This is interupted when we terminate the listener
				go func(l net.Listener) {
					for {
						c, err := l.Accept() // blocks
						if err != nil {
							if !strings.Contains(err.Error(), "use of closed") {
								tcp.pu.Logger.Warn("tcp listen error", zap.String("listen", listen), zap.Reflect("err", err.Error()))
							}

							newConns <- nil
							break
						}
						newConns <- c
					}
				}(l)
			}

			for {
				select {
				case c := <-newConns:
					// new connection or nil if acceptor is down, in which case we should
					// do something (respawn, stop when everyone is down or just explode)
					if c == nil {
						tcp.wg.Done()
						break
					}

					// create a handler for this connection
					go func(conn net.Conn) {
						defer tcp.wg.Done()
						defer func() { _ = conn.Close() }()
						tcp.wg.Add(1)

						now := time.Now()
						rid := hxid.NewTimeSort().String()

						payload, _ := sjson.Set("", "_txc.src", "tcp")
						payload, _ = sjson.Set(payload, "_ts", now.Format(time.RFC3339))
						payload, _ = sjson.Set(payload, "_txc.rid", rid)
						// Listener name is what the ingress router keys on for TCP.
						// v1 supports a single listener with the fixed name "default";
						// multi-listener config arrives later.
						payload, _ = sjson.Set(payload, "_txc.tcp.listener", "default")
						// Private-fields plumbing: same pattern as the web
						// inlet — chassis config decides whether to stamp.
						if tcp.pu.Conf.DebugPrivate {
							payload, _ = sjson.Set(payload, "_txc.flag_private", true)
						}

						if addr, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
							payload, _ = sjson.Set(payload, "_txc.client.ip", addr.IP.String())
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
					}(c)

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
						// closed listener and send nil into newConns; the
						// newConns case calls wg.Done() per listener. Exiting
						// here would block those sends. The outer goroutine
						// effectively leaks until process exit, which is fine
						// since Stop() blocks on wg, not on this goroutine.
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
