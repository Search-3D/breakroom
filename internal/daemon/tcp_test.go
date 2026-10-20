package daemon

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/search-3d/breakroom/internal/paths"
)

func TestGenerateToken(t *testing.T) {
	a, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 64 || a == b {
		t.Fatalf("tokens = %q %q", a, b)
	}
	for _, c := range a {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("non-hex token %q", a)
		}
	}
}

func TestValidateLoopback(t *testing.T) {
	for _, tc := range []struct {
		addr    string
		wantErr bool
	}{
		{"127.0.0.1:7420", false},
		{"127.0.0.2:7420", false},
		{"[::1]:7420", false},
		{"localhost:7420", false},
		{"0.0.0.0:7420", true},
		{":7420", true},
		{"192.168.1.1:7420", true},
		{"[::]:7420", true},
		{"nope", true},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			err := ValidateLoopback(tc.addr)
			if tc.wantErr && err == nil {
				t.Error("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestRequireToken(t *testing.T) {
	h := RequireToken("secret", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	call := func(auth string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, body := call("Bearer secret"); code != 200 || body != "ok" {
		t.Fatalf("valid = %d %q", code, body)
	}
	for _, tc := range []struct{ name, auth string }{
		{"missing", ""},
		{"wrong token", "Bearer wrong"},
		{"wrong scheme", "Basic xyz"},
		{"lowercase scheme", "bearer secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := call(tc.auth)
			if code != 401 || !strings.Contains(body, "unauthorized") {
				t.Fatalf("auth %q = %d %q", tc.auth, code, body)
			}
		})
	}
}

func TestEnsureTCP(t *testing.T) {
	t.Setenv("BREAKROOM_HOME", t.TempDir())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cleanup, err := EnsureTCP("", nil, log)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if _, err := os.Stat(paths.Token()); !os.IsNotExist(err) {
		t.Fatalf("token file exists while disabled")
	}
	if _, err := EnsureTCP("0.0.0.0:7420", nil, log); err == nil {
		t.Fatal("non-loopback: want error")
	}
	if _, err := os.Stat(paths.Token()); !os.IsNotExist(err) {
		t.Fatalf("token file exists after rejected listen")
	}
}
