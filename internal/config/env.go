package config

import (
	"fmt"

	"github.com/kelseyhightower/envconfig"
)

// Env holds process-level settings sourced from environment variables. These
// control how rig talks, not what it starts.
type Env struct {
	LogLevel string `envconfig:"RIG_LOG_LEVEL" default:"info"`
	Verbose  bool   `envconfig:"RIG_VERBOSE" default:"false"`
}

// LoadEnv reads environment variables over the receiver's values.
func (e Env) LoadEnv() (Env, error) {
	cfg := e

	if err := envconfig.Process("", &cfg); err != nil {
		return e, fmt.Errorf("reading environment: %w", err)
	}

	return cfg, nil
}
