package main

import (
	// TODO: add badc0de.net/pkg/flagutil and invoke its Parse in init()
	"github.com/golang/glog"
	"github.com/sourcegraph/jsonrpc2"

	"context"
	"encoding/json"
	"fmt"
	"flag"
	"io"
	"net"
	"net/http"
	// "net/rpc"
	"os"
	"strings"
	"time"
)

var (
	listenType = flag.String("listen_type", "stdio", "listen on stdio, or http, or sse")
	listenAddr = flag.String("listen_addr", ":15974", "listen on addr+port")
	urlBase   = flag.String("url_base", "", "base URL for HTTP/SSE endpoints, without trailing slash; empty defaults to http://${listen_addr}; if listen_addr has no host, os.Hostname() is assumed; intended only for constructing returned endpoint (i.e. does not change handlers, incl. stripping prefixes, for now)")
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

type stdioReadWriteCloser struct{} // https://stackoverflow.com/a/76697349/39974

// var _ io.ReadWriteCloser = (*stdioReadWriteCloser)(nil)

func (c stdioReadWriteCloser) Read(p []byte) (n int, err error) {
	return os.Stdin.Read(p)
}

func (c stdioReadWriteCloser) Write(p []byte) (n int, err error) {
	return os.Stdout.Write(p)
}

func (c stdioReadWriteCloser) Close() error {
	return nil
}

// via https://modelcontextprotocol.io/specification/2025-06-18/basic/lifecycle
type initializeParams struct { // TODO: generate from schema.json
	// JSONRPCVersion string `json:"jsonrpc"` // mandatory
	// ID             string `json:"id"`      // mandatory in mcp
	// Method         string `json:"method"` // mandatory
	// Params:
	ProtocolVersion string `json:"protocolVersion"`
	Capabilities    struct {
		Roots map[string]bool `json:"roots"`
	} `json:"capabilities"`
	Sampling    map[string]interface{} `json:"sampling"`    // ?
	Elicitation map[string]interface{} `json:"elicitation"` // ?
	ClientInfo  peerInfo               `json:"clientInfo"`
}

type initializeResultFlags struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

type initializeResultCaps struct {
	Logging   map[string]bool       `json:"logging"`
	Prompts   initializeResultFlags `json:"prompts"`
	Resources initializeResultFlags `json:"resources"`
	Tools     initializeResultFlags `json:"tools"`
}

type peerInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

// via https://modelcontextprotocol.io/specification/2025-06-18/basic/lifecycle
type initializeResult struct { // TODO: really, really should be generated from schema
	ProtocolVersion string               `json:"protocolVersion"`
	Capabilities    initializeResultCaps `json:"capabilities"`
	ServerInfo      peerInfo             `json:"serverInfo"`
	Instructions    string               `json:"instructions,omitempty"`
}

type inputSchema struct {
	Type        string                  `json:"type"`
	Properties  map[string]*inputSchema `json:"properties,omitempty"`
	Description string                  `json:"description,omitempty"`
	Required    []string                `json:"required,omitempty"`
}

type tool struct {
	Name        string       `json:"name"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	InputSchema *inputSchema `json:"inputSchema"`
}

type toolsListResult struct {
	Tools      []tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

type toolsCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments,omitempty"`
}

type content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type toolsCallResult struct {
	Content []*content `json:"content"`
}

// Represents a notification that can be sent as an SSE or via JSON-RPC.
type notification struct {
	Event string      `json:"event"`
	Data  interface{} `json:"data,omitempty"`
}

// Used as a key for storing/retrieving the SSE channel in the context.
type sseChannelKeyType struct{}

// Key for storing/retrieving the SSE channel in the context.
var sseChannelKey = sseChannelKeyType{}

func sseChannelFromContext(ctx context.Context) chan notification {
	sseChannel, ok := ctx.Value(sseChannelKey).(chan notification)
	if !ok {
		return nil
	}
	return sseChannel
}

func contextWithSSEChannel(ctx context.Context, ch chan notification) context.Context {
	return context.WithValue(ctx, sseChannelKey, ch)
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

type httpPlusSSESession struct{
	id         string
	sseChannel chan notification
	createdAt  time.Time
}

func (h *httpPlusSSESession) Close() error {
	// assume unregister will happen
	close(h.sseChannel)
	h.sseChannel = nil
	return nil
}

func (h *httpPlusSSESession) SendNotification(n notification) error {
	// send notification over sseChannel
	if h.sseChannel == nil {
		glog.Errorf("sseChannel is closed, cannot send notification: %+v", n)
		return fmt.Errorf("sseChannel is closed")
	}
	h.sseChannel <- n
	return nil
}

// these cannot be replied to directly; they are for internal session management
type httpPlusSSEHandlerMgrMessage struct{
	action    string // "register", "unregister", "cleanup", "quit"
	sessionID string
	session   *httpPlusSSESession // only for "register"
}

type httpPlusSSEHandler struct{
	sessions map[string]*httpPlusSSESession
	mux 	 *http.ServeMux
	mgrChan chan httpPlusSSEHandlerMgrMessage
}

func (h *httpPlusSSEHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// Take in messages over a control channel to register new sessions, unregister
// them, or otherwise clean up. Should be a goroutine.
//
// Goroutine avoids mutexes.
func (h *httpPlusSSEHandler) loopSessionManagement() {
	glog.Info("starting loopSessionManagement")
	glog.Flush()

	h.mgrChan = make(chan httpPlusSSEHandlerMgrMessage, 10)
	for {
		select {
		case <-time.After(5 * time.Second):
			// reassure user we are alive via a log message.
			// should be removed.
			glog.Info("loopSessionManagement alive")
			glog.Flush()
		case msg := <-h.mgrChan:
			switch msg.action {
			case "register":
				h.registerSessionInternal(msg.session)
			case "unregister":
				h.unregisterSessionInternal(msg.sessionID)
			case "cleanup":
				h.cleanupSessionsInternal()
			case "quit":
				// TODO: quit all sessions
				return
			}
		}
	}
}

// must not be called directly; assumes either mutex over h.sessions has been
// grabbed, or called from singleton loopSessionManagement goroutine.
func (h *httpPlusSSEHandler) registerSessionInternal(session *httpPlusSSESession) {
	glog.Infof("registering session %s", session.id)
	h.sessions[session.id] = session
}

// must not be called directly; assumes either mutex over h.sessions has been
// grabbed, or called from singleton loopSessionManagement goroutine.
func (h *httpPlusSSEHandler) unregisterSessionInternal(sessionID string) {
	session, ok := h.sessions[sessionID]
	if !ok {
		return
	}
	
	glog.Infof("unregistering session %s", sessionID)
	close(session.sseChannel)
	session.sseChannel = nil

	delete(h.sessions, sessionID)
}

// must not be called directly; assumes either mutex over h.sessions has been
// grabbed, or called from singleton loopSessionManagement goroutine.
//
// TODO: add a wrapper which will send the 'cleanup' message so this get triggered,
// and then be called periodically. alternatively have a ticker in the loopSessionManagement
// goroutine itself.
func (h *httpPlusSSEHandler) cleanupSessionsInternal() {
	const maxSessionAge = 10 * time.Minute
	now := time.Now()
	for id, session := range h.sessions {
		if now.Sub(session.createdAt) > maxSessionAge {
			close(session.sseChannel)
			session.sseChannel = nil
			delete(h.sessions, id)
		}
	}
}

// Register a new SSE session.
//
// Takes w and r in case we want to process the request further.
func (h *httpPlusSSEHandler) registerSession(w http.ResponseWriter, r *http.Request) (*httpPlusSSESession, error) {
	// generate unique session ID
	sessionID := "session-" + time.Now().Format("20060102150405.000000000")

	session := &httpPlusSSESession{
		id:         sessionID,
		sseChannel: make(chan notification, 10), // buffered channel
		createdAt:  time.Now(),
	}

	// send to mgrChan to register in the mgr loop goroutine
	h.mgrChan <- httpPlusSSEHandlerMgrMessage{
		action:  "register",
		session: session,
	}

	return session, nil
}

// Unregister an SSE session.
func (h *httpPlusSSEHandler) unregisterSession(sessionID string) {
	// send to mgrChan to unregister in the mgr loop goroutine
	h.mgrChan <- httpPlusSSEHandlerMgrMessage{
		action:    "unregister",
		sessionID: sessionID,
	}
}

// Handle incoming SSE connections.
//
// These must first respond with the per-session endpoint URL, for sending
// POST messages, and then dispatch events over the SSE channel.
//
// After the session reg, equivalent of JSONRPC2 lib's Conn.Notify, but over
// SSE.
func (h *httpPlusSSEHandler) handleSSE(w http.ResponseWriter, r *http.Request) {
	// Assume valid path.
	// Verify this is GET.
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Set headers for SSE.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Register a new session. 
	session, err := h.registerSession(w, r)
	if err != nil {
		http.Error(w, "Failed to register session", http.StatusInternalServerError)
		glog.Errorf("failed to register sse session: %v", err)
		glog.Flush()
		return
	}
	if session == nil {
		http.Error(w, "Failed to register session", http.StatusInternalServerError)
		glog.Error("failed to register sse session")
		glog.Flush()
		return
	}
	sessionID := session.id
	glog.Infof("registered new SSE session: %s", sessionID)
	glog.Flush()

	// At this point incoming requests could land with the session's ID.
	// Let's notify the client about the session ID.

	// Transmit initial event with session ID-based URL: fling it to the channel.
	notify := notification{
		Event: "endpoint",
		Data:  *urlBase + "/messages/" + sessionID,
	}
	glog.Infof("sending initial SSE notification for session %s: %+v", sessionID, notify)
	session.SendNotification(notify)

	// Start sending events from the session's channel.
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		glog.Error("streaming unsupported")
		glog.Flush()
		return
	}

	// Listen to the session's channel and write events to the response.
	// TODO: move this to a function on session itself.
	for {
		select {
		case <-time.After(5 * time.Second):
			// reassure user session is still alive
			// remove later when this works
			glog.Infof("SSE session %s alive", sessionID)
		case n, ok := <-session.sseChannel:
			// Ending the session closes the channel: we will not be receiving
			// more events from the MCP and should unregister the session.
			if !ok {
				glog.Infof("SSE session %s channel closed", sessionID)
				glog.Flush()
				h.unregisterSession(sessionID)
				return
			}
			// Marshal notification data to JSON -- unless it's a string or
			// []byte already.
			//
			// This doesn't wrap it into JSON-RPC envelope automatically even if needed, it should
			// be done first.
			var data []byte
			switch n.Data.(type) {
				case string:
					data = []byte(n.Data.(string))
				case []byte:
					data = n.Data.([]byte)
				default:
					var err error
					data, err = json.Marshal(n.Data)
					if err != nil {
						glog.Errorf("failed to marshal notification data: %v", err)
						glog.Flush()
						continue
					}
			}
			// Write SSE event.
			_, err = w.Write([]byte("event: " + n.Event + "\n"))
			if err != nil {
				glog.Errorf("failed to write SSE event: %v", err)
				glog.Flush()
				return
			}
			_, err = w.Write([]byte("data: " + string(data) + "\n\n"))
			if err != nil {
				glog.Errorf("failed to write SSE data: %v", err)
				glog.Flush()
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			glog.Infof("SSE session %s context done", sessionID)
			glog.Flush()
			h.unregisterSession(sessionID)
			return
		}
	}
}

type singleRequestReadWriteCloser struct {
	requestData  []byte
	responseData []byte // n.b. we could take a WriteCloser instead, and pass HTTP response writer directly
	readDone     bool
	writeDone    bool
}

func (c *singleRequestReadWriteCloser) Read(p []byte) (n int, err error) {
	if c.readDone {
		return 0, io.EOF
	}
	n = copy(p, c.requestData)
	c.readDone = true
	return n, nil
}

func (c *singleRequestReadWriteCloser) Write(p []byte) (n int, err error) {
	if c.writeDone {
		return 0, io.EOF
	}
	c.responseData = append(c.responseData, p...)
	n = len(p)
	c.writeDone = true
	return n, nil
}

func (c *singleRequestReadWriteCloser) Close() error {
	return nil
}

func (c *singleRequestReadWriteCloser) GetResponseData() []byte {
	if !c.writeDone {
		// TODO: read more intelligently? accept a ctx and wait for write to complete or ctx done?
		return nil
	}
	return c.responseData
}

// Handle incoming messages over POST.
//
// These are JSON-RPC requests sent by the client to the MCP server.
//
// The session ID is extracted from the URL path.
func (h *httpPlusSSEHandler) handleMessages(w http.ResponseWriter, r *http.Request) {
	// Assume valid path.
	// Verify this is POST.
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract session ID from URL path.
	// Expecting /messages/{sessionId}
	sessionID := r.URL.Path[len("/messages/"):]

	// Log the incoming request.
	glog.Infof("incoming POST message for session %s", sessionID)

	// Find the session.
	session, ok := h.sessions[sessionID]
	if !ok {
		http.Error(w, "Session not found", http.StatusNotFound)
		glog.Errorf("session not found: %s", sessionID)
		glog.Flush()
		return
	}

	// Read the request body.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		glog.Errorf("failed to read request body for session %s: %v", sessionID, err)
		glog.Flush()
		return
	}

	// Process the incoming MCP message.
	// Use existing handle function, but with a context that has the session.sseChannel.
	ctx := contextWithSSEChannel(r.Context(), session.sseChannel)

	// quick assertion that the context now contains the sseChannel
	if sseChannelTmp := sseChannelFromContext(ctx); sseChannelTmp == nil {
		glog.Errorf("sseChannel not found in context for session %s", sessionID)
		glog.Flush()
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Create a temporary JSON-RPC connection with a transport that transmits a single
	// RPC and collects the response for flushing into the HTTP response.
	//
	// Equivalent of stdioReadWriteCloser, but for single request/response that
	// can be used for HTTP POST.
	//
	// TODO: make it wrap around http.Request.Body and http.ResponseWriter directly?
	rwc := &singleRequestReadWriteCloser{}

	/*
	conn := jsonrpc2.NewConn(
		ctx,
		jsonrpc2.NewBufferedStream(rwc, jsonrpc2.PlainObjectCodec{}),
		jsonrpc2.HandlerWithError(handle),
	)
		*/

	// Decode the incoming message as a JSON-RPC request.
	/*var req jsonrpc2.Request
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Failed to parse JSON-RPC request", http.StatusBadRequest)
		glog.Errorf("failed to parse JSON-RPC request for session %s: %v", sessionID, err)
		glog.Flush()
		return
	}*/

	// Pass the request into rwc for reading.
	rwc.requestData = body

	// Have conn read the request. We just start a loop because it will exit immediately.
	// It will also invoke handle for us.
	// TODO: make timeout configurable
	ctxT, cancel := context.WithDeadline(ctx, time.Now().Add(30*time.Second))
	defer cancel()
	runConn(ctxT, rwc) // TODO: this does not handle errors
/*
	// Handle the request.
	_, err = handle(ctx, conn, &req)
	if err != nil {
		http.Error(w, "Failed to handle JSON-RPC request", http.StatusInternalServerError)
		glog.Errorf("failed to handle JSON-RPC request for session %s: %v", sessionID, err)
		glog.Flush()
		return
	}
*/
	// Get the response data from rwc.
	responseData := rwc.GetResponseData()

	// Write the response data to the HTTP response.
	// TODO: this is not what's expected? looks like VSCode wants it over the notification chan.
	// https://github.com/modelcontextprotocol/python-sdk/blob/4fee123e72f3e01d01e2fb31282eb206e8cee308/src/mcp/server/sse.py line 247 just sends accepted.
	/*w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(responseData)
	if err != nil {
		glog.Errorf("failed to write HTTP response for session %s: %v", sessionID, err)
		glog.Flush()
		return
	}*/

	if responseData == nil {	
		http.Error(w, "Accepted (but no response", http.StatusAccepted)
		return
	}
	if strings.Contains(string(responseData), "MAGIC_RESPONSE_DO_NOT_TRANSMIT") {
		// do not transmit this response
		http.Error(w, "Accepted (no response transmitted)", http.StatusAccepted)
		return
	}

	// Send the response data as a notification over the session's SSE channel.
	session.SendNotification(notification{
		Event: "message", // line 175 in sse.py
		Data:  responseData, // probably JSON-RPC envelope, not just the response, so send it as-is
	})
	http.Error(w, "Accepted", http.StatusAccepted)
}

func newHTTPPlusSSEHandler() *httpPlusSSEHandler {
	h := &httpPlusSSEHandler{
		sessions: make(map[string]*httpPlusSSESession),
		mux:      http.NewServeMux(),
	}
	h.mux.HandleFunc("/sse", h.handleSSE)
	h.mux.HandleFunc("/messages/", h.handleMessages)
	return h
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
