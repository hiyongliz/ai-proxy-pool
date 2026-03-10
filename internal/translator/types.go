package translator

// API kinds for request translation.
const (
	APIClaude = "claude"
	APICodex  = "codex"
)

// TranslateRequest carries context for translating request payloads.
type TranslateRequest struct {
	Model  string
	Path   string
	Body   []byte
	Stream bool
}

// TranslateResult carries translated payload and optional overrides.
type TranslateResult struct {
	Model string
	Path  string
	Body  []byte
}

// RequestTranslator translates request payload from one API dialect to another.
type RequestTranslator func(req TranslateRequest) (TranslateResult, error)
