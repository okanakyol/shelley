package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// edge serves Shelley's LLM integration contract: /models.json lists the
// models, and /v1/responses forwards requests with the ChatGPT credential
// added here, so Shelley itself never holds it.
type edge struct{ chatgpt *chatgpt }

func (e *edge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/models.json":
		e.models(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/responses":
		e.responses(w, r)
	default:
		http.NotFound(w, r)
	}
}

// integrationModel is one models.json entry (see modelsources.IntegrationModel).
type integrationModel struct {
	ID           string   `json:"id"`
	Provider     string   `json:"provider"`
	APIs         []string `json:"apis"`
	Architecture struct {
		InputModalities []string `json:"input_modalities,omitempty"`
	} `json:"architecture"`
	ExeDev struct {
		Mode string `json:"mode"`
	} `json:"exe_dev"`
}

func (e *edge) models(w http.ResponseWriter, r *http.Request) {
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
	catalog := struct {
		SchemaVersion int                `json:"schema_version"`
		Models        []integrationModel `json:"models"`
	}{SchemaVersion: 1, Models: []integrationModel{}}
	for _, m := range listing.Models {
		if m.Visibility != "list" || m.Slug == "" {
			continue
		}
		model := integrationModel{ID: m.Slug, Provider: "openai", APIs: []string{"openai_responses"}}
		model.Architecture.InputModalities = m.InputModalities
		model.ExeDev.Mode = "chatgpt"
		catalog.Models = append(catalog.Models, model)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(catalog)
}

func (e *edge) responses(w http.ResponseWriter, r *http.Request) {
	token, err := e.chatgpt.accessToken(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	target, err := url.Parse(e.chatgpt.apiURL + "/responses")
	if err != nil {
		writeError(w, err)
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = target
			pr.Out.Host = target.Host
			for name := range pr.Out.Header {
				// Shelley's own headers name local conversations and machines.
				if strings.HasPrefix(strings.ToLower(name), "shelley-") {
					pr.Out.Header.Del(name)
				}
			}
			pr.Out.Header.Set("Authorization", "Bearer "+token)
		},
		Transport:     e.chatgpt.httpc.Transport,
		FlushInterval: -1, // stream SSE events as they arrive
	}
	proxy.ServeHTTP(w, r)
}

// writeError answers in the OpenAI error shape so Shelley shows the message.
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	if errors.Is(err, errSignedOut) {
		status = http.StatusUnauthorized
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": err.Error(), "type": "shelley_edge"}})
}
