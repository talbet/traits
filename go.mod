module github.com/talbet/traits

go 1.27

// Builds use a toolchain with the Go 1.27.2-1.27.4 standard-library
// security fixes (net/http, crypto/x509, crypto/tls, net/textproto).
toolchain go1.27.4

require github.com/goccy/go-yaml v1.19.2
