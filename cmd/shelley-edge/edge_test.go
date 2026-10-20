package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type memStore struct {
	mu    sync.Mutex
	cred  credential
	saves int
}

func (m *memStore) load() (credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cred, nil
}

func (m *memStore) save(c credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cred = c
	m.saves++
	return nil
}

// fakeOpenAI stands in for auth.openai.com and api.openai.com.
type fakeOpenAI struct {
	*httptest.Server
	mu        sync.Mutex
	forms     []url.Values
	responses *http.Request
	body      string
	refresh   int // refresh tokens issued so far
	revoked   bool
}

func newFakeOpenAI(t *testing.T) *fakeOpenAI {
	f := &fakeOpenAI{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/api/accounts/oauth/token":
			r.ParseForm()
			f.forms = append(f.forms, r.PostForm)
			if f.revoked {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			f.refresh++
			fmt.Fprintf(w, `{"access_token":"access-%d","refresh_token":"refresh-%d","expires_in":3600,"scope":"openid offline_access resource.invoke chatgpt.tokens.use.direct"}`, f.refresh, f.refresh)
		case "/v1/models":
			fmt.Fprint(w, `{"models":[{"slug":"gpt-6.1-sol","visibility":"list","input_modalities":["text","image"]},{"slug":"gpt-reserve","visibility":"hide"}]}`)
		case "/v1/responses":
			body, _ := io.ReadAll(r.Body)
			f.responses, f.body = r.Clone(r.Context()), string(body)
			w.WriteHeader(http.StatusOK)
			for _, event := range []string{"response.created", "response.completed"} {
				fmt.Fprintf(w, "event: %s\ndata: {\"type\":%q}\n\n", event, event)
				w.(http.Flusher).Flush()
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func testChatGPT(f *fakeOpenAI, s store, now time.Time) *chatgpt {
	c := newChatGPT(s)
	c.authURL, c.apiURL, c.httpc = f.URL, f.URL+"/v1", f.Client()
	c.now = func() time.Time { return now }
	return c
}

func signedIn(now time.Time, expires time.Duration) *memStore {
	return &memStore{cred: credential{HostID: "urn:uuid:h", ClientID: "oaiapp_1", Access: "access-0", Refresh: "refresh-0", Expires: now.Add(expires)}}
}

func TestLoginRegistersAndSavesCredential(t *testing.T) {
	f := newFakeOpenAI(t)
	s := &memStore{}
	c := testChatGPT(f, s, time.Now())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var authorize url.Values
	err = c.login(t.Context(), ln, func(u string) {
		parsed, _ := url.Parse(u)
		authorize = parsed.Query()
		// The browser returns with the code and the issued client ID.
		go http.Get(authorize.Get("redirect_uri") + "?" + url.Values{
			"code": {"code-1"}, "state": {authorize.Get("state")}, "client_id": {"oaiapp_1"},
		}.Encode())
	})
	if err != nil {
		t.Fatal(err)
	}
	if authorize.Get("client_id") != "dynamic_agent_client" || authorize.Get("agent_name_hint") != "Shelley" ||
		!strings.HasPrefix(authorize.Get("ext_agent_host_id"), "urn:uuid:") || authorize.Get("scope") != tokenScope {
		t.Fatalf("authorize params: %v", authorize)
	}
	exchange := f.forms[0]
	if exchange.Get("client_id") != "oaiapp_1" || exchange.Get("code") != "code-1" || exchange.Get("code_verifier") == "" ||
		exchange.Get("redirect_uri") != authorize.Get("redirect_uri") {
		t.Fatalf("exchange form: %v", exchange)
	}
	if s.cred.ClientID != "oaiapp_1" || s.cred.Access != "access-1" || s.cred.HostID != authorize.Get("ext_agent_host_id") {
		t.Fatalf("saved %+v", s.cred)
	}
}

func TestLoginReauthorizesWithIssuedClient(t *testing.T) {
	f := newFakeOpenAI(t)
	now := time.Now()
	s := signedIn(now, -time.Minute)
	c := testChatGPT(f, s, now)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	var authorize url.Values
	err := c.login(t.Context(), ln, func(u string) {
		parsed, _ := url.Parse(u)
		authorize = parsed.Query()
		go http.Get(authorize.Get("redirect_uri") + "?" + url.Values{"code": {"c"}, "state": {authorize.Get("state")}}.Encode())
	})
	if err != nil {
		t.Fatal(err)
	}
	if authorize.Get("client_id") != "oaiapp_1" || authorize.Has("agent_name_hint") || authorize.Get("ext_agent_host_id") != "urn:uuid:h" {
		t.Fatalf("reauthorize params: %v", authorize)
	}
}

func TestAccessTokenRefreshesOnceAndSavesRotation(t *testing.T) {
	f := newFakeOpenAI(t)
	now := time.Now()
	s := signedIn(now, time.Minute)
	c := testChatGPT(f, s, now)
	if tok, err := c.accessToken(t.Context()); err != nil || tok != "access-0" {
		t.Fatalf("live token: %q %v", tok, err)
	}
	c.now = func() time.Time { return now.Add(2 * time.Minute) }
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tok, err := c.accessToken(t.Context()); err != nil || tok != "access-1" {
				t.Errorf("refreshed token: %q %v", tok, err)
			}
		}()
	}
	wg.Wait()
	if len(f.forms) != 1 || f.forms[0].Get("refresh_token") != "refresh-0" || f.forms[0].Get("client_id") != "oaiapp_1" {
		t.Fatalf("refresh requests: %v", f.forms)
	}
	if s.cred.Refresh != "refresh-1" {
		t.Fatalf("rotated refresh token not saved: %+v", s.cred)
	}
}

func TestResponsesForwardWithCredentialAndStream(t *testing.T) {
	f := newFakeOpenAI(t)
	now := time.Now()
	srv := httptest.NewServer(&edge{chatgpt: testChatGPT(f, signedIn(now, time.Hour), now)})
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-6.1-sol","stream":true}`))
	req.Header.Set("Authorization", "Bearer implicit")
	req.Header.Set("Shelley-Conversation-Id", "conv-1")
	req.Header.Set("Session-Id", "conv-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var events []string
	for sc := bufio.NewScanner(resp.Body); sc.Scan(); {
		if line, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
			events = append(events, line)
		}
	}
	if strings.Join(events, ",") != "response.created,response.completed" {
		t.Fatalf("events %v", events)
	}
	up := f.responses
	if up.Header.Get("Authorization") != "Bearer access-0" || up.Header.Get("Shelley-Conversation-Id") != "" ||
		up.Header.Get("Session-Id") != "conv-1" || f.body != `{"model":"gpt-6.1-sol","stream":true}` {
		t.Fatalf("upstream got headers %v body %s", up.Header, f.body)
	}
}

func TestModelsJSONListsVisibleChatGPTModels(t *testing.T) {
	f := newFakeOpenAI(t)
	now := time.Now()
	srv := httptest.NewServer(&edge{chatgpt: testChatGPT(f, signedIn(now, time.Hour), now)})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/models.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var catalog struct {
		SchemaVersion int                `json:"schema_version"`
		Models        []integrationModel `json:"models"`
	}
	json.NewDecoder(resp.Body).Decode(&catalog)
	if catalog.SchemaVersion != 1 || len(catalog.Models) != 1 {
		t.Fatalf("catalog %+v", catalog)
	}
	m := catalog.Models[0]
	if m.ID != "gpt-6.1-sol" || m.Provider != "openai" || m.APIs[0] != "openai_responses" || m.ExeDev.Mode != "chatgpt" ||
		strings.Join(m.Architecture.InputModalities, ",") != "text,image" {
		t.Fatalf("model %+v", m)
	}
}

func TestSignedOutAnswersUnauthorized(t *testing.T) {
	f := newFakeOpenAI(t)
	now := time.Now()
	expired := signedIn(now, -time.Minute)
	f.revoked = true
	for name, s := range map[string]store{"never signed in": &memStore{}, "revoked": expired} {
		srv := httptest.NewServer(&edge{chatgpt: testChatGPT(f, s, now)})
		resp, err := http.Post(srv.URL+"/v1/responses", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		srv.Close()
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "shelley-edge login") {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
	if f.responses != nil {
		t.Fatal("request reached OpenAI without a credential")
	}
}
