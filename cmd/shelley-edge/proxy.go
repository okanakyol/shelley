package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// edge serves one LLM integration per upstream account, each under its own
// path: /chatgpt and /opencode. Each has the contract Shelley's LLM
// integrations use: <base>/models.json lists the models and <base>/v1/...
// forwards requests with the credential added here, so Shelley itself never
// holds it.
type edge struct {
	chatgpt  *chatgpt
	opencode *opencode
	// hosts are the Host header values the edge answers to (see allowedHosts).
	hosts map[string]bool
}

// allowedHosts lists the names a client may use for the listen address. A
// loopback address answers to every loopback name; any other only to itself.
// Checking Host stops DNS rebinding, where a web page reaches the edge
// through its own domain resolved to this address.
func allowedHosts(listen string) (map[string]bool, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, fmt.Errorf("listen address %q: %w", listen, err)
	}
	hosts := map[string]bool{net.JoinHostPort(host, port): true}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() || host == "localhost" {
		for _, name := range []string{"127.0.0.1", "localhost", "::1"} {
			hosts[net.JoinHostPort(name, port)] = true
		}
	}
	return hosts, nil
}

func (e *edge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Browsers mark every request they send, including ones a web page makes
	// to this address; Shelley's HTTP client sends neither header.
	if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
		http.Error(w, "shelley-edge does not serve browsers", http.StatusForbidden)
		return
	}
	if !e.hosts[strings.ToLower(r.Host)] {
		http.Error(w, "unknown host", http.StatusMisdirectedRequest)
		return
	}
	get, post := r.Method == http.MethodGet, r.Method == http.MethodPost
	switch path := r.URL.Path; {
	case get && path == "/chatgpt/models.json":
		e.chatgptModels(w, r)
	case post && path == "/chatgpt/v1/responses":
		e.chatgptResponses(w, r)
	case get && path == "/opencode/models.json":
		e.opencode.models(w, r)
	case post && (path == "/opencode/v1/chat/completions" || path == "/opencode/v1/responses" || path == "/opencode/v1/messages"):
		e.opencode.forward(w, r, strings.TrimPrefix(path, "/opencode/v1"))
	default:
		http.NotFound(w, r)
	}
}

// integrationCatalog is a models.json body (see modelsources).
type integrationCatalog struct {
	SchemaVersion int                `json:"schema_version"`
	ModelsDevAPI  string             `json:"models_dev_api,omitempty"`
	Models        []integrationModel `json:"models"`
}

type integrationModel struct {
	ID           string   `json:"id"`
	Provider     string   `json:"provider"`
	APIs         []string `json:"apis"`
	Architecture struct {
		InputModalities []string `json:"input_modalities,omitempty"`
	} `json:"architecture"`
	ExeDev struct {
		Mode string `json:"mode,omitempty"`
	} `json:"exe_dev"`
}

func (e *edge) chatgptModels(w http.ResponseWriter, r *http.Request) {
	token, err := e.chatgpt.accessToken(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, e.chatgpt.apiURL+"/models", nil)
	if err != nil {
		writeError(w, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.chatgpt.httpc.Do(req)
	if err != nil {
		writeError(w, fmt.Errorf("list ChatGPT models: %w", err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		writeError(w, fmt.Errorf("list ChatGPT models failed (%d): %s", resp.StatusCode, body))
		return
	}
	var listing struct {
		Models []struct {
			Slug            string   `json:"slug"`
			Visibility      string   `json:"visibility"`
			InputModalities []string `json:"input_modalities"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		writeError(w, fmt.Errorf("decode ChatGPT models: %w", err))
		return
	}
	catalog := integrationCatalog{SchemaVersion: 1, Models: []integrationModel{}}
	for _, m := range listing.Models {
		if m.Visibility != "list" || m.Slug == "" {
			continue
		}
		model := integrationModel{ID: m.Slug, Provider: "openai", APIs: []string{"openai_responses"}}
		model.Architecture.InputModalities = m.InputModalities
		model.ExeDev.Mode = "chatgpt"
		catalog.Models = append(catalog.Models, model)
	}
	writeJSON(w, catalog)
}

func (e *edge) chatgptResponses(w http.ResponseWriter, r *http.Request) {
	token, err := e.chatgpt.accessToken(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	forward(w, r, e.chatgpt.httpc, e.chatgpt.apiURL+"/responses", func(h http.Header) {
		h.Set("Authorization", "Bearer "+token)
	})
}

// forward proxies r to target, streaming the response. auth adds the
// upstream credential to the outgoing headers, which Shelley's own headers
// (naming local conversations and machines) never leave the edge with.
func forward(w http.ResponseWriter, r *http.Request, httpc *http.Client, target string, auth func(http.Header)) {
	u, err := url.Parse(target)
	if err != nil {
		writeError(w, err)
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL, pr.Out.Host = u, u.Host
			auth(pr.Out.Header)
			for name := range pr.Out.Header {
				if strings.HasPrefix(strings.ToLower(name), "shelley-") {
					pr.Out.Header.Del(name)
				}
			}
		},
		Transport:     httpc.Transport,
		FlushInterval: -1, // stream SSE events as they arrive
	}
	proxy.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// writeError answers in the OpenAI error shape so Shelley shows the message.
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	if errors.Is(err, errSignedOut) || errors.Is(err, errNoOpenCodeKey) {
		status = http.StatusUnauthorized
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": err.Error(), "type": "shelley_edge"}})
}
