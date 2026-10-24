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
	ForksCount      int       `json:"forks_count"`
	// SubscribersCount is the watcher count; OpenIssuesCount includes open pull requests.
	SubscribersCount int `json:"subscribers_count"`
	OpenIssuesCount  int `json:"open_issues_count"`
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

type ReleaseAsset struct {
	Name          string `json:"name"`
	DownloadCount int64  `json:"download_count"`
}

type Release struct {
	TagName string         `json:"tag_name"`
	Draft   bool           `json:"draft"`
	Assets  []ReleaseAsset `json:"assets"`
}

// Releases returns published releases with their assets, newest first, up to
// 500 releases.
func (c *Client) Releases(ctx context.Context, repo string) ([]Release, error) {
	var all []Release
	for page := 1; page <= 5; page++ {
		var r []Release
		if err := c.get(ctx, fmt.Sprintf("/repos/%s/releases?per_page=100&page=%d", repo, page), "", &r); err != nil {
			return all, err
		}
		for _, x := range r {
			if !x.Draft {
				all = append(all, x)
			}
		}
		if len(r) < 100 {
			break
		}
	}
	return all, nil
}

// ErrNoToken is returned by GraphQL calls made without a token.
var ErrNoToken = fmt.Errorf("no GitHub token configured")

// graphql posts a GraphQL query; GitHub's GraphQL API always needs a token.
func (c *Client) graphql(ctx context.Context, query string, vars map[string]any, data any) error {
	if c.Token == "" {
		return ErrNoToken
	}
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	base := c.Base
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/graphql", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
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
	if resp.StatusCode != 200 {
		return fmt.Errorf("github graphql: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var r struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	if len(r.Errors) > 0 {
		return fmt.Errorf("github graphql: %s", r.Errors[0].Message)
	}
	return json.Unmarshal(r.Data, data)
}

// DiscussionCount returns the repository's discussion count through GraphQL,
// which needs a token.
func (c *Client) DiscussionCount(ctx context.Context, repo string) (int64, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return 0, fmt.Errorf("repo %q must be owner/name", repo)
	}
	var r struct {
		Repository *struct {
			Discussions struct {
				TotalCount int64 `json:"totalCount"`
			} `json:"discussions"`
		} `json:"repository"`
	}
	if err := c.graphql(ctx, "query($owner:String!,$name:String!){repository(owner:$owner,name:$name){discussions{totalCount}}}",
		map[string]any{"owner": owner, "name": name}, &r); err != nil {
		return 0, err
	}
	if r.Repository == nil {
		return 0, ErrNotFound
	}
	return r.Repository.Discussions.TotalCount, nil
}

// Discussion is a GitHub Discussions thread. Author is empty for a deleted account.
type Discussion struct {
	Number    int
	Title     string
	URL       string
	CreatedAt time.Time
	Author    string
	Bot       bool
	Category  string
}

// Discussions returns discussions created at or after since, newest first,
// through GraphQL (which needs a token), up to 500.
func (c *Client) Discussions(ctx context.Context, repo string, since time.Time) ([]Discussion, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return nil, fmt.Errorf("repo %q must be owner/name", repo)
	}
	const q = `query($owner:String!,$name:String!,$after:String){repository(owner:$owner,name:$name){discussions(first:100,after:$after,orderBy:{field:CREATED_AT,direction:DESC}){pageInfo{hasNextPage endCursor} nodes{number title url createdAt author{login __typename} category{name}}}}}`
	var out []Discussion
	var after *string
	for page := 0; page < 5; page++ {
		var r struct {
			Repository *struct {
				Discussions struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						Number    int       `json:"number"`
						Title     string    `json:"title"`
						URL       string    `json:"url"`
						CreatedAt time.Time `json:"createdAt"`
						Author    *struct {
							Login    string `json:"login"`
							Typename string `json:"__typename"`
						} `json:"author"`
						Category struct {
							Name string `json:"name"`
						} `json:"category"`
					} `json:"nodes"`
				} `json:"discussions"`
			} `json:"repository"`
		}
		if err := c.graphql(ctx, q, map[string]any{"owner": owner, "name": name, "after": after}, &r); err != nil {
			return out, err
		}
		if r.Repository == nil {
			return out, ErrNotFound
		}
		d := r.Repository.Discussions
		for _, n := range d.Nodes {
			if n.CreatedAt.Before(since) {
				return out, nil
			}
			x := Discussion{Number: n.Number, Title: n.Title, URL: n.URL, CreatedAt: n.CreatedAt, Category: n.Category.Name}
			if n.Author != nil {
				x.Author, x.Bot = n.Author.Login, n.Author.Typename == "Bot"
			}
			out = append(out, x)
		}
		if !d.PageInfo.HasNextPage {
			break
		}
		cur := d.PageInfo.EndCursor
		after = &cur
	}
	return out, nil
}
