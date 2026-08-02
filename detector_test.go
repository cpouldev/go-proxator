package proxator

import (
	"errors"
	"testing"
)

func header(pairs map[string][]string) *Response {
	resp := &Response{StatusCode: 404}
	resp.Header = pairs
	return resp
}

func TestBlockDetector_StatusCodes(t *testing.T) {
	t.Parallel()

	d := NewBlockDetector()

	for _, code := range DefaultBlockedStatusCodes {
		if !d.IsBlocked(&Response{StatusCode: code}) {
			t.Errorf("expected status %d to be blocked", code)
		}
	}

	notBlocked := []int{200, 201, 301, 302, 404, 500, 502}
	for _, code := range notBlocked {
		if d.IsBlocked(&Response{StatusCode: code}) {
			t.Errorf("expected status %d to NOT be blocked (no body, no headers)", code)
		}
	}
}

func TestBlockDetector_2xxShortCircuit(t *testing.T) {
	t.Parallel()

	d := NewBlockDetector()

	// A 2xx body mentioning Cloudflare is a page about Cloudflare, not a block.
	resp := &Response{
		StatusCode: 200,
		Body:       []byte("powered by cloudflare"),
	}
	if d.IsBlocked(resp) {
		t.Fatal("expected 2xx to bypass body scanning")
	}

	// Headers are skipped by the same short-circuit.
	withHeaders := header(map[string][]string{"Cf-Ray": {"abc123"}})
	withHeaders.StatusCode = 200
	if d.IsBlocked(withHeaders) {
		t.Fatal("expected 2xx with Cloudflare headers to NOT be blocked")
	}
}

func TestBlockDetector_BodyPatterns(t *testing.T) {
	t.Parallel()

	d := NewBlockDetector()

	blockedBodies := []string{
		"<html>Cloudflare challenge</html>",
		"Please complete the captcha",
		"Access Denied - your IP has been blocked",
		"Rate limit exceeded",
		"<div class='hcaptcha'></div>",
		"Checking your browser before accessing",
		"Just a moment...",
		"DDoS protection by",
		"Attention Required!",
	}

	for _, body := range blockedBodies {
		// Non-blocking status code, so only the body can trigger detection.
		resp := &Response{StatusCode: 404, Body: []byte(body)}
		if !d.IsBlocked(resp) {
			t.Errorf("expected body %q to be detected as blocked", body)
		}
	}
}

func TestBlockDetector_CleanBodyNotBlocked(t *testing.T) {
	t.Parallel()

	d := NewBlockDetector()

	resp := &Response{
		StatusCode: 404,
		Body:       []byte("<html><body>Page not found</body></html>"),
	}
	if d.IsBlocked(resp) {
		t.Fatal("expected a clean 404 page to not be blocked")
	}
}

func TestBlockDetector_CloudflareHeaders(t *testing.T) {
	t.Parallel()

	d := NewBlockDetector()

	tests := []struct {
		name    string
		headers map[string][]string
		want    bool
	}{
		{
			name:    "cf-ray",
			headers: map[string][]string{"Cf-Ray": {"abc123"}, "Cf-Cache-Status": {"HIT"}},
			want:    true,
		},
		{
			name:    "vendor header naming the product",
			headers: map[string][]string{"X-Powered-By-Cloudflare": {"1"}},
			want:    true,
		},
		{
			name:    "unrelated headers",
			headers: map[string][]string{"Content-Type": {"text/html"}, "Server": {"nginx"}},
			want:    false,
		},
		// Amazon CloudFront stamps x-amz-cf-* on every response it serves. Treating
		// those as Cloudflare would flag perfectly healthy traffic as blocked.
		{
			name: "cloudfront is not cloudflare",
			headers: map[string][]string{
				"X-Amz-Cf-Id":  {"abc123=="},
				"X-Amz-Cf-Pop": {"LHR50-C1"},
			},
			want: false,
		},
		{
			name: "cloudfront alongside a real cloudflare header",
			headers: map[string][]string{
				"X-Amz-Cf-Id": {"abc123=="},
				"Cf-Ray":      {"def456"},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				t.Parallel()
				if got := d.IsBlocked(header(tt.headers)); got != tt.want {
					t.Fatalf("IsBlocked(%v) = %t, want %t", tt.headers, got, tt.want)
				}
			},
		)
	}
}

func TestIsCloudflareHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key  string
		want bool
	}{
		{"cf-ray", true},
		{"CF-RAY", true},
		{"Cf-Cache-Status", true},
		{"x-cloudflare-worker", true},
		{"x-amz-cf-id", false},
		{"X-Amz-Cf-Pop", false},
		{"content-type", false},
		{"server", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(
			tt.key, func(t *testing.T) {
				t.Parallel()
				if got := isCloudflareHeader(tt.key); got != tt.want {
					t.Fatalf("isCloudflareHeader(%q) = %t, want %t", tt.key, got, tt.want)
				}
			},
		)
	}
}

func TestBlockDetector_HeaderCheckOnlyAppliesTo4xxAndUp(t *testing.T) {
	t.Parallel()

	d := NewBlockDetector()

	resp := header(map[string][]string{"Cf-Ray": {"abc123"}})
	resp.StatusCode = 301
	if d.IsBlocked(resp) {
		t.Fatal("expected a 3xx with Cloudflare headers to NOT be blocked")
	}
}

