package obelisk

import (
	"context"
	"fmt"
	"io"
	"mime"
	nurl "net/url"
	"strings"

	"github.com/pkg/errors"
)

var errSkippedURL = errors.New("skip processing url")

//nolint:gocyclo,unparam
func (arc *Archiver) processURL(ctx context.Context, url string, parentURL string, embedded ...bool) ([]byte, string, error) {
	// Parse embedded value
	isEmbedded := len(embedded) != 0 && embedded[0]

	// Make sure this URL is not empty, data or hash. If yes, just skip it.
	url = strings.TrimSpace(url)
	if url == "" || strings.HasPrefix(url, "data:") || strings.HasPrefix(url, "#") {
		return nil, "", errSkippedURL
	}

	// Parse URL to make sure it's valid request URL. If not, there might be
	// some error while preparing document, so just skip this URL
	parsedURL, err := nurl.ParseRequestURI(url)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Hostname() == "" {
		return nil, "", errSkippedURL
	}

	// Check in cache to see if this URL already processed
	arc.RLock()
	cache, cacheExist := arc.Cache[url]
	arc.RUnlock()

	if cacheExist {
		arc.logURL(url, parentURL, true)
		return cache.Data, cache.ContentType, nil
	}

	// Download the resource, use semaphore to limit concurrent downloads
	arc.logURL(url, parentURL, false)
	err = arc.dlSemaphore.Acquire(ctx, 1)
	if err != nil {
		return nil, "", err
	}

	resp, err := arc.downloadFile(url, parentURL)
	arc.dlSemaphore.Release(1)
	if err != nil {
		// In strict CSS mode, resource errors for stylesheet resources are
		// never masked by SkipResourceURLError.
		if arc.SkipResourceURLError && !strictResourceErrorFromContext(ctx) {
			return nil, "", errSkippedURL
		} else {
			return nil, "", fmt.Errorf("download failed: %w", err)
		}
	}
	defer resp.Body.Close()

	// Strict CSS mode requires assets referenced from a stylesheet to be
	// successfully retrieved; a 4xx/5xx body is not a usable resource.
	if strictResourceErrorFromContext(ctx) && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		return nil, "", fmt.Errorf("download failed for %s: unexpected status code %d", url, resp.StatusCode)
	}

	// Get content type
	contentType := resp.Header.Get("Content-Type")
	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		contentType = "text/plain"
	}
	mediaType := contentType
	if parsedType, _, err := mime.ParseMediaType(contentType); err == nil {
		mediaType = strings.ToLower(parsedType)
	}

	// Read content of response body. If the downloaded file is HTML
	// or CSS it need to be processed again
	var bodyContent []byte

	switch {
	case mediaType == "text/html" && isEmbedded:
		newHTML, err := arc.processHTML(ctx, resp.Body, parsedURL, false)
		if err == nil {
			bodyContent = s2b(newHTML)
		} else {
			return nil, "", err
		}

	case mediaType == "text/css":
		// Relative references in a stylesheet are resolved against the
		// final URL of its GET request, after following redirects.
		finalURL := parsedURL
		if resp.Request != nil && resp.Request.URL != nil {
			finalURL = resp.Request.URL
		}

		rawCSS, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, "", fmt.Errorf("failed to read CSS: %w", err)
		}
		cssContent, err := decodeCSSCharset(rawCSS, contentType)
		if err != nil {
			return nil, "", err
		}

		newCSS, err := arc.processCSS(ctx, strings.NewReader(cssContent), finalURL)
		if err == nil {
			bodyContent = s2b(newCSS)
		} else {
			return nil, "", err
		}

	default:
		bodyContent, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, "", err
		}
	}

	// Save data URL to cache
	arc.Lock()
	arc.Cache[url] = Asset{
		Data:        bodyContent,
		ContentType: contentType,
	}
	arc.Unlock()

	return bodyContent, contentType, nil
}
