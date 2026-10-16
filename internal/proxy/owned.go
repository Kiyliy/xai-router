package proxy

import (
	"context"
	"encoding/json"
	"net/http"
)

// Team-scoped resources (files, batches, collections) are shared by every
// key of an xAI team. The gateway records which client token created each
// one and, on list endpoints, only shows a token its own resources.
var ownedListPaths = map[string]string{
	"/v1/files":       "data",
	"/v1/batches":     "batches",
	"/v1/collections": "collections",
}

// resourceID returns the id of a resource object, whatever xAI calls it.
func resourceID(raw json.RawMessage) string {
	var v struct {
		ID           string `json:"id"`
		RequestID    string `json:"request_id"`
		FileID       string `json:"file_id"`
		BatchID      string `json:"batch_id"`
		CollectionID string `json:"collection_id"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return firstNonEmpty(v.ID, v.BatchID, v.CollectionID, v.FileID, v.RequestID)
}

// filterOwnedList removes resources created by other tokens (or outside
// the gateway) from file/batch/collection list responses.
func (st *state) filterOwnedList(ctx context.Context, r *http.Request, body []byte) []byte {
	if _, ok := ownedListPaths[r.URL.Path]; !ok || r.Method != http.MethodGet || st.token == nil {
		return body
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return []byte(`{"error":{"message":"unexpected list response from upstream","type":"xai_router_error","code":"upstream_format"}}`)
	}
	// Filter every top-level array, whatever the upstream calls it, so a
	// changed field name cannot expose other tokens' resources.
	for field, raw := range doc {
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			continue
		}
		kept := make([]json.RawMessage, 0, len(items))
		for _, it := range items {
			id := resourceID(it)
			if id == "" {
				continue
			}
			if b, err := st.h.DB.GetBinding(ctx, id); err == nil && b.TokenID == st.token.ID {
				kept = append(kept, it)
			}
		}
		doc[field], _ = json.Marshal(kept)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}
