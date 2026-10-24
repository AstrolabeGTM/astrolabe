package mcp

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// HTTPHandler serves the same tools as Run, over MCP's streamable HTTP
// transport, behind a bearer token. token is called on every request so a
// rotated token takes effect at once; an empty token rejects everything.
//
// Besides the token it enables the SDK's DNS-rebinding protection (requests
// that arrive on a localhost address with a foreign Host header get 403) and
// cross-origin protection (browser requests from another origin are refused).
// Non-browser clients such as Claude Code and Codex send neither header and
// are unaffected.
func (s *Server) HTTPHandler(version string, token func() string) http.Handler {
	mcpHandler := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return s.Build(version) },
		&sdk.StreamableHTTPOptions{CrossOriginProtection: http.NewCrossOriginProtection()},
	)
	return RequireBearer(token, mcpHandler)
}

// RequireBearer lets a request through only if it carries
// "Authorization: Bearer <token>". The comparison is constant time and
// neither the presented nor the expected token is ever logged.
func RequireBearer(token func() string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !bearerOK(r.Header.Get("Authorization"), token()) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="astrolabe"`)
			http.Error(w, "unauthorized: send the MCP token as 'Authorization: Bearer <token>' (astrolabe mcp token prints it)", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerOK(header, want string) bool {
	scheme, got, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || want == "" {
		return false
	}
	// Hash first so the comparison is constant time whatever the lengths.
	a, b := sha256.Sum256([]byte(strings.TrimSpace(got))), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}
