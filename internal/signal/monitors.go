package signal

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/github"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
)

// Sources holds the external clients monitors use; tests point them at fakes.
type Sources struct {
	GitHub     *github.Client
	HTTP       *http.Client
	HNBase     string // default https://hn.algolia.com/api/v1
	StackBase  string // default https://api.stackexchange.com/2.3
	StackKey   string // optional Stack Exchange key for a higher quota
	MaxResults int    // per source per run; default 300
}

func (s *Sources) httpClient() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Run executes one monitor for signals since a time. Community sources fail
// independently: one failing source is reported but the others still run.
func (s *Sources) Run(ctx context.Context, productID string, m *product.Monitor, since time.Time) ([]Signal, error) {
	base := Signal{Product: productID, Monitor: m.ID, Type: m.Signal, Strength: m.Strength, WhyNow: m.WhyNow}
	switch m.Type {
	case "github_search":
		return s.githubSearch(ctx, base, m, since)
	case "github_repo":
		return s.githubRepo(ctx, base, m, since)
	case "community":
		var out []Signal
		var errs []error
		sources := m.Sources
		if len(sources) == 0 {
			sources = []string{"hn", "stackoverflow"}
		}
		for _, src := range sources {
			var got []Signal
			var err error
			switch src {
			case "hn":
				got, err = s.hn(ctx, base, m, since)
			case "stackoverflow":
				got, err = s.stackOverflow(ctx, base, m, since)
			case "rss":
				got, err = s.rss(ctx, base, m, since)
			}
			out = append(out, got...)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", src, err))
			}
		}
		return out, errors.Join(errs...)
	}
	return nil, fmt.Errorf("unknown monitor type %q", m.Type)
}

func (s *Sources) max() int {
	if s.MaxResults > 0 {
		return s.MaxResults
	}
	return 300
}

func (s *Sources) githubSearch(ctx context.Context, base Signal, m *product.Monitor, since time.Time) ([]Signal, error) {
	// Narrow by date: search returns at most 1,000 results per query.
	q := fmt.Sprintf("%s created:>=%s", m.Query, since.UTC().Format("2006-01-02T15:04:05Z"))
	var out []Signal
	for page := 1; page <= 3 && len(out) < s.max(); page++ {
		items, err := s.GitHub.SearchIssues(ctx, q, page)
		if err != nil {
			return out, err
		}
		for _, it := range items {
			if it.User.Type == "Bot" || strings.HasSuffix(it.User.Login, "[bot]") {
				continue
			}
			sig := base
			repo := it.Repo()
			kind := "issue"
			if it.PullRequest != nil {
				kind = "pr"
			}
			sig.OccurredAt, sig.Title, sig.EvidenceURL, sig.Repo = it.CreatedAt, it.Title, it.HTMLURL, repo
			sig.Subject = Subject{GitHub: it.User.Login, GitHubOrg: ownerOrg(repo, it.User.Login)}
			sig.Data = map[string]any{"repo": repo, "kind": kind, "excerpt": excerpt(it.Body, 400)}
			sig.DedupeKey = fmt.Sprintf("gh-%s:%s#%d", kind, repo, it.Number)
			out = append(out, sig)
		}
		if len(items) < 100 {
			break
		}
	}
	return out, nil
}

// ownerOrg treats a repo owner as a company unless it is the person's own account.
func ownerOrg(repo, login string) string {
	owner, _, _ := strings.Cut(repo, "/")
	if strings.EqualFold(owner, login) {
		return ""
	}
	return owner
}

