package config

import (
	"encoding"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// envPrefix starts every configuration environment variable. Other ALIXAI_*
// variables (for example live-test endpoints) are not configuration and are
// ignored here.
const envPrefix = "ALIXAI_"

var (
	durationType        = reflect.TypeOf(time.Duration(0))
	textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
)

// walkEnv visits every field of v that an environment variable can set, with
// the variable's name. v must be addressable for the values handed to visit to
// be settable. A struct section yields ALIXAI_<SECTION>_<FIELD> (nested
// structs add one more _<FIELD>); a scalar section yields ALIXAI_<SECTION>.
// Field names come from the yaml tag, so env names and YAML keys agree.
// Fields of unsupported kinds (maps, slices of non-strings, interfaces,
// functions, ...) have no environment override.
func walkEnv(prefix string, v reflect.Value, visit func(name string, field reflect.Value)) {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if v.Kind() == reflect.Struct && !isTextUnmarshaler(v) {
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			if !sf.IsExported() {
				continue
			}
			name, inline, skip := yamlName(sf)
			if skip {
				continue
			}
			if inline {
				walkEnv(prefix, v.Field(i), visit)
				continue
			}
			walkEnv(prefix+"_"+envSegment(name), v.Field(i), visit)
		}
		return
	}
	if settable(v) {
		visit(prefix, v)
	}
}

// yamlName mirrors yaml.v3's field naming: the tag's first element, or the
// lower-cased Go field name.
func yamlName(sf reflect.StructField) (name string, inline, skip bool) {
	tag := sf.Tag.Get("yaml")
	if tag == "-" {
		return "", false, true
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	for _, opt := range parts[1:] {
		if opt == "inline" {
			inline = true
		}
	}
	if name == "" {
		name = strings.ToLower(sf.Name)
	}
	return name, inline, false
}

func envSegment(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return '_'
	}, name)
}

func isTextUnmarshaler(v reflect.Value) bool {
	return v.CanAddr() && v.Addr().Type().Implements(textUnmarshalerType)
}

func settable(v reflect.Value) bool {
	if isTextUnmarshaler(v) || v.Type() == durationType {
		return true
	}
	switch v.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	case reflect.Slice:
		return v.Type().Elem().Kind() == reflect.String
	}
	return false
}

// applyEnv overrides the working copies with ALIXAI_* variables. A variable
// that is set but cannot be converted is an error, never ignored. Errors name
// the variable but never echo its value, which may be a secret.
func (r *Registry) applyEnv(work []reflect.Value, lookup EnvLookup) error {
	for i, e := range r.entries {
		var firstErr error
		walkEnv(envPrefix+strings.ToUpper(e.name), work[i].Elem(), func(name string, field reflect.Value) {
			if firstErr != nil {
				return
			}
			raw, ok := lookup(name)
			if !ok {
				return
			}
			if err := setFromString(field, raw); err != nil {
				firstErr = fmt.Errorf("config: environment variable %s: %w", name, err)
			}
		})
		if firstErr != nil {
			return firstErr
		}
	}
	return nil
}

func setFromString(v reflect.Value, s string) error {
	if isTextUnmarshaler(v) {
		if err := v.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(s)); err != nil {
			return fmt.Errorf("invalid value (%v)", kindOf(v))
		}
		return nil
	}
	if v.Type() == durationType {
		d, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil {
			return fmt.Errorf("invalid duration")
		}
		v.SetInt(int64(d))
		return nil
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(strings.TrimSpace(s))
		if err != nil {
			return fmt.Errorf("invalid boolean")
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, v.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid integer")
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(strings.TrimSpace(s), 10, v.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid unsigned integer")
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(strings.TrimSpace(s), v.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid number")
		}
		v.SetFloat(f)
	case reflect.Slice: // []string, comma separated
		out := reflect.MakeSlice(v.Type(), 0, 0)
		if strings.TrimSpace(s) != "" {
			for _, part := range strings.Split(s, ",") {
				out = reflect.Append(out, reflect.ValueOf(strings.TrimSpace(part)).Convert(v.Type().Elem()))
			}
		}
		v.Set(out)
	default:
		return fmt.Errorf("unsupported type %s", v.Type())
	}
	return nil
}

func kindOf(v reflect.Value) string { return v.Type().String() }
