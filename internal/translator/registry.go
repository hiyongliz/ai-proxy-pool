package translator

import (
	"fmt"
	"strings"
	"sync"
)

type pairKey struct {
	from string
	to   string
}

var (
	regMu    sync.RWMutex
	registry = map[pairKey]RequestTranslator{}
)

func normalizeAPI(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

// Register registers a translator function for one API pair.
func Register(from, to string, fn RequestTranslator) {
	if fn == nil {
		panic("translator register: nil translator")
	}
	key := pairKey{
		from: normalizeAPI(from),
		to:   normalizeAPI(to),
	}
	if key.from == "" || key.to == "" {
		panic("translator register: empty from/to")
	}

	regMu.Lock()
	defer regMu.Unlock()
	registry[key] = fn
}

// Translate runs the translator for a pair if it exists.
func Translate(from, to string, req TranslateRequest) (TranslateResult, bool, error) {
	key := pairKey{
		from: normalizeAPI(from),
		to:   normalizeAPI(to),
	}

	regMu.RLock()
	fn, ok := registry[key]
	regMu.RUnlock()
	if !ok {
		return TranslateResult{}, false, nil
	}

	out, err := fn(req)
	if err != nil {
		return TranslateResult{}, true, fmt.Errorf("translate %s->%s: %w", key.from, key.to, err)
	}
	return out, true, nil
}
