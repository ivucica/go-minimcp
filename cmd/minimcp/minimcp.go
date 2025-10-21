package main

import (
	// TODO: add badc0de.net/pkg/flagutil and invoke its Parse in init()
	"github.com/golang/glog"
	"github.com/sourcegraph/jsonrpc2"

	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	// "net/rpc"
	"os"
	"time"
)

var (
	listenType = flag.String("listen_type", "stdio", "listen on stdio, or http, or sse")
	listenAddr = flag.String("listen_addr", ":15974", "listen on addr+port")
	urlBase    = flag.String("url_base", "", "base URL for HTTP/SSE endpoints, without trailing slash; empty defaults to http://${listen_addr}; if listen_addr has no host, os.Hostname() is assumed; intended only for constructing returned endpoint (i.e. does not change handlers, incl. stripping prefixes, for now)")
)

func init() {
	flag.Parse()
	if *urlBase == "" {
		*urlBase = defaultURLBase()
	}
}

func defaultURLBase() string {
	// extract host and port from listenAddr
	host, _ := os.Hostname() // TODO: handle error?
	port := "15974"
	addr := *listenAddr
	if addr != "" {
		// split host and port
		// assume there is always a colon in there, for port
		var hostPort string
		if addr[0] == ':' {
			hostPort = host + addr
		} else {
			hostPort = addr
		}
		var err error
		host, port, err = net.SplitHostPort(hostPort)
		if err != nil {
			glog.Errorf("failed to parse listen_addr %s: %v", *listenAddr, err)
			glog.Flush()
			return "http://localhost:15974"
		}
		if host == "" {
			host, _ = os.Hostname()
		}
	}
	return "http://" + host + ":" + port
}

// Represents a notification that can be sent as an SSE or via JSON-RPC.
type notification struct {
	Event string      `json:"event"`
	Data  interface{} `json:"data,omitempty"`
}

