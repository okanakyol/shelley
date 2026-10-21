package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Sign in with ChatGPT plan usage for open-source, locally hosted apps:
// https://developers.openai.com/siwc/token-sharing-open-source
const (
	defaultAuthURL = "https://auth.openai.com"
	defaultAPIURL  = "https://api.openai.com/v1"
	// The resource the tokens are issued for, independent of where requests go.
	tokenResource = "https://api.openai.com/v1"
	tokenScope    = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	callbackPath  = "/auth/callback"
	// Refresh this long before expiry so no request starts with a dying token.
	expiryMargin = 3 * time.Minute
)

var errSignedOut = errors.New("ChatGPT is not signed in; run `shelley-edge login` on this machine")

// credential is the saved Sign in with ChatGPT state. HostID identifies this
// machine to OpenAI and outlives sign-outs; the rest comes from the last
// successful sign-in or refresh.
type credential struct {
	HostID   string    `json:"host_id"`
	ClientID string    `json:"client_id,omitempty"`
	Access   string    `json:"access_token,omitempty"`
	Refresh  string    `json:"refresh_token,omitempty"`
	Expires  time.Time `json:"expires,omitzero"`
}

func (c credential) signedIn() bool { return c.ClientID != "" && c.Refresh != "" }

type chatgpt struct {
	authURL, apiURL string
	httpc           *http.Client
	secrets         secrets
	now             func() time.Time

	// Refreshes rotate the refresh token, so they run one at a time: mu within
	// this process, lock across shelley-edge processes.
	mu   sync.Mutex
	lock fileLock
}

func newChatGPT(s secrets, lock fileLock) *chatgpt {
	return &chatgpt{authURL: defaultAuthURL, apiURL: defaultAPIURL, httpc: http.DefaultClient, secrets: s, now: time.Now, lock: lock}
}

// saveLocked saves cred while holding the cross-process lock.
func (c *chatgpt) saveLocked(cred credential) error {
	unlock, err := c.lock.lock()
	if err != nil {
		return err
	}
	defer unlock()
	return c.save(cred)
}

func (c *chatgpt) load() (credential, error) {
	var cred credential
	raw, err := c.secrets.get("chatgpt")
	if err != nil || raw == nil {
		return cred, err
	}
	if err := json.Unmarshal(raw, &cred); err != nil {
		return cred, fmt.Errorf("decode ChatGPT credential: %w", err)
	}
	return cred, nil
}

func (c *chatgpt) save(cred credential) error {
	raw, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	return c.secrets.set("chatgpt", raw)
}

// accessToken returns a live access token, refreshing and saving the
// credential when it is about to expire.
func (c *chatgpt) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if tok, live, err := c.liveToken(); err != nil || live {
		return tok, err
	}
	unlock, err := c.lock.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	// Another shelley-edge may have refreshed while this one waited.
	if tok, live, err := c.liveToken(); err != nil || live {
		return tok, err
	}
	cred, err := c.load()
	if err != nil {
		return "", err
	}
	tok, err := c.requestToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {cred.ClientID},
		"refresh_token": {cred.Refresh},
		"resource":      {tokenResource},
	})
	if err != nil {
		return "", err
	}
	c.apply(&cred, tok)
	if err := c.save(cred); err != nil {
		return "", err
	}
	return cred.Access, nil
}

// liveToken returns the saved access token when it has not expired.
func (c *chatgpt) liveToken() (string, bool, error) {
	cred, err := c.load()
	if err != nil {
		return "", false, err
	}
	if !cred.signedIn() {
		return "", false, errSignedOut
	}
	return cred.Access, c.now().Before(cred.Expires), nil
}

type tokenResponse struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Expires int    `json:"expires_in"`
	Scope   string `json:"scope"`
}

func (c *chatgpt) apply(cred *credential, tok tokenResponse) {
	cred.Access, cred.Refresh = tok.Access, tok.Refresh
	cred.Expires = c.now().Add(time.Duration(tok.Expires)*time.Second - expiryMargin)
}

