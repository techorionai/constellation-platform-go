package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"

	"github.com/caarlos0/env/v11"
)

var prefixPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*_$`)

// ErrInvalidPrefix is returned by [Load] when the prefix does not match
// ^[A-Z][A-Z0-9]*_$.
var ErrInvalidPrefix = errors.New("config: prefix must match ^[A-Z][A-Z0-9]*_$")

// Load reads the process environment into dst. dst must be a pointer to a
// struct.
//
// prefix is the service prefix, for example "IDENTITY_". The struct uses the
// tags of github.com/caarlos0/env/v11: env, envDefault, and envPrefix. A nested
// struct uses an envPrefix tag such as "DATABASE__". A field with the tag
// envFile:"true" also reads the variable "<NAME>_FILE". The variable gives the
// path of a file, and Load removes one trailing line break from the content.
// If both "<NAME>" and "<NAME>_FILE" have a value, Load fails.
//
// Load collects all errors into one error. An error names the variable and
// never contains the value.
func Load(prefix string, dst any) error {
	if !prefixPattern.MatchString(prefix) {
		return ErrInvalidPrefix
	}

	environ := os.Environ()
	vars := make(map[string]string, len(environ))
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		vars[k] = v
	}

	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return errors.New("config: dst must be a non-nil pointer to a struct")
	}

	var errs []error
	fields := map[string][]string{}
	walk(rv.Elem().Type(), prefix, vars, fields, &errs)
	if len(errs) > 0 {
		return joinErrors(errs)
	}

	err := env.ParseWithOptions(dst, env.Options{
		Environment: vars,
		Prefix:      prefix,
	})
	if err != nil {
		errs = append(errs, sanitize(err, vars, fields)...)
	}
	return joinErrors(errs)
}

func joinErrors(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	}
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = e.Error()
	}
	return fmt.Errorf("config: %d errors: %s", len(errs), strings.Join(parts, "; "))
}

// walk finds the variable name of each field. It applies the _FILE rule to the
// fields with the tag envFile:"true". It records the variable names by field
// name, so that a parse error can name the variable.
func walk(t reflect.Type, prefix string, vars map[string]string, fields map[string][]string, errs *[]error) {
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		ft := sf.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		tag := sf.Tag.Get("env")
		if ft.Kind() == reflect.Struct && tag == "" {
			walk(ft, prefix+sf.Tag.Get("envPrefix"), vars, fields, errs)
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			continue
		}
		key := prefix + sf.Tag.Get("envPrefix") + name
		fields[sf.Name] = append(fields[sf.Name], key)
		if sf.Tag.Get("envFile") != "true" {
			continue
		}
		path, ok := vars[key+"_FILE"]
		if !ok {
			continue
		}
		if v, set := vars[key]; set && v != "" {
			*errs = append(*errs, fmt.Errorf("variables %s and %s_FILE are both set", key, key))
			continue
		}
		data, err := os.ReadFile(path) //nolint:gosec // the operator gives the path
		if err != nil {
			*errs = append(*errs, fmt.Errorf("variable %s_FILE: cannot read file %q: %w", key, path, errors.Unwrap(err)))
			continue
		}
		vars[key] = trimLineBreak(string(data))
	}
}

func trimLineBreak(s string) string {
	if t, ok := strings.CutSuffix(s, "\r\n"); ok {
		return t
	}
	return strings.TrimSuffix(s, "\n")
}

// sanitize turns the errors of the env library into errors that never contain
// a value.
func sanitize(err error, vars map[string]string, fields map[string][]string) []error {
	var agg env.AggregateError
	if !errors.As(err, &agg) {
		return []error{errors.New("config: cannot load environment")}
	}
	out := make([]error, 0, len(agg.Errors))
	for _, e := range agg.Errors {
		var (
			notSet   env.VarIsNotSetError
			empty    env.EmptyVarError
			parseErr env.ParseError
		)
		switch {
		case errors.As(e, &notSet):
			out = append(out, fmt.Errorf("required variable %s is not set", notSet.Key))
		case errors.As(e, &empty):
			out = append(out, fmt.Errorf("variable %s must not be empty", empty.Key))
		case errors.As(e, &parseErr):
			names := set(fields[parseErr.Name], vars)
			if len(names) == 0 {
				names = fields[parseErr.Name]
			}
			out = append(out, fmt.Errorf("invalid value for variable %s (type %s)", strings.Join(names, " or "), parseErr.Type))
		default:
			out = append(out, errors.New("config: cannot load environment"))
		}
	}
	return out
}

func set(keys []string, vars map[string]string) []string {
	var out []string
	for _, k := range keys {
		if _, ok := vars[k]; ok {
			out = append(out, k)
		}
	}
	return out
}
