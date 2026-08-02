package proxator

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStickyEndpoints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts StickyOptions
		want []string
	}{
		{
			name: "indexed sticky sessions by default",
			opts: StickyOptions{
				Username: "myuser",
				Password: "mypass",
				Host:     "gate.example.net",
				Port:     "8000",
				Count:    3,
			},
			want: []string{
				"http://myuser-1:mypass@gate.example.net:8000",
				"http://myuser-2:mypass@gate.example.net:8000",
				"http://myuser-3:mypass@gate.example.net:8000",
			},
		},
		{
			name: "no credentials",
			opts: StickyOptions{Host: "gate.example.net", Port: "8000", Count: 2},
			want: []string{
				"http://gate.example.net:8000",
				"http://gate.example.net:8000",
			},
		},
		{
			name: "username without a password",
			opts: StickyOptions{Username: "myuser", Host: "gate.example.net", Port: "8000", Count: 1},
			want: []string{"http://myuser-1@gate.example.net:8000"},
		},
		{
			name: "credentials are escaped",
			opts: StickyOptions{
				Username: "user",
				Password: "p@ss:word",
				Host:     "gate.example.net",
				Port:     "8000",
				Count:    1,
			},
			want: []string{"http://user-1:p%40ss%3Aword@gate.example.net:8000"},
		},
		{
			name: "custom scheme",
			opts: StickyOptions{
				Scheme:   "socks5",
				Username: "user",
				Password: "pass",
				Host:     "gate.example.net",
				Port:     "1080",
				Count:    1,
			},
			want: []string{"socks5://user-1:pass@gate.example.net:1080"},
		},
		{
			name: "custom username format",
			opts: StickyOptions{
				Username:       "user",
				Password:       "pass",
				Host:           "gate.example.net",
				Port:           "8000",
				Count:          2,
				UsernameFormat: "%s_session_%d",
			},
			want: []string{
				"http://user_session_1:pass@gate.example.net:8000",
				"http://user_session_2:pass@gate.example.net:8000",
			},
		},
		{
			// A single-verb format means "reuse the same username everywhere",
			// and must not leave fmt's %!(EXTRA ...) marker behind.
			name: "single verb format drops the index",
			opts: StickyOptions{
				Username:       "user",
				Password:       "pass",
				Host:           "gate.example.net",
				Port:           "8000",
				Count:          3,
				UsernameFormat: "%s",
			},
			want: []string{
				"http://user:pass@gate.example.net:8000",
				"http://user:pass@gate.example.net:8000",
				"http://user:pass@gate.example.net:8000",
			},
		},
		{
			name: "index leads the username",
			opts: StickyOptions{
				Username:       "user",
				Host:           "gate.example.net",
				Port:           "8000",
				Count:          2,
				UsernameFormat: "%[2]d-%[1]s",
			},
			want: []string{
				"http://1-user@gate.example.net:8000",
				"http://2-user@gate.example.net:8000",
			},
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				t.Parallel()
				got, err := StickyEndpoints(tt.opts)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("StickyEndpoints() =\n%q\nwant\n%q", got, tt.want)
				}
			},
		)
	}
}

func TestStickyEndpoints_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts StickyOptions
	}{
		{"missing host", StickyOptions{Port: "8000", Count: 1}},
		{"missing port", StickyOptions{Host: "gate.example.net", Count: 1}},
		{"zero count", StickyOptions{Host: "gate.example.net", Port: "8000"}},
		{"negative count", StickyOptions{Host: "gate.example.net", Port: "8000", Count: -1}},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				t.Parallel()
				got, err := StickyEndpoints(tt.opts)
				if err == nil {
					t.Fatal("expected an error")
				}
				if got != nil {
					t.Fatalf("expected no endpoints alongside the error, got %q", got)
				}
			},
		)
	}
}

