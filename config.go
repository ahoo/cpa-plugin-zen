package plugin

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// pluginConfig mirrors the plugins.configs.zen mapping the host hands us as
// raw YAML at register/reconfigure time.
type pluginConfig struct {
	Enabled  bool `yaml:"enabled"`
	Priority int  `yaml:"priority"`
	// SessionMapping controls client session forwarding to OpenCode Zen.
	SessionMapping sessionMappingConfig `yaml:"session_mapping"`
	// Paid carries the direct (paid-key) provider: SystemOne today, more
	// endpoint kinds in later phases. Free-tier pools arrive separately and
	// never share these keys.
	Paid paidConfig `yaml:"paid"`
	// Free carries anonymous free-tier models with per-endpoint cloak,
	// egress pool and quota fallback. Never uses paid keys.
	Free freeConfig `yaml:"free"`

	// Derived indexes (see buildIndexes). Not YAML fields.
	claimed   map[string]struct{}
	rewrites  map[string]string
	questions map[string]map[string]QuestionDef
	free      map[string]FreeModelEntry
}

// sessionMappingConfig toggles conversation session forwarding.
type sessionMappingConfig struct {
	// Enabled defaults to true when the stanza is present without it.
	Enabled *bool `yaml:"enabled"`
}

func (s sessionMappingConfig) effective() bool {
	if s.Enabled == nil {
		return true
	}
	return *s.Enabled
}

// paidConfig is the direct provider: own key pool, own base URL, own models.
type paidConfig struct {
	Models  []ModelEntry  `yaml:"models"`
	BaseURL string        `yaml:"base_url"`
	APIKey  string        `yaml:"api_key"`
	APIKeys []APIKeyEntry `yaml:"api_keys"`
}

// ModelEntry maps a client-facing alias to the upstream model name, with
// optional default questions used when the client sends plain chat text.
type ModelEntry struct {
	Alias       string                 `yaml:"alias"`
	Name        string                 `yaml:"name"`
	DisplayName string                 `yaml:"display_name"`
	Questions   map[string]QuestionDef `yaml:"questions"`
}

// UnmarshalYAML accepts the structured mapping and the bare string form.
func (m *ModelEntry) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		m.Alias = strings.TrimSpace(node.Value)
		return nil
	}
	type plain ModelEntry
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*m = ModelEntry(decoded)
	return nil
}

// label resolves the human-readable label for model registration.
func (m ModelEntry) label() string {
	if label := strings.TrimSpace(m.DisplayName); label != "" {
		return label
	}
	if name := strings.TrimSpace(m.Name); name != "" {
		return name
	}
	return strings.TrimSpace(m.Alias)
}

// QuestionDef is one typed SystemOne question: noul (yes/no), choice
// (multiple-choice with a criteria mapping) or score (rubric with a
// criteria list).
type QuestionDef struct {
	Type         string   `yaml:"type" json:"type"`
	Instructions string   `yaml:"instructions" json:"instructions"`
	Criteria     Criteria `yaml:"criteria" json:"criteria,omitempty"`
}

// Criteria accepts a mapping (choice) or a sequence (score).
type Criteria struct {
	Pairs map[string]string
	List  []string
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (c *Criteria) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind == yaml.ScalarNode && strings.TrimSpace(node.Value) == "" {
		return nil
	}
	if node.Kind == yaml.SequenceNode {
		var list []string
		if err := node.Decode(&list); err != nil {
			return err
		}
		c.List = list
		return nil
	}
	var m map[string]string
	if err := node.Decode(&m); err != nil {
		return err
	}
	c.Pairs = m
	return nil
}

// APIKeyEntry is one pool member: key + weight + optional per-key proxy.
type APIKeyEntry struct {
	Key      string `yaml:"key"`
	Weight   int    `yaml:"weight"`
	ProxyURL string `yaml:"proxy_url"`
}

// freeConfig is the free-tier pool: mirrors the opencode provider entry
// shape ({key, proxy_url} pairs) so the established key/proxy mapping is
// reused instead of maintained twice.
type freeConfig struct {
	Enabled     bool             `yaml:"enabled"`
	Models      []FreeModelEntry `yaml:"models"`
	Cloak       cloakConfig      `yaml:"cloak"`
	Members     []FreeMember     `yaml:"members"`
	Quota       quotaConfig      `yaml:"quota"`
	SessionPool string           `yaml:"session_pool"`
}

