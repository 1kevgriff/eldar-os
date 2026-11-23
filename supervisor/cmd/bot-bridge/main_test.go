package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const testToken = "0123456789abcdef0123456789abcdef"

// fakeSup stands in for the supervisor's bot socket.
type fakeSup struct {
	mu       sync.Mutex
	messages []chatMessage
	pending  int
	posted   []map[string]string
}

func (f *fakeSup) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "POST" && r.URL.Path == "/bot/chat":
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b)
		if strings.TrimSpace(b["text"]) == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "empty message"})
			return
		}
		f.posted = append(f.posted, b)
		m := chatMessage{ID: len(f.messages) + 1, From: "ben", Text: "[via " + b["source"] + "] " + b["text"]}
		f.messages = append(f.messages, m)
		json.NewEncoder(w).Encode(map[string]any{"message": m, "connected": true})
	case r.Method == "GET" && r.URL.Path == "/bot/chat":
		after := 0
		json.Unmarshal([]byte(r.URL.Query().Get("after")), &after)
		out := []chatMessage{}
		for _, m := range f.messages {
			if m.ID > after {
				out = append(out, m)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"messages": out})
	case r.Method == "GET" && r.URL.Path == "/bot/status":
		last := 0
		if len(f.messages) > 0 {
			last = f.messages[len(f.messages)-1].ID
		}
		json.NewEncoder(w).Encode(status{EldarConnected: true, PendingApprovals: f.pending, LastChatID: last})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeSup) add(from, text string) {
	f.mu.Lock()
	f.messages = append(f.messages, chatMessage{ID: len(f.messages) + 1, From: from, Text: text})
	f.mu.Unlock()
}

func setup(t *testing.T) (*bridge, *fakeSup, *supervisor) {
	t.Helper()
	f := &fakeSup{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	sup := &supervisor{base: srv.URL, client: srv.Client()}
	return &bridge{token: testToken, source: "grokbot", sup: sup}, f, sup
}

func rpc(t *testing.T, b *bridge, token, method, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, "/mcp", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, req)
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func toolText(t *testing.T, m map[string]any) (string, bool) {
	t.Helper()
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", m)
	}
	content := res["content"].([]any)
	return content[0].(map[string]any)["text"].(string), res["isError"].(bool)
}

func TestAuth(t *testing.T) {
	b, _, _ := setup(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	for _, tok := range []string{"", "wrong", testToken + "x", strings.ToUpper(testToken)} {
		if code, _ := rpc(t, b, tok, "POST", body); code != 401 {
			t.Errorf("token %q: %d, want 401", tok, code)
		}
	}
	if code, _ := rpc(t, b, testToken, "POST", body); code != 200 {
		t.Errorf("good token: %d", code)
	}
	if code, _ := rpc(t, b, testToken, "GET", ""); code != 405 {
		t.Errorf("GET: %d, want 405", code)
	}
	req := httptest.NewRequest("POST", "/other", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Errorf("/other: %d, want 404", rec.Code)
	}
}

func TestInitializeAndList(t *testing.T) {
	b, _, _ := setup(t)
	_, m := rpc(t, b, testToken, "POST", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	res := m["result"].(map[string]any)
	if res["protocolVersion"] != "2025-03-26" {
		t.Errorf("protocolVersion = %v", res["protocolVersion"])
	}
	if code, _ := rpc(t, b, testToken, "POST", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != 202 {
		t.Errorf("notification: %d, want 202", code)
	}
	_, m = rpc(t, b, testToken, "POST", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	names := []string{}
	for _, tl := range m["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "eldar_send,eldar_replies,eldar_status" {
		t.Errorf("tools = %v", names)
	}
	_, m = rpc(t, b, testToken, "POST", `{"jsonrpc":"2.0","id":3,"method":"resources/list"}`)
	if m["error"] == nil {
		t.Errorf("unknown method should error: %v", m)
	}
}

func TestTools(t *testing.T) {
	b, f, _ := setup(t)
	_, m := rpc(t, b, testToken, "POST", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"eldar_send","arguments":{"text":"work SOS-3051"}}}`)
	if text, isErr := toolText(t, m); isErr || !strings.Contains(text, "Sent (message 1)") {
		t.Errorf("eldar_send: %q %v", text, isErr)
	}
	if len(f.posted) != 1 || f.posted[0]["source"] != "grokbot" || f.posted[0]["text"] != "work SOS-3051" {
		t.Errorf("posted: %v", f.posted)
	}

	f.add("eldar", "On it.")
	_, m = rpc(t, b, testToken, "POST", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"eldar_replies","arguments":{"after":0}}}`)
	text, _ := toolText(t, m)
	if !strings.Contains(text, "[1] kevin") || !strings.Contains(text, "[2] eldar") || !strings.Contains(text, "On it.") {
		t.Errorf("eldar_replies:\n%s", text)
	}

	_, m = rpc(t, b, testToken, "POST", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"eldar_send","arguments":{"text":"  "}}}`)
	if text, isErr := toolText(t, m); !isErr || !strings.Contains(text, "empty") {
		t.Errorf("empty send should be a tool error: %q %v", text, isErr)
	}

	_, m = rpc(t, b, testToken, "POST", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"approve","arguments":{}}}`)
	if m["error"] == nil {
		t.Errorf("unknown tool should be an RPC error: %v", m)
	}
}

func TestWebhook(t *testing.T) {
	_, f, sup := setup(t)
	var mu sync.Mutex
	var got []map[string]any
	secret := []byte("shh")
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, secret)
		mac.Write(body)
		if r.Header.Get("X-Eldar-Signature-256") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
			t.Errorf("bad signature")
		}
		var m map[string]any
		json.Unmarshal(body, &m)
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	}))
	defer recv.Close()
	w := &webhook{url: recv.URL, secret: secret, sup: sup, client: recv.Client()}
	ctx := context.Background()

	f.add("eldar", "old reply") // before the first poll: never sent
	if err := w.poll(ctx); err != nil {
		t.Fatal(err)
	}
	f.add("ben", "[via grokbot] hi")
	f.add("eldar", "new reply")
	f.mu.Lock()
	f.pending = 2
	f.mu.Unlock()
	if err := w.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.poll(ctx); err != nil { // nothing new: nothing sent
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("events: %v", got)
	}
	if got[0]["event"] != "eldar.reply" || got[0]["text"] != "new reply" {
		t.Errorf("first event: %v", got[0])
	}
	if got[1]["event"] != "approvals.pending" || got[1]["count"] != float64(2) {
		t.Errorf("second event: %v", got[1])
	}
}
