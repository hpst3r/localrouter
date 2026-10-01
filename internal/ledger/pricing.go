package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/hpst3r/localrouter/internal/core"
	"gopkg.in/yaml.v3"
)

// ModelPrice is the price of one model in USD per 1M tokens.
// CachedInput (cache reads) and CacheCreationInput (cache writes) nil mean
// those tokens are billed at the Input rate.
type ModelPrice struct {
	Input              float64  `yaml:"input"`
	CachedInput        *float64 `yaml:"cached_input,omitempty"`
	CacheCreationInput *float64 `yaml:"cache_creation_input,omitempty"`
	Output             float64  `yaml:"output"`
}

// Pricing is an immutable model price table. The zero value and nil are
// both valid empty tables.
type Pricing struct {
	models map[string]ModelPrice
	lower  map[string]ModelPrice
}

type pricingFile struct {
	Models map[string]ModelPrice `yaml:"models"`
}

// NewPricing builds a Pricing from a model→price map.
func NewPricing(models map[string]ModelPrice) *Pricing {
	p := &Pricing{models: make(map[string]ModelPrice, len(models)), lower: make(map[string]ModelPrice, len(models))}
	for k, v := range models {
		p.models[k] = v
	}
	for k, v := range models {
		lk := strings.ToLower(k)
		// Prefer an entry whose name is already lowercase on collision.
		if _, dup := p.lower[lk]; !dup || k == lk {
			p.lower[lk] = v
		}
	}
	return p
}

// LoadPricing reads a pricing YAML file. A missing file yields an empty table.
func LoadPricing(path string) (*Pricing, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return NewPricing(nil), nil
	}
	if err != nil {
		return nil, fmt.Errorf("pricing: %w", err)
	}
	var f pricingFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("pricing: parse %s: %w", path, err)
	}
	for name, mp := range f.Models {
		if mp.Input < 0 || mp.Output < 0 || (mp.CachedInput != nil && *mp.CachedInput < 0) ||
			(mp.CacheCreationInput != nil && *mp.CacheCreationInput < 0) {
			return nil, fmt.Errorf("pricing: model %q: negative price", name)
		}
	}
	return NewPricing(f.Models), nil
}

// Lookup returns the price for model by exact name, then case-insensitively.
func (p *Pricing) Lookup(model string) (ModelPrice, bool) {
	if p == nil {
		return ModelPrice{}, false
	}
	if mp, ok := p.models[model]; ok {
		return mp, true
	}
	mp, ok := p.lower[strings.ToLower(model)]
	return mp, ok
}

// Cost returns the USD cost of u for model, or ok=false if the model is not
// priced. Reasoning tokens are part of output and not billed separately.
// Cached and cache-creation tokens are subsets of InputTokens; they are
// clamped so the uncached remainder is never negative.
func (p *Pricing) Cost(model string, u core.Usage) (float64, bool) {
	mp, ok := p.Lookup(model)
	if !ok {
		return 0, false
	}
	cachedRate := mp.Input
	if mp.CachedInput != nil {
		cachedRate = *mp.CachedInput
	}
	creationRate := mp.Input
	if mp.CacheCreationInput != nil {
		creationRate = *mp.CacheCreationInput
	}
	input := max(u.InputTokens, 0)
	cached := min(max(u.CachedInputTokens, 0), input)
	creation := min(max(u.CacheCreationInputTokens, 0), input-cached)
	uncached := input - cached - creation
	return (float64(uncached)*mp.Input + float64(cached)*cachedRate +
		float64(creation)*creationRate + float64(max(u.OutputTokens, 0))*mp.Output) / 1e6, true
}

type litellmEntry struct {
	Input      *float64 `json:"input_cost_per_token"`
	Output     *float64 `json:"output_cost_per_token"`
	CacheRead  *float64 `json:"cache_read_input_token_cost"`
	CacheWrite *float64 `json:"cache_creation_input_token_cost"`
}

// ImportLiteLLM parses LiteLLM's model_prices_and_context_window.json and
// converts per-token prices to per-1M. Entries lacking input or output cost
// are skipped. Keys of the form "provider/model" are kept and additionally
// aliased as "model" unless that name is an original key or the alias is
// ambiguous (several providers with different prices).
func ImportLiteLLM(r io.Reader) (map[string]ModelPrice, error) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, fmt.Errorf("pricing: parse litellm json: %w", err)
	}
	out := make(map[string]ModelPrice)
	aliases := make(map[string]ModelPrice)
	ambiguous := make(map[string]bool)
	for key, msg := range raw {
		var e litellmEntry
		if err := json.Unmarshal(msg, &e); err != nil {
			continue // e.g. sample_spec with string values
		}
		if e.Input == nil || e.Output == nil {
			continue
		}
		mp := ModelPrice{Input: *e.Input * 1e6, Output: *e.Output * 1e6}
		if e.CacheRead != nil {
			c := *e.CacheRead * 1e6
			mp.CachedInput = &c
		}
		if e.CacheWrite != nil {
			c := *e.CacheWrite * 1e6
			mp.CacheCreationInput = &c
		}
		out[key] = mp
		if i := strings.Index(key, "/"); i > 0 && i < len(key)-1 {
			alias := key[i+1:]
			if prev, ok := aliases[alias]; ok && !samePrice(prev, mp) {
				ambiguous[alias] = true
			}
			aliases[alias] = mp
		}
	}
	for alias, mp := range aliases {
		if _, exists := out[alias]; exists || ambiguous[alias] {
			continue
		}
		out[alias] = mp
	}
	return out, nil
}

func samePrice(a, b ModelPrice) bool {
	return a.Input == b.Input && a.Output == b.Output &&
		sameOptPrice(a.CachedInput, b.CachedInput) &&
		sameOptPrice(a.CacheCreationInput, b.CacheCreationInput)
}

func sameOptPrice(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// WritePricing atomically writes models to path as pricing YAML (mode 0600,
// keys sorted).
func WritePricing(path string, models map[string]ModelPrice) error {
	if models == nil {
		models = map[string]ModelPrice{}
	}
	// yaml.v3 sorts map keys when encoding.
	b, err := yaml.Marshal(pricingFile{Models: models})
	if err != nil {
		return fmt.Errorf("pricing: encode: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("pricing: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".pricing-*.yaml")
	if err != nil {
		return fmt.Errorf("pricing: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("pricing: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("pricing: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("pricing: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pricing: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("pricing: %w", err)
	}
	return nil
}
