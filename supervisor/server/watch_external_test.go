package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// An external watch: registered with the owner's ambient token, told of each
// new review once, then of the merge (and removed).
func TestExternalWatch(t *testing.T) {
	s := newAmbientTest(t) // owner acme has ambient token "pat"
	var mu sync.Mutex
	pr := map[string]any{"state": "open", "merged": false, "title": "Fix alerts"}
	reviews := []any{map[string]any{"id": 1, "state": "COMMENTED", "user": map[string]any{"login": "old"}}}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer pat" {
			http.Error(w, "bad token", 401)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/app/pulls/5":
			json.NewEncoder(w).Encode(pr)
		case "/repos/acme/app/pulls/5/reviews":
			json.NewEncoder(w).Encode(reviews)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	dir := t.TempDir()
	s.cfg.GitHub = &GitHubApp{APIBase: api.URL} // no App: external only
	s.watches = NewWatches(filepath.Join(dir, "watches.json"))
	s.relay = NewRelay(filepath.Join(dir, "relay.jsonl"), filepath.Join(dir, "relay-state.json"))
	ctx := context.Background()

	x, _, err := s.watchExternal(ctx, "acme/app", Watch{PR: 5, SessionID: "sess"})
	if err != nil || !x.External || x.LastReview != 1 || x.Title != "Fix alerts" {
		t.Fatalf("watch: %+v %v", x, err)
	}
	if _, _, err := s.watchExternal(ctx, "nobody/app", Watch{PR: 5, SessionID: "sess"}); err == nil {
		t.Fatal("an owner without an ambient token should be refused")
	}

	s.checkExternalWatches(ctx) // nothing new
	if len(s.relay.events) != 0 {
		t.Fatalf("nothing new, yet: %+v", s.relay.events)
	}

	mu.Lock()
	reviews = append(reviews, map[string]any{"id": 2, "state": "CHANGES_REQUESTED", "body": "rename  this", "user": map[string]any{"login": "wade"}})
	mu.Unlock()
	s.checkExternalWatches(ctx)
	s.checkExternalWatches(ctx) // reported once
	if len(s.relay.events) != 1 || !strings.Contains(s.relay.events[0].Text, `wade: changes requested "rename this"`) {
		t.Fatalf("review: %+v", s.relay.events)
	}

	mu.Lock()
	pr = map[string]any{"state": "closed", "merged": true, "merge_commit_sha": "abcdef123", "title": "Fix alerts"}
	mu.Unlock()
	s.checkExternalWatches(ctx)
	if len(s.relay.events) != 2 || !strings.Contains(s.relay.events[1].Text, "acme/app#5 (Fix alerts) is merged (abcdef1). Merged is not deployed") {
		t.Fatalf("merge: %+v", s.relay.events)
	}
	if n := len(s.watches.All()); n != 0 {
		t.Fatalf("watch should be done: %d left", n)
	}
}

// Apply reports leave external watches alone.
func TestAppliedSkipsExternal(t *testing.T) {
	s := watchTest(t, map[string]any{"state": "open", "merged": false, "title": "x"}, "ahead")
	s.watches.Add(Watch{Repo: "acme/app", PR: 9, SessionID: "sess", External: true})
	s.checkWatches("0123456789abcdef", nil, true, "")
	if n := len(s.watches.All()); n != 2 {
		t.Fatalf("watches: %d", n)
	}
}
