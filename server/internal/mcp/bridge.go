package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Bridge relays an MCP client that speaks over stdio — one that launches a
// command and writes newline-delimited JSON-RPC to it — to a server's
// Streamable HTTP endpoint. It is how a desktop client on a laptop reaches the
// archive on a Pi: the client runs `tickers mcp --url http://pi:8797/mcp`,
// and everything else happens on the Pi.
//
// Each message is POSTed as it arrives, concurrently, so a long sweep doesn't
// hold up a ping; answers are written as whole lines in whatever order they
// finish, which JSON-RPC's ids allow. A request the server can't be reached
// for is answered here with an error, so the client isn't left waiting on it.
// Bridge returns when in is exhausted and every answer has been written.
func Bridge(ctx context.Context, endpoint string, in io.Reader, out io.Writer, client *http.Client) error {
	if client == nil {
		client = http.DefaultClient
	}
	b := &bridge{endpoint: endpoint, client: client, out: out}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), maxBody)
	var wg sync.WaitGroup
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		msg := append([]byte(nil), line...)
		// The handshake goes alone: the version it settles is a header on
		// everything after it.
		if isInitialize(msg) {
			wg.Wait()
			b.relay(ctx, msg)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.relay(ctx, msg)
		}()
	}
	wg.Wait()
	return scanner.Err()
}

type bridge struct {
	endpoint string
	client   *http.Client

	mu      sync.Mutex
	out     io.Writer
	version string
}

func isInitialize(msg []byte) bool {
	var m message
	return json.Unmarshal(msg, &m) == nil && m.Method == "initialize"
}

func (b *bridge) relay(ctx context.Context, msg []byte) {
	answer, err := b.post(ctx, msg)
	if err != nil {
		b.fail(msg, err)
		return
	}
	if len(answer) == 0 {
		return
	}
	if isInitialize(msg) {
		var r struct {
			Result struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"result"`
		}
		if json.Unmarshal(answer, &r) == nil {
			b.mu.Lock()
			b.version = r.Result.ProtocolVersion
			b.mu.Unlock()
		}
	}
	b.write(answer)
}

func (b *bridge) post(ctx context.Context, msg []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	b.mu.Lock()
	if b.version != "" {
		req.Header.Set("MCP-Protocol-Version", b.version)
	}
	b.mu.Unlock()
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the tickers server at %s can't be reached: %w", b.endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading the tickers server's answer: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusAccepted:
		return nil, nil
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("the tickers server at %s answered %s: %s", b.endpoint, resp.Status, strings.TrimSpace(string(body)))
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		return nil, fmt.Errorf("the tickers server at %s didn't answer with JSON — is that its /mcp address?", b.endpoint)
	}
	return compact.Bytes(), nil
}

// fail answers every request in msg with err, since the server never will.
func (b *bridge) fail(msg []byte, err error) {
	var ids []json.RawMessage
	var batch []message
	if json.Unmarshal(msg, &batch) != nil {
		var one message
		if json.Unmarshal(msg, &one) != nil {
			b.write(encode(failure(nil, CodeParseError, "the message is not valid JSON")))
			return
		}
		batch = []message{one}
	}
	for _, m := range batch {
		if len(m.ID) > 0 && m.Method != "" {
			ids = append(ids, m.ID)
		}
	}
	for _, id := range ids {
		b.write(encode(failure(id, CodeInternalError, err.Error())))
	}
}

func (b *bridge) write(line []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.out.Write(append(line, '\n'))
}
