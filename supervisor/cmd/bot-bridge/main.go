// bot-bridge lets an outside chat bot (one that speaks MCP and takes
// webhooks, e.g. a Grok bot) talk to Eldar on Kevin's behalf.
//
// It joins the tailnet as its own node (tsnet) and serves over Tailscale
// Funnel: public HTTPS, with no access to the tailnet.
//
//   - POST /mcp: an MCP server (JSON-RPC over HTTP, one JSON response per
//     request) with three tools: eldar_send, eldar_replies, eldar_status.
//     Every request needs "Authorization: Bearer <BOT_TOKEN>".
//   - Outbound: when Eldar replies in the chat, or the number of approvals
//     waiting on Kevin goes up, it POSTs an event to BOT_WEBHOOK_URL, signed
//     with HMAC-SHA256 of the body (X-Eldar-Signature-256: sha256=<hex>).
//
// It reaches the supervisor only through the bot socket (server/bot.go):
// chat and counts. It can't approve, grant, read credentials or open a
// terminal; whatever it asks Eldar to do still waits on Kevin where a
// credential is involved.
//
// Configured by environment: BOT_TOKEN (required, 32+ characters),
// BOT_HOSTNAME (tailnet name, default eldar-bot), BOT_STATE_DIR (tsnet state;
// a one-time auth key in authkey there), BOT_SOCKET (default
// /run/supervisor/bot.sock), BOT_SOURCE (label on its chat messages, default
// grokbot), BOT_WEBHOOK_URL and BOT_WEBHOOK_SECRET (optional, together).
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	token := strings.TrimSpace(os.Getenv("BOT_TOKEN"))
	if len(token) < 32 {
		log.Fatal("bot-bridge: BOT_TOKEN must be set (32+ characters)")
	}
	hostname := env("BOT_HOSTNAME", "eldar-bot")
	stateDir := env("BOT_STATE_DIR", "/var/lib/eldar-bot")
	sock := env("BOT_SOCKET", "/run/supervisor/bot.sock")
	sup := &supervisor{base: "http://supervisor", client: &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}},
	}}
	b := &bridge{token: token, source: env("BOT_SOURCE", "grokbot"), sup: sup}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if u, secret := os.Getenv("BOT_WEBHOOK_URL"), os.Getenv("BOT_WEBHOOK_SECRET"); u != "" {
		if secret == "" {
			log.Fatal("bot-bridge: BOT_WEBHOOK_URL needs BOT_WEBHOOK_SECRET")
		}
		w := &webhook{url: u, secret: []byte(secret), sup: sup, client: &http.Client{Timeout: 15 * time.Second}}
		go w.run(ctx, 5*time.Second)
	}

	ts := &tsnet.Server{Hostname: hostname, Dir: stateDir, Logf: func(string, ...any) {}, UserLogf: log.Printf}
	keyFile := filepath.Join(stateDir, "authkey")
	if k, err := os.ReadFile(keyFile); err == nil {
		ts.AuthKey = strings.TrimSpace(string(k))
	}
	defer ts.Close()
	if _, err := ts.Up(ctx); err != nil {
		log.Fatalf("bot-bridge: tailnet: %v", err)
	}
	if ts.AuthKey != "" {
		os.Remove(keyFile) // registered; the one-time key has no further use
	}
	ln, err := ts.ListenFunnel("tcp", ":443", tsnet.FunnelOnly())
	if err != nil {
		log.Fatalf("bot-bridge: funnel: %v", err)
	}
	if d := ts.CertDomains(); len(d) > 0 {
		log.Printf("bot-bridge: MCP at https://%s/mcp", d[0])
	}
	srv := &http.Server{Handler: b, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("bot-bridge: %v", err)
	}
}

// supervisor is the bot socket's client.
type supervisor struct {
	base   string
	client *http.Client
}

func (s *supervisor) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("supervisor unreachable: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != 200 {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = res.Status
		}
		return errors.New(e.Error)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

type chatMessage struct {
	ID   int    `json:"id"`
	From string `json:"from"`
	Text string `json:"text"`
	TS   int64  `json:"ts"`
}

type status struct {
	EldarConnected   bool   `json:"eldarConnected"`
	PendingApprovals int    `json:"pendingApprovals"`
	Sessions         int    `json:"sessions"`
	LastChatID       int    `json:"lastChatId"`
	Mode             string `json:"mode"`
}

// who: the chat protocol labels Kevin "ben"; show the bot plain names.
func who(from string) string {
	if from == "ben" {
		return "kevin"
	}
	return from
}

// ---- MCP over HTTP ----

type bridge struct {
	token  string
	source string
	sup    *supervisor
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

func (b *bridge) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(tok)), []byte(b.token)) == 1
}

func (b *bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}
	if !b.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="eldar"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		// No server-initiated stream: every response comes back on its POST.
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeRPC(w, nil, nil, &rpcError{-32700, "parse error"})
		return
	}
	if len(req.ID) == 0 { // a notification (e.g. notifications/initialized)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rerr := b.call(r.Context(), req)
	writeRPC(w, req.ID, result, rerr)
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, rerr *rpcError) {
	if id == nil {
		id = json.RawMessage("null")
	}
	out := map[string]any{"jsonrpc": "2.0", "id": id}
	if rerr != nil {
		out["error"] = rerr
	} else {
		out["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

var tools = []map[string]any{
	{
		"name":        "eldar_send",
		"description": "Send a message to Eldar, Kevin's chief-of-staff agent, as Kevin. Use it to hand Eldar a task or answer its question. Eldar replies in the chat; read replies with eldar_replies. Anything that needs Kevin's credentials still waits for Kevin's own approval.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"text"}, "properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "The message (up to 4000 characters)."},
		}},
	},
	{
		"name":        "eldar_replies",
		"description": "Read the chat between Kevin and Eldar, oldest first. Pass `after` (a message id) to get only newer messages.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"after": map[string]any{"type": "integer", "description": "Only messages with a larger id."},
			"limit": map[string]any{"type": "integer", "description": "At most this many (default 20, max 100)."},
		}},
	},
	{
		"name":        "eldar_status",
		"description": "Whether Eldar is connected, how many approvals are waiting on Kevin, how many agent sessions exist, and the newest chat message id.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
}

