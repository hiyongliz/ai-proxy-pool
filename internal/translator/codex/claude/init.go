package claude

import "github.com/hiyongliz/ai-proxy-pool/internal/translator"

func init() {
	translator.Register(
		translator.APIClaude,
		translator.APICodex,
		ConvertClaudeRequestToCodex,
	)
}
