package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/search-3d/breakroom/internal/paths"
)

// tokenLength is the raw byte length of daemon auth tokens (hex in use).
const tokenLength = 32

// GenerateToken returns a fresh random hex token.
func GenerateToken() (string, error) {
	b := make([]byte, tokenLength)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// writeToken creates a fresh token file (0600), overwriting any prior one.
func writeToken() (string, error) {
	token, err := GenerateToken()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(paths.Token(), []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// ValidateLoopback rejects listen addresses outside localhost.
func ValidateLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("breakroom: bad tcp_listen %q: %w", addr, err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("breakroom: tcp_listen %q must be localhost", addr)
}

// RequireToken gates a handler behind bearer auth. Failures mimic the
// daemon error shape.
func RequireToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// EnsureTCP validates, writes the token file, and serves h with auth when
// addr is non-empty. Returns a cleanup func (no-op when disabled).
func EnsureTCP(addr string, h http.Handler, log *slog.Logger) (func(), error) {
	noop := func() {}
	if addr == "" {
		return noop, nil
	}
	if err := ValidateLoopback(addr); err != nil {
		return nil, err
	}
	token, err := writeToken()
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("breakroom: listen tcp %s: %w", addr, err)
	}
	srv := &http.Server{Handler: RequireToken(token, h), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	log.Info("tcp listening", "addr", addr)
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = os.Remove(paths.Token())
	}, nil
}
