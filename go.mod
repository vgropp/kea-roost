module github.com/vgropp/kea-roost

go 1.26.0

require (
	github.com/boombuler/barcode v1.1.0
	github.com/chromedp/cdproto v0.159.1
	github.com/chromedp/chromedp v0.23.0
	golang.org/x/crypto v0.58.0
)

require (
	github.com/go-json-experiment/json v0.0.0-20260213210345-44df1a37e875 // indirect
	golang.org/x/sys v0.49.0 // indirect
)

// npm puts a Go package of one of its dependencies there
ignore ./node_modules
