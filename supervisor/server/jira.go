package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Jira: one project's tickets for the admin UIs (the web UI's Jira tab).
// The supervisor reads Jira with credentials from an ambient profile (the same
// ones the project's sessions get), so there is nothing new to store. Kevin's
// actions from the tab are writes to Jira made as him: moving a ticket,
// assigning it to himself or no one, and asking Eldar to start on it (a relay
// event; Eldar plans first). Every write is audited. Lockdown refuses all of
// it, because it uses credentials.

// JiraConfig is the file SUPERVISOR_JIRA names (JSON).
type JiraConfig struct {
	URL      string `json:"url"`      // https://example.atlassian.net
	Project  string `json:"project"`  // project key, e.g. SOS
	BoardID  int    `json:"boardId"`  // scrum board whose active sprint the tab shows; 0 = none
	Profile  string `json:"profile"`  // ambient profile holding the credentials
	EmailVar string `json:"emailVar"` // its variable with the account's email
	TokenVar string `json:"tokenVar"` // its variable with the API token
	// PlansDir is where sessions in the VM keep plan files named <KEY>.md;
	// the tab marks tickets that have one. "" = don't look.
	PlansDir string `json:"plansDir"`
}

const jiraCacheFor = time.Minute

var (
	validJiraKey = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}-[0-9]{1,7}$`)
	validJiraID  = regexp.MustCompile(`^[0-9]{1,10}$`)
)

type jiraIssue struct {
	Key       string `json:"key"`
	Summary   string `json:"summary"`
	Status    string `json:"status"`
	StatusID  string `json:"statusId"`
	Category  string `json:"category"` // new | indeterminate | done
	Type      string `json:"type"`
	Priority  string `json:"priority"`
	Assignee  string `json:"assignee"`
	Mine      bool   `json:"mine"`
	Updated   string `json:"updated"`
	HasPlan   bool   `json:"hasPlan"`
	AccountID string `json:"-"`
}

type jiraColumn struct {
	Name     string      `json:"name"`
	Statuses []string    `json:"-"`
	Issues   []jiraIssue `json:"issues"`
}

type jiraSprint struct {
	Name    string       `json:"name"`
	EndDate string       `json:"endDate"`
	Columns []jiraColumn `json:"columns"`
}

type jiraView struct {
	URL     string      `json:"url"`
	Project string      `json:"project"`
	Mine    []jiraIssue `json:"mine"`
	Sprint  *jiraSprint `json:"sprint"`
	At      int64       `json:"at"`
}

type jiraState struct {
	mu      sync.Mutex
	view    *jiraView
	at      time.Time
	me      string // the account ID behind the credentials
	columns []jiraColumn
}

// jiraCreds reads the credentials from the ambient profile.
func (s *Supervisor) jiraCreds(ctx context.Context) (email, token string, err error) {
	c := s.cfg.Jira
	p, ok := s.profiles.Get(c.Profile)
	if !ok {
		return "", "", fmt.Errorf("unknown profile %q for Jira", c.Profile)
	}
	all, err := s.profiles.Read(ctx, p)
	if err != nil {
		return "", "", err
	}
	email, token = all[c.EmailVar], all[c.TokenVar]
	if email == "" || token == "" {
		return "", "", fmt.Errorf("profile %q has no %s or %s", c.Profile, c.EmailVar, c.TokenVar)
	}
	return email, token, nil
}

// jiraDo calls Jira's REST API and decodes the JSON into out (if not nil).
func (s *Supervisor) jiraDo(ctx context.Context, method, path string, body, out any) error {
	email, token, err := s.jiraCreds(ctx)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.cfg.Jira.URL, "/")+path, rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth(email, token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode >= 300 {
		var e struct {
			ErrorMessages []string          `json:"errorMessages"`
			Errors        map[string]string `json:"errors"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := strings.Join(e.ErrorMessages, "; ")
		for k, v := range e.Errors {
			msg += fmt.Sprintf(" %s: %s", k, v)
		}
		if strings.TrimSpace(msg) == "" {
			msg = truncRunes(string(raw), 200)
		}
		return fmt.Errorf("Jira %d: %s", res.StatusCode, strings.TrimSpace(msg))
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

type jiraRawIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary string `json:"summary"`
		Updated string `json:"updated"`
		Status  struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Category struct {
				Key string `json:"key"`
			} `json:"statusCategory"`
		} `json:"status"`
		IssueType struct {
			Name string `json:"name"`
		} `json:"issuetype"`
		Priority *struct {
			Name string `json:"name"`
		} `json:"priority"`
		Assignee *struct {
			AccountID   string `json:"accountId"`
			DisplayName string `json:"displayName"`
		} `json:"assignee"`
	} `json:"fields"`
}

const jiraFields = "summary,status,issuetype,priority,assignee,updated"

func (r jiraRawIssue) issue(me string) jiraIssue {
	f := r.Fields
	i := jiraIssue{Key: r.Key, Summary: f.Summary, Status: f.Status.Name, StatusID: f.Status.ID,
		Category: f.Status.Category.Key, Type: f.IssueType.Name, Updated: f.Updated}
	if f.Priority != nil {
		i.Priority = f.Priority.Name
	}
	if f.Assignee != nil {
		i.Assignee, i.AccountID = f.Assignee.DisplayName, f.Assignee.AccountID
		i.Mine = me != "" && f.Assignee.AccountID == me
	}
	return i
}

func (s *Supervisor) jiraMe(ctx context.Context) (string, error) {
	s.jira.mu.Lock()
	me := s.jira.me
	s.jira.mu.Unlock()
	if me != "" {
		return me, nil
	}
	var m struct {
		AccountID string `json:"accountId"`
	}
	if err := s.jiraDo(ctx, "GET", "/rest/api/3/myself", nil, &m); err != nil {
		return "", err
	}
	s.jira.mu.Lock()
	s.jira.me = m.AccountID
	s.jira.mu.Unlock()
	return m.AccountID, nil
}

