package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Model-list endpoints. A token with an allow-list only sees its models.
var modelListPaths = map[string]string{
	"/v1/models":                  "data",
	"/v1/language-models":         "models",
	"/v1/image-generation-models": "models",
	"/v1/video-generation-models": "models",
	"/v1/embedding-models":        "models",
}

// modelDetailID returns the model id of GET /v1/<list>/{id}, or "".
func modelDetailID(method, path string) string {
	if method != http.MethodGet {
		return ""
	}
	for list := range modelListPaths {
		if id, ok := strings.CutPrefix(path, list+"/"); ok && id != "" && !strings.Contains(id, "/") {
			return id
		}
	}
	return ""
}

// filterModelList removes models the token may not use from a model-list
// response. Other responses are returned unchanged.
func (st *state) filterModelList(r *http.Request, body []byte) []byte {
	field, ok := modelListPaths[r.URL.Path]
	if !ok || r.Method != http.MethodGet || st.token == nil || len(st.token.AllowedModels) == 0 {
		return body
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return body
	}
	var items []json.RawMessage
	if json.Unmarshal(doc[field], &items) != nil {
		return body
	}
	kept := make([]json.RawMessage, 0, len(items))
	for _, it := range items {
		var m struct {
			ID      string   `json:"id"`
			Aliases []string `json:"aliases"`
		}
		if json.Unmarshal(it, &m) != nil {
			continue
		}
		allowed := st.h.modelAllowed(st.token, m.ID)
		for _, a := range m.Aliases {
			allowed = allowed || st.h.modelAllowed(st.token, a)
		}
		if allowed {
			kept = append(kept, it)
		}
	}
	doc[field], _ = json.Marshal(kept)
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}
