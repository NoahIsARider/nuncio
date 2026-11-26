package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// ErrSecretUnavailable means a valid reference cannot be resolved here (for
// example keyring: on a machine with no keyring backend).
var ErrSecretUnavailable = errors.New("secret is referenced but not resolvable on this host")

// AuthRef is a parsed credential reference.
type AuthRef struct {
	Scheme string
	Value  string
}

func (a AuthRef) String() string {
	if a.Scheme == "" {
		return ""
	}
	return a.Scheme + ":" + a.Value
}

var authSchemes = map[string]bool{
	"env": true, "file": true, "ssh": true, "keyring": true,
}

// ParseAuthRef parses env:NAME, file:/path, ssh:alias or keyring:name.
func ParseAuthRef(s string) (AuthRef, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return AuthRef{}, nil
	}
	scheme, value, ok := strings.Cut(s, ":")
	if !ok || !authSchemes[strings.ToLower(scheme)] {
		return AuthRef{}, fmt.Errorf("auth must be a reference (env:NAME, file:/path, ssh:alias, keyring:name), got %q", redact(s))
	}
	if strings.TrimSpace(value) == "" {
		return AuthRef{}, fmt.Errorf("%s reference has an empty value", scheme)
	}
	return AuthRef{Scheme: strings.ToLower(scheme), Value: value}, nil
}

// ValidateAuthRef parses a reference and rejects anything that looks like a
// literal credential.
func ValidateAuthRef(s string) error {
	if s == "" {
		return nil
	}
	if looksLikeLiteralSecret(s) {
		return fmt.Errorf("value looks like a literal secret; use env:NAME, file:/path, ssh:alias or keyring:name")
	}
	_, err := ParseAuthRef(s)
	return err
}

var literalSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})$`),
	regexp.MustCompile(`^glpat-[A-Za-z0-9_\-]{16,}$`),
	regexp.MustCompile(`^ATATT[A-Za-z0-9_\-]{10,}$`),
	regexp.MustCompile(`^(srht_|hf_|sk-)[A-Za-z0-9_\-]{10,}$`),
	regexp.MustCompile(`^[A-Za-z0-9+/=_\-]{40,}$`),
}

func looksLikeLiteralSecret(s string) bool {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return false
	}
	if scheme, _, ok := strings.Cut(trimmed, ":"); ok && authSchemes[strings.ToLower(scheme)] {
		return false
	}
	if strings.Contains(trimmed, "://") {
		return true
	}
	for _, re := range literalSecretPatterns {
		if re.MatchString(trimmed) {
			return true
		}
	}
	return false
}

func redact(s string) string {
	if len(s) <= 6 {
		return "***"
	}
	return s[:3] + "…"
}

// Resolve resolves a reference to a secret value. ssh: and keyring: do not
// yield a bearer token; ssh credentials are consumed by git and keyring support
// depends on the host, so they return ErrSecretUnavailable rather than a bogus
// empty string.
func Resolve(ref string) (string, error) {
	return ResolveWith(ref, os.Getenv, os.ReadFile)
}

// ResolveWith is Resolve with injectable environment and filesystem access.
func ResolveWith(ref string, getenv func(string) string, readFile func(string) ([]byte, error)) (string, error) {
	a, err := ParseAuthRef(ref)
	if err != nil {
		return "", err
	}
	switch a.Scheme {
	case "":
		return "", nil
	case "env":
		v := getenv(a.Value)
		if v == "" {
			return "", fmt.Errorf("environment variable %s is empty or unset", a.Value)
		}
		return strings.TrimSpace(v), nil
	case "file":
		data, err := readFile(a.Value)
		if err != nil {
			return "", fmt.Errorf("read auth file %s: %w", a.Value, err)
		}
		v := strings.TrimSpace(string(data))
		if v == "" {
			return "", fmt.Errorf("auth file %s is empty", a.Value)
		}
		return v, nil
	case "ssh":
		return "", fmt.Errorf("%w: %s is an SSH alias consumed by git, not a bearer token", ErrSecretUnavailable, a.Value)
	case "keyring":
		return "", fmt.Errorf("%w: keyring backend not available", ErrSecretUnavailable)
	default:
		return "", fmt.Errorf("unsupported auth scheme %q", a.Scheme)
	}
}

// SecretName returns the environment variable or file name behind a reference,
// for doctor output. It never returns a value.
func SecretName(ref string) string {
	a, err := ParseAuthRef(ref)
	if err != nil {
		return "invalid-reference"
	}
	switch a.Scheme {
	case "env":
		return "env:" + a.Value
	case "file":
		return "file:" + a.Value
	case "ssh":
		return "ssh:" + a.Value
	case "keyring":
		return "keyring:" + a.Value
	default:
		return "none"
	}
}
