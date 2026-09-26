package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer() *Server {
	s := NewServer(Info{Name: "test", Version: "1.2.3", Instructions: "use the tools"})
	s.AddTool(Tool{
		Name: "echo", Title: "Echo", Description: "says it back", ReadOnly: true,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Handler: func(_ context.Context, args json.RawMessage) (any, error) {
			var in struct {
				Text string `json:"text"`
			}
			if err := Decode(args, &in); err != nil {
				return nil, err
			}
			if in.Text == "" {
				return nil, errors.New("say something")
			}
			return map[string]string{"said": in.Text}, nil
		},
	})
	s.AddTool(Tool{Name: "save", Description: "writes", InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler: func(context.Context, json.RawMessage) (any, error) { return nil, nil }})
	s.AddResource(Resource{URI: "test://doc", Name: "doc", MIMEType: "text/markdown", Text: "# Doc"})
	return s
}

// rpc sends one message and decodes the response.
func rpc(t *testing.T, s *Server, msg string) map[string]any {
	t.Helper()
	out := s.Handle(context.Background(), []byte(msg))
	if out == nil {
		t.Fatalf("%s: no response", msg)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("%s: response %s is not a JSON object", msg, out)
	}
	return v
}

func errorCode(v map[string]any) float64 {
	e, _ := v["error"].(map[string]any)
	code, _ := e["code"].(float64)
	return code
}

func TestInitializeNegotiatesTheVersion(t *testing.T) {
	s := testServer()
	v := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`)
	res := v["result"].(map[string]any)
	if res["protocolVersion"] != "2025-03-26" {
		t.Errorf("a client asking for a supported version got %v, want it echoed", res["protocolVersion"])
	}
	if res["instructions"] != "use the tools" || res["serverInfo"].(map[string]any)["version"] != "1.2.3" {
		t.Errorf("initialize = %v, want the server's info and instructions", res)
	}
	caps := res["capabilities"].(map[string]any)
	if caps["tools"] == nil || caps["resources"] == nil {
		t.Errorf("capabilities = %v, want tools and resources declared", caps)
	}
	v = rpc(t, s, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if got := v["result"].(map[string]any)["protocolVersion"]; got != Versions[0] {
		t.Errorf("an unknown version got %v, want the newest offered", got)
	}
}

func TestNotificationsAndResponsesGetNoAnswer(t *testing.T) {
	s := testServer()
	for _, msg := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
		`{"jsonrpc":"2.0","id":7,"result":{}}`,
		`[{"jsonrpc":"2.0","method":"notifications/initialized"}]`,
	} {
		if out := s.Handle(context.Background(), []byte(msg)); out != nil {
			t.Errorf("%s was answered with %s", msg, out)
		}
	}
}

func TestProtocolErrors(t *testing.T) {
	s := testServer()
	for msg, want := range map[string]float64{
		`{not json`:                CodeParseError,
		`{"id":1,"method":"ping"}`: CodeInvalidRequest,
		`{"jsonrpc":"2.0","id":1}`: CodeInvalidRequest,
		`[]`:                       CodeInvalidRequest,
		`{"jsonrpc":"2.0","id":1,"method":"sampling/createMessage"}`:              CodeMethodNotFound,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`: CodeInvalidParams,
		`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"x"}}`: CodeResourceNotFound,
	} {
		if got := errorCode(rpc(t, s, msg)); got != want {
			t.Errorf("%s: error code %v, want %v", msg, got, want)
		}
	}
	if v := rpc(t, s, `{"jsonrpc":"2.0","id":"a","method":"ping"}`); v["id"] != "a" || v["result"] == nil {
		t.Errorf("ping = %v, want an empty result under the request's own id", v)
	}
}

