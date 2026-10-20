package models

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"shelley.exe.dev/db"
	"shelley.exe.dev/db/generated"
	"shelley.exe.dev/llm/ant"
	"shelley.exe.dev/llm/oai"
	"shelley.exe.dev/models/modelsdev"
)

// ProviderTypeOpenCode is an OpenCode Zen or Go account, which serves each
// model over the wire protocol models.dev records for it.
const ProviderTypeOpenCode = "opencode"

// openCodeProviderType maps a model's AI SDK package to the provider type that
// speaks its protocol, or "" when Shelley has no matching client.
func openCodeProviderType(npm string) string {
	switch npm {
	case "@ai-sdk/anthropic":
		return "anthropic"
	case "@ai-sdk/openai":
		return "openai-responses"
	case "@ai-sdk/openai-compatible":
		return "openai"
	}
	return ""
}

func ProviderModelEligible(providerType, endpoint, upstreamID string) bool {
	if providerType == ProviderTypeOpenCode {
		known, ok := modelsdev.LookupProviderModel(endpoint, upstreamID)
		return ok && known.Chat && openCodeProviderType(known.NPM) != ""
	}
	if known, ok := modelsdev.LookupProviderModel(endpoint, upstreamID); ok {
		return known.Chat
	}
	for _, model := range All() {
		if model.ID == upstreamID || model.APIModelName == upstreamID {
			return true
		}
	}
	return false
}

func ProviderRouteID(providerID, upstreamID string) string {
	return "provider:" + base64.RawURLEncoding.EncodeToString([]byte(providerID)) + ":" + base64.RawURLEncoding.EncodeToString([]byte(upstreamID))
}

func ParseProviderRouteID(id string) (providerID, upstreamID string, ok bool) {
	parts := strings.Split(id, ":")
	if len(parts) != 3 || parts[0] != "provider" {
		return "", "", false
	}
	provider, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(provider) == 0 {
		return "", "", false
	}
	upstream, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(upstream) == 0 {
		return "", "", false
	}
	return string(provider), string(upstream), true
}

// RefreshProviders loads the persisted snapshots; it never contacts a provider.
func (m *Manager) RefreshProviders() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.db == nil {
		return errors.New("provider storage is unavailable")
	}
	rows, err := m.db.GetModelProviders(context.Background())
	if err != nil {
		return err
	}
	fresh := make(map[string]serviceEntry)
	var order []string
	for _, row := range rows {
		for _, model := range row.Models {
			entry, err := m.providerEntry(row, model.ID)
			if err != nil {
				return err
			}
			fresh[entry.modelID] = entry
			order = append(order, entry.modelID)
		}
	}
	previous := append([]string(nil), m.modelOrder...)
	kept := m.modelOrder[:0]
	for _, id := range m.modelOrder {
		if m.services[id].sourceID != "" {
			delete(m.services, id)
		} else {
			kept = append(kept, id)
		}
	}
	m.modelOrder = append(kept, order...)
	for id, entry := range fresh {
		m.services[id] = entry
	}
	m.providersEnabled = true
	m.preserveProviderOrder(previous)
	return nil
}

func (m *Manager) preserveProviderOrder(previous []string) {
	remaining := make(map[string]bool, len(m.modelOrder))
	for _, id := range m.modelOrder {
		remaining[id] = true
	}
	order := make([]string, 0, len(m.modelOrder))
	for _, id := range previous {
		if remaining[id] {
			order = append(order, id)
			delete(remaining, id)
		}
	}
	for _, id := range m.modelOrder {
		if remaining[id] {
			order = append(order, id)
			delete(remaining, id)
		}
	}
	m.modelOrder = order
}

// Listed routes use the registry; unlisted routes still resolve their saved account.
func (m *Manager) lookupService(id string) (serviceEntry, error) {
	m.mu.RLock()
	entry, ok := m.services[id]
	m.mu.RUnlock()
	if ok {
		return entry, nil
	}
	return m.resolveProviderService(id)
}