func TestStickyEndpoints_FeedsPoolConfig(t *testing.T) {
	t.Parallel()

	endpoints, err := StickyEndpoints(
		StickyOptions{
			Username: "user",
			Password: "pass",
			Host:     "gate.example.net",
			Port:     "8000",
			Count:    4,
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg := PoolConfig{Name: "main", Endpoints: endpoints}
	if err := cfg.validate(); err != nil {
		t.Fatalf("expected generated endpoints to validate, got: %v", err)
	}
}

func TestRenderUsername(t *testing.T) {
	t.Parallel()

	tests := []struct {
		format string
		want   string
	}{
		{"%s-%d", "user-7"},
		{"%s_session_%d", "user_session_7"},
		{"%s", "user"},
		{"session%[2]d-%[1]s", "session7-user"},
	}

	for _, tt := range tests {
		t.Run(
			tt.format, func(t *testing.T) {
				t.Parallel()
				if got := renderUsername(tt.format, "user", 7); got != tt.want {
					t.Fatalf("renderUsername(%q) = %q, want %q", tt.format, got, tt.want)
				}
			},
		)
	}
}

func TestPoolConfig_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     PoolConfig
		wantErr string
	}{
		{
			name: "valid",
			cfg:  PoolConfig{Name: "residential", Endpoints: []string{"http://gate.example.net:8000"}},
		},
		{
			name: "valid with health checks",
			cfg: PoolConfig{
				Name:                "residential",
				Endpoints:           []string{"http://gate.example.net:8000"},
				HealthCheckInterval: time.Minute,
				HealthCheckURL:      "https://example.com/health",
			},
		},
		{
			name:    "missing name",
			cfg:     PoolConfig{Endpoints: []string{"http://gate.example.net:8000"}},
			wantErr: "pool name is required",
		},
		{
			name:    "no endpoints",
			cfg:     PoolConfig{Name: "residential"},
			wantErr: `pool "residential" has no endpoints`,
		},
		{
			name:    "empty endpoint",
			cfg:     PoolConfig{Name: "residential", Endpoints: []string{"http://gate.example.net:8000", ""}},
			wantErr: `pool "residential" endpoint 1 is empty`,
		},
		{
			name:    "unparseable endpoint",
			cfg:     PoolConfig{Name: "residential", Endpoints: []string{"://gate.example.net:8000"}},
			wantErr: "is not a valid URL",
		},
		{
			name: "health checks without a URL",
			cfg: PoolConfig{
				Name:                "residential",
				Endpoints:           []string{"http://gate.example.net:8000"},
				HealthCheckInterval: time.Minute,
			},
			wantErr: "no HealthCheckURL",
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				t.Parallel()
				err := tt.cfg.validate()
				if tt.wantErr == "" {
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
					return
				}
				if err == nil {
					t.Fatalf("expected an error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected an error containing %q, got: %v", tt.wantErr, err)
				}
			},
		)
	}
}

