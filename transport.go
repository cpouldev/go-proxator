package proxator

import (
	"context"
	"net/http"
)

// OrderedHeaders represents headers as ordered rows. Each row contains a field
// name followed by one or more values. Adapters that support ordered headers
// can preserve the row order; HTTPFactory applies the rows to http.Header and
// does not guarantee their order on the wire.
type OrderedHeaders [][]string

// Request is the transport-neutral form of one proxied HTTP request.
type Request struct {
	// Method is the HTTP request method.
	Method string

	// URL is the absolute target URL, not the proxy URL.
	URL string

	// Body is the request payload. The built-in adapters send nil, string,
	// []byte, and io.Reader values without JSON encoding and JSON-encode other
	// supported values. Exact support is adapter-specific. A RequestFunc that
	// may be retried should recreate a consumable io.Reader for every attempt.
	Body any

	// Headers are the request header rows.
	Headers OrderedHeaders
}

// Response contains the response data used by routing, block detection, and
// callers. Transport adapters must fully buffer Body before returning. Client
// methods may return both a Response and an error when retries end on a
// blocking response.
type Response struct {
	// StatusCode is the numeric HTTP status code.
	StatusCode int

	// Status is the complete HTTP status, such as "200 OK".
	Status string

	// Body is the fully buffered response payload.
	Body []byte

	// Header contains the response headers.
	Header http.Header
}

// Session sends requests through one proxy endpoint. A Session is borrowed by
// one goroutine at a time and must not be retained after a RequestFunc returns.
// Do must honor ctx and return a non-nil Response whenever it returns a nil
// error. The Client owns the Session and calls Close when finished.
type Session interface {
	Do(context.Context, Request) (*Response, error)
	Close()
}

// SessionFactory creates a transport session configured for proxyURL.
// New calls it eagerly SessionPoolSize times for each endpoint. A successful
// call must return a distinct Session and transfers its ownership to the
// Client; an error must not transfer ownership. Supported proxy URL schemes are
// implementation-specific. Config.SessionFactory defaults to the built-in
// Azure TLS adapter.
type SessionFactory interface {
	New(proxyURL string) (Session, error)
}