func (s *Sources) githubRepo(ctx context.Context, base Signal, m *product.Monitor, since time.Time) ([]Signal, error) {
	var out []Signal
	for _, ev := range m.Events {
		sig := base
		sig.Type = m.Signal + "." + ev
		sig.Repo = m.Repo
		switch ev {
		case "star":
			r, err := s.GitHub.Repo(ctx, m.Repo)
			if err != nil {
				return out, err
			}
			// Stargazers are listed oldest first: walk back from the last page.
			for page := (r.StargazersCount + 99) / 100; page >= 1; page-- {
				stars, err := s.GitHub.Stargazers(ctx, m.Repo, page)
				if err != nil {
					return out, err
				}
				older := false
				for i := len(stars) - 1; i >= 0; i-- {
					st := stars[i]
					if st.StarredAt.Before(since) {
						older = true
						break
					}
					x := sig
					x.OccurredAt, x.Title = st.StarredAt, st.User.Login+" starred "+m.Repo
					x.EvidenceURL = "https://github.com/" + st.User.Login
					x.Subject = Subject{GitHub: st.User.Login}
					x.DedupeKey = "gh-star:" + m.Repo + ":" + strings.ToLower(st.User.Login)
					out = append(out, x)
				}
				if older || len(out) >= s.max() {
					break
				}
			}
		case "fork":
			forks, err := s.GitHub.Forks(ctx, m.Repo)
			if err != nil {
				return out, err
			}
			for _, f := range forks {
				if f.CreatedAt.Before(since) {
					continue
				}
				x := sig
				x.OccurredAt, x.Title, x.EvidenceURL = f.CreatedAt, f.Owner.Login+" forked "+m.Repo, f.HTMLURL
				x.Subject = Subject{GitHub: f.Owner.Login}
				if f.Owner.Type == "Organization" {
					x.Subject = Subject{GitHubOrg: f.Owner.Login}
				}
				x.DedupeKey = "gh-fork:" + strings.ToLower(f.FullName)
				out = append(out, x)
			}
		case "issue":
			owner, _, _ := strings.Cut(m.Repo, "/")
			issues, err := s.GitHub.Issues(ctx, m.Repo, since)
			if err != nil {
				return out, err
			}
			for _, it := range issues {
				if it.CreatedAt.Before(since) || strings.EqualFold(it.User.Login, owner) || it.User.Type == "Bot" {
					continue
				}
				x := sig
				x.OccurredAt, x.Title, x.EvidenceURL = it.CreatedAt, it.Title, it.HTMLURL
				x.Subject = Subject{GitHub: it.User.Login}
				x.Data = map[string]any{"repo": m.Repo, "excerpt": excerpt(it.Body, 400)}
				x.DedupeKey = fmt.Sprintf("gh-issue:%s#%d", m.Repo, it.Number)
				out = append(out, x)
			}
		}
	}
	return out, nil
}

func (s *Sources) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "astrolabe/0.1")
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %d %s", u, resp.StatusCode, excerpt(string(b), 200))
	}
	return json.Unmarshal(b, out)
}

func (s *Sources) hn(ctx context.Context, base Signal, m *product.Monitor, since time.Time) ([]Signal, error) {
	root := s.HNBase
	if root == "" {
		root = "https://hn.algolia.com/api/v1"
	}
	var out []Signal
	for _, kw := range m.Keywords {
		v := url.Values{"query": {kw}, "tags": {"(story,comment)"}, "hitsPerPage": {"100"},
			"numericFilters": {fmt.Sprintf("created_at_i>%d", since.Unix())}}
		var r struct {
			Hits []struct {
				ObjectID    string `json:"objectID"`
				Author      string `json:"author"`
				Title       string `json:"title"`
				StoryTitle  string `json:"story_title"`
				CommentText string `json:"comment_text"`
				StoryText   string `json:"story_text"`
				URL         string `json:"url"`
				CreatedAtI  int64  `json:"created_at_i"`
			}
		}
		if err := s.getJSON(ctx, root+"/search_by_date?"+v.Encode(), &r); err != nil {
			return out, err
		}
		for _, h := range r.Hits {
			x := base
			x.OccurredAt = time.Unix(h.CreatedAtI, 0).UTC()
			x.Title = h.Title
			kind := "story"
			if x.Title == "" {
				x.Title, kind = "Comment on: "+h.StoryTitle, "comment"
			}
			x.EvidenceURL = "https://news.ycombinator.com/item?id=" + h.ObjectID
			x.Subject = Subject{HN: h.Author}
			x.Data = map[string]any{"source": "hn", "kind": kind, "keyword": kw, "url": h.URL,
				"excerpt": excerpt(stripTags(h.CommentText+h.StoryText), 400)}
			x.DedupeKey = "hn:" + h.ObjectID
			out = append(out, x)
		}
	}
	return out, nil
}

