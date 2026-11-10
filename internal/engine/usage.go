package engine

import (
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jahrulnr/fakeagents/internal/duration"
	"github.com/jahrulnr/fakeagents/internal/scenario"
)

func (e *Engine) computeUsage(req *Request, compiled *scenario.Compiled, reasoningTokens, textTokens, callTokens int) Usage {
	input := e.CountInputTokens(req)
	output := reasoningTokens + textTokens + callTokens
	usage := Usage{
		InputTokens:     input,
		OutputTokens:    output,
		ReasoningTokens: reasoningTokens,
	}
	var override *scenario.UsageOverride
	if compiled != nil {
		override = compiled.Spec.Respond.Usage
	}
	explicitCache := override != nil && (override.InputTokens != nil || override.CachedTokens != nil || override.CacheWriteTokens != nil)
	if e.cfg.Defaults.Usage.SimulateCacheEnabled() && req.CacheHint && !explicitCache && usage.InputTokens > 0 {
		cacheTokens := e.cfg.Defaults.Usage.CacheTokens
		if cacheTokens <= 0 {
			cacheTokens = 1024
		}
		if cacheTokens > usage.InputTokens {
			cacheTokens = usage.InputTokens
		}
		if req.Turn == 0 {
			usage.CacheWriteTokens = cacheTokens
		} else {
			usage.CachedTokens = cacheTokens
		}
		usage.InputTokens -= cacheTokens
	}

	settings := e.cfg.Defaults.Usage
	switch strings.ToLower(settings.Mode) {
	case "zero":
		usage = Usage{}
	case "fixed":
		usage = Usage{
			InputTokens:      settings.InputTokens,
			OutputTokens:     settings.OutputTokens,
			ReasoningTokens:  settings.ReasoningTokens,
			CachedTokens:     settings.CachedTokens,
			CacheWriteTokens: settings.CacheWriteTokens,
		}
	}

	if override != nil {
		if strings.EqualFold(override.Mode, "fixed") {
			usage = Usage{}
		}
		if override.InputTokens != nil {
			usage.InputTokens = *override.InputTokens
		}
		if override.OutputTokens != nil {
			usage.OutputTokens = *override.OutputTokens
		}
		if override.ReasoningTokens != nil {
			usage.ReasoningTokens = *override.ReasoningTokens
		}
		if override.CachedTokens != nil {
			usage.CachedTokens = *override.CachedTokens
		}
		if override.CacheWriteTokens != nil {
			usage.CacheWriteTokens = *override.CacheWriteTokens
		}
	}

	multiplier := settings.Multiplier
	if multiplier <= 0 {
		multiplier = 1
	}
	if override != nil && override.Multiplier != nil {
		multiplier = *override.Multiplier
	}
	if multiplier < 0 {
		multiplier = 0
	}
	if multiplier != 1 {
		usage.InputTokens = scale(usage.InputTokens, multiplier)
		usage.OutputTokens = scale(usage.OutputTokens, multiplier)
		usage.ReasoningTokens = scale(usage.ReasoningTokens, multiplier)
		usage.CachedTokens = scale(usage.CachedTokens, multiplier)
		usage.CacheWriteTokens = scale(usage.CacheWriteTokens, multiplier)
	}
	usage.InputTokens = maxInt(usage.InputTokens, 0)
	usage.OutputTokens = maxInt(usage.OutputTokens, 0)
	usage.ReasoningTokens = maxInt(usage.ReasoningTokens, 0)
	usage.CachedTokens = maxInt(usage.CachedTokens, 0)
	usage.CacheWriteTokens = maxInt(usage.CacheWriteTokens, 0)
	return usage
}

func scale(value int, multiplier float64) int {
	return int(math.Round(float64(value) * multiplier))
}

