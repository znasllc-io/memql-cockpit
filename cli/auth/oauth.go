// Package auth handles authentication for memQL Cockpit against
// memQL's in-house identity service. The flow is modelled after RFC
// 6749 Authorization Code grant, but identity replaces the standard
// /authorize page with an email-driven /login + magic-link
// completion. There is no /.well-known/openid-configuration --
// identity is not an OIDC provider, just a code-flow OAuth issuer.
//
// Sequence:
//
//  1. Cockpit opens a loopback HTTP listener on a random port.
//  2. Cockpit opens the user's browser at
//
//	    <issuer>/login?return_to=http://127.0.0.1:<port>/cockpit/callback
//
//     Identity matches return_to against its registered clients (the
//     "cockpit" client has loopback-any-port redirects per RFC 8252)
//     and renders the email-entry form.
//
//  3. User enters their email, identity issues a magic link.
//  4. User clicks the magic link, identity's /auth/complete consumes
//     the token and 302s the browser to
//
//	    http://127.0.0.1:<port>/cockpit/callback?code=<auth_code>&state=<...>
//
//  5. Cockpit's callback handler captures the code and POSTs to
//     <issuer>/oauth/token to swap it for an access + refresh token
//     pair.
//
// State is supplied by identity (not by the cockpit) and is not
// validated client-side. The CSRF surface is bounded by the
// short-lived loopback listener (only listens for ~5min, only on
// 127.0.0.1) plus identity's own consumed-once auth-code rule.
package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// LoginResult holds the tokens returned from a successful login.
type LoginResult struct {
	AccessToken  string
	RefreshToken string
	Expiry       time.Time
}

// Login runs the cockpit's browser-driven authorization flow. Returns
// a LoginResult on success. Times out after 5 minutes if the user
// doesn't complete the magic-link round trip.
func Login(ctx context.Context, issuer, clientId string) (*LoginResult, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if issuer == "" {
		return nil, errors.New("auth: issuer is required")
	}
	clientId = strings.TrimSpace(clientId)
	if clientId == "" {
		return nil, errors.New("auth: client_id is required")
	}

	// 127.0.0.1 (not "localhost") so the redirect URL the browser
	// follows is unambiguous regardless of the user's /etc/hosts +
	// IPv6 resolution. Identity's registered URIs include both
	// host forms; either matches the loopback rule.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for callback: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURL := fmt.Sprintf("http://127.0.0.1:%d/cockpit/callback", port)

	authURL := buildLoginURL(issuer, redirectURL)

	codeCh := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/cockpit/callback", func(w http.ResponseWriter, r *http.Request) {
		if errParam := r.URL.Query().Get("error"); errParam != "" {
			desc := r.URL.Query().Get("error_description")
			msg := errParam
			if desc != "" {
				msg = errParam + ": " + desc
			}
			codeCh <- callbackResult{err: errors.New(msg)}
			fmt.Fprintf(w, "Authentication failed: %s. You can close this tab.", msg)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			codeCh <- callbackResult{err: errors.New("no code in callback")}
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		codeCh <- callbackResult{code: code}
		fmt.Fprint(w, "Authentication successful. You can close this tab and return to the terminal.")
	})

	server := &http.Server{Handler: mux}
	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- server.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if err := openBrowser(authURL); err != nil {
		return nil, fmt.Errorf("open browser: %w (URL: %s)", err, authURL)
	}

	select {
	case result := <-codeCh:
		if result.err != nil {
			return nil, fmt.Errorf("auth callback: %w", result.err)
		}
		return exchangeCodeForToken(ctx, issuer, clientId, redirectURL, result.code)
	case err := <-serveErrCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil, errors.New("callback server closed before receiving the auth code")
		}
		return nil, fmt.Errorf("callback server: %w", err)
	case <-time.After(5 * time.Minute):
		return nil, errors.New("authentication timed out (5 minutes)")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// buildLoginURL composes the URL the browser should hit to start the
// login flow.
func buildLoginURL(issuer, redirectURL string) string {
	params := url.Values{}
	params.Set("return_to", redirectURL)
	return issuer + "/login?" + params.Encode()
}

// tokenResponse mirrors identity's /oauth/token success body
// (component/identity/http/token.go).
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

// errorResponse mirrors identity's RFC-6749-shaped JSON error body
// (component/identity/http/server.go writeJSONError).
type errorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// exchangeCodeForToken POSTs the auth code to /oauth/token.
func exchangeCodeForToken(ctx context.Context, issuer, clientId, redirectURL, code string) (*LoginResult, error) {
	payload, err := json.Marshal(map[string]string{
		"grant_type":   "authorization_code",
		"code":         code,
		"client_id":    clientId,
		"redirect_uri": redirectURL,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/oauth/token", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var errBody errorResponse
		if json.Unmarshal(body, &errBody) == nil && errBody.Error != "" {
			return nil, fmt.Errorf("token exchange: HTTP %d: %s: %s", resp.StatusCode, errBody.Error, errBody.ErrorDescription)
		}
		return nil, fmt.Errorf("token exchange: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, errors.New("token exchange: response missing access_token")
	}
	expiry := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	if tok.ExpiresIn <= 0 {
		// Defensive: identity always populates ExpiresIn, but if a
		// future change drops it, treat the token as short-lived
		// rather than infinite.
		expiry = time.Now().Add(15 * time.Minute)
	}
	return &LoginResult{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Expiry:       expiry,
	}, nil
}

type callbackResult struct {
	code string
	err  error
}

// randomString generates a random URL-safe string of the given byte
// length. Currently unused by the OAuth flow (identity supplies state)
// but kept as a public helper for future token-id generation.
func randomString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// openBrowser opens the default browser with the given URL.
func openBrowser(target string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target).Start()
	case "linux":
		return exec.Command("xdg-open", target).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}
