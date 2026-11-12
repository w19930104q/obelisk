package obelisk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	nurl "net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/css"
	"golang.org/x/net/html/charset"
)

// Strict CSS mode resolves @import rules recursively and embeds every
// referenced stylesheet in place. All state below is scoped to one Archive
// call, and only successful downloads are cached, so a failed archival can
// safely be retried.
var rxCSSCharset = regexp.MustCompile(`^@charset "([^"]+)"[ \t\r\n\f]*;`)

type ctxKeyStrictCSSState struct{}
type ctxKeyStrictResourceError struct{}
type ctxKeyStrictCSSProcessing struct{}

type strictCSSState struct {
	mu        sync.Mutex
	processMu sync.Mutex
	fetches   map[string]*cssFetchEntry
	processed map[string]string
}

type cssFetchEntry struct {
	done chan struct{}
	res  *cssResource
	err  error
}

type cssResource struct {
	requested   *nurl.URL
	final       *nurl.URL
	raw         []byte
	contentType string
}

type cssChainEntry struct {
	requested string
	final     string
}

type cssToken struct {
	typ  css.TokenType
	data []byte
}

type cssImportRule struct {
	url           string
	hasLayer      bool
	layerName     string
	supports      string
	media         string
	leading       string
	afterLayer    string
	afterSupports string
	end           int // index of the terminating semicolon
}

func newStrictCSSState() *strictCSSState {
	return &strictCSSState{
		fetches:   make(map[string]*cssFetchEntry),
		processed: make(map[string]string),
	}
}

func withStrictCSSState(ctx context.Context, state *strictCSSState) context.Context {
	return context.WithValue(ctx, ctxKeyStrictCSSState{}, state)
}

func strictCSSStateFromContext(ctx context.Context) *strictCSSState {
	state, _ := ctx.Value(ctxKeyStrictCSSState{}).(*strictCSSState)
	return state
}

func withStrictResourceError(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyStrictResourceError{}, true)
}

func strictResourceErrorFromContext(ctx context.Context) bool {
	strict, _ := ctx.Value(ctxKeyStrictResourceError{}).(bool)
	return strict
}

func withStrictCSSProcessing(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyStrictCSSProcessing{}, true)
}

func strictCSSProcessingFromContext(ctx context.Context) bool {
	processing, _ := ctx.Value(ctxKeyStrictCSSProcessing{}).(bool)
	return processing
}

// strictCSSCallContext serializes strict CSS processing within an Archive.
// The context flag makes the lock re-entrant for recursive @import
// resolution, which avoids both duplicated downloads and import-graph
// deadlocks between concurrently processed stylesheets.
func strictCSSCallContext(ctx context.Context) (context.Context, func()) {
	if strictCSSProcessingFromContext(ctx) {
		return ctx, func() {}
	}
	state := strictCSSStateFromContext(ctx)
	if state == nil {
		return ctx, func() {}
	}

	state.processMu.Lock()
	return withStrictCSSProcessing(ctx), state.processMu.Unlock
}

// strictCSSEnabled reports whether the Archiver configuration allows strict
// CSS processing. It is deliberately limited to single-file archives.
func (arc *Archiver) strictCSSEnabled() bool {
	return arc.StrictCSS && !arc.DisableCSS && arc.WrapDirectory == ""
}

// strictCSSActive reports whether strict CSS processing is configured and the
// current context belongs to an Archive call that initialized its state.
func (arc *Archiver) strictCSSActive(ctx context.Context) bool {
	return arc.strictCSSEnabled() && strictCSSStateFromContext(ctx) != nil
}

// processStrictStylesheetURL downloads an external stylesheet, processes its
// @import rules recursively and returns the resulting CSS.
func (arc *Archiver) processStrictStylesheetURL(ctx context.Context, rawURL string, baseURL *nurl.URL, chain []cssChainEntry) (string, error) {
	ctx, release := strictCSSCallContext(ctx)
	defer release()

	cssURL := createAbsoluteURL(rawURL, baseURL)
	if cssURL == "" || strings.HasPrefix(cssURL, "data:") || strings.HasPrefix(cssURL, "#") {
		return "", errSkippedURL
	}

	res, err := arc.fetchStrictCSS(ctx, cssURL, baseURL, chain)
	if err != nil {
		return "", err
	}

	return arc.processStrictCSSResource(ctx, res, chain)
}

