package tmpl

import (
	"errors"
	"testing"

	"github.com/matryer/is"
)

func TestRender(t *testing.T) {
	scope := Scope{
		"services.db.url":     "postgres://rig:rig@localhost:5432/rig",
		"apps.web-admin.port": "5174",
		"port":                "8081",
	}

	tests := []struct {
		name string
		in   string
		want string
		err  error
	}{
		{name: "plain text", in: "go build ./...", want: "go build ./..."},
		{name: "one variable", in: "{{ services.db.url }}", want: "postgres://rig:rig@localhost:5432/rig"},
		{name: "no spaces", in: "--port={{port}}", want: "--port=8081"},
		{name: "dash in name", in: "{{ apps.web-admin.port }}", want: "5174"},
		{name: "several", in: "{{ port }}:{{ port }}", want: "8081:8081"},
		{name: "unknown", in: "{{ services.db.uri }}", err: ErrUnknown},
		{name: "pipeline", in: "{{ port | quote }}", err: ErrSyntax},
		{name: "empty", in: "{{ }}", err: ErrSyntax},
		{name: "single braces pass through", in: "${HOME} {x}", want: "${HOME} {x}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i := is.New(t)

			got, err := Render(tt.in, scope)
			if tt.err != nil {
				i.True(errors.Is(err, tt.err))
				return
			}

			i.NoErr(err)
			i.Equal(got, tt.want)
		})
	}
}

func TestRefs(t *testing.T) {
	i := is.New(t)

	refs, err := Refs("DATABASE_URL={{ services.db.url }} PORT={{ port }}")
	i.NoErr(err)
	i.Equal(refs, []string{"services.db.url", "port"})

	_, err = Refs("{{ if .x }}")
	i.True(errors.Is(err, ErrSyntax))
}

func TestRenderMap_NamesTheKey(t *testing.T) {
	i := is.New(t)

	_, err := RenderMap(map[string]string{"DATABASE_URL": "{{ nope }}"}, Scope{})
	i.True(errors.Is(err, ErrUnknown))
	i.Equal(err.Error(), "DATABASE_URL: unknown variable: nope")
}

func TestScope_WithDoesNotMutate(t *testing.T) {
	i := is.New(t)

	base := Scope{"tmp": "/tmp/x"}
	got := base.With(Scope{"port": "1"})

	i.Equal(got["port"], "1")
	i.Equal(got["tmp"], "/tmp/x")

	_, leaked := base["port"]
	i.True(!leaked)
}