func (c *chatgpt) requestToken(ctx context.Context, form url.Values) (tokenResponse, error) {
	// Bounded so a stuck refresh cannot hold the lock indefinitely.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.authURL+"/api/accounts/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("ChatGPT token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
		// invalid_grant: the grant was revoked or the refresh token was reused.
		return tokenResponse{}, fmt.Errorf("%w (%s)", errSignedOut, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode != http.StatusOK {
		return tokenResponse{}, fmt.Errorf("ChatGPT token request failed (%d): %s", resp.StatusCode, body)
	}
	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return tokenResponse{}, fmt.Errorf("decode ChatGPT token response: %w", err)
	}
	if tok.Access == "" || tok.Refresh == "" || tok.Expires <= 0 {
		return tokenResponse{}, errors.New("ChatGPT token response is missing fields")
	}
	if !strings.Contains(" "+tok.Scope+" ", " chatgpt.tokens.use.direct ") {
		return tokenResponse{}, errors.New("ChatGPT did not grant plan usage (chatgpt.tokens.use.direct)")
	}
	return tok, nil
}

// login runs the browser sign-in. ln is the loopback listener the browser is
// sent back to; show receives the URL to open.
func (c *chatgpt) login(ctx context.Context, ln net.Listener, show func(string)) error {
	cred, err := c.load()
	if err != nil {
		return err
	}
	if cred.HostID == "" {
		cred.HostID = "urn:uuid:" + uuid()
		// Saved before sign-in so the host keeps one identity across attempts.
		if err := c.saveLocked(cred); err != nil {
			return err
		}
	}
	verifier := random()
	sum := sha256.Sum256([]byte(verifier))
	state := random()
	redirect := "http://" + ln.Addr().String() + callbackPath

	params := url.Values{
		"ext_agent_host_id":     {cred.HostID},
		"response_type":         {"code"},
		"redirect_uri":          {redirect},
		"resource":              {tokenResource},
		"scope":                 {tokenScope},
		"state":                 {state},
		"nonce":                 {random()},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	if cred.ClientID != "" {
		params.Set("client_id", cred.ClientID)
	} else {
		params.Set("client_id", "dynamic_agent_client")
		params.Set("agent_name_hint", "Shelley")
	}

	type callback struct {
		code, clientID string
		err            error
	}
	got := make(chan callback, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var cb callback
		switch {
		case r.URL.Path != callbackPath:
			http.NotFound(w, r)
			return
		case q.Get("state") != state:
			http.Error(w, "This sign-in link is stale. Start again with shelley-edge login.", http.StatusBadRequest)
			return
		case q.Get("error") != "":
			cb.err = fmt.Errorf("ChatGPT sign-in failed: %s %s", q.Get("error"), q.Get("error_description"))
		case q.Get("code") == "":
			cb.err = errors.New("ChatGPT sign-in callback has no code")
		default:
			// Registration returns the issued client ID; reauthorization keeps ours.
			cb.code, cb.clientID = q.Get("code"), cmp.Or(q.Get("client_id"), cred.ClientID)
		}
		if cb.err != nil {
			http.Error(w, cb.err.Error(), http.StatusBadRequest)
		} else {
			fmt.Fprintln(w, "ChatGPT is connected to Shelley. You can close this window.")
		}
		select {
		case got <- cb:
		default:
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	u := c.authURL + "/api/accounts/authorize?" + params.Encode()
	show(u)

	var cb callback
	select {
	case cb = <-got:
	case <-ctx.Done():
		return ctx.Err()
	}
	if cb.err != nil {
		return cb.err
	}
	if cb.clientID == "" {
		return errors.New("ChatGPT sign-in callback has no client_id")
	}
	tok, err := c.requestToken(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {cb.clientID},
		"code":          {cb.code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirect},
		"resource":      {tokenResource},
	})
	if err != nil {
		return err
	}
	cred.ClientID = cb.clientID
	c.apply(&cred, tok)
	return c.saveLocked(cred)
}

func random() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func uuid() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
