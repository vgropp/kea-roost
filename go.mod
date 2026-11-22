module github.com/vgropp/kea-roost

go 1.26.0

require (
	github.com/boombuler/barcode v1.1.1
	github.com/chromedp/cdproto v0.160.3
	github.com/chromedp/chromedp v0.24.0
	github.com/coreos/go-oidc/v3 v3.21.0
	github.com/go-jose/go-jose/v4 v4.1.6
	golang.org/x/crypto v0.59.0
	golang.org/x/oauth2 v0.38.0
)

require (
	github.com/go-json-experiment/json v0.0.0-20260213210345-44df1a37e875 // indirect
	golang.org/x/sys v0.50.0 // indirect
)

// npm puts a Go package of one of its dependencies there
ignore ./node_modules
