package openai

import (
	"strconv"
	"strings"

	"github.com/jahrulnr/fakeagents/internal/engine"
)

func (h *Handler) openRouterCosts(model string, usage engine.Usage) (float64, float64) {
	var pricing map[string]string
	for _, entry := range h.cfg.Models {
		if strings.EqualFold(entry.ID, model) {
			pricing = entry.Pricing
			break
		}
		for _, alias := range entry.Aliases {
			if strings.EqualFold(alias, model) {
				pricing = entry.Pricing
				break
			}
		}
		if pricing != nil {
			break
		}
	}
	price := func(name string) float64 {
		value, _ := strconv.ParseFloat(pricing[name], 64)
		return value
	}
	input := float64(usage.InputTokens)*price("prompt") +
		float64(usage.CachedTokens)*price("input_cache_read") +
		float64(usage.CacheWriteTokens)*price("input_cache_write")
	output := float64(usage.OutputTokens) * price("completion")
	return input, output
}

func (h *Handler) openRouterChatUsage(model string, usage engine.Usage) map[string]any {
	input, output := h.openRouterCosts(model, usage)
	prompt := usage.InputTokens + usage.CachedTokens + usage.CacheWriteTokens
	return map[string]any{
		"prompt_tokens": prompt, "completion_tokens": usage.OutputTokens, "total_tokens": prompt + usage.OutputTokens,
		"cost": input + output, "is_byok": false,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": usage.CachedTokens, "cache_write_tokens": usage.CacheWriteTokens, "audio_tokens": 0, "video_tokens": 0,
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": usage.ReasoningTokens, "image_tokens": 0, "audio_tokens": 0,
		},
		"cost_details": map[string]any{
			"upstream_inference_cost": input + output, "upstream_inference_prompt_cost": input,
			"upstream_inference_completions_cost": output,
		},
	}
}

func (h *Handler) openRouterResponsesUsage(model string, usage engine.Usage) map[string]any {
	input, output := h.openRouterCosts(model, usage)
	result := buildResponsesUsage(usage)
	if usage.CacheWriteTokens > 0 {
		result["input_tokens_details"].(map[string]any)["cache_write_tokens"] = usage.CacheWriteTokens
	}
	result["cost"] = input + output
	result["is_byok"] = false
	result["cost_details"] = map[string]any{
		"upstream_inference_cost": input + output, "upstream_inference_input_cost": input, "upstream_inference_output_cost": output,
	}
	return result
}
