package forms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
)

// Netlify reads form submissions through Netlify's API
// (https://docs.netlify.com/api-and-cli-guides/api-guides/get-started-with-api/#forms):
//
//	GET /api/v1/sites/{site}/forms              the site's forms, to find one by name
//	GET /api/v1/forms/{form_id}/submissions     verified submissions, newest first
//	DELETE /api/v1/submissions/{submission_id}  erase one submission (204), used when a person asks to be forgotten
//
// The submissions endpoint returns verified submissions unless asked for
// state=spam, so spam is never fetched; a submission that says it is spam
// anyway is skipped too.
type Netlify struct {
	Token string
	Base  string // default https://api.netlify.com/api/v1
	HTTP  *http.Client
}

const (
	netlifyPerPage = 100
	netlifyMaxPage = 20 // at most 2,000 submissions per run
)

func (n *Netlify) base() string {
	if n.Base != "" {
		return strings.TrimRight(n.Base, "/")
	}
	return "https://api.netlify.com/api/v1"
}

func (n *Netlify) get(ctx context.Context, path string, q url.Values, out any) error {
	return n.call(ctx, "GET", path, q, out)
}

// DeleteSubmission erases one submission at Netlify. A submission Netlify no
// longer has (404) counts as erased.
func (n *Netlify) DeleteSubmission(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("no submission id")
	}
	err := n.call(ctx, "DELETE", "/submissions/"+url.PathEscape(id), nil, nil)
	var he *httpError
	if errors.As(err, &he) && he.status == http.StatusNotFound {
		return nil
	}
	return err
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func (n *Netlify) call(ctx context.Context, method, path string, q url.Values, out any) error {
	if n.Token == "" {
		return errors.New("ASTROLABE_NETLIFY_TOKEN is not set")
	}
	u := n.base() + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.Token)
	req.Header.Set("User-Agent", "astrolabe/0.1")
	c := n.HTTP
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if method == "DELETE" && (resp.StatusCode == 200 || resp.StatusCode == 204) {
		return nil
	}
	if resp.StatusCode != 200 {
		msg := strings.Join(strings.Fields(string(b)), " ")
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return &httpError{resp.StatusCode, fmt.Sprintf("netlify %s: %d %s", path, resp.StatusCode, msg)}
	}
	return json.Unmarshal(b, out)
}

type netlifySubmission struct {
	ID        string         `json:"id"`
	Number    int            `json:"number"`
	Email     string         `json:"email"`
	Name      string         `json:"name"`
	Company   string         `json:"company"`
	Summary   string         `json:"summary"`
	State     string         `json:"state"`
	Data      map[string]any `json:"data"`
	CreatedAt time.Time      `json:"created_at"`
}

// Fetch returns a form's verified submissions created at or after since,
// oldest first.
func (n *Netlify) Fetch(ctx context.Context, m *product.Monitor, since time.Time) ([]Submission, error) {
	var forms []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := n.get(ctx, "/sites/"+url.PathEscape(m.Site)+"/forms", url.Values{"per_page": {"100"}}, &forms); err != nil {
		return nil, err
	}
	id := ""
	for _, f := range forms {
		if strings.EqualFold(f.Name, m.Form) {
			id = f.ID
		}
	}
	if id == "" {
		return nil, fmt.Errorf("site %s has no form named %q", m.Site, m.Form)
	}
	var out []Submission
	for page := 1; page <= netlifyMaxPage; page++ {
		var subs []netlifySubmission
		q := url.Values{"page": {fmt.Sprint(page)}, "per_page": {fmt.Sprint(netlifyPerPage)}}
		if err := n.get(ctx, "/forms/"+url.PathEscape(id)+"/submissions", q, &subs); err != nil {
			return out, err
		}
		older := false
		for _, s := range subs {
			if s.CreatedAt.Before(since) {
				older = true
				continue
			}
			if s.State == "spam" || s.ID == "" {
				continue
			}
			fields := map[string]any{}
			for k, v := range s.Data {
				fields[k] = v
			}
			// Netlify also keeps the top-level email, name and company.
			for k, v := range map[string]string{"email": s.Email, "name": s.Name, "company": s.Company} {
				if _, ok := fields[k]; !ok && v != "" {
					fields[k] = v
				}
			}
			out = append(out, Submission{Source: "netlify", ExternalID: s.ID, At: s.CreatedAt.UTC(), Fields: fields})
		}
		if older || len(subs) < netlifyPerPage {
			break
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
