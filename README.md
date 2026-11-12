<p align="center">
	<img src="https://raw.githubusercontent.com/go-shiori/obelisk/master/docs/readme/logo.png" alt="Obelisk" width="450">
</p>
<p align="center">Go packages and CLI tool for saving web page as single HTML file</p>
<p align="center">
	<a href="https://choosealicense.com/licenses/mit"><img src="https://img.shields.io/static/v1?label=license&message=MIT&color=5fa6b0"></a>
	<a href="https://goreportcard.com/report/github.com/go-shiori/obelisk"><img src="https://goreportcard.com/badge/github.com/go-shiori/obelisk"></a>
	<a href="https://godoc.org/github.com/go-shiori/obelisk"><img src="https://img.shields.io/static/v1?label=godoc&message=reference&color=5272B4&logo=go"></a>
	<a href="https://www.paypal.me/RadhiFadlillah"><img src="https://img.shields.io/static/v1?label=donate&message=PayPal&color=00457C&logo=paypal"></a>
	<a href="https://ko-fi.com/radhifadlillah"><img src="https://img.shields.io/static/v1?label=donate&message=Ko-fi&color=F16061&logo=ko-fi"></a>
</p>

---

Obelisk is a Go package and CLI tool for saving web page as single HTML file, with all of its assets embedded. It's inspired by the great [Monolith](https://github.com/Y2Z/monolith) and intended as improvement for my old [WARC](https://github.com/go-shiori/warc) package.

## Features

- Embeds all resources (e.g. CSS, image, JavaScript, etc) producing a single HTML5 document that is easy to store and share.
- In case the submitted URL is not HTML (for example a PDF page), Obelisk will still save it as it is.
- Downloading each assets are done concurrently, which make the archival process for a web page is quite fast.
- Accepts cookies, useful for pages that need login or article behind paywall.
- Optional strict CSS mode that resolves and embeds all stylesheet `@import` rules recursively (see below).

## As Go package

Run following command inside your Go project :

```shell
go get -u -v github.com/go-shiori/obelisk
```

Next, include Obelisk in your application :

```go
import "github.com/go-shiori/obelisk"
```

