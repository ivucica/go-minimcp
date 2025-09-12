package main

import (
	// TODO: add badc0de.net/pkg/flagutil and invoke its Parse in init()
	"github.com/sourcegraph/jsonrpc2"

	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	// "net/rpc"
	"os"
	"time"
)

var (
	listenType = flag.String("listen_type", "stdio", "listen on stdio, or http, or sse")
	listenAddr = flag.String("listen_addr", ":15974", "listen on addr+port")
)

func init() {
	flag.Parse()
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

func handle(ctx context.Context, c *jsonrpc2.Conn, r *jsonrpc2.Request) (result interface{}, err error) {
	log.Printf("request: %+v", r)
	switch r.Method {
	case "initialize":
		// decode params according to iniitalizeParams
		//if err := c.Reply(ctx, r.ID, "test") { // this is if func (h *handler) Handle(ctx context.Context, c *jsonrpc2.Conn, r *jsonrpc2.Request) { }; or func (h *MyHandler) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) { might be the right signature, need to verify

		params := initializeParams{}
		if err := json.Unmarshal(*r.Params, &params); err != nil {
			// If unmarshaling fails, the params were invalid.
			log.Printf("Failed to unmarshal params: %v", err)
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "invalid params"}
		}
		log.Printf("conn from %+v", params.ClientInfo)

		go func() {
			time.Sleep(1 * time.Second)
			// TODO: ctx might be invalid, c might be unusable...
			ctx := context.TODO() // add a time block ...
			c.Notify(ctx, "notification/initialized", nil)
		}()

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
	default:
		//return struct{ A string }{A: "abc"}, nil
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeMethodNotFound, Message: "method not found"}
	}
}

func runConn(ctx context.Context, rwc io.ReadWriteCloser) {
	handler := jsonrpc2.HandlerWithError(handle) // s.Handle

	// conn := jsonrpc2.NewConn(ctx, jsonrpc2.NewPlainObjectStream(os.Stdio
	conn := jsonrpc2.NewConn(
		ctx,
		jsonrpc2.NewBufferedStream(rwc, jsonrpc2.VSCodeObjectCodec{}), // correct codec for mcp?
		handler)
	<-conn.DisconnectNotify()
	log.Println("closed a conn")
}

func main() {

	ctx := context.Background()

	var rwc io.ReadWriteCloser
	switch *listenType {
	case "stdio":
		rwc = stdioReadWriteCloser{}
		runConn(ctx, rwc)
	case "http":
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
		log.Fatal(http.ListenAndServe(*listenAddr, nil))
	case "sse":
		log.Fatal("sse not supported yet")
	}

}
