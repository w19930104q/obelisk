package obelisk

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type cssTestRouter struct {
	mu     sync.Mutex
	counts map[string]int
	routes map[string]http.HandlerFunc
}

func newCSSTestRouter() *cssTestRouter {
	return &cssTestRouter{
		counts: make(map[string]int),
		routes: make(map[string]http.HandlerFunc),
	}
}

func (r *cssTestRouter) handler(path string, handler http.HandlerFunc) {
	r.routes[path] = handler
}

func (r *cssTestRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.counts[req.Method+" "+req.URL.Path]++
	r.mu.Unlock()

	handler, exists := r.routes[req.URL.Path]
	if !exists {
		http.NotFound(w, req)
		return
	}
	handler(w, req)
}

func (r *cssTestRouter) count(method, path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[method+" "+path]
}

func serveCSS(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = io.WriteString(w, body)
	}
}

func serveHTML(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, body)
	}
}

func newStrictTestArchiver(strict bool) *Archiver {
	arc := &Archiver{
		StrictCSS:             strict,
		MaxRetries:            0,
		RequestTimeout:        10 * time.Second,
		MaxConcurrentDownload: 10,
	}
	arc.Validate()
	return arc
}

func archiveTestURL(t *testing.T, arc *Archiver, url string) ([]byte, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return arc.Archive(ctx, Request{URL: url})
}