// FreeMember is one free identity×egress pair, mirroring an
// openai-compatibility api-key-entry. Key "public" (or empty) selects the
// anonymous tier; anything else is a logged-in free identity.
type FreeMember struct {
	Key      string `yaml:"key"`
	ProxyURL string `yaml:"proxy_url"`
	Weight   int    `yaml:"weight"`
	Disabled bool   `yaml:"disabled"`
}

func (en FreeMember) normWeight() int {
	if en.Weight <= 0 {
		return 1
	}
	return en.Weight
}

func (en FreeMember) authKey() string {
	if strings.TrimSpace(en.Key) == "" || strings.EqualFold(strings.TrimSpace(en.Key), "public") {
		return ""
	}
	return strings.TrimSpace(en.Key)
}

// FreeModelEntry maps a client alias to an upstream free model + endpoint.
type FreeModelEntry struct {
	Alias       string                 `yaml:"alias"`
	Name        string                 `yaml:"name"`
	Endpoint    string                 `yaml:"endpoint"` // chat|responses|systemone
	DisplayName string                 `yaml:"display_name"`
	Questions   map[string]QuestionDef `yaml:"questions"`
	// MinOutputTokens floors the output budget (thinking models burn budget
	// on reasoning first; too small yields empty streams). Default 1024 for
	// responses, 0 (no floor) otherwise.
	MinOutputTokens int `yaml:"min_output_tokens"`
}

// cloakConfig is the agent-shape disguise for gated chat/responses models.
type cloakConfig struct {
	Stream   bool             `yaml:"stream"`
	MinTools int              `yaml:"min_tools"`
	Tools    []map[string]any `yaml:"tools"`
}

// quotaConfig governs free-pool cooldown and paid fallback.
type quotaConfig struct {
	Cooldown     int    `yaml:"cooldown"` // seconds a failed member/session cools down
	FailoverPaid bool   `yaml:"failover_paid"`
	PaidFallback string `yaml:"paid_fallback"` // paid model alias used when free is exhausted
	// FallbackMinTokens floors the output budget on fallback calls: tiny
	// budgets burn out on thinking models and return empty. Default 512.
	FallbackMinTokens int `yaml:"fallback_min_tokens"`
}

func (c *pluginConfig) fallbackFloor() int {
	if c != nil && c.Free.Quota.FallbackMinTokens > 0 {
		return c.Free.Quota.FallbackMinTokens
	}
	return 512
}

func (c *pluginConfig) cooldown() int {
	if c != nil && c.Free.Quota.Cooldown > 0 {
		return c.Free.Quota.Cooldown
	}
	return 300
}

func (en APIKeyEntry) normWeight() int {
	if en.Weight <= 0 {
		return 1
	}
	return en.Weight
}

// defaultModelEntries are the built-in alias -> upstream mapping.
func defaultModelEntries() []ModelEntry {
	return []ModelEntry{
		{Alias: "jev", Name: "jev-1.13"},
	}
}

// defaultFreeModels are the built-in free aliases. jev-free moved here from
// paid: free-first with paid fallback beats paid-only.
func defaultFreeModels() []FreeModelEntry {
	return []FreeModelEntry{
		{Alias: "mimo-free", Name: "mimo-v2.6-flash-free", Endpoint: "chat"},
		{Alias: "ling-free", Name: "ling-3.0-flash-fin-free", Endpoint: "chat"},
		{Alias: "nemotron-free", Name: "nemotron-3-ultra-free", Endpoint: "chat"},
		{Alias: "muse-free", Name: "muse-spark-1.3-contributor-free", Endpoint: "responses"},
		{Alias: "jev-free", Name: "jev-1.13-free", Endpoint: "systemone"},
	}
}

// effectiveFreeModels returns configured free entries or built-in defaults.
func (c *pluginConfig) effectiveFreeModels() []FreeModelEntry {
	if c != nil && len(c.Free.Models) > 0 {
		return c.Free.Models
	}
	return defaultFreeModels()
}

func parseConfig(raw []byte) *pluginConfig {
	cfg, _ := parseConfigStrict(raw)
	return cfg
}

