// Package config reads the settings the application starts with.
package config

import (
	"cmp"
	"os"

	"github.com/floatdrop/di"
)

type Config struct {
	Addr string
	DSN  string
}

func load() Config {
	return Config{
		Addr: env("ADDR", ":8080"),
		DSN:  env("DSN", "postgres://localhost/app"),
	}
}

func env(key, fallback string) string { return cmp.Or(os.Getenv(key), fallback) }

// Module registers the configuration as a value. A test overrides it with
// s.Value(config.Config{...}).Override() and everything downstream follows.
func Module(s *di.Scope) { s.Value(load()) }
