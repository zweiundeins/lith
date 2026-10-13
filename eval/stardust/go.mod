module stardusteval

go 1.27.2

require (
	github.com/zweiundeins/lith v0.0.0
	golang.org/x/crypto v0.57.0
	stardust v0.0.0
)

require (
	github.com/andybalholm/brotli v1.2.6 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.20.3 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.49.0 // indirect
	modernc.org/libc v1.78.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	modernc.org/sqlite v1.60.1 // indirect
)

replace stardust => /tmp/stardust-release/stardust-alpha-2610112101-linux-amd64/abi/bindings/go

replace github.com/zweiundeins/lith => ../..