Now you can use Obelisk archival feature for your application. For basic usage you can check the [example](https://github.com/go-shiori/obelisk/blob/master/examples/basic.go).

## As CLI application

You can download the latest version of Obelisk from [release page](https://github.com/go-shiori/obelisk/releases). To build from source, make sure you use `go >= 1.13` then run following commands :

```shell
go get -u -v github.com/go-shiori/obelisk/cmd/obelisk
```

Now you can use it from your terminal :

```shell
$ obelisk -h

CLI tool for saving web page as single HTML file

Usage:
  obelisk [url1] [url2] ... [urlN] [flags]

Flags:
  -z, --gzip                          gzip archival result
  -h, --help                          help for obelisk
  -i, --input string                  path to file which contains URLs
      --insecure                      skip X.509 (TLS) certificate verification
  -c, --load-cookies string           path to Netscape cookie file
      --max-concurrent-download int   max concurrent download at a time (default 10)
      --no-css                        disable CSS styling
      --no-embeds                     remove embedded elements (e.g iframe)
      --no-js                         disable JavaScript
      --no-medias                     remove media elements (e.g img, audio)
  -o, --output string                 path to save archival result
  -q, --quiet                         disable logging
      --skip-resource-url-error       skip process resource url error
      --strict-css                    resolve and embed CSS @import rules strictly (single-file only)
  -t, --timeout int                   maximum time (in second) before request timeout (default 60)
  -u, --user-agent string             set custom user agent
      --verbose                       more verbose logging
```

There are some CLI behavior that I think need to be explained more here :

- The `--input` flag accepts text file that contains list of urls that look like this :

    ```plain
	http://www.domain1.com/some/path
	http://www.domain2.com/some/path
	http://www.domain3.com/some/path
	```

- The `--load-cookies` flag accepts Netscape cookie file that usually look like this :

    ```plain
	# Netscape HTTP Cookie File
	# https://curl.haxx.se/rfc/cookie_spec.html
	# This is a generated file! Do not edit.
	
	#HttpOnly_.google.com	TRUE	/	FALSE	1631153524	KEY	VALUE
	#HttpOnly_.google.com	TRUE	/ads	TRUE	1621062000	KEY	VALUE
	.developers.google.com	TRUE	/	FALSE	1642167486	KEY	VALUE
	```

- If `--output` flag is not specified then Obelisk will generate file name for the archive and save it in current working directory.
- If `--output` flag is set to `-` and there is only one URL to process (either from input file or from CLI arguments) then the default output will be `stdout`.
- If `--output` flag is specified but there are more than one URL to process, Obelisk will generate file name for the archive, but keep using the directory from the specified output path.
- If `--output` flag is specified but it sets to an existing directory, Obelisk will also generate file name for the archive.

### Strict CSS mode

By default Obelisk keeps the historical CSS handling unchanged. Pass
`--strict-css` (or set `Archiver.StrictCSS` when using the Go package) to
enable strict CSS processing:

- `@import` rules are resolved recursively for external stylesheets
  (`<link rel="stylesheet">`), `<style>` elements and inline `style`
  attributes, then embedded in place. Both the string form
  (`@import "a.css"`) and the `url(...)` form are supported, including CSS
  escapes.
- The optional `layer`, `supports(...)` and media conditions of an `@import`
  are preserved, in source order, by wrapping the embedded rules in matching
  `@layer`, `@supports` and `@media` blocks. Multiple imports keep their
  original position and order.
- Relative URLs in a stylesheet (imports and `url(...)` resources) are
  resolved against the final URL of that stylesheet's GET request, after
  following redirects. The stylesheet charset is taken from the HTTP
  `Content-Type`, a BOM or an `@charset` rule, and decoded to UTF-8.
- Stylesheets shared within a single `Archive` call are downloaded only once;
  HTTP retries and redirect hops are performed as usual.
- Comments and ordinary CSS strings are never rewritten. Only real
  `@import` rules and `url(...)` tokens are modified.
- Import cycles, including cycles that involve redirect aliases, fail with an
  error that lists the dependency chain. Any download, read or parse failure
  is returned as an error: a partial archive is never returned, failed
  downloads are not cached, and `--skip-resource-url-error` cannot hide
  errors raised while processing stylesheets.

Strict CSS mode is only available for single-file archives. It is ignored
when `Archiver.WrapDirectory` is set (directory mode keeps the legacy
behavior) and when CSS is disabled with `--no-css`.

## F.A.Q

**Why the name is Obelisk ?**

It's inspired by Monolith, therefore it's Obelisk.

**How does it compare to WARC ?**

My WARC package uses `bolt` database to contain archival result, which make it hard to share and view. I also think my code in WARC is not really easy to understand, so I often confused when I try to add additional feature or refactoring it.

**How does it compare to Monolith ?**

- Both embeds all resources to HTML file, mostly using base64 data URL. The difference is Obelisk will use inline `<script>` and `<style>` for external JavaScript and CSS files. This is done because in many page the browser will struggles to load JavaScript that encoded into data URL. Inlining scripts and styles also make archival result smaller since we don't encode them using base64.
- In Obelisk all request to external URL is disabled by default using Content Security Policy, while in Monolith we need to specify it manually. This is done because in my opinion archive shouldn't need and shouldn't be able to send request to external resources.
- In Obelisk downloading assets are done concurrently. Thanks to this, Obelisk (most of the time) will be faster than Monolith when archiving a web page.

**Why not just contribute to Monolith ?**

- I don't have any knowledge about Rust. I do want to learn it though.
- I have a plan to update [Shiori](https://github.com/go-shiori/shiori), so I need a Go package for archiving web page.

## Attributions

Original logo is created by [Freepik](https://www.flaticon.com/authors/freepik) in theirs [egypt](https://www.flaticon.com/packs/egypt-23) and [desert](https://www.flaticon.com/packs/desert-7) pack, which can be downloaded from [www.flaticon.com](https://www.flaticon.com/).

## License

Obelisk is distributed using [MIT license](https://choosealicense.com/licenses/mit/), which means you can use and modify it however you want. However, if you make an enhancement for it, if possible, please send a pull request. If you like this project, please consider donating to me either via [PayPal](https://www.paypal.me/RadhiFadlillah) or [Ko-Fi](https://ko-fi.com/radhifadlillah).
