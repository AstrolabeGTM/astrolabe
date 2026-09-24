package gmail

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

func parseAddress(s string) (string, error) {
	a, err := mail.ParseAddress(s)
	if err != nil {
		return "", err
	}
	return strings.ToLower(a.Address), nil
}

// Authorize runs the OAuth loopback flow for one mailbox: it prints a URL,
// waits for Google to redirect back to a local port, and checks that the
// account signed in is the expected address.
func (a *Accounts) Authorize(ctx context.Context, address string, show func(url string)) error {
	address = strings.ToLower(address)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	cfg := *a.OAuth
	cfg.RedirectURL = "http://" + ln.Addr().String() + "/callback"

	b := make([]byte, 16)
	rand.Read(b)
	state := hex.EncodeToString(b)
	verifier := oauth2.GenerateVerifier()
	show(cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "consent"), oauth2.SetAuthURLParam("login_hint", address)))

	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		switch {
		case q.Get("state") != state:
			done <- result{err: errors.New("OAuth state mismatch")}
		case q.Get("error") != "":
			done <- result{err: fmt.Errorf("Google returned %s", q.Get("error"))}
		default:
			done <- result{code: q.Get("code")}
		}
		fmt.Fprintln(w, "Astrolabe: you can close this tab.")
	})}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	var res result
	select {
	case res = <-done:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Minute):
		return errors.New("timed out waiting for the browser")
	}
	if res.err != nil {
		return res.err
	}
	tok, err := cfg.Exchange(ctx, res.code, oauth2.VerifierOption(verifier))
	if err != nil {
		return err
	}
	if tok.RefreshToken == "" {
		return errors.New("Google returned no refresh token; remove Astrolabe's access at myaccount.google.com/permissions and retry")
	}
	base := a.APIURL
	if base == "" {
		base = apiBase
	}
	mb := NewMailbox(address, base, cfg.Client(ctx, tok))
	var profile struct{ EmailAddress string }
	if err := mb.get(ctx, "/profile", &profile); err != nil {
		return err
	}
	if !strings.EqualFold(profile.EmailAddress, address) {
		return fmt.Errorf("signed in as %s, expected %s", profile.EmailAddress, address)
	}
	return a.SaveToken(address, tok)
}