func (e *Engine) buildTiming(req *Request, compiled *scenario.Compiled, outputTokens int, rng *rand.Rand) Latency {
	settings := e.cfg.Defaults.Stream
	latency := Latency{ChunkSize: settings.ChunkSize, ChunkUnit: strings.ToLower(settings.ChunkUnit)}
	if latency.ChunkSize <= 0 {
		latency.ChunkSize = 24
	}
	if latency.ChunkUnit == "" {
		latency.ChunkUnit = "runes"
	}
	streamOverride := (*scenario.StreamOverride)(nil)
	if compiled != nil {
		streamOverride = compiled.Spec.Respond.Stream
	}
	if streamOverride != nil {
		if streamOverride.ChunkSize != nil {
			latency.ChunkSize = *streamOverride.ChunkSize
		}
		if streamOverride.ChunkUnit != "" {
			latency.ChunkUnit = strings.ToLower(streamOverride.ChunkUnit)
		}
	}
	if latency.ChunkSize <= 0 {
		latency.ChunkSize = 1
	}
	if !e.cfg.Defaults.Latency.IsEnabled() {
		return latency
	}

	ttft := e.cfg.Defaults.Latency.TimeToFirstToken
	tps := e.cfg.Defaults.Latency.TokensPerSecond
	perChunk := e.cfg.Defaults.Latency.PerChunk
	var explicitChunkDelay *duration.Range
	if e.cfg.Defaults.Stream.ChunkDelay.Min != 0 || e.cfg.Defaults.Stream.ChunkDelay.Max != 0 {
		configured := e.cfg.Defaults.Stream.ChunkDelay
		explicitChunkDelay = &configured
	}
	if compiled != nil && compiled.Spec.Respond.Latency != nil {
		override := compiled.Spec.Respond.Latency
		if override.TimeToFirstToken != nil {
			ttft = *override.TimeToFirstToken
		}
		if override.TokensPerSecond != nil {
			tps = *override.TokensPerSecond
		}
		if override.ChunkDelay != nil {
			explicitChunkDelay = override.ChunkDelay
		}
	}
	if streamOverride != nil && streamOverride.ChunkDelay != nil {
		explicitChunkDelay = streamOverride.ChunkDelay
	}

	first := ttft.Pick(rng)
	rate := tps.Pick(rng)
	latency.FirstToken = maxDuration(first, 0)

	chunkTokens := chunkTokenEstimate(latency.ChunkSize, latency.ChunkUnit)
	var chunk time.Duration
	if explicitChunkDelay != nil {
		chunk = explicitChunkDelay.Pick(rng)
	} else {
		if rate > 0 {
			chunk = time.Duration(float64(chunkTokens) / rate * float64(time.Second))
		}
		chunk += perChunk.Pick(rng)
	}
	latency.Chunk = maxDuration(chunk, 0)

	generation := time.Duration(0)
	if rate > 0 && outputTokens > 0 {
		generation = time.Duration(float64(outputTokens) / rate * float64(time.Second))
	}
	latency.Total = maxDuration(latency.FirstToken+generation, 0)
	return latency
}

func chunkTokenEstimate(size int, unit string) int {
	if size <= 0 {
		return 1
	}
	switch strings.ToLower(unit) {
	case "words":
		return maxInt(int(math.Ceil(float64(size)*1.3)), 1)
	default:
		return maxInt(int(math.Ceil(float64(size)/4.0)), 1)
	}
}

func (e *Engine) buildStreamPlan(req *Request, compiled *scenario.Compiled) StreamPlan {
	plan := StreamPlan{}
	if compiled == nil || compiled.Spec.Respond.Stream == nil {
		return plan
	}
	override := compiled.Spec.Respond.Stream
	if override.Error != nil {
		plan.Error = buildScenarioError(override.Error, req.Provider)
	}
	plan.ErrorAfterChunks = override.FailAfterChunks
	plan.AbortAfterChunks = override.AbortAfterChunks
	if override.OmitUsage != nil {
		plan.OmitUsage = *override.OmitUsage
	}
	if override.OmitDone != nil {
		plan.OmitDone = *override.OmitDone
	}
	if override.SendPings != nil {
		plan.SendPings = *override.SendPings
	}
	return plan
}

func maxDuration(value, floor time.Duration) time.Duration {
	if value < floor {
		return floor
	}
	return value
}

func maxInt(value, floor int) int {
	if value < floor {
		return floor
	}
	return value
}