func (b *bridge) call(ctx context.Context, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		v := protocolVersions[0]
		for _, s := range protocolVersions {
			if s == p.ProtocolVersion {
				v = s
			}
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "eldar", "version": "1"},
			"instructions":    "Eldar is Kevin's chief-of-staff agent. Hand it tasks with eldar_send and follow up with eldar_replies.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params"}
		}
		text, err := b.tool(ctx, p.Name, p.Arguments)
		if errors.Is(err, errUnknownTool) {
			return nil, &rpcError{-32602, "unknown tool: " + p.Name}
		}
		if err != nil {
			return toolResult(err.Error(), true), nil
		}
		return toolResult(text, false), nil
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method}
}

var errUnknownTool = errors.New("unknown tool")

func toolResult(text string, isErr bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}
}

func (b *bridge) tool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	switch name {
	case "eldar_send":
		var a struct {
			Text string `json:"text"`
		}
		json.Unmarshal(args, &a)
		var out struct {
			Message   chatMessage `json:"message"`
			Connected bool        `json:"connected"`
		}
		if err := b.sup.do(ctx, "POST", "/bot/chat", map[string]string{"text": a.Text, "source": b.source}, &out); err != nil {
			return "", err
		}
		if !out.Connected {
			return fmt.Sprintf("Sent (message %d). Eldar isn't connected right now; it will read it when it is.", out.Message.ID), nil
		}
		return fmt.Sprintf("Sent (message %d). Check eldar_replies with after=%d for the answer.", out.Message.ID, out.Message.ID), nil
	case "eldar_replies":
		var a struct {
			After int `json:"after"`
			Limit int `json:"limit"`
		}
		json.Unmarshal(args, &a)
		q := url.Values{"after": {strconv.Itoa(a.After)}, "limit": {strconv.Itoa(a.Limit)}}
		var out struct {
			Messages []chatMessage `json:"messages"`
		}
		if err := b.sup.do(ctx, "GET", "/bot/chat?"+q.Encode(), nil, &out); err != nil {
			return "", err
		}
		if len(out.Messages) == 0 {
			return "No messages.", nil
		}
		var sb strings.Builder
		for _, m := range out.Messages {
			fmt.Fprintf(&sb, "[%d] %s (%s): %s\n", m.ID, who(m.From), time.UnixMilli(m.TS).UTC().Format(time.RFC3339), m.Text)
		}
		return sb.String(), nil
	case "eldar_status":
		var st status
		if err := b.sup.do(ctx, "GET", "/bot/status", nil, &st); err != nil {
			return "", err
		}
		j, _ := json.Marshal(st)
		return string(j), nil
	}
	return "", errUnknownTool
}

// ---- outbound webhook ----

type webhook struct {
	url    string
	secret []byte
	sup    *supervisor
	client *http.Client

	lastID  int
	pending int
	started bool
}

func (w *webhook) run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := w.poll(ctx); err != nil {
			log.Printf("bot-bridge: webhook poll: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// poll sends one event per new Eldar reply and one when more approvals are
// waiting than last time. The first poll only records where things stand, so
// a restart doesn't replay old messages.
func (w *webhook) poll(ctx context.Context) error {
	var st status
	if err := w.sup.do(ctx, "GET", "/bot/status", nil, &st); err != nil {
		return err
	}
	if !w.started {
		w.lastID, w.pending, w.started = st.LastChatID, st.PendingApprovals, true
		return nil
	}
	if st.LastChatID > w.lastID {
		var out struct {
			Messages []chatMessage `json:"messages"`
		}
		if err := w.sup.do(ctx, "GET", fmt.Sprintf("/bot/chat?after=%d&limit=100", w.lastID), nil, &out); err != nil {
			return err
		}
		for _, m := range out.Messages {
			if m.From == "eldar" {
				w.send(ctx, map[string]any{"event": "eldar.reply", "id": m.ID, "text": m.Text, "ts": m.TS})
			}
			w.lastID = max(w.lastID, m.ID)
		}
	}
	if st.PendingApprovals > w.pending {
		w.send(ctx, map[string]any{"event": "approvals.pending", "count": st.PendingApprovals})
	}
	w.pending = st.PendingApprovals
	return nil
}

func (w *webhook) send(ctx context.Context, payload map[string]any) {
	body, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, w.secret)
	mac.Write(body)
	req, err := http.NewRequestWithContext(ctx, "POST", w.url, bytes.NewReader(body))
	if err != nil {
		log.Printf("bot-bridge: webhook: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Eldar-Event", fmt.Sprint(payload["event"]))
	req.Header.Set("X-Eldar-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	res, err := w.client.Do(req)
	if err != nil {
		log.Printf("bot-bridge: webhook %s: %v", payload["event"], err)
		return
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode >= 300 {
		log.Printf("bot-bridge: webhook %s: %s", payload["event"], res.Status)
	}
}
