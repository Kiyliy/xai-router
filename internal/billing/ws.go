package billing

import "strings"

// Voice prices from docs.x.ai/developers/pricing (2026-10-16). The WebSocket
// APIs do not report cost_in_usd_ticks, so sessions are priced from these.
const (
	RealtimeTicksPerMinute = 800_000_000   // speech to speech: $0.08 / min
	RealtimeTicksPerText   = 40_000_000    // speech to speech: $0.004 / text input
	TTSTicksPerChar        = 150_000       // text to speech: $15.00 / 1M characters
	STTStreamTicksPerHour  = 2_000_000_000 // streaming speech to text: $0.20 / hour
)

// WSUsage is what the gateway measured during a WebSocket session.
type WSUsage struct {
	Path        string
	DurationMs  int64   // connection time
	TextInputs  int64   // realtime: client text items
	TTSChars    int64   // tts: characters sent in text.delta
	AudioSecond float64 // stt: audio duration reported by transcript.done
}

// WSCost prices a WebSocket session. It returns 0 for endpoints without a
// published per-session price.
func WSCost(u WSUsage) int64 {
	switch {
	case strings.HasPrefix(u.Path, "/v1/realtime"):
		return u.DurationMs*RealtimeTicksPerMinute/60_000 + u.TextInputs*RealtimeTicksPerText
	case u.Path == "/v1/tts":
		return u.TTSChars * TTSTicksPerChar
	case u.Path == "/v1/stt":
		secs := u.AudioSecond
		if secs <= 0 {
			secs = float64(u.DurationMs) / 1000
		}
		return int64(secs * STTStreamTicksPerHour / 3600)
	}
	return 0
}
