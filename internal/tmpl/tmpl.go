// Package tmpl substitutes {{ name }} placeholders in config values.
//
// It is deliberately not text/template: a value in .rig.toml names a thing
// rig knows, such as {{ services.db.url }}, and nothing else. There are no
// expressions, pipelines, or conditionals, so a value reads the same to a
// person as it does to rig.
package tmpl

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"
)

var (
	// ErrUnknown is returned when a placeholder names a variable the scope
	// does not hold.
	ErrUnknown = errors.New("unknown variable")
	// ErrSyntax is returned when a {{ … }} holds anything but a plain name.
	ErrSyntax = errors.New("unsupported template expression")
)

// _placeholder finds every {{ … }}, well formed or not, so a malformed one is
// reported instead of passed through to a command as literal braces.
var _placeholder = regexp.MustCompile(`\{\{(.*?)\}\}`)

// _name is what a placeholder may hold: dotted segments of letters, digits,
// underscores, and dashes.
var _name = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)

// Scope maps variable names, such as "services.db.url", to their values.
type Scope map[string]string

// Refs returns the names a string refers to, in order of appearance. A
// malformed placeholder fails with ErrSyntax.
func Refs(s string) ([]string, error) {
	matches := _placeholder.FindAllStringSubmatch(s, -1)
	names := make([]string, 0, len(matches))

	for _, m := range matches {
		name := strings.TrimSpace(m[1])
		if !_name.MatchString(name) {
			return nil, fmt.Errorf("%w: %s", ErrSyntax, m[0])
		}

		names = append(names, name)
	}

	return names, nil
}

// Render replaces every placeholder in s with its value from scope.
func Render(s string, scope Scope) (string, error) {
	var err error

	out := _placeholder.ReplaceAllStringFunc(s, func(m string) string {
		if err != nil {
			return m
		}

		name := strings.TrimSpace(m[2 : len(m)-2])
		if !_name.MatchString(name) {
			err = fmt.Errorf("%w: %s", ErrSyntax, m)
			return m
		}

		v, ok := scope[name]
		if !ok {
			err = fmt.Errorf("%w: %s", ErrUnknown, name)
			return m
		}

		return v
	})
	if err != nil {
		return "", err
	}

	return out, nil
}

// RenderMap renders every value of m, returning a new map.
func RenderMap(m map[string]string, scope Scope) (map[string]string, error) {
	out := make(map[string]string, len(m))

	for k, v := range m {
		r, err := Render(v, scope)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}

		out[k] = r
	}

	return out, nil
}

// With returns a copy of scope with extra variables added over it, so a
// per-app value like {{ port }} never leaks into another app's scope.
func (s Scope) With(extra Scope) Scope {
	out := make(Scope, len(s)+len(extra))
	maps.Copy(out, s)
	maps.Copy(out, extra)

	return out
}