func handle(ctx context.Context, c *jsonrpc2.Conn, r *jsonrpc2.Request) (result interface{}, err error) {
	glog.Infof("request: %+v", r)
	glog.Flush()

	// NOTE:
	// minimcp.go:176] request: &{Method:notifications/initialized Params:<nil> ID:0 Notif:true Meta:<nil> ExtraFields:[]}
	// jsonrpc2 handler: notification "notifications/initialized" handling error: jsonrpc2: code -32601 message: method not found
	// other messages have Notif:false and ID set, presumably this is a way to enable notifications

	// log if we have a notification channel in context
	debuggingSSEChannel := false
	var backupSSEChannelHack chan notification
	if sseChannelTmp := sseChannelFromContext(ctx); sseChannelTmp != nil {
		glog.Info("found sseChannel in context; it should be used for notifications")
		debuggingSSEChannel = true
		backupSSEChannelHack = sseChannelTmp
	}

	// helper to send notification via sseChannel if available, else via c.Notify
	// handleCtx := ctx // capture for use in notify might allow attempting to skip send of notification if request is done, but this is a trap:
	// it might be an HTTP POST ctx, which will almost certainly be done by the time a delayed async notification ise sent.
	//
	// Do not fall for this.
	notify := func(ctx context.Context, n notification) {
		// main reply is also sent to sseChannel, but that's done in main POST handler
		sseChannel := sseChannelFromContext(ctx)

		if debuggingSSEChannel && sseChannel == nil {
			glog.Error("expected sseChannel in context but not found")
			sseChannel = backupSSEChannelHack // Why is ctx not containing it?
		}

		// send via sseChannel if not nil
		if sseChannel != nil {
			// check if sseChannel is closed?
			// add timeout?
			// maybe we are happy with using ctx to signal channel should not be used,
			// and closing it only here? (writers are supposed to close)
			//
			// IT MAY BE OK: the connection closed, the SSE channel closed, this was
			// set to zero, but context was not canceled yet for whatever reason.
			// We can just skip sending then.
			glog.Infof("handle: sending notification via sseChannel: %+v", n)
			glog.Flush()

			// TODO: we need to restructure substantially to notice that a notification is sent way after the sse channel is gone.
			// session.SendNotification should handle this.
			//
			// however we don't have that at this moment, so let's recover from the panic rather than crash the http server.
			defer func() {
				// to make even this work right, we would need to wrap the calling code in yet another routine
				if r := recover(); r != nil {
					glog.Errorf("recovered from panic while sending notification via sseChannel: %+v, panic: %v", n, r)
					glog.Flush()
				}
			}()

			// TODO! BROKEN! Should use session.SendNotification(n) instead!
			// That function should check if ctx is done, and close the channel properly.
			select {
			case <-ctx.Done():
				glog.Warningf("context done before sending notification via sseChannel: %+v", n)
				glog.Flush()
				//close(sseChannel)
				// Note! THIS MIGHT NOT BE CLOSURE OF THE SESSION. DO NOT CLOSE.
				// ctx MIGHT be unrelated to the session.
				// TODO: pass session instead so its SendNotification can be used!
				return
			case sseChannel <- n: // dispatched into sseChannel queue
			}

			return
		} else if c != nil {
			// else send via c.Notify; sending via jsonrpc won't work for single-response connections since we nuke the conn (albeit it looks like we maybe shouldn't)
			glog.Infof("sending notification via c.Notify: %+v", n)
			glog.Flush()

			// check if c is usable?
			// add timeout?
			select {
			case <-ctx.Done():
				glog.Warningf("context done before sending notification via c.Notify: %+v", n)
				glog.Flush()
				return
			default:
			}
			err := c.Notify(ctx, n.Event, n.Data)
			if err != nil {
				glog.Errorf("failed to send notification via c.Notify: %+v, err: %v", n, err)
				glog.Flush()
			}

			return
		}
		glog.Warningf("cannot send notification, no sseChannel nor c: %+v", n)
		glog.Flush()
	}

	if r.Notif {
		// notification enablement? need to check.
		switch r.Method {
		case "notifications/initialized":
			glog.Info("received r.Notif=true: method:notifications/initialized")
			glog.Flush()
		default:
			glog.Infof("received r.Notif=true: method unhandled:%s", r.Method)
			glog.Flush()
		}
		return "MAGIC_RESPONSE_DO_NOT_TRANSMIT", nil // TODO: smarter way to tell not to transmit this response to SSE channel
	}

	switch r.Method {
	case "initialize":
		// decode params according to iniitalizeParams
		//if err := c.Reply(ctx, r.ID, "test") { // this is if func (h *handler) Handle(ctx context.Context, c *jsonrpc2.Conn, r *jsonrpc2.Request) { }; or func (h *MyHandler) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) { might be the right signature, need to verify

		params := initializeParams{}
		if err := json.Unmarshal(*r.Params, &params); err != nil {
			// If unmarshaling fails, the params were invalid.
			glog.Errorf("Failed to unmarshal params: %v", err)
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "invalid params"}
		}
		glog.Infof("conn from %+v", params.ClientInfo)
		glog.Flush()

		go func(sseChannel chan notification) {
			time.Sleep(1 * time.Second)
			// TODO: ctx might be invalid, c might be unusable...
			ctx := context.TODO() // add a time block ... we do need to attach the sseChannel from the original ctx if any
			ctx = contextWithSSEChannel(ctx, sseChannel)
			notify(ctx, notification{
				Event: "notification/initialized",
				Data:  nil,
			})
		}(sseChannelFromContext(ctx))

		return initializeResult{
			ProtocolVersion: "2024-11-05",
			Capabilities: initializeResultCaps{
				Logging:   map[string]bool{},
				Prompts:   initializeResultFlags{},
				Resources: initializeResultFlags{},
				Tools:     initializeResultFlags{},
			},
			ServerInfo: peerInfo{
				Name:    "MiniMCP",
				Title:   "MiniMCP Display Name",
				Version: "0.0.1",
			},
		}, nil
	case "tools/list":
		// no params, but may be "params": {"cursor": "some-cursor-value"}

		return toolsListResult{
			Tools: []tool{
				tool{
					Name:        "get_weather",
					Title:       "Weather Information Provider",
					Description: "Get current weather information for a location.",
					InputSchema: &inputSchema{
						Type: "object",
						Properties: map[string]*inputSchema{
							"location": &inputSchema{
								Type:        "string",
								Description: "City name or zip code",
							},
						},
						Required: []string{"location"},
					},
				},
			},
		}, nil
	case "tools/call":
		params := toolsCallParams{}
		if err := json.Unmarshal(*r.Params, &params); err != nil {
			// If unmarshaling fails, the params were invalid.
			glog.Errorf("Failed to unmarshal params: %v", err)
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "invalid params"}
		}
		glog.Info("tool call %+v", params)
		glog.Flush()

		if params.Name != "get_weather" {
			// TODO: return a response with isError = true
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "unknown tool " + params.Name}
		}

		location, ok := params.Arguments["location"]
		if !ok {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "no location passed"}
		}
		locationS := location.(string)
		if !ok {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "location not a string"}
		}
		if locationS == "" {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "location is empty"}
		}

		return toolsCallResult{
			Content: []*content{
				&content{
					Type: "text",
					Text: "Current weather in " + locationS + ":\nTemperature: 72°F\nConditions: Partly cloudy",
				},
			},
		}, nil

	case "logging/setLevel":
		// params: { "level": "debug" }
		// VSCode insists on passing this in
		var params struct {
			Level string `json:"level"`
		}
		if err := json.Unmarshal(*r.Params, &params); err != nil {
			// If unmarshaling fails, the params were invalid.
			glog.Errorf("Failed to unmarshal params: %v", err)
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "invalid params"}
		}
		glog.Infof("setting log level to %s (not really implemented)", params.Level)
		glog.Flush()
		return struct{}{}, nil
	default:
		//return struct{ A string }{A: "abc"}, nil
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeMethodNotFound, Message: "method not found"}
	}
}

