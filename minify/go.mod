module github.com/zweiundeins/lith/minify

go 1.27.4

require (
	github.com/evanw/esbuild v0.28.2
	github.com/tdewolff/minify/v2 v2.24.25
	github.com/tdewolff/parse/v2 v2.8.20
	github.com/zweiundeins/lith v0.5.9
)

require (
	github.com/andybalholm/brotli v1.3.0 // indirect
	github.com/klauspost/compress v1.20.4 // indirect
	golang.org/x/sys v0.49.0 // indirect
)

replace github.com/zweiundeins/lith => ../