// fetchStrictCSS returns a stylesheet resource, downloading it at most once
// per Archive call. Cycles are detected before waiting for an in-flight
// download, and again using the final URL after redirects, so aliases that
// point back into the import chain produce an error instead of a deadlock.
func (arc *Archiver) fetchStrictCSS(ctx context.Context, rawURL string, baseURL *nurl.URL, chain []cssChainEntry) (*cssResource, error) {
	parsed, err := nurl.ParseRequestURI(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" {
		return nil, fmt.Errorf("strict CSS: invalid @import URL %q", rawURL)
	}

	if cssChainContains(chain, parsed.String()) {
		return nil, newCSSCycleError(chain, parsed.String(), "")
	}

	parentURL := ""
	if baseURL != nil {
		parentURL = baseURL.String()
	}

	state := strictCSSStateFromContext(ctx)
	if state == nil {
		return nil, fmt.Errorf("strict CSS mode is not initialized for this archival")
	}
	res, err := state.fetch(ctx, arc, parsed, parentURL)
	if err != nil {
		return nil, err
	}

	// A redirect can turn a new URL into one that is already an ancestor.
	// Detect that before the stylesheet is processed, otherwise the importer
	// would recursively embed itself.
	if res.final != nil && cssChainContains(chain, res.final.String()) {
		return nil, newCSSCycleError(chain, parsed.String(), res.final.String())
	}

	return res, nil
}

