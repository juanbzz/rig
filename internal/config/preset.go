package config

import (
	"maps"
	"slices"
)

// preset is everything rig knows about running one well-known image: how to
// configure it, when it is ready, and how a client reaches it.
type preset struct {
	image string
	port  int
	env   map[string]string
	ready Ready
	// url is rendered against host, port, and vars.
	url string
	// vars are exposed as services.<name>.<var>, so a project that needs the
	// parts of a URL, like Laravel's DB_HOST and DB_PASSWORD, gets them.
	vars map[string]string
}

// The credentials are fixed and public: the database lives as long as one
// test run, on a random port, and holds nothing worth protecting.
const (
	_user     = "rig"
	_password = "rig"
	_database = "rig"
)

var _presets = map[string]preset{
	"postgres": {
		image: "postgres:16-alpine",
		port:  5432,
		env: map[string]string{
			"POSTGRES_USER":     _user,
			"POSTGRES_PASSWORD": _password,
			"POSTGRES_DB":       _database,
		},
		// Postgres logs this line twice: once for the init server that runs
		// the entrypoint scripts, and once for the real one.
		ready: Ready{Log: "database system is ready to accept connections", Occurrences: 2},
		url:   "postgres://{{ user }}:{{ password }}@{{ host }}:{{ port }}/{{ database }}?sslmode=disable",
		vars:  map[string]string{"user": _user, "password": _password, "database": _database},
	},
	"mysql": {
		image: "mysql:8.4",
		port:  3306,
		env: map[string]string{
			"MYSQL_ROOT_PASSWORD": _password,
			"MYSQL_USER":          _user,
			"MYSQL_PASSWORD":      _password,
			"MYSQL_DATABASE":      _database,
		},
		// The init server reports "ready for connections" on port 0; only the
		// real one names 3306.
		ready: Ready{Log: `ready for connections.*port: 3306`},
		url:   "mysql://{{ user }}:{{ password }}@{{ host }}:{{ port }}/{{ database }}",
		vars:  map[string]string{"user": _user, "password": _password, "database": _database},
	},
}

// Presets returns the preset names rig ships, sorted.
func Presets() []string {
	return slices.Sorted(maps.Keys(_presets))
}

// apply fills every field the service leaves empty from the preset. Env is
// merged key by key, so a project can add a variable without restating the
// credentials.
func (p preset) apply(svc Service) Service {
	if svc.Image == "" {
		svc.Image = p.image
	}

	if svc.Port == 0 {
		svc.Port = p.port
	}

	env := maps.Clone(p.env)
	maps.Copy(env, svc.Env)
	svc.Env = env

	if svc.Ready == (Ready{}) {
		svc.Ready = p.ready
	}

	if svc.URL == "" {
		svc.URL = p.url
	}

	svc.Vars = maps.Clone(p.vars)

	return svc
}
