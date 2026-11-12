package obelisk

import (
	"bytes"
	"context"
	"fmt"
	"io"
	nurl "net/url"
	"strings"
	"sync"

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/css"
	"golang.org/x/sync/errgroup"
)

func (arc *Archiver) processCSS(ctx context.Context, input io.Reader, baseURL *nurl.URL) (string, error) {
	// In strict mode we need to rewrite the CSS token by token, so we need
	// the whole stylesheet first. This mode is deliberately opt-in because
	// it changes how @import rules and resource errors are handled.
	if arc.strictCSSActive(ctx) {
		ctx, release := strictCSSCallContext(ctx)
		defer release()

		content, err := io.ReadAll(input)
		if err != nil {
			return "", fmt.Errorf("failed to read CSS: %w", err)
		}
		return arc.processCSSContent(ctx, string(content), baseURL, nil)
	}

	return arc.processCSSLegacy(ctx, input, baseURL)
}

func (arc *Archiver) processCSSLegacy(ctx context.Context, input io.Reader, baseURL *nurl.URL) (string, error) {
	// Prepare buffer to store content from input
	buffer := bytes.NewBuffer(nil)

	// Scan CSS and find all URLs
	urls := make(map[string]struct{})
	lexer := css.NewLexer(parse.NewInput(input))

	for {
		token, bt := lexer.Next()

		// Check for error or EOF
		if token == css.ErrorToken {
			break
		}

		// If it's URL save it
		if token == css.URLToken {
			urls[string(bt)] = struct{}{}
		}

		buffer.Write(bt)
	}

	// Process each url concurrently
	mutex := sync.RWMutex{}
	processedURLs := make(map[string]string)

	g, ctx := errgroup.WithContext(ctx)
	for url := range urls {
		url := url
		g.Go(func() error {
			cssURL := sanitizeStyleURL(url)
			cssURL = createAbsoluteURL(cssURL, baseURL)
			content, contentType, err := arc.processURL(ctx, cssURL, baseURL.String())
			if err != nil && err != errSkippedURL {
				return err
			}

			var result string
			if err == errSkippedURL {
				result = `url("` + cssURL + `")`
			} else {
				result = `url("` + arc.transform(cssURL, content, contentType) + `")`
			}

			mutex.Lock()
			processedURLs[url] = result
			mutex.Unlock()

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return buffer.String(), err
	}

	// Convert all url into the processed URL
	cssRules := buffer.String()
	for url, processedURL := range processedURLs {
		cssRules = strings.ReplaceAll(cssRules, url, processedURL)
	}

	return cssRules, nil
}
