package main


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
