package router

import (
	"testing"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
)

func boolPtr(v bool) *bool {
	return &v
}

func TestSelectorSelect(t *testing.T) {
	t.Parallel()

	providers := []config.ProviderConfig{
		{
			Name:       "p1",
			BaseURL:    "https://p1.example.com",
			Enabled:    boolPtr(true),
			Weight:     1,
			ModelRegex: "^claude-3",
		},
		{
			Name:          "p2",
			BaseURL:       "https://p2.example.com",
			Enabled:       boolPtr(true),
			Weight:        1,
			ModelPrefixes: []string{"claude-4"},
		},
	}

	selector, err := NewSelector(config.RouterConfig{
		Strategy:        "round_robin",
		DefaultProvider: "p1",
		RouteRules: []config.RouteRule{
			{
				PathPrefix: "/v1/messages",
				ModelRegex: "^claude-4",
				Providers:  []string{"p2"},
			},
		},
	}, providers)
	if err != nil {
		t.Fatalf("new selector: %v", err)
	}

	testCases := []struct {
		name     string
		input    SelectionInput
		wantName string
		wantErr  bool
	}{
		{
			name: "forced provider wins",
			input: SelectionInput{
				ForcedProvider: "p1",
				Model:          "claude-4-sonnet",
				Path:           "/v1/messages",
			},
			wantName: "p1",
		},
		{
			name: "rule by model regex",
			input: SelectionInput{
				Model: "claude-4-sonnet",
				Path:  "/v1/messages",
			},
			wantName: "p2",
		},
		{
			name: "provider model hint",
			input: SelectionInput{
				Model: "claude-3-opus",
				Path:  "/v1/messages",
			},
			wantName: "p1",
		},
		{
			name: "fallback default provider",
			input: SelectionInput{
				Model: "unknown-model",
				Path:  "/v1/messages",
			},
			wantName: "p1",
		},
		{
			name: "forced provider not found",
			input: SelectionInput{
				ForcedProvider: "missing",
			},
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := selector.Select(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("select: %v", err)
			}
			if got.Name != tc.wantName {
				t.Fatalf("got provider %q want %q", got.Name, tc.wantName)
			}
		})
	}
}
