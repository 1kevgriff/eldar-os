package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The Jira tab's data, and Kevin's actions from it: credentials from the
// ambient profile, writes audited, Start relayed to Eldar, lockdown refused.
func TestJira(t *testing.T) {
	s, runner := newTest(t) // profile work: OTHER="quoted", GITHUB_TOKEN=gh
	runner.result = ExecResult{Stdout: "SOS-2.md\nnotes.txt\nBAD.md\n"}
	var mu sync.Mutex
	var writes []string
	issue := func(key, status, statusID, assignee string) map[string]any {
		f := map[string]any{"summary": "Fix " + key, "updated": "2026-11-27T10:00:00.000-0500",
			"status":    map[string]any{"id": statusID, "name": status, "statusCategory": map[string]any{"key": "indeterminate"}},
			"issuetype": map[string]any{"name": "Bug"}}
		if assignee != "" {
			f["assignee"] = map[string]any{"accountId": assignee, "displayName": "Person " + assignee}
		}
		return map[string]any{"key": key, "fields": f}
	}
	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "quoted" || p != "gh" {
			http.Error(w, `{"errorMessages":["bad auth"]}`, 401)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/rest/api/3/myself":
			json.NewEncoder(w).Encode(map[string]any{"accountId": "me1"})
		case r.URL.Path == "/rest/api/3/search/jql" && strings.Contains(r.URL.Query().Get("jql"), "currentUser()"):
			json.NewEncoder(w).Encode(map[string]any{"issues": []any{issue("SOS-1", "In Progress", "3", "me1")}})
		case r.URL.Path == "/rest/api/3/search/jql" && strings.HasPrefix(r.URL.Query().Get("jql"), "sprint = 7"):
			json.NewEncoder(w).Encode(map[string]any{"issues": []any{issue("SOS-1", "In Progress", "3", "me1"), issue("SOS-2", "To Do", "1", "")}})
		case r.URL.Path == "/rest/agile/1.0/board/14/sprint":
			json.NewEncoder(w).Encode(map[string]any{"values": []any{map[string]any{"id": 7, "name": "Sprint 65", "endDate": "2026-12-02T08:00:00.000Z"}}})
		case r.URL.Path == "/rest/agile/1.0/board/14/configuration":
			json.NewEncoder(w).Encode(map[string]any{"columnConfig": map[string]any{"columns": []any{
				map[string]any{"name": "DEV READY", "statuses": []any{map[string]any{"id": "1"}}},
				map[string]any{"name": "In Progress", "statuses": []any{map[string]any{"id": "3"}}},
				map[string]any{"name": "Done", "statuses": []any{map[string]any{"id": "9"}}}}}})
		case r.URL.Path == "/rest/api/3/issue/SOS-2/transitions" && r.Method == "GET":
			json.NewEncoder(w).Encode(map[string]any{"transitions": []any{map[string]any{"id": "21", "name": "Start", "to": map[string]any{"name": "In Progress"}}}})
		case r.URL.Path == "/rest/api/3/issue/SOS-2" && r.Method == "GET":
			json.NewEncoder(w).Encode(issue("SOS-2", "To Do", "1", ""))
		case r.Method == "POST" || r.Method == "PUT":
			b, _ := io.ReadAll(r.Body)
			writes = append(writes, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(b)))
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	defer jira.Close()
	dir := t.TempDir()
	s.relay = NewRelay(filepath.Join(dir, "relay.jsonl"), filepath.Join(dir, "relay-state.json"))
	h := s.adminHandler()
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var m map[string]any
		json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}

	if code, _ := call("GET", "/admin/jira", ""); code != 404 {
		t.Fatalf("unconfigured: %d", code)
	}
	s.cfg.Jira = &JiraConfig{URL: jira.URL, Project: "SOS", BoardID: 14, Profile: "work", EmailVar: "OTHER", TokenVar: "GITHUB_TOKEN", PlansDir: "/home/eldar/plans"}

	code, v := call("GET", "/admin/jira", "")
	if code != 200 {
		t.Fatalf("view: %d %v", code, v)
	}
	mine := v["mine"].([]any)
	if len(mine) != 1 || mine[0].(map[string]any)["key"] != "SOS-1" || mine[0].(map[string]any)["mine"] != true {
		t.Errorf("mine: %v", mine)
	}
	cols := v["sprint"].(map[string]any)["columns"].([]any)
	ready := cols[0].(map[string]any)["issues"].([]any)
	if len(cols) != 3 || len(ready) != 1 || ready[0].(map[string]any)["key"] != "SOS-2" || ready[0].(map[string]any)["hasPlan"] != true {
		t.Errorf("sprint columns: %v", cols)
	}
	if got := cols[1].(map[string]any)["issues"].([]any); len(got) != 1 || got[0].(map[string]any)["hasPlan"] != false {
		t.Errorf("in progress: %v", got)
	}

	// Another project's key, or a malformed one, never reaches Jira.
	for _, k := range []string{"ABC-1", "SOS-1%2F..%2Fx", "sos-1"} {
		if code, _ := call("POST", "/admin/jira/"+k+"/assign", `{"to":"me"}`); code != 400 && code != 404 {
			t.Errorf("%s: %d", k, code)
		}
	}
	if code, _ := call("POST", "/admin/jira/SOS-2/transition", `{"id":"21; drop"}`); code != 400 {
		t.Errorf("bad transition id: %d", code)
	}

	if code, m := call("GET", "/admin/jira/SOS-2/transitions", ""); code != 200 || len(m["transitions"].([]any)) != 1 {
		t.Errorf("transitions: %d %v", code, m)
	}
	if code, m := call("POST", "/admin/jira/SOS-2/transition", `{"id":"21","name":"Start"}`); code != 200 {
		t.Errorf("transition: %d %v", code, m)
	}
	if code, m := call("POST", "/admin/jira/SOS-2/assign", `{"to":"me"}`); code != 200 {
		t.Errorf("assign: %d %v", code, m)
	}
	if code, m := call("POST", "/admin/jira/SOS-2/assign", `{"to":"none"}`); code != 200 {
		t.Errorf("unassign: %d %v", code, m)
	}
	mu.Lock()
	want := []string{
		`POST /rest/api/3/issue/SOS-2/transitions {"transition":{"id":"21"}}`,
		`PUT /rest/api/3/issue/SOS-2/assignee {"accountId":"me1"}`,
		`PUT /rest/api/3/issue/SOS-2/assignee {"accountId":null}`,
	}
	if strings.Join(writes, "\n") != strings.Join(want, "\n") {
		t.Errorf("writes:\n%s", strings.Join(writes, "\n"))
	}
	mu.Unlock()

	if code, m := call("POST", "/admin/jira/SOS-2/start", `{}`); code != 200 {
		t.Fatalf("start: %d %v", code, m)
	}
	if len(s.relay.events) != 1 || !strings.Contains(s.relay.events[0].Text, "start on SOS-2 (Fix SOS-2, To Do). Plan first: write the plan to /home/eldar/plans/SOS-2.md") {
		t.Errorf("relay: %+v", s.relay.events)
	}

	s.modes.Lockdown()
	if code, _ := call("GET", "/admin/jira?refresh=1", ""); code != 403 {
		t.Errorf("lockdown: %d", code)
	}
}
