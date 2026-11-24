// Package config loads a .env file (tiny loader, no external dependency).
package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// LoadDotEnv looks for a .env file in the current directory and up to 6 parent
// directories (so it also works when `go test` runs inside a sub-package).
// Variables already set in the real environment are NOT overridden.
func LoadDotEnv() {
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	for i := 0; i < 7; i++ {
		path := filepath.Join(dir, ".env")
		if f, err := os.Open(path); err == nil {
			defer f.Close()
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				k, v, ok := strings.Cut(line, "=")
				if !ok {
					continue
				}
				k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
				if _, exists := os.LookupEnv(k); !exists {
					_ = os.Setenv(k, v)
				}
			}
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

// Getenv returns the variable or a default.
func Getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
