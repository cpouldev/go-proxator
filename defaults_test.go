package proxator

import (
	"errors"
	"strings"
	"testing"
)

// These cases replace package-level Default sets. They must not call
// t.Parallel(): the parallel cases elsewhere in this package read the same
// variables and only stay race-free because sequential tests finish, and their
// cleanups restore the defaults, before any parallel test resumes.

func TestNewBlockDetector_HonorsReplacedDefaults(t *testing.T) {
	original := DefaultBlockedStatusCodes
	DefaultBlockedStatusCodes = []int{418}
	t.Cleanup(func() { DefaultBlockedStatusCodes = original })

	detector := NewBlockDetector()
	if !detector.IsBlocked(&Response{StatusCode: 418}) {
		t.Fatal("replaced status-code default was not applied")
	}
	if detector.IsBlocked(&Response{StatusCode: 403}) {
		t.Fatal("built-in status code still blocked after the default was replaced")
	}
}

func TestNewBlockDetector_ClearedDefaultsDisableCheck(t *testing.T) {
	for name, empty := range map[string][]int{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			original := DefaultBlockedStatusCodes
			DefaultBlockedStatusCodes = empty
			t.Cleanup(func() { DefaultBlockedStatusCodes = original })

			if NewBlockDetector().IsBlocked(&Response{StatusCode: 403}) {
				t.Fatal("cleared status-code default still blocked a 403")
			}
		})
	}
}

func TestNewBlockDetectorWith_CopiesDefaults(t *testing.T) {
	original := DefaultBlockedStatusCodes
	DefaultBlockedStatusCodes = []int{418}
	t.Cleanup(func() { DefaultBlockedStatusCodes = original })

	detector, err := NewBlockDetectorWith(DetectorConfig{})
	if err != nil {
		t.Fatalf("NewBlockDetectorWith: %v", err)
	}
	DefaultBlockedStatusCodes[0] = 200

	if !detector.IsBlocked(&Response{StatusCode: 418}) {
		t.Fatal("mutating the default set changed an already-built detector")
	}
}

func TestNew_InvalidDefaultPatternsReturnError(t *testing.T) {
	original := DefaultBodyPatterns
	DefaultBodyPatterns = []string{"(unclosed"}
	t.Cleanup(func() { DefaultBodyPatterns = original })

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("New panicked instead of returning an error: %v", r)
		}
	}()

	cfg := Config{Pools: []PoolConfig{{
		Name:            "main",
		Endpoints:       []string{testProxyURL},
		SessionPoolSize: 1,
	}}}
	_, err := New(cfg)
	if err == nil {
		t.Fatal("New accepted a default pattern that cannot compile")
	}
	if !strings.Contains(err.Error(), "body pattern") {
		t.Fatalf("New returned %v, want a body-pattern compile error", err)
	}
}

func TestNewTransientClassifier_HonorsReplacedDefaults(t *testing.T) {
	original := DefaultUnambiguousPatterns
	DefaultUnambiguousPatterns = []string{"temporary test failure"}
	t.Cleanup(func() { DefaultUnambiguousPatterns = original })

	classifier := NewTransientClassifier()
	if !classifier.IsTransient(errors.New("temporary test failure")) {
		t.Fatal("replaced transient default was not applied")
	}
}

func TestIsTransient_HonorsReplacedDefaults(t *testing.T) {
	original := DefaultUnambiguousPatterns
	DefaultUnambiguousPatterns = []string{"temporary test failure"}
	t.Cleanup(func() { DefaultUnambiguousPatterns = original })

	if !IsTransient(errors.New("temporary test failure")) {
		t.Fatal("package-level IsTransient ignored the replaced default")
	}
	if IsTransient(errors.New("connection reset by peer")) {
		t.Fatal("package-level IsTransient kept a built-in pattern after replacement")
	}
}
