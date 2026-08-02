package proxator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// HTTPFactory creates sessions backed by Go's standard net/http transport.
// It uses the standard TLS certificate verification behavior. Headers are
// applied through http.Header, so their field order is not guaranteed on the
// wire. Request bodies supplied as string, []byte, or io.Reader are sent raw;
// other values are JSON-encoded and receive an application/json content type
// when no content type was supplied. Use it when browser TLS fingerprinting is
// unnecessary.
type HTTPFactory struct{}

// New creates a standard HTTP session routed through an HTTP or HTTPS proxy
// URL. Other proxy schemes, including SOCKS5, are rejected.
func (HTTPFactory) New(proxyURL string) (Session, error) {
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parsing proxy URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported net/http proxy scheme %q", parsed.Scheme)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(parsed)
	return &httpSession{
		client:    &http.Client{Transport: transport},
		transport: transport,
	}, nil
}

type httpSession struct {
	client    *http.Client
	transport *http.Transport
}

func (s *httpSession) Do(ctx context.Context, req Request) (*Response, error) {
	body, jsonBody, err := httpRequestBody(req.Body)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, body)
	if err != nil {
		return nil, fmt.Errorf("creating HTTP request: %w", err)
	}
	for _, header := range req.Headers {
		if len(header) < 2 {
			continue
		}
		for _, value := range header[1:] {
			httpReq.Header.Add(header[0], value)
		}
	}
	if jsonBody && httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	httpResp, err := s.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	responseBody, readErr := io.ReadAll(httpResp.Body)
	closeErr := httpResp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("reading HTTP response: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("closing HTTP response: %w", closeErr)
	}

	return &Response{
		StatusCode: httpResp.StatusCode,
		Status:     httpResp.Status,
		Body:       responseBody,
		Header:     httpResp.Header.Clone(),
	}, nil
}

func (s *httpSession) Close() {
	s.transport.CloseIdleConnections()
}

func httpRequestBody(body any) (io.Reader, bool, error) {
	switch value := body.(type) {
	case nil:
		return nil, false, nil
	case io.Reader:
		return value, false, nil
	case []byte:
		return bytes.NewReader(value), false, nil
	case string:
		return strings.NewReader(value), false, nil
	default:
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, false, fmt.Errorf("encoding net/http request body as JSON: %w", err)
		}
		return bytes.NewReader(encoded), true, nil
	}
}