// parseConfigStrict parses the stanza and reports errors. A non-empty
// payload that fails to parse must NEVER degrade silently to defaults:
// the host is known to emit dangling YAML aliases (*id001) when an
// identical subtree exists in another (e.g. disabled) stanza, which would
// otherwise wipe the key pool without a trace.
func parseConfigStrict(raw []byte) (*pluginConfig, error) {
	cfg := &pluginConfig{}
	if len(raw) == 0 {
		cfg.buildIndexes()
		return cfg, nil
	}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, err
	}
	cfg.buildIndexes()
	return cfg, nil
}

// effectiveModels returns the configured entries, or the built-in defaults
// when configuration declares none.
func (c *pluginConfig) effectiveModels() []ModelEntry {
	if c != nil && len(c.Paid.Models) > 0 {
		return c.Paid.Models
	}
	return defaultModelEntries()
}

// buildIndexes derives the lookup tables once per configuration.
func (c *pluginConfig) buildIndexes() {
	entries := c.effectiveModels()
	c.claimed = make(map[string]struct{}, len(entries)*2)
	c.rewrites = make(map[string]string, len(entries)*2)
	c.questions = make(map[string]map[string]QuestionDef, len(entries)*2)
	for _, entry := range entries {
		alias := normalizeModel(entry.Alias)
		name := strings.TrimSpace(entry.Name)
		if alias != "" {
			c.claimed[alias] = struct{}{}
		}
		if name == "" {
			continue
		}
		normalizedName := normalizeModel(name)
		if normalizedName != "" {
			c.claimed[normalizedName] = struct{}{}
		}
		if alias != "" {
			c.rewrites[alias] = name
		}
		if normalizedName != "" {
			c.rewrites[normalizedName] = name
		}
		if len(entry.Questions) > 0 {
			if alias != "" {
				c.questions[alias] = entry.Questions
			}
			if normalizedName != "" {
				c.questions[normalizedName] = entry.Questions
			}
		}
	}
	// Free claims take precedence and additionally resolve endpoint routing.
	// Unclaimed when free is disabled so aliases fall through to native
	// channels instead of being hijacked with nowhere to go.
	c.free = make(map[string]FreeModelEntry, len(entries)*2)
	if !c.Free.Enabled {
		return
	}
	for _, entry := range c.effectiveFreeModels() {
		alias := normalizeModel(entry.Alias)
		name := strings.TrimSpace(entry.Name)
		if alias != "" {
			c.claimed[alias] = struct{}{}
			c.free[alias] = entry
		}
		normalizedName := normalizeModel(name)
		if normalizedName != "" {
			c.claimed[normalizedName] = struct{}{}
			c.free[normalizedName] = entry
		}
		if len(entry.Questions) > 0 {
			if alias != "" {
				c.questions[alias] = entry.Questions
			}
			if normalizedName != "" {
				c.questions[normalizedName] = entry.Questions
			}
		}
		// Free aliases never rewrite: the executor maps them itself.
	}
}

// ensureIndexes builds the lookup tables if missing (zero-value configs in
// tests). Must not run concurrently with request handling.
func (c *pluginConfig) ensureIndexes() {
	if c != nil && c.claimed == nil {
		c.buildIndexes()
	}
}

// modelSet returns the normalized names this plugin claims.
func (c *pluginConfig) modelSet() map[string]struct{} {
	if c == nil || c.claimed == nil {
		return map[string]struct{}{}
	}
	return c.claimed
}

// upstreamName returns the vendor's model name for a client-requested model.
func (c *pluginConfig) upstreamName(model string) string {
	if c == nil || len(c.rewrites) == 0 {
		return ""
	}
	return c.rewrites[normalizeModel(model)]
}

// questionsFor returns the configured default questions for a
// client-requested model, or nil when the client must send an envelope.
func (c *pluginConfig) questionsFor(model string) map[string]QuestionDef {
	if c == nil || len(c.questions) == 0 {
		return nil
	}
	return c.questions[normalizeModel(model)]
}

// freeEntry returns the free model entry for a client-requested model, or
// nil when the model is not a free model (paid path applies).
func (c *pluginConfig) freeEntry(model string) *FreeModelEntry {
	if c == nil || !c.Free.Enabled {
		return nil
	}
	c.ensureIndexes()
	if entry, ok := c.free[normalizeModel(model)]; ok {
		return &entry
	}
	return nil
}

func (c *pluginConfig) baseURL() string {
	if c != nil && c.Paid.BaseURL != "" {
		return c.Paid.BaseURL
	}
	return upstreamBaseURL
}
