package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// The OpenAPI 3.1 document is valid JSON Schema 2020-12 at every schema
// position, so responses are validated with the same library as seeds.
const specURL = "mem://openapi.json"

var spec struct {
	once     sync.Once
	err      error
	doc      map[string]any
	compiler *jsonschema.Compiler
	mu       sync.Mutex
	cache    map[string]*jsonschema.Schema
}

func loadSpec() error {
	spec.once.Do(func() {
		b, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
		if err != nil {
			spec.err = err
			return
		}
		var y any
		if err := yaml.Unmarshal(b, &y); err != nil {
			spec.err = err
			return
		}
		j, err := json.Marshal(y)
		if err != nil {
			spec.err = err
			return
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(j))
		if err != nil {
			spec.err = err
			return
		}
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		if err := c.AddResource(specURL, doc); err != nil {
			spec.err = err
			return
		}
		spec.doc, spec.compiler, spec.cache = doc.(map[string]any), c, map[string]*jsonschema.Schema{}
	})
	return spec.err
}

func ptrEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func lookup(doc any, pointer string) (any, bool) {
	cur := doc
	for _, part := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// conform checks that the status is documented for the operation and that a
// JSON body matches its schema.
func conform(t *testing.T, method, op string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if err := loadSpec(); err != nil {
		t.Fatalf("load spec: %v", err)
	}
	opPtr := "/paths/" + ptrEscape(op) + "/" + strings.ToLower(method)
	if _, ok := lookup(spec.doc, opPtr); !ok {
		t.Fatalf("operation %s %s not in api/openapi.yaml", method, op)
	}
	respPtr := opPtr + "/responses/" + strconv.Itoa(rec.Code)
	resp, ok := lookup(spec.doc, respPtr)
	if !ok {
		t.Fatalf("%s %s: status %d not documented; body: %s", method, op, rec.Code, rec.Body.String())
	}
	if ref, isRef := resp.(map[string]any)["$ref"].(string); isRef {
		respPtr = strings.TrimPrefix(ref, "#")
		resp, _ = lookup(spec.doc, respPtr)
	}
	if _, hasJSON := lookup(resp, "/content/application~1json/schema"); !hasJSON {
		if rec.Body.Len() > 0 && rec.Code != 204 {
			t.Fatalf("%s %s %d: undocumented body %s", method, op, rec.Code, rec.Body.String())
		}
		return
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("%s %s %d: content type %q", method, op, rec.Code, ct)
	}
	loc := specURL + "#" + respPtr + "/content/application~1json/schema"
	spec.mu.Lock()
	sch, ok := spec.cache[loc]
	if !ok {
		var err error
		if sch, err = spec.compiler.Compile(loc); err != nil {
			spec.mu.Unlock()
			t.Fatalf("compile %s: %v", loc, err)
		}
		spec.cache[loc] = sch
	}
	spec.mu.Unlock()
	body, err := jsonschema.UnmarshalJSON(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("%s %s %d: body is not JSON: %v", method, op, rec.Code, err)
	}
	if err := sch.Validate(body); err != nil {
		t.Fatalf("%s %s %d: body does not match spec: %v\nbody: %s", method, op, rec.Code, err, rec.Body.String())
	}
}

// TestRoutesAreInSpec fails if a registered route is missing from the spec.
func TestRoutesAreInSpec(t *testing.T) {
	h := newHarness(t)
	if err := loadSpec(); err != nil {
		t.Fatal(err)
	}
	routes := routeList(h.srv)
	if len(routes) < 18 {
		t.Fatalf("walked only %d routes", len(routes))
	}
	for _, rt := range routes {
		ptr := "/paths/" + ptrEscape(rt.path) + "/" + strings.ToLower(rt.method)
		op, ok := lookup(spec.doc, ptr)
		if !ok {
			t.Errorf("route %s %s not in spec", rt.method, rt.path)
			continue
		}
		if rt.path != "/v1/auth/login" {
			if _, ok := op.(map[string]any)["x-required-role"]; !ok {
				t.Errorf("route %s %s: spec has no x-required-role", rt.method, rt.path)
			}
		}
	}
}

type routeInfo struct{ method, path string }

func routeList(s *Server) []routeInfo {
	var out []routeInfo
	walk(s, func(method, path string) { out = append(out, routeInfo{method, path}) })
	return out
}

func walk(s *Server, fn func(method, path string)) {
	mux, ok := s.routes().(chi.Routes)
	if !ok {
		panic("routes is not a chi router")
	}
	_ = chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		fn(method, strings.TrimSuffix(route, "/"))
		return nil
	})
}
