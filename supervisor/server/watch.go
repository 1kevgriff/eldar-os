package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// PR watches: the session that opened a PR on the staging repo is told the
// moment its change is live (docs/decisions/0012-cycle-time.md). A session
// registers its PR (supervisor_watch_pr); after every apply, the apply service
// reports the revision and outcome on the admin socket (POST /admin/applied),
// and the supervisor asks GitHub whether each watched PR is merged and
// contained in that revision. If so, it types a notice into the session's
// herdr pane (or, if the session is gone, relays it to Eldar). Nothing in the
// report is trusted beyond "an apply of this revision finished".
//
// File (supervisor state): watches.json.

var validRev = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// validRepo: owner/name as GitHub allows them.
var validRepo = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

type Watch struct {
	ID        int    `json:"id"`
	Repo      string `json:"repo"` // owner/name
	PR        int    `json:"pr"`
	Title     string `json:"title"`
	SessionID string `json:"sessionId"`
	PaneID    string `json:"paneId,omitempty"`
	Created   int64  `json:"created"`
	// The last failed revision reported to the session, so one failure
	// isn't reported twice.
	FailedRev string `json:"failedRev,omitempty"`
	// External: a repo outside this system, polled with its owner's
	// ambient token (watch_external.go); LastReview is the newest review
	// already reported.
	External   bool  `json:"external,omitempty"`
	LastReview int64 `json:"lastReview,omitempty"`
}

type Watches struct {
	path   string
	mu     sync.Mutex
	list   []Watch
	nextID int
}

func NewWatches(path string) *Watches {
	w := &Watches{path: path, nextID: 1}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &w.list)
	}
	for _, x := range w.list {
		w.nextID = max(w.nextID, x.ID+1)
	}
	return w
}

func (w *Watches) saveLocked() {
	b, _ := json.MarshalIndent(w.list, "", "  ")
	if err := os.WriteFile(w.path, b, 0o600); err != nil {
		logf("watches: %v", err)
	}
}

func (w *Watches) Add(x Watch) Watch {
	w.mu.Lock()
	defer w.mu.Unlock()
	// One watch per session and PR.
	w.list = slices.DeleteFunc(w.list, func(o Watch) bool { return o.Repo == x.Repo && o.PR == x.PR && o.SessionID == x.SessionID })
	x.ID, x.Created = w.nextID, time.Now().UnixMilli()
	w.nextID++
	w.list = append(w.list, x)
	w.saveLocked()
	return x
}

func (w *Watches) All() []Watch {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.list)
}

func (w *Watches) Remove(id int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.list = slices.DeleteFunc(w.list, func(o Watch) bool { return o.ID == id })
	w.saveLocked()
}

func (w *Watches) SetLastReview(id int, review int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := range w.list {
		if w.list[i].ID == id {
			w.list[i].LastReview = review
		}
	}
	w.saveLocked()
}

func (w *Watches) SetFailed(id int, rev string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := range w.list {
		if w.list[i].ID == id {
			w.list[i].FailedRev = rev
		}
	}
	w.saveLocked()
}