func TestPoolConfig_Validate_DomainBlockSettings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ttl     time.Duration
		penalty float64
		wantErr string
	}{
		{name: "defaults", ttl: 0, penalty: 0},
		{name: "custom values", ttl: time.Hour, penalty: 0.25},
		{name: "full weight", ttl: time.Minute, penalty: 1},
		{name: "negative TTL", ttl: -time.Second, wantErr: "DomainBlockTTL"},
		{name: "negative penalty", penalty: -0.1, wantErr: "DomainBlockPenalty"},
		{name: "penalty above one", penalty: 1.1, wantErr: "DomainBlockPenalty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := PoolConfig{
				Name:               "main",
				Endpoints:          []string{testProxyURL},
				DomainBlockTTL:     tt.ttl,
				DomainBlockPenalty: tt.penalty,
			}
			err := cfg.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate returned an error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validate error = %v, want message containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestPoolConfig_Defaults(t *testing.T) {
	t.Parallel()

	var zero PoolConfig
	if got := zero.sessionPoolSize(); got != defaultSessionPoolSize {
		t.Fatalf("expected session pool size %d, got %d", defaultSessionPoolSize, got)
	}
	if got := zero.healthCheckTimeout(); got != defaultHealthCheckTimeout {
		t.Fatalf("expected health check timeout %v, got %v", defaultHealthCheckTimeout, got)
	}

	negative := PoolConfig{SessionPoolSize: -1, HealthCheckTimeout: -time.Second}
	if got := negative.sessionPoolSize(); got != defaultSessionPoolSize {
		t.Fatalf("expected a negative session pool size to fall back, got %d", got)
	}
	if got := negative.healthCheckTimeout(); got != defaultHealthCheckTimeout {
		t.Fatalf("expected a negative timeout to fall back, got %v", got)
	}

	set := PoolConfig{SessionPoolSize: 4, HealthCheckTimeout: time.Second}
	if got := set.sessionPoolSize(); got != 4 {
		t.Fatalf("expected 4, got %d", got)
	}
	if got := set.healthCheckTimeout(); got != time.Second {
		t.Fatalf("expected 1s, got %v", got)
	}
}

func TestRetryConfig_WithDefaults(t *testing.T) {
	t.Parallel()

	defaults := DefaultRetryConfig()

	t.Run(
		"zero value matches the defaults", func(t *testing.T) {
			t.Parallel()
			var zero RetryConfig
			if got := zero.withDefaults(); got != defaults {
				t.Fatalf("expected %+v, got %+v", defaults, got)
			}
		},
	)

	t.Run(
		"negative values fall back", func(t *testing.T) {
			t.Parallel()
			negative := RetryConfig{
				MaxAttempts:    -1,
				InitialDelay:   -time.Second,
				MaxDelay:       -time.Second,
				CooldownPeriod: -time.Second,
				MaxCooldown:    -time.Second,
				FailThreshold:  -1,
			}
			if got := negative.withDefaults(); got != defaults {
				t.Fatalf("expected %+v, got %+v", defaults, got)
			}
		},
	)

	t.Run(
		"set fields survive", func(t *testing.T) {
			t.Parallel()
			partial := RetryConfig{MaxAttempts: 7, FailThreshold: 1}
			got := partial.withDefaults()

			if got.MaxAttempts != 7 {
				t.Fatalf("expected MaxAttempts 7, got %d", got.MaxAttempts)
			}
			if got.FailThreshold != 1 {
				t.Fatalf("expected FailThreshold 1, got %d", got.FailThreshold)
			}
			if got.InitialDelay != defaults.InitialDelay {
				t.Fatalf("expected InitialDelay %v, got %v", defaults.InitialDelay, got.InitialDelay)
			}
			if got.MaxDelay != defaults.MaxDelay {
				t.Fatalf("expected MaxDelay %v, got %v", defaults.MaxDelay, got.MaxDelay)
			}
			if got.CooldownPeriod != defaults.CooldownPeriod {
				t.Fatalf("expected CooldownPeriod %v, got %v", defaults.CooldownPeriod, got.CooldownPeriod)
			}
			if got.MaxCooldown != defaults.MaxCooldown {
				t.Fatalf("expected MaxCooldown %v, got %v", defaults.MaxCooldown, got.MaxCooldown)
			}
		},
	)

	t.Run(
		"idempotent", func(t *testing.T) {
			t.Parallel()
			once := RetryConfig{MaxAttempts: 5}.withDefaults()
			if twice := once.withDefaults(); twice != once {
				t.Fatalf("expected withDefaults to be idempotent, got %+v then %+v", once, twice)
			}
		},
	)
}

func TestRetryConfig_MaxAttempts_ZeroDefaultsToThree(t *testing.T) {
	t.Parallel()

	got := (RetryConfig{}).withDefaults().MaxAttempts
	if got != 3 {
		t.Fatalf("MaxAttempts = %d, want 3", got)
	}
}

func TestRetryConfig_FailThreshold(t *testing.T) {
	t.Parallel()

	var zero RetryConfig
	if got := zero.failThreshold(); got != defaultFailThreshold {
		t.Fatalf("expected %d, got %d", defaultFailThreshold, got)
	}
	if got := (RetryConfig{FailThreshold: -2}).failThreshold(); got != defaultFailThreshold {
		t.Fatalf("expected a negative threshold to fall back, got %d", got)
	}
	if got := (RetryConfig{FailThreshold: 9}).failThreshold(); got != 9 {
		t.Fatalf("expected 9, got %d", got)
	}
}

func TestState_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state State
		want  string
	}{
		{StateAlive, "alive"},
		{StateDead, "dead"},
		{StateCooldown, "cooldown"},
		{State(99), "unknown"},
	}

	for _, tt := range tests {
		t.Run(
			tt.want, func(t *testing.T) {
				t.Parallel()
				if got := tt.state.String(); got != tt.want {
					t.Fatalf("State(%d).String() = %q, want %q", tt.state, got, tt.want)
				}
			},
		)
	}
}
