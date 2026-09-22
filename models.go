package plugin

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ModelProvider contributes the Zen direct model list to the host registry.
// The ABI only offers static + per-auth discovery, so the list is derived
// from the same configuration that drives routing.
type ModelProvider struct {
	cfg *pluginConfig
}

func NewModelProvider(cfg *pluginConfig) *ModelProvider { return &ModelProvider{cfg: cfg} }

// modelDef is the registry-facing shape of one claimed model.
type modelDef struct {
	id          string
	displayName string
}

// registryModels builds the advertised list from configuration. IDs use the
// zen/ namespace so they never collide with native executors.
func (p *ModelProvider) registryModels() []modelDef {
	entries := p.cfg.effectiveModels()
	defs := make([]modelDef, 0, len(entries))
	for _, entry := range entries {
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			name = strings.TrimSpace(entry.Alias)
		}
		if name == "" {
			continue
		}
		defs = append(defs, modelDef{
			id:          Provider + "/" + name,
			displayName: entry.label() + " via Zen",
		})
	}
	return defs
}

func (p *ModelProvider) StaticModels(context.Context, pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return pluginapi.ModelResponse{Provider: Provider, Models: p.models()}, nil
}

func (p *ModelProvider) ModelsForAuth(context.Context, pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	return pluginapi.ModelResponse{Provider: Provider, Models: p.models()}, nil
}

func (p *ModelProvider) models() []pluginapi.ModelInfo {
	defs := p.registryModels()
	for _, entry := range p.cfg.effectiveFreeModels() {
		if p.cfg == nil || !p.cfg.Free.Enabled {
			break
		}
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			name = strings.TrimSpace(entry.Alias)
		}
		if name == "" {
			continue
		}
		label := entry.Alias
		if label == "" {
			label = name
		}
		if entry.DisplayName != "" {
			label = strings.TrimSpace(entry.DisplayName)
		}
		defs = append(defs, modelDef{
			id:          Provider + "/" + name,
			displayName: label + " via Zen",
		})
	}
	models := make([]pluginapi.ModelInfo, 0, len(defs))
	for _, def := range defs {
		models = append(models, pluginapi.ModelInfo{
			ID:                         def.id,
			Object:                     "model",
			OwnedBy:                    "zen",
			Type:                       "chat",
			DisplayName:                def.displayName,
			Name:                       def.id,
			Description:                def.displayName,
			SupportedGenerationMethods: []string{"chatCompletions"},
			SupportedInputModalities:   []string{"text"},
			SupportedOutputModalities:  []string{"text"},
			SupportedParameters:        []string{"max_tokens", "temperature", "top_p", "stop"},
		})
	}
	return models
}
