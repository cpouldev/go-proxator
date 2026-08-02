package proxator

import (
	"fmt"
	"regexp"
	"strings"
)

// DefaultBlockedStatusCodes are HTTP status codes treated as definitive blocks
// without inspecting the response body.
var DefaultBlockedStatusCodes = []int{
	401, // Unauthorized
	403, // Forbidden
	407, // Proxy Authentication Required
	429, // Too Many Requests
	503, // Service Unavailable
}

// DefaultBodyPatterns are regular expressions matched against the body of
// non-2xx responses to spot interstitial challenge and bot-wall pages.
var DefaultBodyPatterns = []string{
	`(?i)cloudflare`,
	`(?i)captcha`,
	`(?i)access.denied`,
	`(?i)forbidden`,
	`(?i)rate.limit`,
	`(?i)too.many.requests`,
	`(?i)please.verify`,
	`(?i)security.check`,
	`(?i)bot.detection`,
	`(?i)suspicious.activity`,
	`(?i)cf-ray`,
	`(?i)__cf_chl`,
	`(?i)hcaptcha`,
	`(?i)recaptcha`,
	`(?i)turnstile`,
	`(?i)just.a.moment`,
	`(?i)checking.your.browser`,
	`(?i)ddos.protection`,
	`(?i)attention.required`,
}

// DefaultErrorPatterns are lowercase substrings matched against transport
// error strings to spot blocking that surfaced as an error rather than a
// response.
var DefaultErrorPatterns = []string{
	"403",
	"forbidden",
	"blocked",
	"captcha",
	"challenge",
	"rate limit",
	"too many requests",
}

// DetectorConfig customises block detection.
type DetectorConfig struct {
	// StatusCodes replaces DefaultBlockedStatusCodes when non-empty.
	StatusCodes []int

	// BodyPatterns replaces DefaultBodyPatterns when non-empty. Entries are
	// regular expressions; an invalid one makes NewBlockDetectorWith fail.
	BodyPatterns []string

	// ErrorPatterns replaces DefaultErrorPatterns when non-empty. Entries are
	// matched as lowercase substrings.
	ErrorPatterns []string

	// DisableCloudflareHeaderCheck turns off the heuristic that treats a >=400
	// response carrying genuine Cloudflare headers as a block.
	DisableCloudflareHeaderCheck bool
}

// BlockDetector classifies responses and transport errors as blocking.
// A zero-value BlockDetector is not usable — construct one with
// NewBlockDetector or NewBlockDetectorWith.
type BlockDetector struct {
	statusCodes    map[int]bool
	bodyPatterns   []*regexp.Regexp
	errorPatterns  []string
	checkCFHeaders bool
}

// NewBlockDetector returns a detector using the package defaults.
func NewBlockDetector() *BlockDetector {
	d, err := NewBlockDetectorWith(DetectorConfig{})
	if err != nil {
		// The default patterns are compile-time constants and always valid.
		panic(fmt.Sprintf("proxator: default block patterns failed to compile: %v", err))
	}
	return d
}

// NewBlockDetectorWith returns a detector built from cfg, falling back to the
// package defaults for any field left empty.
func NewBlockDetectorWith(cfg DetectorConfig) (*BlockDetector, error) {
	// A nil field selects the defaults; an empty non-nil field disables that
	// check, which callers cannot express if the fallback triggers on length.
	statusCodes := cfg.StatusCodes
	if statusCodes == nil {
		statusCodes = DefaultBlockedStatusCodes
	}
	bodyPatterns := cfg.BodyPatterns
	if bodyPatterns == nil {
		bodyPatterns = DefaultBodyPatterns
	}
	errorPatterns := cfg.ErrorPatterns
	if errorPatterns == nil {
		errorPatterns = DefaultErrorPatterns
	}

	codes := make(map[int]bool, len(statusCodes))
	for _, code := range statusCodes {
		codes[code] = true
	}

	compiled := make([]*regexp.Regexp, 0, len(bodyPatterns))
	for _, p := range bodyPatterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("proxator: compiling body pattern %q: %w", p, err)
		}
		compiled = append(compiled, re)
	}

	lowered := make([]string, len(errorPatterns))
	for i, p := range errorPatterns {
		lowered[i] = strings.ToLower(p)
	}

	return &BlockDetector{
		statusCodes:    codes,
		bodyPatterns:   compiled,
		errorPatterns:  lowered,
		checkCFHeaders: !cfg.DisableCloudflareHeaderCheck,
	}, nil
}

// IsBlocked reports whether a response indicates blocking.
//
// Successful (2xx) responses short-circuit before any body scan, so the common
// path costs one map lookup rather than running every pattern over the body.
func (d *BlockDetector) IsBlocked(resp *Response) bool {
	if d == nil || resp == nil {
		return false
	}

	if d.statusCodes[resp.StatusCode] {
		return true
	}

	// 2xx is never a block — skip the expensive body and header scan.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return false
	}

	body := string(resp.Body)
	for _, re := range d.bodyPatterns {
		if re.MatchString(body) {
			return true
		}
	}

	if d.checkCFHeaders && resp.StatusCode >= 400 {
		for key := range resp.Header {
			if isCloudflareHeader(key) {
				return true
			}
		}
	}

	return false
}

// isCloudflareHeader matches genuine Cloudflare headers (cf-ray,
// cf-cache-status, ...) while excluding Amazon CloudFront's x-amz-cf-* family,
// which appears on every response from a CloudFront-served origin and would
// otherwise flag healthy traffic as blocked.
func isCloudflareHeader(key string) bool {
	k := strings.ToLower(key)
	if strings.HasPrefix(k, "x-amz-cf-") {
		return false
	}
	return strings.HasPrefix(k, "cf-") || strings.Contains(k, "cloudflare")
}

// IsBlockedError reports whether a transport error indicates blocking.
func (d *BlockDetector) IsBlockedError(err error) bool {
	if d == nil || err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())
	for _, pattern := range d.errorPatterns {
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}
