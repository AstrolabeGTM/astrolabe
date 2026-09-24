// Package github is a small GitHub REST client for monitors and enrichment.
package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	Token string // optional; raises the rate limit from 60 to 5,000 requests/hour
	Base  string // default https://api.github.com
	HTTP  *http.Client
}

// ErrNotFound is returned for 404s (missing file, deleted user).
var ErrNotFound = fmt.Errorf("github: not found")

func (c *Client) get(ctx context.Context, path string, accept string, out any) error {
	base := c.Base
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", base+path, nil)
	if err != nil {
		return err
	}
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	switch {
	case resp.StatusCode == 404:
		return ErrNotFound
	case resp.StatusCode != 200:
		msg := strings.TrimSpace(string(b))
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			msg = "rate limited until " + resp.Header.Get("X-RateLimit-Reset") + "; set ASTROLABE_GITHUB_TOKEN"
		}
		return fmt.Errorf("github %s: %d %s", path, resp.StatusCode, msg)
	}
	return json.Unmarshal(b, out)
}

type User struct {
	Login   string `json:"login"`
	Name    string `json:"name"`
	Company string `json:"company"`
	Blog    string `json:"blog"`
	Email   string `json:"email"`
	Bio     string `json:"bio"`
	Type    string `json:"type"` // User or Organization
}

type Issue struct {
	Number        int       `json:"number"`
	Title         string    `json:"title"`
	Body          string    `json:"body"`
	HTMLURL       string    `json:"html_url"`
	RepositoryURL string    `json:"repository_url"`
	CreatedAt     time.Time `json:"created_at"`
	User          User      `json:"user"`
	PullRequest   *struct{} `json:"pull_request"`
}

// Repo returns "owner/name" from the issue's repository URL.
func (i Issue) Repo() string {
	parts := strings.Split(i.RepositoryURL, "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1]
}

// SearchIssues runs an issue/PR search. GitHub returns at most 1,000 results
// per query, so callers narrow queries by date.
func (c *Client) SearchIssues(ctx context.Context, q string, page int) ([]Issue, error) {
	var r struct{ Items []Issue }
	v := url.Values{"q": {q}, "sort": {"created"}, "order": {"desc"}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
	err := c.get(ctx, "/search/issues?"+v.Encode(), "", &r)
	return r.Items, err
}

type Star struct {
	StarredAt time.Time `json:"starred_at"`
	User      User      `json:"user"`
}

// Stargazers pages oldest first; page is 1-based.
func (c *Client) Stargazers(ctx context.Context, repo string, page int) ([]Star, error) {
	var r []Star
	err := c.get(ctx, fmt.Sprintf("/repos/%s/stargazers?per_page=100&page=%d", repo, page), "application/vnd.github.star+json", &r)
	return r, err
}

type Repository struct {
	FullName        string    `json:"full_name"`
	HTMLURL         string    `json:"html_url"`
	CreatedAt       time.Time `json:"created_at"`
	Owner           User      `json:"owner"`
	StargazersCount int       `json:"stargazers_count"`
}

func (c *Client) Repo(ctx context.Context, repo string) (*Repository, error) {
	r := &Repository{}
	return r, c.get(ctx, "/repos/"+repo, "", r)
}

// Forks returns the newest forks.
func (c *Client) Forks(ctx context.Context, repo string) ([]Repository, error) {
	var r []Repository
	err := c.get(ctx, "/repos/"+repo+"/forks?sort=newest&per_page=100", "", &r)
	return r, err
}

// Issues returns issues and PRs updated since a time.
func (c *Client) Issues(ctx context.Context, repo string, since time.Time) ([]Issue, error) {
	var r []Issue
	v := url.Values{"since": {since.UTC().Format(time.RFC3339)}, "state": {"all"}, "per_page": {"100"}, "sort": {"created"}, "direction": {"desc"}}
	err := c.get(ctx, "/repos/"+repo+"/issues?"+v.Encode(), "", &r)
	return r, err
}

func (c *Client) User(ctx context.Context, login string) (*User, error) {
	u := &User{}
	return u, c.get(ctx, "/users/"+url.PathEscape(login), "", u)
}

// Languages returns bytes of code per language.
func (c *Client) Languages(ctx context.Context, repo string) (map[string]int, error) {
	r := map[string]int{}
	return r, c.get(ctx, "/repos/"+repo+"/languages", "", &r)
}

// File returns a file's contents from the default branch.
func (c *Client) File(ctx context.Context, repo, path string) ([]byte, error) {
	var r struct{ Content, Encoding string }
	if err := c.get(ctx, "/repos/"+repo+"/contents/"+path, "", &r); err != nil {
		return nil, err
	}
	if r.Encoding != "base64" {
		return nil, fmt.Errorf("github: unexpected encoding %q", r.Encoding)
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(r.Content, "\n", ""))
}
