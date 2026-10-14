package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PasswordVar is the environment variable (and env file key) holding the SIP digest password.
const PasswordVar = "SIPTOOTH_SIP_PASSWORD"

// PlaceholderEnvFile is the env file written when no password is known yet.
func PlaceholderEnvFile() string {
	return "# Siptooth secrets (keep this file mode 0600).\n" +
		"# Set the SIP digest password if sip.username is configured.\n" +
		PasswordVar + "=\n"
}

// EnvFilePath is the env file next to the config file (~/.siptooth/env).
func EnvFilePath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "env")
}

// ParseEnvFile reads simple KEY=VALUE lines. Blank lines and lines starting with # or ;
// are skipped, an optional leading "export" (followed by a space or tab) is ignored, a
// value in single or double quotes is taken verbatim up to the closing quote, an unquoted
// value ends at a " #" inline comment, and the last assignment of a key wins.
func ParseEnvFile(content string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if rest, found := strings.CutPrefix(line, "export"); found && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
			line = strings.TrimSpace(rest)
		}
		name, value, found := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !found || name == "" {
			continue
		}
		out[name] = envValue(strings.TrimSpace(value))
	}
	return out
}

// envValue unquotes a value or strips an inline comment from an unquoted one.
func envValue(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : end+1]
		}
	}
	if strings.HasPrefix(v, "#") {
		return ""
	}
	for i := 1; i < len(v); i++ {
		if v[i] == '#' && (v[i-1] == ' ' || v[i-1] == '\t') {
			return strings.TrimSpace(v[:i])
		}
	}
	return v
}

// LookupPassword returns SIPTOOTH_SIP_PASSWORD from the environment or, when unset there,
// from the env file next to configPath. A missing env file is not an error; an unreadable
// one is. The password is empty when neither has it.
func LookupPassword(configPath string) (string, error) {
	if v := os.Getenv(PasswordVar); v != "" {
		return v, nil
	}
	b, err := os.ReadFile(EnvFilePath(configPath))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read env file: %w", err)
	}
	return ParseEnvFile(string(b))[PasswordVar], nil
}