func TestStrictCSSImportsEmbeddedInPlace(t *testing.T) {
	router := newCSSTestRouter()
	router.handler("/", serveHTML(`<!doctype html>
<html><head>
<link rel="stylesheet" href="/css/main.css">
<link rel="stylesheet" href="/css/main.css">
<style>@import url("/css/imports/shared.css"); .inline { color: green; }</style>
</head><body><div class="hero" style="background: url('/css/img/bg.png')"></div></body></html>`))
	router.handler("/css/main.css", serveCSS(`@import url("imports/a.css") layer(theme) supports(display: grid) screen and (min-width: 600px);
/* keep-comment url("img/hero.png") */
.hero { background: url("img/hero.png"); }
.literal::before { content: "url(/css/img/hero.png)"; }
.content { color: red; }`))
	router.handler("/css/imports/a.css", serveCSS(`@import url("shared.css");
.a { color: blue; }`))
	router.handler("/css/imports/shared.css", serveCSS(`.shared { font-weight: bold; }`))
	router.handler("/css/img/hero.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G'})
	})
	router.handler("/css/img/bg.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G'})
	})

	server := httptest.NewServer(router)
	defer server.Close()

	arc := newStrictTestArchiver(true)
	result, _, err := archiveTestURL(t, arc, server.URL+"/")
	if err != nil {
		t.Fatalf("Archive() error = %v", err)
	}
	html := string(result)

	if strings.Contains(html, "@import") {
		t.Errorf("strict CSS left an @import rule in the archive")
	}
	for _, want := range []string{
		".a { color: blue; }",
		".shared { font-weight: bold; }",
		".inline { color: green; }",
		".content { color: red; }",
		"data:image/png;base64,",
		`/* keep-comment url("img/hero.png") */`,
		`content: "url(/css/img/hero.png)"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("archive is missing %q", want)
		}
	}

	layerPos := strings.Index(html, "@layer theme")
	supportsPos := strings.Index(html, "@supports (display: grid)")
	mediaPos := strings.Index(html, "@media screen and (min-width: 600px)")
	if layerPos < 0 || supportsPos < 0 || mediaPos < 0 {
		t.Errorf("import conditions were not preserved: layer=%d supports=%d media=%d", layerPos, supportsPos, mediaPos)
	} else if !(layerPos < supportsPos && supportsPos < mediaPos) {
		t.Errorf("import conditions are out of order: layer=%d supports=%d media=%d", layerPos, supportsPos, mediaPos)
	}

	if got := router.count(http.MethodGet, "/css/main.css"); got != 1 {
		t.Errorf("main.css was requested %d times, want 1", got)
	}
	if got := router.count(http.MethodGet, "/css/imports/a.css"); got != 1 {
		t.Errorf("a.css was requested %d times, want 1", got)
	}
	if got := router.count(http.MethodGet, "/css/imports/shared.css"); got != 1 {
		t.Errorf("shared.css was requested %d times, want 1", got)
	}
	if got := router.count(http.MethodGet, "/css/img/hero.png"); got != 1 {
		t.Errorf("hero.png was requested %d times, want 1", got)
	}
	if got := router.count(http.MethodGet, "/css/img/bg.png"); got != 1 {
		t.Errorf("bg.png was requested %d times, want 1", got)
	}
}

func TestStrictCSSLinkMediaConditionPreserved(t *testing.T) {
	router := newCSSTestRouter()
	router.handler("/", serveHTML(`<!doctype html>
<html><head><link rel="stylesheet" media="print and (min-width: 10cm)" href="/css/print.css"></head><body></body></html>`))
	router.handler("/css/print.css", serveCSS(`.print-only { color: black; }`))

	server := httptest.NewServer(router)
	defer server.Close()

	arc := newStrictTestArchiver(true)
	result, _, err := archiveTestURL(t, arc, server.URL+"/")
	if err != nil {
		t.Fatalf("Archive() error = %v", err)
	}
	html := string(result)

	if strings.Contains(html, `<link`) {
		t.Errorf("strict CSS did not replace the stylesheet link: %s", html)
	}
	if !strings.Contains(html, `@media print and (min-width: 10cm) {`) {
		t.Errorf("link media condition was not preserved: %s", html)
	}
	if !strings.Contains(html, ".print-only { color: black; }") {
		t.Errorf("stylesheet content was not embedded: %s", html)
	}
}

func TestStrictCSSRelativeURLsUseFinalGETURL(t *testing.T) {
	router := newCSSTestRouter()
	router.handler("/", serveHTML(`<!doctype html><html><head>
<link rel="stylesheet" href="/redirect/main.css">
</head><body></body></html>`))
	router.handler("/redirect/main.css", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/real/main.css", http.StatusFound)
	})
	router.handler("/real/main.css", serveCSS(`@import "child.css";
.r { background: url("img.png"); }`))
	router.handler("/real/child.css", serveCSS(`.child { color: teal; }`))
	router.handler("/real/img.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("PNG"))
	})
	router.handler("/redirect/child.css", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	router.handler("/redirect/img.png", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	server := httptest.NewServer(router)
	defer server.Close()

	arc := newStrictTestArchiver(true)
	result, _, err := archiveTestURL(t, arc, server.URL+"/")
	if err != nil {
		t.Fatalf("Archive() error = %v", err)
	}
	html := string(result)

	if !strings.Contains(html, ".child { color: teal; }") {
		t.Error("relative @import was not resolved against the final stylesheet URL")
	}
	if !strings.Contains(html, "data:image/png;base64,") {
		t.Error("relative url() was not resolved against the final stylesheet URL")
	}
	if got := router.count(http.MethodGet, "/real/child.css"); got != 1 {
		t.Errorf("/real/child.css was requested %d times, want 1", got)
	}
	if got := router.count(http.MethodGet, "/real/img.png"); got != 1 {
		t.Errorf("/real/img.png was requested %d times, want 1", got)
	}
	for _, path := range []string{"/redirect/child.css", "/redirect/img.png"} {
		if got := router.count(http.MethodGet, path); got != 0 {
			t.Errorf("%s was requested %d times, want 0", path, got)
		}
	}
}

func TestStrictCSSCharset(t *testing.T) {
	router := newCSSTestRouter()
	router.handler("/", serveHTML(`<!doctype html><html><head>
<link rel="stylesheet" href="/header-charset.css">
<link rel="stylesheet" href="/at-charset.css">
</head><body></body></html>`))
	router.handler("/header-charset.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=windows-1252")
		_, _ = w.Write([]byte(".header { content: \"caf\xe9\"; }"))
	})
	router.handler("/at-charset.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write([]byte("@charset \"windows-1252\";\n.at { content: \"caf\xe9\"; }"))
	})

	server := httptest.NewServer(router)
	defer server.Close()

	arc := newStrictTestArchiver(true)
	result, _, err := archiveTestURL(t, arc, server.URL+"/")
	if err != nil {
		t.Fatalf("Archive() error = %v", err)
	}

	if !strings.Contains(string(result), "café") {
		t.Error("HTTP charset of a stylesheet was not decoded to UTF-8")
	}
	if !strings.Contains(string(result), `@charset "windows-1252"`) {
		t.Error("@charset declaration was not preserved in the embedded stylesheet")
	}
}

func TestStrictCSSImportCycleWithRedirectAlias(t *testing.T) {
	router := newCSSTestRouter()
	router.handler("/", serveHTML(`<!doctype html><html><head>
<link rel="stylesheet" href="/cycle/a.css">
</head><body></body></html>`))
	router.handler("/cycle/a.css", serveCSS(`@import "b.css"; .a { color: red; }`))
	router.handler("/cycle/b.css", serveCSS(`@import "/cycle-alias.css"; .b { color: blue; }`))
	router.handler("/cycle-alias.css", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/cycle/a.css", http.StatusFound)
	})

	server := httptest.NewServer(router)
	defer server.Close()

	arc := newStrictTestArchiver(true)
	result, _, err := archiveTestURL(t, arc, server.URL+"/")
	if err == nil {
		t.Fatalf("Archive() returned no error for a cycle through a redirect alias; result = %d bytes", len(result))
	}
	if result != nil {
		t.Error("Archive() returned a partial archive together with an error")
	}

	message := err.Error()
	if !strings.Contains(message, "cycle") {
		t.Errorf("cycle error does not mention a cycle: %q", message)
	}
	for _, want := range []string{"/cycle/a.css", "/cycle/b.css", "/cycle-alias.css"} {
		if !strings.Contains(message, want) {
			t.Errorf("cycle error is missing %q: %q", want, message)
		}
	}
}

func TestStrictCSSFailureIsNotCachedAndSkipDoesNotMaskIt(t *testing.T) {
	router := newCSSTestRouter()

	failing := true
	router.handler("/", serveHTML(`<!doctype html><html><head>
<link rel="stylesheet" href="/fail.css">
</head><body></body></html>`))
	router.handler("/fail.css", func(w http.ResponseWriter, _ *http.Request) {
		router.mu.Lock()
		current := failing
		if current {
			failing = false
		}
		router.mu.Unlock()

		if current {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = io.WriteString(w, `.recovered { color: green; }`)
	})

	server := httptest.NewServer(router)
	defer server.Close()

	arc := newStrictTestArchiver(true)
	arc.SkipResourceURLError = true

	if result, _, err := archiveTestURL(t, arc, server.URL+"/"); err == nil {
		t.Fatal("Archive() did not fail although strict CSS download failed")
	} else if result != nil {
		t.Error("Archive() returned a partial archive together with an error")
	}

	if got := router.count(http.MethodGet, "/fail.css"); got != 1 {
		t.Errorf("failed stylesheet was requested %d times, want 1", got)
	}

	result, _, err := archiveTestURL(t, arc, server.URL+"/")
	if err != nil {
		t.Fatalf("Archive() after recovery error = %v", err)
	}
	if !strings.Contains(string(result), ".recovered { color: green; }") {
		t.Error("recovered stylesheet was not embedded on retry")
	}
	if got := router.count(http.MethodGet, "/fail.css"); got != 2 {
		t.Errorf("stylesheets requests = %d, want 2 (failed attempt must not be cached)", got)
	}
}

func TestStrictCSSRetriesWithinSingleArchive(t *testing.T) {
	router := newCSSTestRouter()

	attempts := 0
	router.handler("/", serveHTML(`<!doctype html><html><head>
<link rel="stylesheet" href="/retry.css">
</head><body></body></html>`))
	router.handler("/retry.css", func(w http.ResponseWriter, _ *http.Request) {
		router.mu.Lock()
		attempts++
		current := attempts
		router.mu.Unlock()

		if current == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = io.WriteString(w, `.retried { color: orange; }`)
	})

	server := httptest.NewServer(router)
	defer server.Close()

	arc := newStrictTestArchiver(true)
	arc.MaxRetries = 1

	result, _, err := archiveTestURL(t, arc, server.URL+"/")
	if err != nil {
		t.Fatalf("Archive() error = %v", err)
	}
	if !strings.Contains(string(result), ".retried { color: orange; }") {
		t.Error("stylesheet was not embedded after retry")
	}
	if got := router.count(http.MethodGet, "/retry.css"); got != 2 {
		t.Errorf("stylesheet requests = %d, want 2", got)
	}
}

func TestStrictCSSParseFailure(t *testing.T) {
	for name, body := range map[string]string{
		"unterminated string":  `.broken { content: "unterminated; }`,
		"unterminated comment": `.broken { color: red; } /* unterminated`,
		"missing import semi":  `@import url("child.css")`,
		"unbalanced block":     `.broken { color: red;`,
	} {
		t.Run(name, func(t *testing.T) {
			router := newCSSTestRouter()
			router.handler("/", serveHTML(`<!doctype html><html><head>
<link rel="stylesheet" href="/broken.css">
</head><body></body></html>`))
			router.handler("/broken.css", serveCSS(body))

			server := httptest.NewServer(router)
			defer server.Close()

			arc := newStrictTestArchiver(true)
			result, _, err := archiveTestURL(t, arc, server.URL+"/")
			if err == nil {
				t.Fatalf("Archive() did not fail for malformed CSS")
			}
			if result != nil {
				t.Error("Archive() returned a partial archive together with an error")
			}
		})
	}
}

func TestStrictCSSCrossBranchCycleDoesNotDeadlock(t *testing.T) {
	router := newCSSTestRouter()
	router.handler("/", serveHTML(`<!doctype html><html><head>
<link rel="stylesheet" href="/cross/a.css">
<link rel="stylesheet" href="/cross/b.css">
</head><body></body></html>`))
	router.handler("/cross/a.css", serveCSS(`@import "b.css"; .a { color: red; }`))
	router.handler("/cross/b.css", serveCSS(`@import "a.css"; .b { color: blue; }`))

	server := httptest.NewServer(router)
	defer server.Close()

	arc := newStrictTestArchiver(true)
	result, _, err := archiveTestURL(t, arc, server.URL+"/")
	if err == nil {
		t.Fatal("Archive() returned no error for a mutual cross-stylesheet cycle")
	}
	if result != nil {
		t.Error("Archive() returned a partial archive together with an error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("cross-branch cycle error is not clear: %q", err.Error())
	}
	for _, want := range []string{"/cross/a.css", "/cross/b.css"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("cross-branch cycle error is missing %q: %q", want, err.Error())
		}
	}

	// Each stylesheet was fetched once even though both links were processed.
	if got := router.count(http.MethodGet, "/cross/a.css"); got != 1 {
		t.Errorf("/cross/a.css was requested %d times, want 1", got)
	}
	if got := router.count(http.MethodGet, "/cross/b.css"); got != 1 {
		t.Errorf("/cross/b.css was requested %d times, want 1", got)
	}
}

func TestStrictCSSDisabledAndDisableCSSCompatibility(t *testing.T) {
	router := newCSSTestRouter()
	router.handler("/", serveHTML(`<!doctype html><html><head>
<style>@import url("/child.css"); .inline { color: black; }</style>
</head><body></body></html>`))
	router.handler("/child.css", serveCSS(`.child { color: purple; }`))

	server := httptest.NewServer(router)
	defer server.Close()

	t.Run("strict off leaves legacy behavior", func(t *testing.T) {
		arc := newStrictTestArchiver(false)
		result, _, err := archiveTestURL(t, arc, server.URL+"/")
		if err != nil {
			t.Fatalf("Archive() error = %v", err)
		}
		if strings.Contains(string(result), ".child { color: purple; }") {
			t.Error("@import was inlined even though strict CSS mode is disabled")
		}
	})

	t.Run("disable CSS disables strict mode", func(t *testing.T) {
		arc := newStrictTestArchiver(true)
		arc.DisableCSS = true
		before := router.count(http.MethodGet, "/child.css")

		result, _, err := archiveTestURL(t, arc, server.URL+"/")
		if err != nil {
			t.Fatalf("Archive() error = %v", err)
		}
		if strings.Contains(string(result), ".inline { color: black; }") {
			t.Error("inline style was kept even though CSS is disabled")
		}
		if got := router.count(http.MethodGet, "/child.css"); got != before {
			t.Errorf("CSS resource was requested in strict+DisableCSS mode (%d -> %d)", before, got)
		}
	})
}

func TestStrictCSSOnlySingleFile(t *testing.T) {
	router := newCSSTestRouter()
	router.handler("/", serveHTML(`<!doctype html><html><head>
<style>@import url("/child.css"); .inline { color: black; }</style>
</head><body></body></html>`))
	router.handler("/child.css", serveCSS(`.child { color: purple; }`))

	server := httptest.NewServer(router)
	defer server.Close()

	arc := newStrictTestArchiver(true)
	arc.WrapDirectory = t.TempDir()

	// Strict mode is limited to single-file archives, so the directory mode
	// keeps its legacy behavior without returning a misleading error.
	result, _, err := archiveTestURL(t, arc, server.URL+"/")
	if err != nil {
		t.Fatalf("Archive() error = %v", err)
	}
	if strings.Contains(string(result), ".child { color: purple; }") {
		t.Error("@import was inlined in directory mode")
	}
}

func TestStrictCSSImportForms(t *testing.T) {
	router := newCSSTestRouter()
	router.handler("/", serveHTML(`<!doctype html><html><head>
<link rel="stylesheet" href="/forms.css">
</head><body></body></html>`))
	router.handler("/forms.css", serveCSS(`@import url('/one.css') print;
@import url(../two.css) layer;
@import 'thr\65 e.css' layer(named) supports((display: flex) and (color: red)) all;
@import url("four\2e css") screen;`))
	router.handler("/one.css", serveCSS(`.one { color: red; }`))
	router.handler("/two.css", serveCSS(`.two { color: green; }`))
	router.handler("/three.css", serveCSS(`.three { color: blue; }`))
	router.handler("/four.css", serveCSS(`.four { color: yellow; }`))

	server := httptest.NewServer(router)
	defer server.Close()

	arc := newStrictTestArchiver(true)
	result, _, err := archiveTestURL(t, arc, server.URL+"/")
	if err != nil {
		t.Fatalf("Archive() error = %v", err)
	}
	html := string(result)

	for _, want := range []string{
		`@media print`,
		`.one { color: red; }`,
		`@layer {`,
		`.two { color: green; }`,
		`@layer named {`,
		`@supports ((display: flex) and (color: red)) {`,
		`@media all`,
		`.three { color: blue; }`,
		`@media screen`,
		`.four { color: yellow; }`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("archive is missing %q", want)
		}
	}
	if strings.Contains(html, "@import") {
		t.Error("strict CSS left an @import rule in the archive")
	}
}

