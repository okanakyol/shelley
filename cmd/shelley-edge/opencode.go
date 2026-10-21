package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"shelley.exe.dev/models/modelsdev"
)

const defaultOpenCodeURL = "https://opencode.ai/zen/go/v1"

var errNoOpenCodeKey = errors.New("no OpenCode API key; run `shelley-edge set-key opencode` on this machine")

// opencode proxies an OpenCode Zen or Go account. Each model speaks the wire
// protocol models.dev records for it.
type opencode struct {
	apiURL  string
	httpc   *http.Client
	secrets secrets
}

func (o *opencode) key() (string, error) {
	key, err := o.secrets.get("opencode")
	if err != nil {
		return "", err
	}
	if len(key) == 0 {
		return "", errNoOpenCodeKey
	}
	return string(key), nil
}

// integrationAPI maps a model's AI SDK package to the models.json API it
// is served over, or "" when Shelley has no client for it.
func integrationAPI(npm string) string {
	switch npm {
	case "@ai-sdk/anthropic":
		return "anthropic_messages"
	case "@ai-sdk/openai":
		return "openai_responses"
	case "@ai-sdk/openai-compatible":
		return "openai_chat"
	}
	return ""
}

func (o *opencode) models(w http.ResponseWriter, r *http.Request) {
	key, err := o.key()
	if err != nil {
		writeError(w, err)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, o.apiURL+"/models", nil)
	if err != nil {
		writeError(w, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := o.httpc.Do(req)
	if err != nil {
		writeError(w, fmt.Errorf("list OpenCode models: %w", err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		writeError(w, fmt.Errorf("list OpenCode models failed (%d): %s", resp.StatusCode, body))
		return
	}
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		writeError(w, fmt.Errorf("decode OpenCode models: %w", err))
		return
	}
	catalog := integrationCatalog{SchemaVersion: 1, ModelsDevAPI: o.apiURL, Models: []integrationModel{}}
	for _, m := range listing.Data {
		known, ok := modelsdev.LookupProviderModel(o.apiURL, m.ID)
		api := integrationAPI(known.NPM)
		if !ok || !known.Chat || api == "" {
			continue
		}
		// OpenAI models match Shelley's own catalog entries for them, which
		// carry tuning such as apply_patch support.
		provider := "opencode"
		if api == "openai_responses" {
			provider = "openai"
		}
		model := integrationModel{ID: m.ID, Provider: provider, APIs: []string{api}}
		model.Architecture.InputModalities = []string{"text"}
		if known.Images {
			model.Architecture.InputModalities = append(model.Architecture.InputModalities, "image")
		}
		catalog.Models = append(catalog.Models, model)
	}
	writeJSON(w, catalog)
}

func (o *opencode) forward(w http.ResponseWriter, r *http.Request, operation string) {
	key, err := o.key()
	if err != nil {
		writeError(w, err)
		return
	}
	forward(w, r, o.httpc, o.apiURL+operation, func(h http.Header) {
		// OpenCode keeps a conversation on one upstream by this header.
		if id := h.Get("Shelley-Conversation-Id"); id != "" {
			h.Set("x-opencode-session", id)
		}
		h.Del("Authorization")
		h.Del("X-Api-Key")
		if strings.HasSuffix(operation, "/messages") {
			h.Set("X-Api-Key", key)
		} else {
			h.Set("Authorization", "Bearer "+key)
		}
	})
}
