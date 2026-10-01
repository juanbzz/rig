package container

import (
	"context"
	"net"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/rs/zerolog"

	"github.com/j0lvera/rig/internal/config"
)

// TestEngine_Presets starts each preset in real Docker. It is the check that a
// preset's ready condition is right: a wrong one either never passes or
// passes before the database accepts connections.
func TestEngine_Presets(t *testing.T) {
	if testing.Short() {
		t.Skip("starts containers")
	}

	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not available")
	}

	// One engine, as rig uses: New sets testcontainers' package logger.
	engine := New(zerolog.Nop())

	for _, preset := range config.Presets() {
		t.Run(preset, func(t *testing.T) {
			t.Parallel()
			i := is.New(t)

			cfg, err := config.Parse([]byte(`
expose = "web"
[services.db]
preset = "` + preset + `"
[apps.web]
run = "x"
`))
			i.NoErr(err)

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			c, err := engine.Start(ctx, "db", cfg.Services["db"],
				map[string]string{"dev.rig.service": "db"})
			i.NoErr(err)

			defer func() { i.NoErr(c.Terminate(context.Background())) }()

			i.True(c.ID() != "")
			i.True(c.Port() > 0)

			conn, err := net.DialTimeout("tcp", net.JoinHostPort(c.Host(), strconv.Itoa(c.Port())), 5*time.Second)
			i.NoErr(err)
			i.NoErr(conn.Close())
		})
	}
}
