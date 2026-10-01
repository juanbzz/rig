package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
)

// _startapp is the v1 target: startapp's e2e harness as a .rig.toml.
const _startapp = `
expose = "web"

[services.db]
preset = "postgres"

[services.mail]
image = "axllent/mailpit:latest"
port  = 1025
ready = { http = "/livez" }
url   = "smtp://{{ host }}:{{ port }}"

[[steps]]
name = "migrate"
run  = "goose -dir apps/api/internal/db/migrations up"
env  = { GOOSE_DRIVER = "postgres", GOOSE_DBSTRING = "{{ services.db.url }}" }

[apps.api]
build  = "cd apps/api && go build -o {{ tmp }}/api ./cmd/main"
run    = "{{ tmp }}/api"
health = "/health"
env    = { DATABASE_URL = "{{ services.db.url }}", PORT = "{{ port }}", ORIGINS = "{{ apps.web.url }}" }

[apps.web]
run        = "bun run --filter @startapp/dash dev --port {{ port }} --strictPort"
env        = { VITE_API_PROXY_TARGET = "{{ apps.api.url }}" }
depends_on = ["api"]
timeout    = "2m"
`

func TestParse_Startapp(t *testing.T) {
	i := is.New(t)

	cfg, err := Parse([]byte(_startapp))
	i.NoErr(err)

	i.Equal(cfg.Expose, "web")
	i.Equal(cfg.ServiceNames(), []string{"db", "mail"})
	i.Equal(cfg.AppNames(), []string{"api", "web"})

	db := cfg.Services["db"]
	i.Equal(db.Image, "postgres:16-alpine")
	i.Equal(db.Port, 5432)
	i.Equal(db.Env["POSTGRES_DB"], "rig")
	i.Equal(db.Ready.Occurrences, 2)
	i.Equal(db.Vars["password"], "rig")
	i.Equal(db.Timeout.Duration, DefaultServiceTimeout)

	mail := cfg.Services["mail"]
	i.Equal(mail.Ready.HTTP, "/livez")
	i.Equal(mail.URL, "smtp://{{ host }}:{{ port }}")

	api := cfg.Apps["api"]
	i.Equal(api.Port, PortAuto)
	i.Equal(api.Timeout.Duration, DefaultAppTimeout)
	// Env keys keep their case; a TOML library that folds keys would turn
	// DATABASE_URL into database_url and break every app.
	i.Equal(api.Env["DATABASE_URL"], "{{ services.db.url }}")

	i.Equal(cfg.Apps["web"].Timeout.Duration, 2*time.Minute)
	i.Equal(cfg.Apps["web"].DependsOn, []string{"api"})

	i.Equal(cfg.Steps[0].Label(), "migrate")
}

func TestParse_PresetOverrides(t *testing.T) {
	i := is.New(t)

	cfg, err := Parse([]byte(`
expose = "app"
[services.db]
preset = "mysql"
image  = "mysql:8.0"
env    = { TZ = "UTC" }
[apps.app]
run = "php artisan serve --port={{ port }}"
`))
	i.NoErr(err)

	db := cfg.Services["db"]
	i.Equal(db.Image, "mysql:8.0")
	i.Equal(db.Env["TZ"], "UTC")
	i.Equal(db.Env["MYSQL_DATABASE"], "rig") // merged, not replaced
	i.Equal(db.Port, 3306)
}

