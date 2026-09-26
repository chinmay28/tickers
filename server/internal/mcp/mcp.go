// Package mcp is a Model Context Protocol server: JSON-RPC 2.0 messages, the
// handshake, tools and static resources, over the Streamable HTTP transport.
//
// It is written here rather than imported for the same reason the SQLite
// driver is the only dependency: the protocol surface a tool server needs is a
// few hundred lines, and the binary has to cross-compile to a Pi with nothing
// else installed. It knows nothing about markets; what the tools do is the
// mcptools package's business, the way the REST API's is engine's.
//
// Only what a tool server needs is implemented. There is no server-to-client
// stream — every answer is the response to the request that asked — so a GET
// is refused, as the transport allows, and there are no sessions to track.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

// Versions are the protocol revisions this server speaks, newest first. A
// client asking for one of them gets it; any other gets the newest, and
// decides for itself whether it can carry on.
var Versions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Info introduces the server during the handshake.
type Info struct {
	Name    string
	Title   string
	Version string
	// Instructions tell a client's model how the tools fit together — the
	// things no single tool's description can say.
	Instructions string
}

// Tool is one callable tool.
type Tool struct {
	Name        string
	Title       string
	Description string
	// InputSchema is the JSON Schema of the arguments object.
	InputSchema json.RawMessage
	// ReadOnly says the tool changes nothing a person would notice, which
	// lets a client run it without asking first.
	ReadOnly bool
	// Handler runs the tool. What it returns is sent back as JSON; an error
	// is sent back as the tool's result, marked as one, so the model reads
	// the sentence and can correct itself — it is not a protocol failure.
	Handler func(ctx context.Context, args json.RawMessage) (any, error)
}

// Resource is a fixed document a client can read: reference material too
// long for a tool description.
type Resource struct {
	URI         string
	Name        string
	Title       string
	Description string
	MIMEType    string
	Text        string
}

// Server dispatches MCP messages to its tools and resources. Register
// everything before serving; the registries are not guarded for writes.
type Server struct {
	info      Info
	tools     []Tool
	resources []Resource
}

// NewServer returns a server with nothing registered.
func NewServer(info Info) *Server { return &Server{info: info} }

// AddTool registers a tool. A duplicate name or a schema that isn't a JSON
// object is a programming error and panics.
func (s *Server) AddTool(t Tool) {
	if s.tool(t.Name) != nil {
		panic("mcp: duplicate tool " + t.Name)
	}
	var schema map[string]any
	if err := json.Unmarshal(t.InputSchema, &schema); err != nil || schema["type"] != "object" {
		panic("mcp: tool " + t.Name + " needs an object input schema")
	}
	s.tools = append(s.tools, t)
}

// AddResource registers a resource. A duplicate URI panics.
func (s *Server) AddResource(r Resource) {
	if s.resource(r.URI) != nil {
		panic("mcp: duplicate resource " + r.URI)
	}
	s.resources = append(s.resources, r)
}

func (s *Server) tool(name string) *Tool {
	for i := range s.tools {
		if s.tools[i].Name == name {
			return &s.tools[i]
		}
	}
	return nil
}

func (s *Server) resource(uri string) *Resource {
	for i := range s.resources {
		if s.resources[i].URI == uri {
			return &s.resources[i]
		}
	}
	return nil
}

// JSON-RPC error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	// CodeResourceNotFound is MCP's own, for a resource URI it doesn't know.
	CodeResourceNotFound = -32002
)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Handle answers one JSON-RPC message or batch. It returns nil when there is
// nothing to answer: a notification, or a client's response to a request.
func (s *Server) Handle(ctx context.Context, raw []byte) []byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(raw, &batch); err != nil {
			return encode(failure(nil, CodeParseError, "the message is not valid JSON"))
		}
		if len(batch) == 0 {
			return encode(failure(nil, CodeInvalidRequest, "an empty batch"))
		}
		var out []*response
		for _, m := range batch {
			if r := s.handleOne(ctx, m); r != nil {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return encode(out)
	}
	if r := s.handleOne(ctx, raw); r != nil {
		return encode(r)
	}
	return nil
}

func encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(failure(nil, CodeInternalError, "the response could not be encoded"))
	}
	return b
}

func failure(id json.RawMessage, code int, msg string) *response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func (s *Server) handleOne(ctx context.Context, raw json.RawMessage) *response {
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		return failure(nil, CodeParseError, "the message is not valid JSON")
	}
	if m.JSONRPC != "2.0" {
		return failure(m.ID, CodeInvalidRequest, `the message is not JSON-RPC 2.0 ("jsonrpc": "2.0" is missing)`)
	}
	if m.Method == "" {
		if m.Result != nil || m.Error != nil {
			return nil // a client answering something; this server never asks
		}
		return failure(m.ID, CodeInvalidRequest, "the message has no method")
	}
	notification := len(m.ID) == 0
	result, rerr := s.dispatch(ctx, m.Method, m.Params)
	if notification {
		return nil
	}
	if rerr != nil {
		return failure(m.ID, rerr.Code, rerr.Message)
	}
	return &response{JSONRPC: "2.0", ID: m.ID, Result: result}
}

