package test

import (
	"bufio"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// specOperations reads "METHOD /path" pairs from web/static/openapi.yaml
// (top-level paths and their HTTP-method keys).
func specOperations(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open("../web/static/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pathRe := regexp.MustCompile(`^  (/[^:]*):\s*$`)
	methodRe := regexp.MustCompile(`^    (get|post|put|patch|delete):`)
	ops := map[string]bool{}
	inPaths, cur := false, ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "paths:":
			inPaths = true
		case inPaths && len(line) > 0 && line[0] != ' ' && line[0] != '#':
			inPaths = false
		case inPaths && pathRe.MatchString(line):
			cur = pathRe.FindStringSubmatch(line)[1]
		case inPaths && cur != "" && methodRe.MatchString(line):
			ops[strings.ToUpper(methodRe.FindStringSubmatch(line)[1])+" "+cur] = true
		}
	}
	return ops
}

// The OpenAPI document and the registered admin routes match one to one.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	e := newEnv(t)
	spec := specOperations(t)
	routes := map[string]bool{"GET /healthz": true}
	for _, r := range e.app.Admin.Routes() {
		routes[r.Pattern] = true
	}
	var missingInSpec, missingInCode []string
	for r := range routes {
		if !spec[r] {
			missingInSpec = append(missingInSpec, r)
		}
	}
	for op := range spec {
		if !routes[op] {
			missingInCode = append(missingInCode, op)
		}
	}
	sort.Strings(missingInSpec)
	sort.Strings(missingInCode)
	if len(missingInSpec)+len(missingInCode) > 0 {
		t.Fatalf("routes not in openapi.yaml: %v\nspec operations without a route: %v", missingInSpec, missingInCode)
	}
	if len(spec) < 30 {
		t.Fatalf("parsed only %d operations from the spec", len(spec))
	}
	// The spec is served to the console.
	e.do("GET", "/openapi.yaml", "", nil, 200, nil)
}

// Every non-public admin route rejects requests without credentials, with
// a bad token and with a client token.
func TestEveryAdminRouteRequiresAuth(t *testing.T) {
	e := newEnv(t)
	for _, r := range e.app.Admin.Routes() {
		if r.Public {
			continue
		}
		method, path, _ := strings.Cut(r.Pattern, " ")
		path = strings.NewReplacer("{id}", "1").Replace(path)
		for _, cred := range []string{"", "xrs-forged", "xra-forged", e.client} {
			req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader("{}"))
			if cred != "" {
				req.Header.Set("Authorization", "Bearer "+cred)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s with %q: status %d, want 401", r.Pattern, cred, resp.StatusCode)
			}
		}
	}
}