// Runs a JSON-RPC 2.0 connection over the provided ReadWriteCloser.
//
// Mainly for stdio transport.
func runConn(ctx context.Context, rwc io.ReadWriteCloser) {
	handler := jsonrpc2.HandlerWithError(handle) // s.Handle

	// conn := jsonrpc2.NewConn(ctx, jsonrpc2.NewPlainObjectStream(os.Stdio
	conn := jsonrpc2.NewConn(
		ctx,
		jsonrpc2.NewBufferedStream(rwc, jsonrpc2.PlainObjectCodec{}), // jsonrpc2.VSCodeObjectCodec{}), // correct codec for mcp? maybe not due to Content-Length being sent? n.b. for PlainObjectCodec "Deprecated: use NewPlainObjectStream"
		handler)
	<-conn.DisconnectNotify()
	glog.Info("closed a conn")
	glog.Flush()
}


func main() {
	// log startup info without glog since that might direct only to file
	fmt.Fprintf(os.Stderr, "MiniMCP starting with listen_type=%s, listen_addr=%s, url_base=%s\n", *listenType, *listenAddr, *urlBase)
	// ensure glog logs are flushed on exit
	defer glog.Flush()

	// main context
	ctx := context.Background()

	var rwc io.ReadWriteCloser
	switch *listenType {
	case "stdio":
		/*
			In the stdio transport:
			The client launches the MCP server as a subprocess.
			The server reads JSON-RPC messages from its standard input (stdin) and sends messages to its standard output (stdout).
			Messages are individual JSON-RPC requests, notifications, or responses.
			Messages are delimited by newlines, and MUST NOT contain embedded newlines.
			The server MAY write UTF-8 strings to its standard error (stderr) for logging purposes. Clients MAY capture, forward, or ignore this logging.
			The server MUST NOT write anything to its stdout that is not a valid MCP message.
			The client MUST NOT write anything to the server’s stdin that is not a valid MCP message.
		*/
		glog.Infof("using stdio rwc")
		glog.Flush()
		rwc = stdioReadWriteCloser{}
		runConn(ctx, rwc)
	case "http":
		glog.Infof("using http")
		glog.Flush()
		// option1: https://sourcegraph.com/github.com/powerman/rpc-codec/-/blob/jsonrpc2/server.go
		// does mcp do this? we'd get NewServerCodec() which is an "net/rpc".ServerCodec
		//rpc.Register(jsonrpc2.HandlerWithError(handle))
		//http.HandleFunc("/rpc", jsonrpc2.HTTPHandler(nil)) // nil == use "net/rpc".DefaultServer

		// option2: somehow do bidi with just usual http
		//http.HandleFunc("/rpc",
		////func(w http.ResponseWriter, r *http.Request) {
		//	// runConn(r.Context(), w) // Not bidi!
		////})

		// option3: mcp with ws is a thing, presumably?
		// websockets: https://github.com/sourcegraph/jsonrpc2/blob/3c4c92ad61e8a64c37816d2c573f5d0094d96d33/jsonrpc2_test.go#L175-L201

		// option4: use https://pkg.go.dev/github.com/AdamSLevy/jsonrpc2 which has clear docs

		// option5: https://github.com/viant/jsonrpc is explicitly mentioning mcp; has "streamable" and "sse" transports

		// option6: dedicated mcp package? won't help with LSP later. https://pkg.go.dev/github.com/rvoh-emccaleb/mcp-golang/transport/sse

		// general info on SSE in Go: https://medium.com/@kristian15994/how-i-implemented-server-sent-events-in-go-3a55edcf4607

		// mcp spec: https://modelcontextprotocol.io/specification/2025-06-18 (schema defined in... ... ...typescript: https://github.com/modelcontextprotocol/modelcontextprotocol/blob/main/schema/2025-06-18/schema.ts)
		// but also https://github.com/modelcontextprotocol/modelcontextprotocol/blob/main/schema/2025-06-18/schema.json
		// mcp spec says: If using HTTP, the client MUST include the MCP-Protocol-Version: <protocol-version> HTTP header on all subsequent requests to the MCP server. For details, see the Protocol Version Header section in Transports.
		glog.Fatal(http.ListenAndServe(*listenAddr, nil))
	case "sse":
		// old deprecated http+sse method. see:
		// https://modelcontextprotocol.io/specification/2024-11-05/basic/transports#http-with-sse
		//
		// spec just says "use sse for server messages" (presumably
		// notifications only) and "use POST for clients to transmit messages"
		// (and presumably respond). there's no spec of actual endpoints to use;
		// official python SDK seems to suggest POST /messages/ and GET /sse
		// respectively, but on the client side it is unclear how to configure
		// this.
		//
		// actual behavior seems to be:
		// - client does GET /sse with "Accept: text/event-stream" to receive
		//   notifications
		// - client receives 'event: endpoint' + newline +
		//   'data: /sse/message?sessionId=...' + newline, or similar.
		// - client starts using POST to that endpoint to send messages
		// and this seems to match the python SDK behavior.
		//
		// see:
		// https://github.com/modelcontextprotocol/python-sdk/blob/4fee123e72f3e01d01e2fb31282eb206e8cee308/src/mcp/server/sse.py
		// where the creation of session URL is on line 161, and the endpoint is
		// indeed the first transmitted message / notification before any other.

		glog.Infof("using sse")
		glog.Flush()
		glog.Infof("url_base: %s (does not affect /sse and /messages/ handlers)", *urlBase)
		glog.Flush()
		sseHandler := newHTTPPlusSSEHandler()
		go sseHandler.loopSessionManagement()
		glog.Fatal(http.ListenAndServe(*listenAddr, sseHandler))
	default:
		glog.Errorf("unknown listen_type: %s", *listenType)
		glog.Flush()
		return
	}

}
