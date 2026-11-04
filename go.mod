module github.com/Alex-Zechariah-02/EAT40005-Gateway

go 1.26.0

toolchain go1.27.3

// The two web applications have no Go code.
ignore (
	./client
	./dashboard
)

require (
	github.com/go-jose/go-jose/v4 v4.1.8
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
	golang.org/x/crypto v0.58.0
	golang.org/x/sys v0.49.0
	modernc.org/sqlite v1.61.1
)

require (
	github.com/dlclark/regexp2 v1.11.5 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/text v0.43.0 // indirect
	modernc.org/libc v1.79.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