func (state *strictCSSState) fetch(ctx context.Context, arc *Archiver, requested *nurl.URL, parentURL string) (*cssResource, error) {
	key := requested.String()

	state.mu.Lock()
	if entry, exists := state.fetches[key]; exists {
		state.mu.Unlock()

		select {
		case <-entry.done:
			return entry.res, entry.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	entry := &cssFetchEntry{done: make(chan struct{})}
	state.fetches[key] = entry
	state.mu.Unlock()

	res, err := arc.downloadStrictCSS(requested, parentURL)

	state.mu.Lock()
	entry.res = res
	entry.err = err

	if err != nil {
		// Never cache failures, so calling Archive again (or a later import
		// after a transient error) performs a fresh request.
		delete(state.fetches, key)
	} else if res != nil && res.final != nil {
		// Different requested URLs can redirect to the same stylesheet.
		finalKey := res.final.String()
		if finalKey != key {
			if _, exists := state.fetches[finalKey]; !exists {
				state.fetches[finalKey] = entry
			}
		}
	}

	close(entry.done)
	state.mu.Unlock()

	return res, err
}

func (arc *Archiver) downloadStrictCSS(requested *nurl.URL, parentURL string) (*cssResource, error) {
	resp, err := arc.downloadFile(requested.String(), parentURL)
	if err != nil {
		return nil, fmt.Errorf("strict CSS download failed for %s: %w", requested, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("strict CSS download failed for %s: unexpected status code %d", requested, resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("strict CSS read failed for %s: %w", requested, err)
	}

	finalURL := requested
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL
	}

	return &cssResource{
		requested:   requested,
		final:       finalURL,
		raw:         raw,
		contentType: resp.Header.Get("Content-Type"),
	}, nil
}

func (arc *Archiver) processStrictCSSResource(ctx context.Context, res *cssResource, chain []cssChainEntry) (string, error) {
	state := strictCSSStateFromContext(ctx)
	finalKey := res.final.String()

	state.mu.Lock()
	if processed, exists := state.processed[finalKey]; exists {
		state.mu.Unlock()
		return processed, nil
	}
	state.mu.Unlock()

	cssText, err := decodeCSSCharset(res.raw, res.contentType)
	if err != nil {
		return "", fmt.Errorf("strict CSS parse failed for %s: %w", res.final, err)
	}

	nextChain := append(append([]cssChainEntry(nil), chain...), cssChainEntry{
		requested: res.requested.String(),
		final:     res.final.String(),
	})
	result, err := arc.processCSSContent(ctx, cssText, res.final, nextChain)
	if err != nil {
		return "", err
	}

	state.mu.Lock()
	state.processed[finalKey] = result
	state.mu.Unlock()

	return result, nil
}

func cssChainContains(chain []cssChainEntry, target string) bool {
	for _, entry := range chain {
		if entry.requested == target || entry.final == target {
			return true
		}
	}
	return false
}

func newCSSCycleError(chain []cssChainEntry, requested, final string) error {
	parts := make([]string, 0, len(chain)+1)
	for _, entry := range chain {
		display := entry.requested
		if entry.final != "" && entry.final != entry.requested {
			display = fmt.Sprintf("%s (redirects to %s)", entry.requested, entry.final)
		}
		parts = append(parts, display)
	}

	display := requested
	if final != "" && final != requested {
		display = fmt.Sprintf("%s (redirects to %s)", requested, final)
	}
	parts = append(parts, display)

	return fmt.Errorf("strict CSS @import cycle detected: %s", strings.Join(parts, " -> "))
}

// processCSSContent tokenizes a stylesheet and rewrites only real URL and
// @import tokens. Comments and ordinary strings are copied verbatim.
func (arc *Archiver) processCSSContent(ctx context.Context, content string, baseURL *nurl.URL, chain []cssChainEntry) (string, error) {
	tokens, err := tokenizeCSS(content)
	if err != nil {
		return "", err
	}

	var out bytes.Buffer
	for i := 0; i < len(tokens); {
		token := tokens[i]

		if token.typ == css.AtKeywordToken && isCSSImportKeyword(token.data) {
			rule, err := parseCSSImport(tokens, i)
			if err != nil {
				return "", err
			}

			imported, err := arc.processStrictStylesheetURL(ctx, rule.url, baseURL, chain)
			if err != nil {
				if errors.Is(err, errSkippedURL) {
					// Self-contained imports (data:, #) are left untouched.
					writeCSSTokens(&out, tokens, i, rule.end+1)
					i = rule.end + 1
					continue
				}
				return "", err
			}

			out.WriteString(wrapCSSImport(imported, rule))
			i = rule.end + 1
			continue
		}

		if token.typ == css.URLToken {
			replacement, err := arc.processCSSURLToken(ctx, token.data, baseURL)
			if err != nil {
				return "", err
			}
			if replacement != "" {
				out.WriteString(replacement)
			} else {
				out.Write(token.data)
			}
			i++
			continue
		}

		out.Write(token.data)
		i++
	}

	return out.String(), nil
}

func writeCSSTokens(out *bytes.Buffer, tokens []cssToken, from, to int) {
	for i := from; i < to && i < len(tokens); i++ {
		out.Write(tokens[i].data)
	}
}

// processCSSURLToken embeds one url(...) resource. It returns an empty string
// when the token should be left as is (data URLs, fragments and empty URLs).
func (arc *Archiver) processCSSURLToken(ctx context.Context, tokenData []byte, baseURL *nurl.URL) (string, error) {
	value, err := parseCSSURLTokenValue(tokenData)
	if err != nil {
		return "", err
	}
	if value == "" || strings.HasPrefix(value, "data:") || strings.HasPrefix(value, "#") {
		return "", nil
	}

	cssURL := createAbsoluteURL(value, baseURL)
	if cssURL == "" || strings.HasPrefix(cssURL, "data:") || strings.HasPrefix(cssURL, "#") {
		return "", nil
	}

	content, contentType, err := arc.processURL(withStrictResourceError(ctx), cssURL, baseURL.String())
	if err != nil {
		if errors.Is(err, errSkippedURL) {
			return "", nil
		}
		// In strict CSS mode resource errors are never masked by
		// SkipResourceURLError.
		return "", err
	}

	return `url("` + arc.transform(cssURL, content, contentType) + `")`, nil
}

func tokenizeCSS(content string) ([]cssToken, error) {
	lexer := css.NewLexer(parse.NewInputString(content))

	var tokens []cssToken
	for {
		tokenType, data := lexer.Next()
		if tokenType == css.ErrorToken {
			if err := lexer.Err(); err != nil && !errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("failed to parse CSS: %w", err)
			}
			break
		}

		if err := validateCSSToken(tokenType, data); err != nil {
			return nil, err
		}
		tokens = append(tokens, cssToken{typ: tokenType, data: data})
	}

	if err := validateCSSBalance(tokens); err != nil {
		return nil, err
	}

	return tokens, nil
}

func validateCSSToken(tokenType css.TokenType, data []byte) error {
	switch tokenType {
	case css.BadStringToken, css.BadURLToken:
		return fmt.Errorf("failed to parse CSS: invalid %s token", tokenType)

	case css.CommentToken:
		if !bytes.HasSuffix(data, []byte("*/")) {
			return fmt.Errorf("failed to parse CSS: unterminated comment")
		}

	case css.StringToken:
		if !isTerminatedCSSString(data) {
			return fmt.Errorf("failed to parse CSS: unterminated string")
		}

	case css.URLToken:
		trimmed := bytes.TrimSpace(data)
		if len(trimmed) == 0 || !bytes.HasSuffix(trimmed, []byte(")")) {
			return fmt.Errorf("failed to parse CSS: unterminated url()")
		}
	}

	return nil
}

func isTerminatedCSSString(data []byte) bool {
	if len(data) < 2 || data[0] != '"' && data[0] != '\'' {
		return false
	}
	if data[len(data)-1] != data[0] {
		return false
	}

	backslashes := 0
	for i := len(data) - 2; i >= 0 && data[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 == 0
}

// validateCSSBalance catches truncated stylesheets that the lexer itself
// accepts (for example an unclosed block or parenthesis).
func validateCSSBalance(tokens []cssToken) error {
	var parens, brackets, braces int
	for _, token := range tokens {
		switch token.typ {
		case css.FunctionToken, css.LeftParenthesisToken:
			parens++
		case css.RightParenthesisToken:
			parens--
		case css.LeftBracketToken:
			brackets++
		case css.RightBracketToken:
			brackets--
		case css.LeftBraceToken:
			braces++
		case css.RightBraceToken:
			braces--
		}

		if parens < 0 || brackets < 0 || braces < 0 {
			return fmt.Errorf("failed to parse CSS: unbalanced closing token")
		}
	}

	if parens != 0 || brackets != 0 || braces != 0 {
		return fmt.Errorf("failed to parse CSS: unbalanced tokens")
	}
	return nil
}

func parseCSSImport(tokens []cssToken, at int) (cssImportRule, error) {
	rule := cssImportRule{end: -1}

	// Find the terminating semicolon, taking nested functions and brackets
	// into account. @import has no block, so a top-level brace is invalid.
	depth := 0
	end := -1
	for i := at + 1; i < len(tokens); i++ {
		switch tokens[i].typ {
		case css.FunctionToken, css.LeftParenthesisToken, css.LeftBracketToken:
			depth++
		case css.LeftBraceToken:
			if depth == 0 {
				return rule, fmt.Errorf("failed to parse CSS: invalid @import rule")
			}
			depth++
		case css.RightParenthesisToken, css.RightBracketToken, css.RightBraceToken:
			depth--
			if depth < 0 {
				return rule, fmt.Errorf("failed to parse CSS: unexpected closing token in @import")
			}
		case css.SemicolonToken:
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}

	if end < 0 {
		return rule, fmt.Errorf("failed to parse CSS: unterminated @import rule")
	}
	rule.end = end

	prelude := tokens[at+1 : end]
	idx := skipCSSTrivia(prelude, 0)
	if idx >= len(prelude) {
		return rule, fmt.Errorf("failed to parse CSS: @import is missing a URL")
	}

	var err error
	switch {
	case prelude[idx].typ == css.StringToken:
		rule.url, err = cssStringValue(prelude[idx].data)
		idx++

	case prelude[idx].typ == css.URLToken:
		rule.url, err = parseCSSURLTokenValue(prelude[idx].data)
		idx++

	case prelude[idx].typ == css.FunctionToken && strings.EqualFold(cssTokenName(prelude[idx].data), "url"):
		var inner []cssToken
		inner, idx, err = collectCSSFunctionArgs(prelude, idx)
		if err == nil {
			rule.url, err = parseCSSURLArgument(joinCSSTokens(inner))
		}

	default:
		err = fmt.Errorf("failed to parse CSS: @import is missing a URL")
	}

	if err != nil {
		return rule, err
	}
	if strings.TrimSpace(rule.url) == "" {
		return rule, fmt.Errorf("failed to parse CSS: @import URL is empty")
	}

	conditions := prelude[idx:]
	rule.leading, rule.afterLayer, rule.afterSupports, rule.hasLayer, rule.layerName, rule.supports, rule.media, err = parseCSSConditions(conditions)
	if err != nil {
		return rule, err
	}

	return rule, nil
}

// parseCSSConditions splits an @import condition list into its layer,
// supports and media parts while keeping every comment and whitespace run in
// its original position.
func parseCSSConditions(tokens []cssToken) (leading, afterLayer, afterSupports string, hasLayer bool, layerName, supports, media string, err error) {
	first := skipCSSTrivia(tokens, 0)
	leading = joinCSSTokens(tokens[:first])
	idx := first

	if idx < len(tokens) {
		switch {
		case tokens[idx].typ == css.IdentToken && strings.EqualFold(cssTokenName(tokens[idx].data), "layer"):
			hasLayer = true
			idx++

		case tokens[idx].typ == css.FunctionToken && strings.EqualFold(cssTokenName(tokens[idx].data), "layer"):
			var end int
			end, err = findMatchingCSSParen(tokens, idx)
			if err != nil {
				return
			}
			hasLayer = true
			layerName = strings.TrimSpace(joinCSSTokens(tokens[idx+1 : end]))
			idx = end + 1
		}
	}

	if hasLayer {
		next := skipCSSTrivia(tokens, idx)
		afterLayer = joinCSSTokens(tokens[idx:next])
		idx = next
	}

	if idx < len(tokens) {
		switch {
		case tokens[idx].typ == css.FunctionToken && strings.EqualFold(cssTokenName(tokens[idx].data), "supports"):
			var end int
			end, err = findMatchingCSSParen(tokens, idx)
			if err != nil {
				return
			}
			supports = strings.TrimSpace(joinCSSTokens(tokens[idx+1 : end]))
			idx = end + 1

		case tokens[idx].typ == css.IdentToken && strings.EqualFold(cssTokenName(tokens[idx].data), "supports"):
			next := skipCSSTrivia(tokens, idx+1)
			if next < len(tokens) && tokens[next].typ == css.LeftParenthesisToken {
				var end int
				end, err = findMatchingCSSParen(tokens, next)
				if err != nil {
					return
				}
				supports = strings.TrimSpace(joinCSSTokens(tokens[next+1 : end]))
				idx = end + 1
			}
		}
	}

	if supports != "" {
		next := skipCSSTrivia(tokens, idx)
		afterSupports = joinCSSTokens(tokens[idx:next])
		idx = next
	}

	media = strings.TrimSpace(joinCSSTokens(tokens[idx:]))
	if media == "" {
		// All remaining tokens were trivia. Keep them in the closest
		// enclosing condition wrapper so comments are not lost.
		if supports != "" {
			afterSupports += joinCSSTokens(tokens[idx:])
		} else if hasLayer {
			afterLayer += joinCSSTokens(tokens[idx:])
		} else {
			leading += joinCSSTokens(tokens[idx:])
		}
	}

	return
}

func wrapCSSImport(imported string, rule cssImportRule) string {
	content := imported

	// Keep the source order of conditions (layer, supports, media) as well
	// as comments and whitespace between them.
	if rule.media != "" {
		content = "@media " + rule.media + " {\n" + content + "\n}"
	}
	if rule.supports != "" {
		content = "@supports (" + rule.supports + ") {\n" + rule.afterSupports + content + "\n}"
	}
	if rule.hasLayer {
		if rule.layerName != "" {
			content = "@layer " + rule.layerName + " {\n" + rule.afterLayer + content + "\n}"
		} else {
			content = "@layer {\n" + rule.afterLayer + content + "\n}"
		}
	}

	return rule.leading + content
}

func isCSSImportKeyword(data []byte) bool {
	if len(data) == 0 || data[0] != '@' {
		return false
	}
	return strings.EqualFold(unescapeCSSString(string(data[1:])), "import")
}

// cssTokenName returns the decoded name of an at-keyword, function or
// identifier token, with the leading @ and trailing ( removed.
func cssTokenName(data []byte) string {
	name := string(data)
	if strings.HasPrefix(name, "@") {
		name = name[1:]
	}
	if strings.HasSuffix(name, "(") {
		name = name[:len(name)-1]
	}
	return unescapeCSSString(name)
}

func parseCSSURLTokenValue(data []byte) (string, error) {
	value := strings.TrimSpace(string(data))
	open := strings.IndexByte(value, '(')
	if open < 0 || !strings.HasSuffix(value, ")") {
		return "", fmt.Errorf("failed to parse CSS: invalid url() token")
	}

	inner := strings.TrimSpace(value[open+1 : len(value)-1])
	if inner == "" {
		// An empty url() is valid CSS and simply refers to no resource.
		return "", nil
	}

	return parseCSSURLArgument(inner)
}

func parseCSSURLArgument(argument string) (string, error) {
	argument = strings.TrimSpace(argument)
	if argument == "" {
		return "", nil
	}

	if argument[0] == '"' || argument[0] == '\'' {
		if len(argument) < 2 || argument[len(argument)-1] != argument[0] {
			return "", fmt.Errorf("failed to parse CSS: unterminated string in url()")
		}
		argument = argument[1 : len(argument)-1]
	}

	return unescapeCSSString(argument), nil
}

func cssStringValue(data []byte) (string, error) {
	if !isTerminatedCSSString(data) {
		return "", fmt.Errorf("failed to parse CSS: unterminated string")
	}
	return unescapeCSSString(string(data[1 : len(data)-1])), nil
}

func collectCSSFunctionArgs(tokens []cssToken, start int) ([]cssToken, int, error) {
	end, err := findMatchingCSSParen(tokens, start)
	if err != nil {
		return nil, start, err
	}
	return tokens[start+1 : end], end + 1, nil
}

func findMatchingCSSParen(tokens []cssToken, start int) (int, error) {
	if start >= len(tokens) {
		return 0, fmt.Errorf("failed to parse CSS: missing function")
	}
	if tokens[start].typ != css.FunctionToken && tokens[start].typ != css.LeftParenthesisToken {
		return 0, fmt.Errorf("failed to parse CSS: expected function or parenthesis")
	}

	depth := 1
	for i := start + 1; i < len(tokens); i++ {
		switch tokens[i].typ {
		case css.FunctionToken, css.LeftParenthesisToken:
			depth++
		case css.RightParenthesisToken:
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}

	return 0, fmt.Errorf("failed to parse CSS: unbalanced parenthesis")
}

func skipCSSTrivia(tokens []cssToken, from int) int {
	for from < len(tokens) {
		if tokens[from].typ != css.WhitespaceToken && tokens[from].typ != css.CommentToken {
			break
		}
		from++
	}
	return from
}

func joinCSSTokens(tokens []cssToken) string {
	var builder strings.Builder
	for _, token := range tokens {
		builder.Write(token.data)
	}
	return builder.String()
}

// unescapeCSSString decodes CSS escape sequences. It is used for URLs and
// identifiers, never for ordinary string contents that are copied verbatim.
func unescapeCSSString(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))

	for i := 0; i < len(value); {
		if value[i] != '\\' {
			builder.WriteByte(value[i])
			i++
			continue
		}

		i++
		if i >= len(value) {
			break
		}

		if value[i] == '\n' || value[i] == '\f' {
			i++
			continue
		}
		if value[i] == '\r' {
			i++
			if i < len(value) && value[i] == '\n' {
				i++
			}
			continue
		}

		if isCSSHexDigit(value[i]) {
			hexValue := 0
			digits := 0
			for i < len(value) && digits < 6 && isCSSHexDigit(value[i]) {
				hexValue = hexValue*16 + cssHexValue(value[i])
				i++
				digits++
			}
			// A single whitespace character after a hex escape is part of
			// the escape itself.
			if i < len(value) {
				if value[i] == '\r' && i+1 < len(value) && value[i+1] == '\n' {
					i += 2
				} else if value[i] == ' ' || value[i] == '\t' || value[i] == '\n' || value[i] == '\r' || value[i] == '\f' {
					i++
				}
			}

			if hexValue == 0 || hexValue > 0x10FFFF || (hexValue >= 0xD800 && hexValue <= 0xDFFF) {
				builder.WriteRune('�')
			} else {
				builder.WriteRune(rune(hexValue))
			}
			continue
		}

		builder.WriteByte(value[i])
		i++
	}

	return builder.String()
}

func isCSSHexDigit(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func cssHexValue(value byte) int {
	switch {
	case value >= '0' && value <= '9':
		return int(value - '0')
	case value >= 'a' && value <= 'f':
		return int(value-'a') + 10
	default:
		return int(value-'A') + 10
	}
}

// decodeCSSCharset converts stylesheet bytes to UTF-8. HTTP charset has
// precedence, then a BOM (handled by charset.NewReader), then @charset.
// Characters outside UTF-8 without any declaration fall back to the same
// legacy encoding used by charset.NewReader.
func decodeCSSCharset(raw []byte, contentType string) (string, error) {
	charsetLabel := ""

	if _, params, err := mime.ParseMediaType(contentType); err == nil {
		charsetLabel = params["charset"]
	}

	if charsetLabel == "" {
		if match := rxCSSCharset.FindSubmatch(raw); len(match) == 2 {
			charsetLabel = string(match[1])
		}
	}

	if charsetLabel != "" {
		if encoding, _ := charset.Lookup(charsetLabel); encoding == nil {
			return "", fmt.Errorf("unsupported CSS charset %q", charsetLabel)
		}
	}

	effectiveContentType := contentType
	if charsetLabel != "" {
		effectiveContentType = "text/css; charset=" + charsetLabel
	}

	reader, err := charset.NewReader(bytes.NewReader(raw), effectiveContentType)
	if err != nil {
		return "", fmt.Errorf("failed to decode CSS: %w", err)
	}

	decoded, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("failed to decode CSS: %w", err)
	}

	return string(decoded), nil
}