func (s *Server) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return s.initialize(params)
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return s.listTools(), nil
	case "tools/call":
		return s.callTool(ctx, params)
	case "resources/list":
		return s.listResources(), nil
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []any{}}, nil
	case "resources/read":
		return s.readResource(params)
	}
	if strings.HasPrefix(method, "notifications/") {
		return struct{}{}, nil // initialized, cancelled: nothing to do
	}
	return nil, &rpcError{Code: CodeMethodNotFound, Message: fmt.Sprintf("method %q is not supported", method)}
}

func (s *Server) initialize(params json.RawMessage) (any, *rpcError) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: CodeInvalidParams, Message: "initialize: the params are not an object"}
		}
	}
	version := Versions[0]
	if slices.Contains(Versions, p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	info := map[string]string{"name": s.info.Name, "version": s.info.Version}
	if s.info.Title != "" {
		info["title"] = s.info.Title
	}
	caps := map[string]any{"tools": map[string]bool{"listChanged": false}}
	if len(s.resources) > 0 {
		caps["resources"] = map[string]bool{"listChanged": false, "subscribe": false}
	}
	out := map[string]any{"protocolVersion": version, "capabilities": caps, "serverInfo": info}
	if s.info.Instructions != "" {
		out["instructions"] = s.info.Instructions
	}
	return out, nil
}

type toolView struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations map[string]any  `json:"annotations"`
}

func (s *Server) listTools() any {
	out := make([]toolView, len(s.tools))
	for i, t := range s.tools {
		ann := map[string]any{"readOnlyHint": t.ReadOnly, "openWorldHint": false}
		if t.Title != "" {
			ann["title"] = t.Title
		}
		if !t.ReadOnly {
			ann["destructiveHint"] = false
		}
		out[i] = toolView{Name: t.Name, Title: t.Title, Description: t.Description, InputSchema: t.InputSchema, Annotations: ann}
	}
	return map[string]any{"tools": out}
}

type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type callResult struct {
	Content []content `json:"content"`
	IsError bool      `json:"isError"`
}

func (s *Server) callTool(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: CodeInvalidParams, Message: "tools/call: the params are not an object"}
	}
	t := s.tool(p.Name)
	if t == nil {
		return nil, &rpcError{Code: CodeInvalidParams, Message: fmt.Sprintf("unknown tool %q", p.Name)}
	}
	args := p.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	v, err := t.Handler(ctx, args)
	if err != nil {
		return callResult{Content: []content{{Type: "text", Text: err.Error()}}, IsError: true}, nil
	}
	text, err := json.Marshal(v)
	if err != nil {
		return callResult{Content: []content{{Type: "text", Text: "the result could not be encoded: " + err.Error()}}, IsError: true}, nil
	}
	return callResult{Content: []content{{Type: "text", Text: string(text)}}}, nil
}

func (s *Server) listResources() any {
	out := make([]map[string]string, len(s.resources))
	for i, r := range s.resources {
		v := map[string]string{"uri": r.URI, "name": r.Name, "mimeType": r.MIMEType}
		if r.Title != "" {
			v["title"] = r.Title
		}
		if r.Description != "" {
			v["description"] = r.Description
		}
		out[i] = v
	}
	return map[string]any{"resources": out}
}

func (s *Server) readResource(params json.RawMessage) (any, *rpcError) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: CodeInvalidParams, Message: "resources/read: the params are not an object"}
	}
	r := s.resource(p.URI)
	if r == nil {
		return nil, &rpcError{Code: CodeResourceNotFound, Message: fmt.Sprintf("no resource %q", p.URI)}
	}
	return map[string]any{"contents": []map[string]string{{"uri": r.URI, "mimeType": r.MIMEType, "text": r.Text}}}, nil
}

// Decode reads a tool's arguments into v, refusing fields v doesn't have: a
// misspelt argument silently ignored is a model left wondering why its
// setting did nothing.
func Decode(args json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var syntax *json.SyntaxError
		var typ *json.UnmarshalTypeError
		switch {
		case errors.As(err, &typ) && typ.Field != "":
			return fmt.Errorf("invalid arguments: %s must be a %s", typ.Field, jsonKind(typ.Type.Kind().String()))
		case errors.As(err, &syntax):
			return errors.New("invalid arguments: not valid JSON")
		}
		return fmt.Errorf("invalid arguments: %s", strings.TrimPrefix(err.Error(), "json: "))
	}
	return nil
}

func jsonKind(goKind string) string {
	switch goKind {
	case "string":
		return "string"
	case "slice", "array":
		return "list"
	case "map", "struct":
		return "object"
	case "bool":
		return "boolean"
	}
	return "number"
}

// maxBody bounds one POSTed message. Arguments are a strategy or a list of
// symbols; a megabyte is room for anything legitimate.
const maxBody = 1 << 20

// ServeHTTP is the Streamable HTTP transport, answering every request in its
// own response.
//
// A request carrying an Origin is refused. MCP clients are programs, not web
// pages, and don't send one; a browser does, and a page on another site that
// makes a visitor's browser POST here — to a server on their own network with
// no login in front of it — is exactly what the specification asks servers to
// stop.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "this MCP endpoint takes POSTed JSON-RPC messages and has no event stream", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Origin") != "" {
		http.Error(w, "requests from web pages are refused", http.StatusForbidden)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(Versions, v) {
		http.Error(w, fmt.Sprintf("unsupported MCP protocol version %q (this server speaks %s)", v, strings.Join(Versions, ", ")), http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "the message is too large", http.StatusRequestEntityTooLarge)
		return
	}
	out := s.Handle(r.Context(), body)
	if out == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}