func (s *Supervisor) jiraSearch(ctx context.Context, jql, me string) ([]jiraIssue, error) {
	q := url.Values{"jql": {jql}, "fields": {jiraFields}, "maxResults": {"100"}}
	var out struct {
		Issues []jiraRawIssue `json:"issues"`
	}
	if err := s.jiraDo(ctx, "GET", "/rest/api/3/search/jql?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	issues := make([]jiraIssue, 0, len(out.Issues))
	for _, r := range out.Issues {
		issues = append(issues, r.issue(me))
	}
	return issues, nil
}

// jiraBoardColumns: the board's columns and the statuses in each (cached; a
// board's columns rarely change).
func (s *Supervisor) jiraBoardColumns(ctx context.Context) ([]jiraColumn, error) {
	s.jira.mu.Lock()
	cols := s.jira.columns
	s.jira.mu.Unlock()
	if cols != nil {
		return cols, nil
	}
	var cfg struct {
		ColumnConfig struct {
			Columns []struct {
				Name     string `json:"name"`
				Statuses []struct {
					ID string `json:"id"`
				} `json:"statuses"`
			} `json:"columns"`
		} `json:"columnConfig"`
	}
	if err := s.jiraDo(ctx, "GET", fmt.Sprintf("/rest/agile/1.0/board/%d/configuration", s.cfg.Jira.BoardID), nil, &cfg); err != nil {
		return nil, err
	}
	for _, c := range cfg.ColumnConfig.Columns {
		col := jiraColumn{Name: c.Name}
		for _, st := range c.Statuses {
			col.Statuses = append(col.Statuses, st.ID)
		}
		cols = append(cols, col)
	}
	s.jira.mu.Lock()
	s.jira.columns = cols
	s.jira.mu.Unlock()
	return cols, nil
}

// jiraActiveSprint: the board's active sprint, its issues sorted into the
// board's columns. nil when the board has none.
func (s *Supervisor) jiraActiveSprint(ctx context.Context, me string) (*jiraSprint, error) {
	var sprints struct {
		Values []struct {
			ID      int    `json:"id"`
			Name    string `json:"name"`
			EndDate string `json:"endDate"`
		} `json:"values"`
	}
	if err := s.jiraDo(ctx, "GET", fmt.Sprintf("/rest/agile/1.0/board/%d/sprint?state=active", s.cfg.Jira.BoardID), nil, &sprints); err != nil {
		return nil, err
	}
	if len(sprints.Values) == 0 {
		return nil, nil
	}
	sp := sprints.Values[0]
	cols, err := s.jiraBoardColumns(ctx)
	if err != nil {
		return nil, err
	}
	issues, err := s.jiraSearch(ctx, fmt.Sprintf("sprint = %d ORDER BY rank", sp.ID), me)
	if err != nil {
		return nil, err
	}
	out := &jiraSprint{Name: sp.Name, EndDate: sp.EndDate, Columns: make([]jiraColumn, len(cols))}
	where := map[string]int{}
	for i, c := range cols {
		out.Columns[i] = jiraColumn{Name: c.Name, Issues: []jiraIssue{}}
		for _, st := range c.Statuses {
			where[st] = i
		}
	}
	for _, is := range issues {
		if i, ok := where[is.StatusID]; ok {
			out.Columns[i].Issues = append(out.Columns[i].Issues, is)
		}
	}
	return out, nil
}

// jiraPlans: ticket keys with a plan file in PlansDir (in the VM). Best
// effort: no directory, or no VM, is no plans.
func (s *Supervisor) jiraPlans(ctx context.Context) map[string]bool {
	plans := map[string]bool{}
	dir := s.cfg.Jira.PlansDir
	if dir == "" || s.runner == nil {
		return plans
	}
	res, err := s.runner.Exec(ctx, ExecSpec{Argv: []string{"ls", "-1", "--", dir}, User: s.cfg.AgentUser, Timeout: 10 * time.Second, MaxOutput: 1 << 20})
	if err != nil || res.ExitCode != 0 {
		return plans
	}
	for _, f := range strings.Split(res.Stdout, "\n") {
		if k, ok := strings.CutSuffix(strings.TrimSpace(f), ".md"); ok && validJiraKey.MatchString(k) {
			plans[k] = true
		}
	}
	return plans
}

// JiraView: the tab's data, cached for a minute unless refresh.
func (s *Supervisor) JiraView(ctx context.Context, refresh bool) (*jiraView, error) {
	s.jira.mu.Lock()
	if v := s.jira.view; v != nil && !refresh && time.Since(s.jira.at) < jiraCacheFor {
		s.jira.mu.Unlock()
		return v, nil
	}
	s.jira.mu.Unlock()
	c := s.cfg.Jira
	me, err := s.jiraMe(ctx)
	if err != nil {
		return nil, err
	}
	mine, err := s.jiraSearch(ctx, fmt.Sprintf("project = %s AND assignee = currentUser() AND statusCategory != Done ORDER BY status, updated DESC", c.Project), me)
	if err != nil {
		return nil, err
	}
	var sprint *jiraSprint
	if c.BoardID > 0 {
		if sprint, err = s.jiraActiveSprint(ctx, me); err != nil {
			return nil, err
		}
	}
	plans := s.jiraPlans(ctx)
	mark := func(is []jiraIssue) {
		for i := range is {
			is[i].HasPlan = plans[is[i].Key]
		}
	}
	mark(mine)
	sort.SliceStable(mine, func(a, b int) bool { return mine[a].Status < mine[b].Status })
	if sprint != nil {
		for i := range sprint.Columns {
			mark(sprint.Columns[i].Issues)
		}
	}
	v := &jiraView{URL: strings.TrimRight(c.URL, "/"), Project: c.Project, Mine: mine, Sprint: sprint, At: time.Now().UnixMilli()}
	s.jira.mu.Lock()
	s.jira.view, s.jira.at = v, time.Now()
	s.jira.mu.Unlock()
	return v, nil
}

func (s *Supervisor) jiraInvalidate() {
	s.jira.mu.Lock()
	s.jira.view = nil
	s.jira.mu.Unlock()
}

// jiraKey validates {key} for this project.
func (s *Supervisor) jiraKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	k := r.PathValue("key")
	if !validJiraKey.MatchString(k) || !strings.HasPrefix(k, s.cfg.Jira.Project+"-") {
		sendJSON(w, 400, map[string]any{"error": fmt.Sprintf("%q isn't a %s ticket.", k, s.cfg.Jira.Project)})
		return "", false
	}
	return k, true
}

func (s *Supervisor) jiraIssue(ctx context.Context, key string) (jiraIssue, error) {
	var r jiraRawIssue
	if err := s.jiraDo(ctx, "GET", "/rest/api/3/issue/"+key+"?fields="+jiraFields, nil, &r); err != nil {
		return jiraIssue{}, err
	}
	me, _ := s.jiraMe(ctx)
	return r.issue(me), nil
}

