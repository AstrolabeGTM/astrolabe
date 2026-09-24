// Package llm calls the Claude Messages API and records every call (prompt,
// model, tokens, output) in llm_calls for audit and spend.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Client struct {
	APIKey string
	Base   string // default https://api.anthropic.com
	HTTP   *http.Client
	Pool   *pgxpool.Pool
	// Writer is used for briefs and drafts; Fast for classification.
	Writer, Fast string
}

// ErrDisabled means no API key is configured; callers fall back to manual.
var ErrDisabled = errors.New("llm: ASTROLABE_ANTHROPIC_API_KEY is not set")

// FromEnv builds a client; Enabled() is false without an API key.
func FromEnv(pool *pgxpool.Pool) *Client {
	c := &Client{APIKey: os.Getenv("ASTROLABE_ANTHROPIC_API_KEY"), Pool: pool,
		Writer: os.Getenv("ASTROLABE_MODEL_WRITER"), Fast: os.Getenv("ASTROLABE_MODEL_FAST")}
	if c.APIKey == "" {
		c.APIKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if c.Writer == "" {
		c.Writer = "claude-sonnet-5"
	}
	if c.Fast == "" {
		c.Fast = "claude-haiku-4-5-20251001"
	}
	return c
}

func (c *Client) Enabled() bool { return c != nil && c.APIKey != "" }

type Request struct {
	Purpose   string // brief, draft, classify, answer, content, digest
	Product   string
	Model     string // defaults to Writer
	System    string
	Prompt    string
	MaxTokens int
}

type Response struct {
	Text      string
	CallID    int64
	InTokens  int
	OutTokens int
}

// Complete sends one user message and returns the text reply.
func (c *Client) Complete(ctx context.Context, r Request) (Response, error) {
	if !c.Enabled() {
		return Response{}, ErrDisabled
	}
	if r.Model == "" {
		r.Model = c.Writer
	}
	if r.MaxTokens == 0 {
		r.MaxTokens = 1500
	}
	body, _ := json.Marshal(map[string]any{
		"model": r.Model, "max_tokens": r.MaxTokens, "system": r.System,
		"messages": []map[string]string{{"role": "user", "content": r.Prompt}},
	})
	base := c.Base
	if base == "" {
		base = "https://api.anthropic.com"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 3 * time.Minute}
	}
	start := time.Now()
	var res Response
	var callErr error
	resp, err := hc.Do(req)
	if err != nil {
		callErr = err
	} else {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
		resp.Body.Close()
		var out struct {
			Content []struct{ Type, Text string }
			Usage   struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			}
			StopReason string `json:"stop_reason"`
			Error      struct{ Type, Message string }
		}
		json.Unmarshal(b, &out)
		switch {
		case resp.StatusCode != 200:
			msg := out.Error.Message
			if msg == "" {
				msg = strings.TrimSpace(string(b))
			}
			callErr = fmt.Errorf("claude %d: %s", resp.StatusCode, msg)
		default:
			for _, part := range out.Content {
				if part.Type == "text" {
					res.Text += part.Text
				}
			}
			res.InTokens, res.OutTokens = out.Usage.InputTokens, out.Usage.OutputTokens
			if out.StopReason == "max_tokens" {
				callErr = errors.New("claude: reply was cut off at max_tokens")
			}
		}
	}
	errMsg := ""
	if callErr != nil {
		errMsg = callErr.Error()
	}
	if c.Pool != nil {
		var product *string
		if r.Product != "" {
			product = &r.Product
		}
		prompt := "SYSTEM:\n" + r.System + "\n\nUSER:\n" + r.Prompt
		// Logging must not lose the result of a call already paid for.
		if err := c.Pool.QueryRow(context.WithoutCancel(ctx), `
			INSERT INTO llm_calls (purpose, product_id, model, input_tokens, output_tokens, prompt, output, error, duration_ms)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
			r.Purpose, product, r.Model, res.InTokens, res.OutTokens, prompt, res.Text, errMsg, time.Since(start).Milliseconds()).Scan(&res.CallID); err != nil {
			return res, fmt.Errorf("log llm call: %w", err)
		}
	}
	return res, callErr
}

// Prices are USD per million tokens, from ASTROLABE_LLM_PRICES like
// "claude-sonnet-5=3:15,claude-haiku-4-5-20251001=1:5". Without them, spend
// reports show tokens only rather than guessed dollars.
func Prices() map[string][2]float64 {
	out := map[string][2]float64{}
	for _, part := range strings.Split(os.Getenv("ASTROLABE_LLM_PRICES"), ",") {
		model, p, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		in, outp, ok := strings.Cut(p, ":")
		if !ok {
			continue
		}
		i, err1 := strconv.ParseFloat(in, 64)
		o, err2 := strconv.ParseFloat(outp, 64)
		if err1 == nil && err2 == nil {
			out[model] = [2]float64{i, o}
		}
	}
	return out
}

// JSON extracts the first JSON object from a model reply into v.
func JSON(text string, v any) error {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return fmt.Errorf("no JSON object in reply: %.200s", text)
	}
	return json.Unmarshal([]byte(text[start:end+1]), v)
}
