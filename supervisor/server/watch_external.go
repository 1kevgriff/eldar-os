package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// External PR watches: PRs on repos outside this system (an owner with an
// ambient GitHub token in ambient.yaml, decision 0015), such as a work repo.
// No apply tells the supervisor about those, so it polls them with the
// owner's token and tells the session when a review comes in and when the PR
// is merged or closed. Merged is not deployed: the notice says so.

const externalWatchEvery = 2 * time.Minute

// ownerToken: the owner's ambient GitHub token, or "" if it has none.
func (s *Supervisor) ownerToken(ctx context.Context, owner string) string {
	res := s.AmbientGit(ctx, owner)
	if res.status != http.StatusOK {
		return ""
	}
	m, _ := res.body.(map[string]any)
	tok, _ := m["token"].(string)
	return tok
}

// ghGet calls GitHub's API with tok and decodes the JSON.
func (s *Supervisor) ghGet(ctx context.Context, tok, path string, out any) error {
	base := s.cfg.GitHub.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub %s: %d %s", path, res.StatusCode, truncRunes(string(raw), 200))
	}
	return json.Unmarshal(raw, out)
}

type prReview struct {
	ID    int64  `json:"id"`
	State string `json:"state"` // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED | PENDING
	Body  string `json:"body"`
	User  struct {
		Login string `json:"login"`
	} `json:"user"`
}

// lastReviewID: the newest submitted review's ID, so a watch reports only
// reviews that come after it.
func lastReviewID(rs []prReview) int64 {
	var id int64
	for _, r := range rs {
		if r.State != "PENDING" {
			id = max(id, r.ID)
		}
	}
	return id
}

// watchExternal registers a watch on owner/name with the owner's token.
func (s *Supervisor) watchExternal(ctx context.Context, full string, x Watch) (Watch, prInfo, error) {
	owner, _, _ := strings.Cut(full, "/")
	tok := s.ownerToken(ctx, owner)
	if tok == "" {
		return x, prInfo{}, fmt.Errorf("no ambient GitHub token for %s, and the App isn't installed there", owner)
	}
	var pr prInfo
	if err := s.ghGet(ctx, tok, fmt.Sprintf("/repos/%s/pulls/%d", full, x.PR), &pr); err != nil {
		return x, pr, err
	}
	var rs []prReview
	if err := s.ghGet(ctx, tok, fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=100", full, x.PR), &rs); err != nil {
		return x, pr, err
	}
	x.Repo, x.Title, x.External, x.LastReview = full, pr.Title, true, lastReviewID(rs)
	return s.watches.Add(x), pr, nil
}

// pollExternalWatches runs until ctx ends.
func (s *Supervisor) pollExternalWatches(ctx context.Context) {
	t := time.NewTicker(externalWatchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkExternalWatches(ctx)
		}
	}
}

func (s *Supervisor) checkExternalWatches(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	toks := map[string]string{}
	for _, x := range s.watches.All() {
		if !x.External {
			continue
		}
		owner, _, _ := strings.Cut(x.Repo, "/")
		tok, ok := toks[owner]
		if !ok {
			tok = s.ownerToken(ctx, owner)
			toks[owner] = tok
		}
		if tok == "" {
			continue // lockdown, or the token is gone: try again later
		}
		var pr prInfo
		if err := s.ghGet(ctx, tok, fmt.Sprintf("/repos/%s/pulls/%d", x.Repo, x.PR), &pr); err != nil {
			logf("watch %d: %v", x.ID, err)
			continue
		}
		name := fmt.Sprintf("%s#%d (%s)", x.Repo, x.PR, pr.Title)
		var rs []prReview
		if err := s.ghGet(ctx, tok, fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=100", x.Repo, x.PR), &rs); err == nil {
			var lines []string
			for _, r := range rs {
				if r.ID > x.LastReview && r.State != "PENDING" {
					line := fmt.Sprintf("%s: %s", r.User.Login, strings.ReplaceAll(strings.ToLower(r.State), "_", " "))
					if b := strings.TrimSpace(r.Body); b != "" {
						line += ` "` + truncRunes(strings.Join(strings.Fields(b), " "), 200) + `"`
					}
					lines = append(lines, line)
				}
			}
			if len(lines) > 0 {
				s.tellSession(ctx, x, fmt.Sprintf("[watch] New review on %s: %s. Read the review comments before acting.", name, strings.Join(lines, "; ")))
				s.watches.SetLastReview(x.ID, lastReviewID(rs))
			}
		}
		switch {
		case pr.Merged:
			s.tellSession(ctx, x, fmt.Sprintf("[watch] %s is merged (%s). Merged is not deployed: check the deploy workflow run before calling it live.", name, pr.MergeCommitSHA[:min(len(pr.MergeCommitSHA), 7)]))
			s.watches.Remove(x.ID)
		case pr.State == "closed":
			s.tellSession(ctx, x, fmt.Sprintf("[watch] %s was closed without merging.", name))
			s.watches.Remove(x.ID)
		}
	}
}
