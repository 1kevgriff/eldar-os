package server

import (
	"net/http"
	"strconv"
	"strings"
)

// The bot API: a narrow socket for an outside chat bot (cmd/bot-bridge) to
// talk to Eldar on Kevin's behalf. It can do three things: post to Kevin's
// chat with Eldar, read that chat, and read counts. Approvals, grants,
// credentials, settings and terminals exist only on the admin socket, so a
// compromised bot can ask Eldar to do things but can never approve them:
// every credentialed command still waits for Kevin.
//
// Messages from the bot are stored as Kevin's ("ben", the chat protocol's
// label for him) so Eldar's chat channel delivers them, and are prefixed with
// their origin so Eldar and Kevin can tell them apart.

const botMaxText = 4000

func (s *Supervisor) botHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /bot/chat", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Text   string `json:"text"`
			Source string `json:"source"`
		}
		readJSON(r, &b)
		text := strings.TrimSpace(b.Text)
		if text == "" {
			sendJSON(w, 400, map[string]any{"error": "empty message"})
			return
		}
		if len([]rune(text)) > botMaxText {
			sendJSON(w, 400, map[string]any{"error": "message too long (max " + strconv.Itoa(botMaxText) + " characters)"})
			return
		}
		source := botSource(b.Source)
		m, err := s.chat.Add("ben", "[via "+source+"] "+text, "")
		if err != nil {
			sendJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		logf("bot: %s posted chat message %d (%d characters)", source, m.ID, len([]rune(text)))
		connected, _ := s.chatStatus(r.Context())
		sendJSON(w, 200, map[string]any{"message": m, "connected": connected})
	})
	mux.HandleFunc("GET /bot/chat", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		after, _ := strconv.Atoi(q.Get("after"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 || limit > 100 {
			limit = 20
		}
		sendJSON(w, 200, map[string]any{"messages": s.chat.List(after, limit)})
	})
	mux.HandleFunc("GET /bot/status", func(w http.ResponseWriter, r *http.Request) {
		st := s.Snapshot()
		connected, _ := s.chatStatus(r.Context())
		sendJSON(w, 200, map[string]any{
			"eldarConnected":   connected,
			"pendingApprovals": len(st.Pending),
			"sessions":         len(st.Sessions),
			"lastChatId":       st.Chat.LastID,
			"mode":             st.Mode,
		})
	})
	return mux
}

// botSource: a short, plain label for where a message came from.
func botSource(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return -1
	}, s)
	if s == "" {
		return "bot"
	}
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}