func TestParse_Invalid(t *testing.T) {
	tests := []struct {
		name string
		toml string
		want []string
	}{
		{
			name: "no apps",
			toml: `[services.db]
preset = "postgres"`,
			want: []string{"no apps"},
		},
		{
			name: "missing expose",
			toml: `[apps.web]
run = "x"`,
			want: []string{"expose: name the app whose URL is the result (one of: web)"},
		},
		{
			name: "unknown key",
			toml: `expose = "web"
[apps.web]
run = "x"
helth = "/health"`,
			want: []string{"line 4: unknown key apps.web.helth"},
		},
		{
			name: "every problem at once",
			toml: `expose = "webb"
[services.db]
preset = "postgress"
[services.cache]
image = "redis:7"
[apps.web]
run = "x {{ services.db.uri }}"
port = "8080"
health = "health"`,
			want: []string{
				`services.db: unknown preset "postgress" (known: mysql, postgres)`,
				`expose: "webb" is not an app`,
				"services.cache: port is required",
				"apps.web.run: unknown variable services.db.uri",
				`apps.web: port "8080" is not supported`,
				"apps.web: health must be a path starting with /",
			},
		},
		{
			name: "template syntax",
			toml: `expose = "web"
[apps.web]
run = "{{ port | quote }}"`,
			want: []string{"apps.web.run: unsupported template expression"},
		},
		{
			name: "port outside an app",
			toml: `expose = "web"
[[steps]]
run = "migrate --port {{ port }}"
[apps.web]
run = "x"`,
			want: []string{"steps[0].run: unknown variable port"},
		},
		{
			name: "service url sees only its own variables",
			toml: `expose = "web"
[services.q]
image = "redis:7"
port = 6379
url = "redis://{{ host }}:{{ port }}/{{ apps.web.port }}"
[apps.web]
run = "x"`,
			want: []string{"services.q.url: unknown variable apps.web.port (available: host, port)"},
		},
		{
			name: "dependency problems",
			toml: `expose = "a"
[apps.a]
run = "x"
depends_on = ["b", "ghost"]
[apps.b]
run = "x"
depends_on = ["a"]`,
			want: []string{`apps.a: depends_on "ghost" is not an app`, "depends_on: cycle a → b → a"},
		},
		{
			name: "env name clash",
			toml: `expose = "web-app"
[apps.web-app]
run = "x"
[apps.web_app]
run = "x"`,
			want: []string{"apps.web_app: clashes with apps.web-app (both become RIG_WEB_APP_URL)"},
		},
		{
			name: "bad name",
			toml: `expose = "Web"
[apps.Web]
run = "x"`,
			want: []string{"apps.Web: names must be lowercase"},
		},
		{
			name: "bad duration",
			toml: `expose = "web"
[apps.web]
run = "x"
timeout = "soon"`,
			want: []string{"invalid duration"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i := is.New(t)

			_, err := Parse([]byte(tt.toml))
			i.True(errors.Is(err, ErrInvalid))

			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q:\n%v", want, err)
				}
			}
		})
	}
}

func TestFind_WalksUp(t *testing.T) {
	i := is.New(t)

	root := t.TempDir()
	deep := filepath.Join(root, "apps", "dash", "e2e")
	i.NoErr(os.MkdirAll(deep, 0o755))
	i.NoErr(os.WriteFile(filepath.Join(root, FileName), []byte(""), 0o644))

	got, err := Find(deep)
	i.NoErr(err)
	i.Equal(got, filepath.Join(root, FileName))
}

func TestFind_NotFound(t *testing.T) {
	i := is.New(t)

	_, err := Find(t.TempDir())
	i.True(errors.Is(err, ErrNotFound))
}

func TestLoad_SetsRoot(t *testing.T) {
	i := is.New(t)

	root := t.TempDir()
	path := filepath.Join(root, FileName)
	i.NoErr(os.WriteFile(path, []byte("expose = \"web\"\n[apps.web]\nrun = \"x\"\ndir = \"apps/web\"\n"), 0o644))

	cfg, err := Load(path)
	i.NoErr(err)
	i.Equal(cfg.Root, root)
	i.Equal(cfg.Dir(cfg.Apps["web"].Dir), filepath.Join(root, "apps", "web"))
	i.Equal(cfg.Dir(""), root)
	i.Equal(cfg.Dir("/abs"), "/abs")
}

func TestEnvName(t *testing.T) {
	i := is.New(t)

	i.Equal(EnvName("db"), "DB")
	i.Equal(EnvName("web-admin"), "WEB_ADMIN")
}

// TestLoad_Example keeps the documented example a config rig accepts.
func TestLoad_Example(t *testing.T) {
	i := is.New(t)

	cfg, err := Load(filepath.Join("..", "..", ".rig.toml.example"))
	i.NoErr(err)
	i.Equal(cfg.Expose, "web")
}
