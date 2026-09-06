// Package config reads the settings the application starts with.
package config

import (
	"os"

	"github.com/floatdrop/di"
)

type Config struct {
	Addr string
	DSN  string
}

func Load() Config {
	return Config{
		Addr: env("ADDR", ":8080"),
		DSN:  env("DSN", "postgres://localhost/app"),
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Module registers the configuration as a value. A test overrides it with
// s.Value(config.Config{...}).Override() and everything downstream follows.
func Module(s *di.Scope) { s.Value(Load()) }