func (s *Sources) stackOverflow(ctx context.Context, base Signal, m *product.Monitor, since time.Time) ([]Signal, error) {
	root := s.StackBase
	if root == "" {
		root = "https://api.stackexchange.com/2.3"
	}
	var out []Signal
	for _, kw := range m.Keywords {
		v := url.Values{"order": {"desc"}, "sort": {"creation"}, "q": {kw}, "site": {"stackoverflow"},
			"fromdate": {fmt.Sprint(since.Unix())}, "pagesize": {"100"}}
		if s.StackKey != "" {
			v.Set("key", s.StackKey)
		}
		var r struct {
			Items []struct {
				QuestionID   int64    `json:"question_id"`
				Title        string   `json:"title"`
				Link         string   `json:"link"`
				CreationDate int64    `json:"creation_date"`
				Tags         []string `json:"tags"`
				Owner        struct {
					UserID      int64  `json:"user_id"`
					DisplayName string `json:"display_name"`
				} `json:"owner"`
			}
		}
		if err := s.getJSON(ctx, root+"/search/advanced?"+v.Encode(), &r); err != nil {
			return out, err
		}
		for _, it := range r.Items {
			x := base
			x.OccurredAt = time.Unix(it.CreationDate, 0).UTC()
			x.Title, x.EvidenceURL = html.UnescapeString(it.Title), it.Link
			if it.Owner.UserID != 0 {
				x.Subject = Subject{StackOverflow: fmt.Sprint(it.Owner.UserID), Name: html.UnescapeString(it.Owner.DisplayName)}
			}
			x.Data = map[string]any{"source": "stackoverflow", "keyword": kw, "tags": it.Tags}
			x.DedupeKey = fmt.Sprintf("so:%d", it.QuestionID)
			out = append(out, x)
		}
	}
	return out, nil
}

type feed struct {
	Items []struct {
		Title       string `xml:"title"`
		Link        string `xml:"link"`
		GUID        string `xml:"guid"`
		PubDate     string `xml:"pubDate"`
		Description string `xml:"description"`
	} `xml:"channel>item"`
	Entries []struct {
		Title     string `xml:"title"`
		ID        string `xml:"id"`
		Updated   string `xml:"updated"`
		Published string `xml:"published"`
		Summary   string `xml:"summary"`
		Content   string `xml:"content"`
		Links     []struct {
			Href string `xml:"href,attr"`
		} `xml:"link"`
	} `xml:"entry"`
}

func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC3339, "Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func (s *Sources) rss(ctx context.Context, base Signal, m *product.Monitor, since time.Time) ([]Signal, error) {
	var out []Signal
	var errs []error
	for _, u := range m.Feeds {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		req.Header.Set("User-Agent", "astrolabe/0.1")
		resp, err := s.httpClient().Do(req)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var f feed
		err = xml.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(&f)
		resp.Body.Close()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", u, err))
			continue
		}
		type item struct {
			title, link, id, text string
			at                    time.Time
		}
		var items []item
		for _, it := range f.Items {
			id := it.GUID
			if id == "" {
				id = it.Link
			}
			items = append(items, item{it.Title, it.Link, id, it.Description, parseTime(it.PubDate)})
		}
		for _, e := range f.Entries {
			link := ""
			if len(e.Links) > 0 {
				link = e.Links[0].Href
			}
			at := parseTime(e.Published)
			if at.IsZero() {
				at = parseTime(e.Updated)
			}
			items = append(items, item{e.Title, link, e.ID, e.Summary + e.Content, at})
		}
		for _, it := range items {
			if it.at.IsZero() || it.at.Before(since) {
				continue
			}
			text := strings.ToLower(it.title + " " + stripTags(it.text))
			kw := ""
			for _, k := range m.Keywords {
				if strings.Contains(text, strings.ToLower(k)) {
					kw = k
					break
				}
			}
			if kw == "" {
				continue
			}
			x := base
			x.OccurredAt, x.Title, x.EvidenceURL = it.at, html.UnescapeString(it.title), it.link
			x.Data = map[string]any{"source": "rss", "feed": u, "keyword": kw, "excerpt": excerpt(stripTags(it.text), 400)}
			x.DedupeKey = "rss:" + it.id
			out = append(out, x)
		}
	}
	return out, errors.Join(errs...)
}

var tagRE = regexp.MustCompile(`<[^>]*>`)

func stripTags(s string) string { return html.UnescapeString(tagRE.ReplaceAllString(s, " ")) }

func excerpt(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