func TestToolsListAndCall(t *testing.T) {
	s := testServer()
	tools := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("listed %d tools, want 2", len(tools))
	}
	echo := tools[0].(map[string]any)
	ann := echo["annotations"].(map[string]any)
	if echo["name"] != "echo" || ann["readOnlyHint"] != true || echo["inputSchema"].(map[string]any)["type"] != "object" {
		t.Errorf("echo = %v, want its schema and a read-only hint", echo)
	}
	if ann := tools[1].(map[string]any)["annotations"].(map[string]any); ann["readOnlyHint"] != false || ann["destructiveHint"] != false {
		t.Errorf("save's annotations = %v, want it marked as writing but not destroying", ann)
	}

	call := func(args string) (string, bool) {
		t.Helper()
		v := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":`+args+`}}`)
		res := v["result"].(map[string]any)
		return res["content"].([]any)[0].(map[string]any)["text"].(string), res["isError"].(bool)
	}
	if text, isErr := call(`{"text":"hi"}`); isErr || text != `{"said":"hi"}` {
		t.Errorf("echo = %s (error %v), want the handler's value as JSON", text, isErr)
	}
	if text, isErr := call(`{}`); !isErr || text != "say something" {
		t.Errorf("a failing handler = %s (error %v), want its sentence marked as an error result", text, isErr)
	}
	if text, isErr := call(`{"txt":"hi"}`); !isErr || !strings.Contains(text, `"txt"`) {
		t.Errorf("a misspelt argument = %s (error %v), want it named", text, isErr)
	}
	if text, isErr := call(`{"text":5}`); !isErr || !strings.Contains(text, "text must be a string") {
		t.Errorf("a mistyped argument = %s (error %v), want the field and its type named", text, isErr)
	}
}

func TestBatchesAnswerEachRequest(t *testing.T) {
	out := testServer().Handle(context.Background(), []byte(`[
		{"jsonrpc":"2.0","id":1,"method":"ping"},
		{"jsonrpc":"2.0","method":"notifications/initialized"},
		{"jsonrpc":"2.0","id":2,"method":"nope"}]`))
	var v []map[string]any
	if err := json.Unmarshal(out, &v); err != nil || len(v) != 2 {
		t.Fatalf("batch = %s, want two answers — the notification gets none", out)
	}
	if v[0]["id"] != float64(1) || errorCode(v[1]) != CodeMethodNotFound {
		t.Errorf("batch = %v, want the ping answered and the unknown method refused", v)
	}
}

func TestResources(t *testing.T) {
	s := testServer()
	list := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`)["result"].(map[string]any)["resources"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["uri"] != "test://doc" {
		t.Errorf("resources = %v, want the one registered", list)
	}
	read := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"test://doc"}}`)
	c := read["result"].(map[string]any)["contents"].([]any)[0].(map[string]any)
	if c["text"] != "# Doc" || c["mimeType"] != "text/markdown" {
		t.Errorf("read = %v, want the document", c)
	}
}

func TestRegisteringTwiceOrWithoutASchemaPanics(t *testing.T) {
	for name, add := range map[string]func(*Server){
		"a duplicate tool":     func(s *Server) { s.AddTool(Tool{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}) },
		"a non-object schema":  func(s *Server) { s.AddTool(Tool{Name: "x", InputSchema: json.RawMessage(`{"type":"string"}`)}) },
		"a duplicate resource": func(s *Server) { s.AddResource(Resource{URI: "test://doc"}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s was accepted", name)
				}
			}()
			add(testServer())
		}()
	}
}

func TestHTTPTransport(t *testing.T) {
	s := testServer()
	post := func(body string, header map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}
	rec := post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" || !strings.Contains(rec.Body.String(), `"id":1`) {
		t.Errorf("a request = %d %q %s, want 200 JSON with its answer", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	if rec := post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Errorf("a notification = %d %s, want 202 with no body", rec.Code, rec.Body)
	}
	if rec := post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Origin": "https://evil.example"}); rec.Code != http.StatusForbidden {
		t.Errorf("a request from a web page = %d, want 403", rec.Code)
	}
	if rec := post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"MCP-Protocol-Version": "1999-01-01"}); rec.Code != http.StatusBadRequest {
		t.Errorf("an unsupported version header = %d, want 400", rec.Code)
	}
	if rec := post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"MCP-Protocol-Version": "2025-06-18"}); rec.Code != http.StatusOK {
		t.Errorf("a supported version header = %d, want 200", rec.Code)
	}
	if rec := post(strings.Repeat(" ", maxBody+1), nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized body = %d, want 413", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	get := httptest.NewRecorder()
	s.ServeHTTP(get, req)
	if get.Code != http.StatusMethodNotAllowed || get.Header().Get("Allow") != http.MethodPost {
		t.Errorf("a GET = %d (Allow %q), want 405 naming POST — there is no event stream", get.Code, get.Header().Get("Allow"))
	}
}
