package proxy

import (
	"net/http"
	"strings"
)

// Kinds of upstream resources that must be accessed with the key that created
// them. xAI scopes deferred jobs, stored responses, files, batches and
// collections to the team that owns the key.
const (
	KindVideo    = "video"
	KindImage    = "image"
	KindDeferred = "deferred"
	KindResponse = "response"
	KindFile     = "file"
	KindBatch    = "batch"
	KindColl     = "collection"
)

// referencedResource returns the resource id a request refers to by path,
// e.g. GET /v1/videos/{id} → ("video", id).
func referencedResource(method, path string) (kind, id string) {
	seg := strings.Split(strings.Trim(path, "/"), "/")
	if len(seg) < 3 || seg[0] != "v1" {
		return "", ""
	}
	id = seg[2]
	switch seg[1] {
	case "videos":
		if id == "generations" || id == "extensions" || id == "edits" {
			return "", ""
		}
		return KindVideo, id
	case "images":
		if id == "generations" || id == "edits" {
			return "", ""
		}
		return KindImage, id
	case "chat":
		if len(seg) >= 4 && seg[2] == "deferred-completion" {
			return KindDeferred, seg[3]
		}
	case "responses":
		if id == "compact" {
			return "", ""
		}
		return KindResponse, id
	case "files":
		return KindFile, id
	case "batches":
		return KindBatch, strings.SplitN(id, ":", 2)[0] // /v1/batches/{id}:cancel
	case "collections":
		return KindColl, id
	}
	return "", ""
}

// createdResourceKind returns the kind of resource a successful response
// creates (its id is read from the response body), or "".
func createdResourceKind(method, path string, deferredImage bool) string {
	if method != http.MethodPost {
		return ""
	}
	switch path {
	case "/v1/videos/generations", "/v1/videos/extensions", "/v1/videos/edits":
		return KindVideo
	case "/v1/images/generations", "/v1/images/edits":
		if deferredImage {
			return KindImage
		}
	case "/v1/chat/completions":
		if deferredImage { // "deferred": true on chat completions returns a request_id
			return KindDeferred
		}
	case "/v1/responses":
		return KindResponse
	case "/v1/files":
		return KindFile
	case "/v1/batches":
		return KindBatch
	case "/v1/collections":
		return KindColl
	}
	return ""
}

// isAsyncPoll reports whether the request polls a deferred media job whose
// final response carries the cost of the whole job.
func isAsyncPoll(method, path string) (kind, id string, ok bool) {
	if method != http.MethodGet {
		return "", "", false
	}
	kind, id = referencedResource(method, path)
	return kind, id, kind == KindVideo || kind == KindImage || kind == KindDeferred
}

// Request headers forwarded to xAI. Everything else (cookies, forwarding
// headers, client-specific headers) is dropped.
var forwardHeaders = map[string]bool{
	"Content-Type":      true,
	"Accept":            true,
	"User-Agent":        true,
	"Anthropic-Version": true,
	"Anthropic-Beta":    true,
	"Idempotency-Key":   true,
	"Openai-Beta":       true,
}

func shouldForward(name string) bool {
	if forwardHeaders[name] {
		return true
	}
	l := strings.ToLower(name)
	return strings.HasPrefix(l, "x-grok-") || strings.HasPrefix(l, "xai-")
}

// Response headers never copied back to the client.
var dropResponseHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "Set-Cookie": true,
	"Content-Length": true, "Content-Encoding": true, "Alt-Svc": true,
}
