package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func botDo(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func TestBotChatPostsAsKevinWithOrigin(t *testing.T) {
	s, _ := newTest(t)
	h := s.botHandler()

	code, m := botDo(t, h, "POST", "/bot/chat", `{"text":"  look at SOS-3051 ","source":"grok bot!"}`)
	if code != 200 {
		t.Fatalf("POST /bot/chat: %d %v", code, m)
	}
	msgs := s.chat.List(0, 10)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	if msgs[0].From != "ben" {
		t.Errorf("from = %q, want the chat's label for Kevin", msgs[0].From)
	}
	if msgs[0].Text != "[via grokbot] look at SOS-3051" {
		t.Errorf("text = %q", msgs[0].Text)
	}
}

func TestBotChatRejectsEmptyAndLong(t *testing.T) {
	s, _ := newTest(t)
	h := s.botHandler()
	if code, _ := botDo(t, h, "POST", "/bot/chat", `{"text":"   "}`); code != 400 {
		t.Errorf("empty: %d, want 400", code)
	}
	long := strings.Repeat("x", botMaxText+1)
	if code, _ := botDo(t, h, "POST", "/bot/chat", `{"text":"`+long+`"}`); code != 400 {
		t.Errorf("long: %d, want 400", code)
	}
	if n := len(s.chat.List(0, 10)); n != 0 {
		t.Errorf("%d messages stored, want 0", n)
	}
}

func TestBotChatReadAndStatus(t *testing.T) {
	s, _ := newTest(t)
	h := s.botHandler()
	s.chat.Add("ben", "hi", "")
	s.chat.Add("eldar", "on it", "")

	code, m := botDo(t, h, "GET", "/bot/chat?after=1", "")
	if code != 200 {
		t.Fatalf("GET /bot/chat: %d", code)
	}
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["text"] != "on it" {
		t.Errorf("after=1: %v", m["messages"])
	}

	code, m = botDo(t, h, "GET", "/bot/status", "")
	if code != 200 {
		t.Fatalf("GET /bot/status: %d", code)
	}
	if m["pendingApprovals"] != float64(0) || m["lastChatId"] != float64(2) {
		t.Errorf("status: %v", m)
	}
}

// The bot socket must not expose anything from the admin API.
func TestBotHandlerHasNoAdminRoutes(t *testing.T) {
	s, _ := newTest(t)
	h := s.botHandler()
	for _, p := range []string{"/admin/state", "/admin/chat", "/admin/approvals", "/admin/grants", "/admin/cache/clear", "/admin/terminal"} {
		for _, method := range []string{"GET", "POST"} {
			if code, _ := botDo(t, h, method, p, `{}`); code != 404 && code != 405 {
				t.Errorf("%s %s on the bot socket: %d, want 404", method, p, code)
			}
		}
	}
}

func TestBotSource(t *testing.T) {
	for in, want := range map[string]string{"": "bot", "grokbot": "grokbot", "x y<z>": "xyz", strings.Repeat("a", 40): strings.Repeat("a", 32)} {
		if got := botSource(in); got != want {
			t.Errorf("botSource(%q) = %q, want %q", in, got, want)
		}
	}
}
