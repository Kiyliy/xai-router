package proxy

import "net/http"

// Inference endpoints that are billed per request. modelOptional marks the
// ones whose price does not depend on a "model" field.
var inferencePaths = map[string]struct{ modelOptional bool }{
	"/v1/chat/completions":   {},
	"/v1/responses":          {},
	"/v1/responses/compact":  {},
	"/v1/messages":           {},
	"/v1/completions":        {},
	"/v1/complete":           {},
	"/v1/embeddings":         {},
	"/v1/images/generations": {},
	"/v1/images/edits":       {},
	"/v1/videos/generations": {},
	"/v1/videos/extensions":  {},
	"/v1/videos/edits":       {},
	"/v1/tts":                {modelOptional: true},
	"/v1/stt":                {modelOptional: true},
}

// admit applies the token policy to a parsed request. Resource ownership is
// checked separately once bindings are loaded.
func (h *Handler) admit(st *state, r *http.Request) *apiError {
	tok, s := st.token, st.s
	ep, inference := inferencePaths[r.URL.Path]
	inference = inference && r.Method == http.MethodPost
	model := st.info.Model
	if st.info.Ambiguous {
		return errf(http.StatusBadRequest, "ambiguous_model", "the request's model could not be determined unambiguously (duplicate or malformed model field)")
	}
	if inference && !ep.modelOptional && model == "" && len(tok.AllowedModels) > 0 {
		// Fail closed: a restricted token must name its model.
		return errf(http.StatusBadRequest, "model_required", "this API key is restricted to specific models; the request must set \"model\"")
	}
	if model == "" {
		return nil
	}
	if !h.modelAllowed(tok, model) {
		return errf(http.StatusForbidden, "model_not_allowed", "this token is not allowed to use model %q", model)
	}
	m := h.Prices.Lookup(model)
	if m != nil && !m.Enabled {
		return errf(http.StatusForbidden, "model_disabled", "the model %q is disabled on this gateway", model)
	}
	if m == nil && inference && !ep.modelOptional && tok.QuotaTicks > 0 && s.UnpricedModels != "allow" {
		return errf(http.StatusForbidden, "model_not_priced",
			"the model %q has no known price, so API keys with a spending limit cannot use it (sync models or add a price)", model)
	}
	return nil
}

// ownsResource reports whether the token may access a bound resource.
// Resources created before ownership was recorded (token 0) stay shared.
func ownsResource(tokenID, owner int64) bool {
	return owner == 0 || owner == tokenID
}
