package proxy

import (
	"bytes"
	"strings"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/store"
)

// Defaults used to bound the cost of a request before it is sent, so a token
// quota can be reserved up front (see package quota).
const (
	defaultOutputTokens = 4096 // when the request sets no max_tokens
	reasoningAllowance  = 4096 // reasoning tokens are not bounded by max_tokens
	maxVideoSeconds     = 15
	defaultVideoSeconds = 8
	maxImagesPerRequest = 10
)

// estimate returns an upper-bound cost for the request in ticks. It errs on
// the high side: the difference is released when the request finishes.
// Requests whose cost cannot be predicted (unknown model, free endpoints)
// reserve nothing.
func (st *state) estimate(method, path string) int64 {
	if method != "POST" {
		return 0
	}
	switch path {
	case "/v1/tts":
		return st.info.TextChars * billing.TTSTicksPerChar
	case "/v1/stt":
		return billing.STTRestTicksPerHour // assume up to one hour of audio
	}
	m := st.h.Prices.Lookup(st.info.Model)
	if m == nil {
		return 0
	}
	switch {
	case path == "/v1/images/generations" || path == "/v1/images/edits":
		n := min(max(st.info.N, 1), maxImagesPerRequest)
		price := m.ImageTicks
		for _, t := range m.ImageTiers {
			price = max(price, t.Ticks)
		}
		return n*price + st.info.InputImages*m.InputImageTicks
	case strings.HasPrefix(path, "/v1/videos/"):
		secs := st.info.Duration
		if secs <= 0 || path != "/v1/videos/generations" {
			// Edits keep the input's length and extensions add to it; neither
			// sets a duration, so assume the longest.
			secs = defaultVideoSeconds
			if path != "/v1/videos/generations" {
				secs = maxVideoSeconds
			}
		}
		secs = min(secs, maxVideoSeconds)
		// Reserve at the requested resolution's price (the highest when the
		// resolution is not one of the model's tiers).
		price := billing.VideoPrice(m, st.info.Resolution)
		if !billing.HasVideoTier(m, st.info.Resolution) {
			price = billing.MaxVideoPrice(m)
		}
		cost := secs*price + st.info.InputImages*m.InputImageTicks
		if path == "/v1/videos/edits" {
			cost += secs * m.InputVideoTicksPerSec
		}
		return cost
	case m.Kind == "language":
		return languageEstimate(m, int64(len(st.reqBody)), st.info.MaxTokens, st.info.ServiceTier) +
			toolAllowance(st.reqBody)
	}
	return 0
}

func languageEstimate(m *store.Model, bodyBytes, maxTokens int64, tier string) int64 {
	// A token is at least one byte of UTF-8 text in practice; /2 keeps a wide
	// margin over typical 3-4 bytes per token while not reserving absurd
	// amounts for large prompts.
	in := bodyBytes/2 + 16
	out := maxTokens
	if out <= 0 {
		out = defaultOutputTokens
	}
	if !strings.Contains(m.ID, "non-reasoning") {
		out += reasoningAllowance
	}
	inP, outP := max(m.InputTicks, m.ImageInputTicks), m.OutputTicks
	if m.LongThreshold > 0 && in >= m.LongThreshold {
		inP, outP = max(m.LongInputTicks, inP), max(m.LongOutputTicks, outP)
	}
	cost := in*inP + out*outP
	if tier == "priority" {
		cost *= 2
	}
	return cost
}

// toolAllowance covers server-side tool calls (web/X search, code
// execution, collections) that an agentic request may trigger.
func toolAllowance(body []byte) int64 {
	for _, t := range []string{`"web_search"`, `"x_search"`, `"code_execution"`, `"code_interpreter"`,
		`"collections_search"`, `"file_search"`, `"attachment_search"`} {
		if bytes.Contains(body, []byte(t)) {
			return 20 * 100_000_000 // 20 calls at the highest per-call rate ($0.01)
		}
	}
	return 0
}
