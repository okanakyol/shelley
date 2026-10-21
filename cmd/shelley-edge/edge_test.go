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

	"shelley.exe.dev/models/modelsdev"
)

type memSecrets struct {
	mu     sync.Mutex
	values map[string][]byte
}

func (m *memSecrets) get(account string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.values[account], nil
}

func (m *memSecrets) set(account string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.values == nil {
		m.values = map[string][]byte{}
	}
	m.values[account] = value
	return nil
}

func (m *memSecrets) cred(t *testing.T) credential {
	t.Helper()
	var c credential
	if raw, _ := m.get("chatgpt"); raw != nil {
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
	}
	return c
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
	opencode  []*http.Request
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
		case "/opencode/models":
			f.opencode = append(f.opencode, r.Clone(r.Context()))
			fmt.Fprint(w, `{"object":"list","data":[{"id":"glm-5.3"},{"id":"minimax-m3"},{"id":"gpt-5.6-luna"},{"id":"not-in-catalog"}]}`)
		case "/opencode/chat/completions", "/opencode/messages", "/opencode/responses":
			f.opencode = append(f.opencode, r.Clone(r.Context()))
			fmt.Fprint(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	modelsdev.SetEndpointAlias(f.URL+"/opencode", "https://opencode.ai/zen/go/v1")
	return f
}

func testChatGPT(f *fakeOpenAI, s secrets, now time.Time) *chatgpt {
	c := newChatGPT(s)
	c.authURL, c.apiURL, c.httpc = f.URL, f.URL+"/v1", f.Client()
	c.now = func() time.Time { return now }
	return c
}

func testEdge(f *fakeOpenAI, s secrets, now time.Time) *httptest.Server {
	e := &edge{
		chatgpt:  testChatGPT(f, s, now),
		opencode: &opencode{apiURL: f.URL + "/opencode", httpc: f.Client(), secrets: s},
	}
	srv := httptest.NewUnstartedServer(e)
	hosts, err := allowedHosts(srv.Listener.Addr().String())
	if err != nil {
		panic(err)
	}
	e.hosts = hosts
	srv.Start()
	return srv
}

func signedIn(now time.Time, expires time.Duration) *memSecrets {
	s := &memSecrets{}
	raw, _ := json.Marshal(credential{HostID: "urn:uuid:h", ClientID: "oaiapp_1", Access: "access-0", Refresh: "refresh-0", Expires: now.Add(expires)})
	s.set("chatgpt", raw)
	return s
}

func TestLoginRegistersAndSavesCredential(t *testing.T) {
	f := newFakeOpenAI(t)
	s := &memSecrets{}
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
	if c := s.cred(t); c.ClientID != "oaiapp_1" || c.Access != "access-1" || c.HostID != authorize.Get("ext_agent_host_id") {
		t.Fatalf("saved %+v", c)
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
	if c := s.cred(t); c.Refresh != "refresh-1" {
		t.Fatalf("rotated refresh token not saved: %+v", c)
	}
}

func TestResponsesForwardWithCredentialAndStream(t *testing.T) {
	f := newFakeOpenAI(t)
	now := time.Now()
	srv := testEdge(f, signedIn(now, time.Hour), now)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/chatgpt/v1/responses", strings.NewReader(`{"model":"gpt-6.1-sol","stream":true}`))
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
	srv := testEdge(f, signedIn(now, time.Hour), now)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/chatgpt/models.json")
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
	for name, s := range map[string]secrets{"never signed in": &memSecrets{}, "revoked": expired} {
		srv := testEdge(f, s, now)
		resp, err := http.Post(srv.URL+"/chatgpt/v1/responses", "application/json", strings.NewReader(`{}`))
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

func withOpenCodeKey(s *memSecrets) *memSecrets {
	s.set("opencode", []byte("oc-key"))
	return s
}

func TestOpenCodeModelsJSONPicksEachModelsProtocol(t *testing.T) {
	f := newFakeOpenAI(t)
	now := time.Now()
	srv := testEdge(f, withOpenCodeKey(&memSecrets{}), now)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/opencode/models.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var catalog integrationCatalog
	json.NewDecoder(resp.Body).Decode(&catalog)
	got := map[string]string{}
	for _, m := range catalog.Models {
		got[m.ID] = m.Provider + " " + strings.Join(m.APIs, ",") + " " + strings.Join(m.Architecture.InputModalities, ",")
	}
	want := map[string]string{
		"glm-5.3":      "opencode openai_chat text",
		"minimax-m3":   "opencode anthropic_messages text,image",
		"gpt-5.6-luna": "openai openai_responses text,image",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) || catalog.ModelsDevAPI != f.URL+"/opencode" {
		t.Fatalf("catalog %v (models_dev_api %q)", got, catalog.ModelsDevAPI)
	}
	if f.opencode[0].Header.Get("Authorization") != "Bearer oc-key" {
		t.Fatalf("model listing sent %v", f.opencode[0].Header)
	}
}

func TestOpenCodeForwardsWithKeyAndSession(t *testing.T) {
	f := newFakeOpenAI(t)
	srv := testEdge(f, withOpenCodeKey(&memSecrets{}), time.Now())
	defer srv.Close()
	for _, op := range []string{"/chat/completions", "/responses", "/messages"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/opencode/v1"+op, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer implicit")
		req.Header.Set("X-Api-Key", "implicit")
		req.Header.Set("Shelley-Conversation-Id", "conv-1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		up := f.opencode[len(f.opencode)-1]
		auth, apiKey := up.Header.Get("Authorization"), up.Header.Get("X-Api-Key")
		if op == "/messages" && (auth != "" || apiKey != "oc-key") || op != "/messages" && (auth != "Bearer oc-key" || apiKey != "") {
			t.Errorf("%s: Authorization=%q X-Api-Key=%q", op, auth, apiKey)
		}
		if up.URL.Path != "/opencode"+op || up.Header.Get("X-Opencode-Session") != "conv-1" || up.Header.Get("Shelley-Conversation-Id") != "" {
			t.Errorf("%s: path %s headers %v", op, up.URL.Path, up.Header)
		}
	}
}

func TestOpenCodeWithoutKeyAnswersUnauthorized(t *testing.T) {
	f := newFakeOpenAI(t)
	srv := testEdge(f, &memSecrets{}, time.Now())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/opencode/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "set-key opencode") || len(f.opencode) != 0 {
		t.Fatalf("%d %s (upstream calls %d)", resp.StatusCode, body, len(f.opencode))
	}
}

func TestEdgeRefusesBrowsersAndUnknownHosts(t *testing.T) {
	f := newFakeOpenAI(t)
	srv := testEdge(f, withOpenCodeKey(signedIn(time.Now(), time.Hour)), time.Now())
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	for _, tc := range []struct {
		name, host, header, value string
		want                      int
	}{
		{"shelley", "", "", "", http.StatusOK},
		{"localhost name", "localhost:" + port, "", "", http.StatusOK},
		{"cross-site page", "", "Origin", "https://evil.example", http.StatusForbidden},
		{"embedded request", "", "Sec-Fetch-Site", "cross-site", http.StatusForbidden},
		{"dns rebinding", "evil.example:" + port, "", "", http.StatusMisdirectedRequest},
	} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/opencode/v1/chat/completions", strings.NewReader(`{}`))
		if tc.host != "" {
			req.Host = tc.host
		}
		if tc.header != "" {
			req.Header.Set(tc.header, tc.value)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}
	if len(f.opencode) != 2 {
		t.Fatalf("upstream saw %d requests, want only the 2 allowed", len(f.opencode))
	}
}

func TestAllowedHosts(t *testing.T) {
	loopback, _ := allowedHosts("127.0.0.1:8080")
	lima, _ := allowedHosts("192.168.5.2:8080")
	if !loopback["localhost:8080"] || !loopback["[::1]:8080"] || loopback["192.168.5.2:8080"] ||
		!lima["192.168.5.2:8080"] || lima["localhost:8080"] {
		t.Fatalf("loopback %v lima %v", loopback, lima)
	}
	if _, err := allowedHosts("8080"); err == nil {
		t.Fatal("accepted a listen address without a host")
	}
}