// Ensure the error for a redirect-alias cycle is not simply a context error.
func TestStrictCSSCycleErrorChainFormat(t *testing.T) {
	chain := []cssChainEntry{
		{requested: "https://example.com/a.css", final: "https://example.com/a-final.css"},
		{requested: "https://example.com/b.css", final: "https://example.com/b.css"},
	}
	err := newCSSCycleError(chain, "https://example.com/alias.css", "https://example.com/a-final.css")
	for _, want := range []string{
		"strict CSS @import cycle detected",
		"https://example.com/a.css (redirects to https://example.com/a-final.css)",
		"https://example.com/b.css",
		"https://example.com/alias.css (redirects to https://example.com/a-final.css)",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("cycle error %q is missing %q", err.Error(), want)
		}
	}
}

func TestStrictCSSMultilineConditionImports(t *testing.T) {
	// Whitespace and comments inside import conditions must not break parsing
	// or cause the comments to be rewritten.
	router := newCSSTestRouter()
	router.handler("/", serveHTML(`<!doctype html><html><head>
<link rel="stylesheet" href="/cond.css">
</head><body></body></html>`))
	router.handler("/cond.css", serveCSS(`@import url("cond-child.css")
	layer(theme) /* keep condition comment */ supports(display: grid)
	screen and (min-width: 600px);`))
	router.handler("/cond-child.css", serveCSS(`.cond-child { color: navy; }`))

	server := httptest.NewServer(router)
	defer server.Close()

	arc := newStrictTestArchiver(true)
	result, _, err := archiveTestURL(t, arc, server.URL+"/")
	if err != nil {
		t.Fatalf("Archive() error = %v", err)
	}
	html := string(result)

	for _, want := range []string{
		`@layer theme`,
		`@supports (display: grid)`,
		`@media screen and (min-width: 600px)`,
		`/* keep condition comment */`,
		`.cond-child { color: navy; }`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("archive is missing %q", want)
		}
	}

	layerPos := strings.Index(html, "@layer theme")
	commentPos := strings.Index(html, "/* keep condition comment */")
	supportsPos := strings.Index(html, "@supports (display: grid)")
	if !(layerPos < commentPos && commentPos < supportsPos) {
		t.Errorf("condition order/comment position was not preserved: layer=%d comment=%d supports=%d", layerPos, commentPos, supportsPos)
	}
}