func TestBlockDetector_NilSafety(t *testing.T) {
	t.Parallel()

	d := NewBlockDetector()
	if d.IsBlocked(nil) {
		t.Fatal("expected nil response to not be blocked")
	}

	var nilDetector *BlockDetector
	if nilDetector.IsBlocked(&Response{StatusCode: 403}) {
		t.Fatal("expected nil detector to report false")
	}
	if nilDetector.IsBlockedError(errors.New("403 forbidden")) {
		t.Fatal("expected nil detector to report false for errors")
	}
}

func TestBlockDetector_IsBlockedError(t *testing.T) {
	t.Parallel()

	d := NewBlockDetector()

	blocked := []string{
		"received 403 from server",
		"forbidden access to resource",
		"request blocked by firewall",
		"captcha verification required",
		"challenge page detected",
		"rate limit reached",
		"too many requests from this IP",
	}
	for _, msg := range blocked {
		if !d.IsBlockedError(errors.New(msg)) {
			t.Errorf("expected error %q to be detected as blocked", msg)
		}
	}

	notBlocked := []string{
		"connection timeout",
		"DNS resolution failed",
		"unexpected EOF",
	}
	for _, msg := range notBlocked {
		if d.IsBlockedError(errors.New(msg)) {
			t.Errorf("expected error %q to NOT be detected as blocked", msg)
		}
	}

	if d.IsBlockedError(nil) {
		t.Fatal("expected nil error to not be blocked")
	}
}

func TestNewBlockDetectorWith_CustomStatusCodes(t *testing.T) {
	t.Parallel()

	d, err := NewBlockDetectorWith(DetectorConfig{StatusCodes: []int{418}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !d.IsBlocked(&Response{StatusCode: 418}) {
		t.Fatal("expected the custom status code to be blocked")
	}
	// Custom codes replace the defaults rather than extending them.
	if d.IsBlocked(&Response{StatusCode: 403}) {
		t.Fatal("expected the default status codes to be replaced")
	}
}

func TestNewBlockDetectorWith_CustomBodyPatterns(t *testing.T) {
	t.Parallel()

	d, err := NewBlockDetectorWith(DetectorConfig{BodyPatterns: []string{`(?i)go\s+away`}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !d.IsBlocked(&Response{StatusCode: 404, Body: []byte("GO AWAY")}) {
		t.Fatal("expected the custom body pattern to match")
	}
	if d.IsBlocked(&Response{StatusCode: 404, Body: []byte("cloudflare challenge")}) {
		t.Fatal("expected the default body patterns to be replaced")
	}
}

func TestNewBlockDetectorWith_CustomErrorPatterns(t *testing.T) {
	t.Parallel()

	d, err := NewBlockDetectorWith(DetectorConfig{ErrorPatterns: []string{"Tarpit"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !d.IsBlockedError(errors.New("dropped into a tarpit")) {
		t.Fatal("expected the custom error pattern to match case-insensitively")
	}
	if d.IsBlockedError(errors.New("403 forbidden")) {
		t.Fatal("expected the default error patterns to be replaced")
	}
}

func TestNewBlockDetectorWith_DisableCloudflareHeaderCheck(t *testing.T) {
	t.Parallel()

	d, err := NewBlockDetectorWith(DetectorConfig{DisableCloudflareHeaderCheck: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if d.IsBlocked(header(map[string][]string{"Cf-Ray": {"abc123"}})) {
		t.Fatal("expected the header heuristic to be disabled")
	}
	// Status codes and body patterns keep working.
	if !d.IsBlocked(&Response{StatusCode: 403}) {
		t.Fatal("expected status detection to survive the header opt-out")
	}
}

func TestNewBlockDetectorWith_InvalidBodyPattern(t *testing.T) {
	t.Parallel()

	d, err := NewBlockDetectorWith(DetectorConfig{BodyPatterns: []string{"([unterminated"}})
	if err == nil {
		t.Fatal("expected an error from an invalid body pattern")
	}
	if d != nil {
		t.Fatal("expected a nil detector alongside the error")
	}
}

func TestNewBlockDetector_UsesDefaults(t *testing.T) {
	t.Parallel()

	fromDefaults := NewBlockDetector()
	fromEmptyConfig, err := NewBlockDetectorWith(DetectorConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(fromDefaults.statusCodes) != len(fromEmptyConfig.statusCodes) {
		t.Fatal("expected NewBlockDetector to match an empty DetectorConfig")
	}
	if len(fromDefaults.bodyPatterns) != len(DefaultBodyPatterns) {
		t.Fatalf("expected %d body patterns, got %d", len(DefaultBodyPatterns), len(fromDefaults.bodyPatterns))
	}
	if len(fromDefaults.errorPatterns) != len(DefaultErrorPatterns) {
		t.Fatalf("expected %d error patterns, got %d", len(DefaultErrorPatterns), len(fromDefaults.errorPatterns))
	}
	if !fromDefaults.checkCFHeaders {
		t.Fatal("expected the Cloudflare header check to be on by default")
	}
}
