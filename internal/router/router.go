package router

import (
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
)// SelectionInput describes the attributes used to select an upstream provider.
type SelectionInput struct {
	Path              string
	Model             string
	ForcedProvider    string
	ExcludedProviders []string
}

// Selector picks provider targets according to route rules and fallback policy.
type Selector struct {
	strategy         string
	defaultProvider  string
	providers        map[string]providerItem
	allProviderNames []string
	rules            []compiledRule
	roundRobinCount  atomic.Uint64
	randomMu         sync.Mutex
	random           *rand.Rand
}

type providerItem struct {
	cfg        config.ProviderConfig
	modelRegex *regexp.Regexp
}

type compiledRule struct {
	pathPrefix  string
	modelPrefix string
	modelRegex  *regexp.Regexp
	providers   []string
}

// NewSelector builds a selector from config.
func NewSelector(routerCfg config.RouterConfig, providers []config.ProviderConfig) (*Selector, error) {
	s := &Selector{
		strategy:         routerCfg.Strategy,
		defaultProvider:  routerCfg.DefaultProvider,
		providers:        map[string]providerItem{},
		allProviderNames: make([]string, 0, len(providers)),
		random:           rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec
	}

	for _, provider := range providers {
		if !provider.EnabledOrDefault() {
			continue
		}
		item := providerItem{cfg: provider}
		if provider.ModelRegex != "" {
			re, err := regexp.Compile(provider.ModelRegex)
			if err != nil {
				return nil, fmt.Errorf("compile provider %q model_regex: %w", provider.Name, err)
			}
			item.modelRegex = re
		}
		s.providers[provider.Name] = item
		s.allProviderNames = append(s.allProviderNames, provider.Name)
	}

	// 注入熔断器检查函数 (用于过滤不可用的节点)
	// 此处为解耦设计，我们不再直接在 router 里加锁读取 proxy.Stats
	// 这部分逻辑将主要放到 Select 时结合外部传进来的 excluded() 实现。


	if len(s.allProviderNames) == 0 {
		return nil, errors.New("selector has no enabled providers")
	}

	for _, rule := range routerCfg.RouteRules {
		compiled := compiledRule{
			pathPrefix:  rule.PathPrefix,
			modelPrefix: rule.ModelPrefix,
		}
		if rule.ModelRegex != "" {
			re, err := regexp.Compile(rule.ModelRegex)
			if err != nil {
				return nil, fmt.Errorf("compile route rule regex %q: %w", rule.ModelRegex, err)
			}
			compiled.modelRegex = re
		}

		for _, providerName := range rule.Providers {
			if _, ok := s.providers[providerName]; ok {
				compiled.providers = append(compiled.providers, providerName)
			}
		}
		if len(compiled.providers) > 0 {
			s.rules = append(s.rules, compiled)
		}
	}

	return s, nil
}

// Select returns a provider for the request.
func (s *Selector) Select(input SelectionInput) (config.ProviderConfig, error) {
	excluded := toSet(input.ExcludedProviders)

	if input.ForcedProvider != "" {
		if item, ok := s.providers[input.ForcedProvider]; ok {
			if _, isExcluded := excluded[input.ForcedProvider]; !isExcluded {
				return item.cfg, nil
			}
			return config.ProviderConfig{}, fmt.Errorf("forced provider %q is excluded", input.ForcedProvider)
		}
		return config.ProviderConfig{}, fmt.Errorf("forced provider %q is not available", input.ForcedProvider)
	}

	for _, rule := range s.rules {
		if ruleMatches(rule, input.Path, input.Model) {
			candidates := filterExcluded(rule.providers, excluded)
			if len(candidates) > 0 {
				return s.pick(candidates)
			}
		}
	}

	if input.Model != "" {
		modelCandidates := s.matchProvidersByModel(input.Model)
		candidates := filterExcluded(modelCandidates, excluded)
		if len(candidates) > 0 {
			return s.pick(candidates)
		}
	}

	if s.defaultProvider != "" {
		if _, isExcluded := excluded[s.defaultProvider]; !isExcluded {
			if item, ok := s.providers[s.defaultProvider]; ok {
				return item.cfg, nil
			}
		}
	}

	candidates := filterExcluded(s.allProviderNames, excluded)
	return s.pick(candidates)
}

func toSet(names []string) map[string]struct{} {
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return m
}

func filterExcluded(names []string, excluded map[string]struct{}) []string {
	if len(excluded) == 0 {
		return names
	}
	filtered := make([]string, 0, len(names))
	for _, n := range names {
		if _, skip := excluded[n]; !skip {
			filtered = append(filtered, n)
		}
	}
	return filtered
}

func (s *Selector) matchProvidersByModel(model string) []string {
	candidates := make([]string, 0)
	for _, providerName := range s.allProviderNames {
		item := s.providers[providerName]
		if matchesModel(item.cfg.ModelPrefixes, item.modelRegex, model) {
			candidates = append(candidates, providerName)
		}
	}
	return candidates
}

func ruleMatches(rule compiledRule, pathValue, model string) bool {
	if rule.pathPrefix != "" && !strings.HasPrefix(pathValue, rule.pathPrefix) {
		return false
	}

	if rule.modelPrefix != "" && !strings.HasPrefix(model, rule.modelPrefix) {
		return false
	}

	if rule.modelRegex != nil && !rule.modelRegex.MatchString(model) {
		return false
	}

	return true
}

func matchesModel(prefixes []string, re *regexp.Regexp, model string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return re != nil && re.MatchString(model)
}

func (s *Selector) pick(candidates []string) (config.ProviderConfig, error) {
	if len(candidates) == 0 {
		return config.ProviderConfig{}, errors.New("no provider candidates")
	}

	switch s.strategy {
	case config.StrategyWeightedRandom:
		return s.pickWeightedRandom(candidates)
	default:
		return s.pickRoundRobin(candidates), nil
	}
}

func (s *Selector) pickRoundRobin(candidates []string) config.ProviderConfig {
	idx := s.roundRobinCount.Add(1) - 1
	name := candidates[idx%uint64(len(candidates))]
	return s.providers[name].cfg
}

func (s *Selector) pickWeightedRandom(candidates []string) (config.ProviderConfig, error) {
	totalWeight := 0
	for _, providerName := range candidates {
		totalWeight += s.providers[providerName].cfg.Weight
	}
	if totalWeight <= 0 {
		return s.pickRoundRobin(candidates), nil
	}

	s.randomMu.Lock()
	choice := s.random.Intn(totalWeight)
	s.randomMu.Unlock()

	accWeight := 0
	for _, providerName := range candidates {
		accWeight += s.providers[providerName].cfg.Weight
		if choice < accWeight {
			return s.providers[providerName].cfg, nil
		}
	}

	return config.ProviderConfig{}, errors.New("weighted random selection failed")
}
