package main

import (
	// TODO: add badc0de.net/pkg/flagutil and invoke its Parse in init()
	"github.com/golang/glog"

	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	// "net/rpc"
	"strings"
	"time"
)

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

type httpPlusSSESession struct {
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
type httpPlusSSEHandlerMgrMessage struct {
	action    string // "register", "unregister", "cleanup", "quit"
	sessionID string
	session   *httpPlusSSESession // only for "register"
}

type httpPlusSSEHandler struct {
	sessions map[string]*httpPlusSSESession
	mux      *http.ServeMux
	mgrChan  chan httpPlusSSEHandlerMgrMessage
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
		Event: "message",    // line 175 in sse.py
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
