package provider

import "strings"

// ContextWindow guesses a model's context window in tokens from its name.
// It is a fallback for when the model registry has no entry; guesses err
// small so context management starts early rather than too late.
func ContextWindow(model string) int {
	m := strings.ToLower(model)
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	switch {
	case strings.Contains(m, "[1m]") || strings.Contains(m, "-1m"):
		return 1_000_000
	case strings.HasPrefix(m, "claude"):
		return 200_000
	case strings.HasPrefix(m, "gemini"):
		return 1_000_000
	case strings.HasPrefix(m, "gpt-4.1"):
		return 1_000_000
	case strings.HasPrefix(m, "gpt-5"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
		return 272_000
	case strings.HasPrefix(m, "gpt-4o"), strings.HasPrefix(m, "gpt-oss"):
		return 128_000
	case strings.HasPrefix(m, "grok"):
		return 256_000
	case strings.HasPrefix(m, "deepseek"), strings.HasPrefix(m, "kimi"), strings.HasPrefix(m, "qwen"), strings.HasPrefix(m, "glm"), strings.HasPrefix(m, "mistral"), strings.HasPrefix(m, "devstral"), strings.HasPrefix(m, "codestral"):
		return 128_000
	case strings.HasPrefix(m, "llama"):
		return 128_000
	}
	return 64_000
}
