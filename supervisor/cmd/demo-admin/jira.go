package main

import (
	"net/http"
	"time"
)

// The Jira tab's made-up project: tickets for Kevin and a sprint board.
func demoJira() map[string]any {
	issue := func(key, summary, status, typ, who string, mine, plan bool) map[string]any {
		return map[string]any{"key": key, "summary": summary, "status": status, "type": typ,
			"priority": "Medium", "assignee": who, "mine": mine, "hasPlan": plan}
	}
	a := issue("DEMO-142", "Export button drops the last row", "In Progress", "Bug", "Kevin", true, true)
	b := issue("DEMO-151", "Retry failed webhook deliveries", "DEV READY", "Story", "Kevin", true, false)
	c := issue("DEMO-138", "Cache the venue list for an hour", "REVIEW", "Task", "Kevin", true, true)
	d := issue("DEMO-155", "Dark mode for the settings page", "DEV READY", "Story", "", false, false)
	e := issue("DEMO-149", "Typo in the welcome email", "DEV / QA", "Bug", "Sam", false, false)
	col := func(name string, is ...map[string]any) map[string]any {
		if is == nil {
			is = []map[string]any{}
		}
		return map[string]any{"name": name, "issues": is}
	}
	return map[string]any{
		"url": "https://example.atlassian.net", "project": "DEMO", "at": time.Now().UnixMilli(),
		"mine": []map[string]any{b, a, c},
		"sprint": map[string]any{"name": "DEMO Sprint 12", "endDate": time.Now().Add(5 * 24 * time.Hour).UTC().Format(time.RFC3339),
			"columns": []any{col("DEV READY", b, d), col("In Progress", a), col("REVIEW", c), col("DEV / QA", e), col("Done")}},
	}
}

func jiraRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/jira", func(w http.ResponseWriter, r *http.Request) { send(w, demoJira()) })
	mux.HandleFunc("GET /admin/jira/{key}/transitions", func(w http.ResponseWriter, r *http.Request) {
		send(w, map[string]any{"key": r.PathValue("key"), "transitions": []any{
			map[string]any{"id": "21", "name": "Start work", "to": "In Progress"},
			map[string]any{"id": "31", "name": "Send to review", "to": "REVIEW"}}})
	})
	mux.HandleFunc("POST /admin/jira/{key}/{action}", func(w http.ResponseWriter, r *http.Request) { send(w, map[string]any{"ok": true}) })
}