func TestStrictCSSURLArgumentEdgeCases(t *testing.T) {
	router := newCSSTestRouter()
	router.handler("/", serveHTML(`<!doctype html><html><head>
<link rel="stylesheet" href="/edge.css">
</head><body></body></html>`))
	router.handler("/edge.css", serveCSS(`.edges {
	background: url( "spaced.png" );
	border-image: url(escaped\20 name.png);
	cursor: url(data:image/png;base64,AAAA), auto;
	empty: url();
}`))
	router.handler("/spaced.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("spaced"))
	})
	router.handler("/escaped name.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("escaped"))
	})

	server := httptest.NewServer(router)
	defer server.Close()

	arc := newStrictTestArchiver(true)
	result, _, err := archiveTestURL(t, arc, server.URL+"/")
	if err != nil {
		t.Fatalf("Archive() error = %v", err)
	}
	html := string(result)

	if !strings.Contains(html, "data:image/png;base64,") {
		t.Error("spaced url() was not embedded")
	}
	if got := router.count(http.MethodGet, "/spaced.png"); got != 1 {
		t.Errorf("/spaced.png was requested %d times, want 1", got)
	}
	if got := router.count(http.MethodGet, "/escaped name.png"); got != 1 {
		t.Errorf("escaped url was requested %d times, want 1", got)
	}
	if !strings.Contains(html, "url(data:image/png;base64,AAAA)") {
		t.Error("data URL in url() was rewritten")
	}
	if !strings.Contains(html, "empty: url()") {
		t.Error("empty url() was rewritten")
	}
}