func (s *Supervisor) jiraRoutes(mux *http.ServeMux) {
	// guard: configured, and not in lockdown (the calls use credentials).
	guard := func(h func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if s.cfg.Jira == nil {
				sendJSON(w, 404, map[string]any{"error": "Jira isn't configured."})
				return
			}
			if res, locked := s.ambientLocked(); locked {
				sendJSON(w, res.status, res.body)
				return
			}
			h(w, r)
		}
	}
	fail := func(w http.ResponseWriter, err error) { sendJSON(w, 502, map[string]any{"error": err.Error()}) }

	mux.HandleFunc("GET /admin/jira", guard(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.JiraView(r.Context(), r.URL.Query().Get("refresh") == "1")
		if err != nil {
			fail(w, err)
			return
		}
		sendJSON(w, 200, v)
	}))
	mux.HandleFunc("GET /admin/jira/{key}/transitions", guard(func(w http.ResponseWriter, r *http.Request) {
		key, ok := s.jiraKey(w, r)
		if !ok {
			return
		}
		var out struct {
			Transitions []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				To   struct {
					Name string `json:"name"`
				} `json:"to"`
			} `json:"transitions"`
		}
		if err := s.jiraDo(r.Context(), "GET", "/rest/api/3/issue/"+key+"/transitions", nil, &out); err != nil {
			fail(w, err)
			return
		}
		list := []map[string]string{}
		for _, t := range out.Transitions {
			list = append(list, map[string]string{"id": t.ID, "name": t.Name, "to": t.To.Name})
		}
		sendJSON(w, 200, map[string]any{"key": key, "transitions": list})
	}))
	mux.HandleFunc("POST /admin/jira/{key}/transition", guard(func(w http.ResponseWriter, r *http.Request) {
		key, ok := s.jiraKey(w, r)
		if !ok {
			return
		}
		var b struct {
			ID   string `json:"id"`
			Name string `json:"name"` // for the audit line only
		}
		if readJSON(r, &b); !validJiraID.MatchString(b.ID) {
			sendJSON(w, 400, map[string]any{"error": "'id' must be a transition ID."})
			return
		}
		if err := s.jiraDo(r.Context(), "POST", "/rest/api/3/issue/"+key+"/transitions", map[string]any{"transition": map[string]string{"id": b.ID}}, nil); err != nil {
			fail(w, err)
			return
		}
		s.jiraInvalidate()
		s.adminLog(r, "JIRA MOVE "+key, fmt.Sprintf("%s moved: %s (transition %s)", key, truncRunes(b.Name, 60), b.ID))
		sendJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /admin/jira/{key}/assign", guard(func(w http.ResponseWriter, r *http.Request) {
		key, ok := s.jiraKey(w, r)
		if !ok {
			return
		}
		var b struct {
			To string `json:"to"` // me | none
		}
		readJSON(r, &b)
		var account any // null unassigns
		switch b.To {
		case "me":
			me, err := s.jiraMe(r.Context())
			if err != nil {
				fail(w, err)
				return
			}
			account = me
		case "none":
		default:
			sendJSON(w, 400, map[string]any{"error": "'to' must be me or none."})
			return
		}
		if err := s.jiraDo(r.Context(), "PUT", "/rest/api/3/issue/"+key+"/assignee", map[string]any{"accountId": account}, nil); err != nil {
			fail(w, err)
			return
		}
		s.jiraInvalidate()
		who := map[string]string{"me": "Kevin", "none": "no one"}[b.To]
		s.adminLog(r, "JIRA ASSIGN "+key, key+" assigned to "+who)
		sendJSON(w, 200, map[string]any{"ok": true})
	}))
	// Start: ask Eldar to take the ticket on. Eldar plans first (its
	// workspace rules); nothing changes in Jira.
	mux.HandleFunc("POST /admin/jira/{key}/start", guard(func(w http.ResponseWriter, r *http.Request) {
		key, ok := s.jiraKey(w, r)
		if !ok {
			return
		}
		is, err := s.jiraIssue(r.Context(), key)
		if err != nil {
			fail(w, err)
			return
		}
		text := fmt.Sprintf("Kevin, from the Jira tab: start on %s (%s, %s). Plan first", key, truncRunes(is.Summary, 160), is.Status)
		if d := s.cfg.Jira.PlansDir; d != "" {
			text += fmt.Sprintf(": write the plan to %s/%s.md", strings.TrimRight(d, "/"), key)
		}
		text += ", then stop for his go."
		e, err := s.relay.Add(text, map[string]string{"origin": "Kevin (Jira tab)"})
		if err != nil {
			sendJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		s.adminLog(r, "JIRA START "+key, "Asked Eldar to start on "+key+" (relay "+strconv.Itoa(e.ID)+")")
		sendJSON(w, 200, map[string]any{"ok": true, "relay": e.ID})
	}))
}