type prInfo struct {
	State          string `json:"state"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Title          string `json:"title"`
	HTMLURL        string `json:"html_url"`
}

// handleWatchPR: POST /watch-pr from a session in the VM.
func (s *Supervisor) handleWatchPR(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Repo       string `json:"repo"`
		PR         int    `json:"pr"`
		SessionID  string `json:"sessionId"`
		SessionKey string `json:"sessionKey"`
		PaneID     string `json:"paneId"`
	}
	if err := readJSON(r, &b); err != nil || b.PR <= 0 || b.SessionID == "" {
		sendJSON(w, 400, map[string]any{"error": "need pr (a number) and sessionId", "outcome": OutBadRequest})
		return
	}
	g := s.cfg.GitHub
	if !validPane.MatchString(b.PaneID) {
		b.PaneID = ""
	}
	s.sessions.Touch(b.SessionID, b.SessionKey, TouchMeta{HerdrPane: b.PaneID, Action: fmt.Sprintf("watch PR #%d", b.PR)})
	if b.Repo == "" && len(g.Repos) > 0 {
		b.Repo = g.Repos[0]
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	// owner/name outside the App's repos: an external watch, if the owner
	// has an ambient token (watch_external.go).
	var full string
	var err error
	if g.configured() {
		full, err = g.fullName(ctx, b.Repo)
	} else {
		err = fmt.Errorf("no GitHub App is configured on this supervisor")
	}
	if err != nil && validRepo.MatchString(b.Repo) {
		x, pr, xerr := s.watchExternal(ctx, b.Repo, Watch{PR: b.PR, SessionID: b.SessionID, PaneID: b.PaneID})
		if xerr != nil {
			sendJSON(w, 400, map[string]any{"error": xerr.Error(), "outcome": OutBadRequest})
			return
		}
		s.audit.Log(AuditEntry{RequestID: s.nextID(), Action: fmt.Sprintf("watch %s#%d", x.Repo, b.PR), Decision: "allow",
			DecisionReason: "the session is told of reviews, the merge or the close", SessionID: b.SessionID, Mode: "normal"})
		sendJSON(w, 200, map[string]any{"id": x.ID, "repo": x.Repo, "pr": b.PR, "title": pr.Title, "state": pr.State, "merged": pr.Merged, "external": true})
		return
	}
	if err != nil {
		sendJSON(w, 400, map[string]any{"error": err.Error(), "outcome": OutBadRequest})
		return
	}
	var pr prInfo
	if err := g.get(ctx, fmt.Sprintf("/repos/%s/pulls/%d", full, b.PR), &pr); err != nil {
		sendJSON(w, 400, map[string]any{"error": err.Error(), "outcome": OutBadRequest})
		return
	}
	x := s.watches.Add(Watch{Repo: full, PR: b.PR, Title: pr.Title, SessionID: b.SessionID, PaneID: b.PaneID})
	s.audit.Log(AuditEntry{RequestID: s.nextID(), Action: fmt.Sprintf("watch %s#%d", full, b.PR), Decision: "allow",
		DecisionReason: "the session is told when it's applied", SessionID: b.SessionID, Mode: "normal"})
	sendJSON(w, 200, map[string]any{"id": x.ID, "repo": full, "pr": b.PR, "title": pr.Title, "state": pr.State, "merged": pr.Merged})
}

// handleApplied: POST /admin/applied from the apply service, as root.
func (s *Supervisor) handleApplied(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Rev     string            `json:"rev"`
		Revs    map[string]string `json:"revs"` // owner/name -> the revision of that repo applied
		Outcome string            `json:"outcome"` // applied | failed
		Detail  string            `json:"detail"`
	}
	if err := readJSON(r, &b); err != nil || !validRev.MatchString(b.Rev) {
		sendJSON(w, 400, map[string]any{"error": "need rev (a commit SHA) and outcome"})
		return
	}
	revs := map[string]string{}
	for repo, rev := range b.Revs {
		if validRev.MatchString(rev) {
			revs[strings.ToLower(repo)] = rev
		}
	}
	go s.checkWatches(b.Rev, revs, b.Outcome == "applied", truncRunes(b.Detail, 300))
	sendJSON(w, 202, map[string]any{"watches": len(s.watches.All())})
}

// checkWatches: revs gives each applied repo's revision; a watch on a repo
// it doesn't name is left for an apply that does. Without revs (an older
// apply), every watch is checked against rev.
func (s *Supervisor) checkWatches(rev string, revs map[string]string, ok bool, detail string) {
	g := s.cfg.GitHub
	if !g.configured() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, x := range s.watches.All() {
		if x.External {
			continue // polled with its owner's token (watch_external.go)
		}
		rev := rev
		if len(revs) > 0 {
			if rev = revs[strings.ToLower(x.Repo)]; rev == "" {
				continue
			}
		}
		short := rev[:min(len(rev), 7)]
		var pr prInfo
		if err := g.get(ctx, fmt.Sprintf("/repos/%s/pulls/%d", x.Repo, x.PR), &pr); err != nil {
			logf("watch %d: %v", x.ID, err)
			continue
		}
		name := fmt.Sprintf("PR #%d (%s)", x.PR, pr.Title)
		if !pr.Merged {
			if pr.State == "closed" {
				s.tellSession(ctx, x, fmt.Sprintf("[eldar-os] %s was closed without merging; nothing from it was applied.", name))
				s.watches.Remove(x.ID)
			}
			continue
		}
		var cmp struct {
			Status string `json:"status"`
		}
		if err := g.get(ctx, fmt.Sprintf("/repos/%s/compare/%s...%s", x.Repo, pr.MergeCommitSHA, rev), &cmp); err != nil {
			logf("watch %d: %v", x.ID, err)
			continue
		}
		if cmp.Status != "identical" && cmp.Status != "ahead" {
			continue // merged, but not in this revision yet
		}
		if ok {
			s.tellSession(ctx, x, fmt.Sprintf("[eldar-os] %s is merged and applied (%s): your change is live in this VM now.", name, short))
			s.watches.Remove(x.ID)
		} else if x.FailedRev != rev {
			s.tellSession(ctx, x, fmt.Sprintf("[eldar-os] %s is merged, but applying %s failed: %s. The machine is still on the previous system. A later apply that succeeds will tell you.", name, short, detail))
			s.watches.SetFailed(x.ID, rev)
		}
	}
}

// tellSession types a notice into the session's herdr pane, as tell-agent
// does, following the session if it moved panes. If it's gone, the notice
// goes to Eldar through the relay instead.
func (s *Supervisor) tellSession(ctx context.Context, x Watch, text string) {
	pane := ""
	for _, p := range ClaudePanes(s.herdr.View(ctx, "", 0)) {
		if p.SessionID == x.SessionID && (pane == "" || p.PaneID == x.PaneID) {
			pane = p.PaneID
		}
	}
	how := "relayed to Eldar (session gone)"
	if pane != "" {
		if _, err := s.herdr.run(ctx, s.herdr.DefaultSession, 30*time.Second, "agent", "prompt", pane, text); err == nil {
			how = "told the session in " + pane
		} else {
			logf("watch %d: herdr agent prompt %s: %v", x.ID, pane, err)
			pane = ""
		}
	}
	if pane == "" {
		s.relay.Add(text+" (The session that opened it is gone; tell Kevin.)", map[string]string{"origin": "supervisor: PR watch", "session_id": x.SessionID})
	}
	s.audit.Log(AuditEntry{RequestID: s.nextID(), Action: fmt.Sprintf("PR watch %s#%d: %s", x.Repo, x.PR, truncRunes(text, 160)),
		Decision: "allow", DecisionReason: how, SessionID: x.SessionID, Mode: "normal"})
}