func (m *Manager) resolveProviderService(id string) (serviceEntry, error) {
	m.mu.RLock()
	enabled := m.providersEnabled
	m.mu.RUnlock()
	source, wire, valid := ParseProviderRouteID(id)
	if !valid || !enabled || m.db == nil {
		return serviceEntry{}, fmt.Errorf("unsupported model: %s", id)
	}
	row, err := m.db.GetModelProvider(context.Background(), source)
	if errors.Is(err, sql.ErrNoRows) {
		return serviceEntry{}, fmt.Errorf("model %s belongs to a removed provider connection", id)
	}
	if err != nil {
		return serviceEntry{}, err
	}
	return m.providerEntry(*row, wire)
}

func (m *Manager) providerEntry(row db.ModelProvider, wire string) (serviceEntry, error) {
	model := generated.Model{
		ModelID: ProviderRouteID(row.ProviderID, wire), ModelName: wire,
		DisplayName: wire, ProviderType: row.ProviderType, Endpoint: row.Endpoint, ApiKey: row.APIKey,
		MaxTokens: 8192, ImageSupport: "no", ReasoningSupport: "no",
	}
	for _, item := range row.Models {
		if item.ID == wire && item.Name != "" {
			model.DisplayName = item.Name
			break
		}
	}
	known, exact := modelsdev.LookupProviderModel(row.Endpoint, wire)
	if exact {
		if known.Images {
			model.ImageSupport = "yes"
		}
		if known.OutputLimit > 0 {
			model.MaxTokens = int64(known.OutputLimit)
		}
		if known.Reasoning.Supported && len(known.Reasoning.Levels) > 0 {
			model.ReasoningSupport = "yes"
		}
	}
	if row.ProviderType == ProviderTypeOpenCode {
		if model.ProviderType = openCodeProviderType(known.NPM); !exact || model.ProviderType == "" {
			return serviceEntry{}, fmt.Errorf("OpenCode model %s has no supported protocol", wire)
		}
	}
	var apiType APIType
	switch model.ProviderType {
	case "anthropic":
		model.Endpoint = strings.TrimRight(row.Endpoint, "/") + "/messages"
		apiType = APITypeAnthropicMessages
	case "openai":
		apiType = APITypeOpenAIChat
	case "openai-responses":
		apiType = APITypeOpenAIResponses
	default:
		return serviceEntry{}, fmt.Errorf("unsupported provider type: %s", model.ProviderType)
	}
	service := m.createServiceFromModel(&model).(*reasoningService)
	service.disableWebSearch = true
	// Advertise levels without imposing a custom-model override map.
	service.levels = known.Reasoning.Levels
	disabled := model.ReasoningSupport == "no"
	switch raw := service.Service.(type) {
	case *ant.Service:
		raw.DisableReasoning = disabled
	case *oai.Service:
		raw.DisableReasoning = disabled
	case *oai.ResponsesService:
		raw.DisableReasoning = disabled
		if catalog := ByID(wire); exact && catalog != nil && catalog.APIType == APITypeOpenAIResponses {
			if base, ok := catalog.Build("", "", m.httpc).(*oai.ResponsesService); ok {
				raw.Model.TextVerbosity, raw.Model.SupportsApplyPatch = base.Model.TextVerbosity, base.Model.SupportsApplyPatch
			}
		}
	}

	label := row.DisplayName
	if label == "" {
		label = row.Endpoint
	}
	provider := ProviderOpenAI
	if model.ProviderType == "anthropic" {
		provider = ProviderAnthropic
	}
	return serviceEntry{
		service: service, provider: provider, modelID: model.ModelID, sourceID: row.ProviderID,
		serviceRevision: ConnectionRevision(row.ProviderType, row.Endpoint, row.APIKey, wire), source: label, displayName: model.DisplayName,
		baseURL: row.Endpoint, apiType: apiType, apiModelName: wire, releaseDate: known.ReleaseDate,
	}, nil
}
