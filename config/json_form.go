package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
)

// jsonFormNote explains every refusal of a value with no JSON form.
const jsonFormNote = "a running config is stored, served and compared as JSON"

// validateJSONForm refuses a config with no JSON form.
//
// JSON is the form a config is kept in once it is running: the config-history
// store persists it, GET /admin/config serves it, and PluginSharingKey decides
// which plugin entries are one instance by comparing it. YAML can spell values
// JSON cannot — a mapping key that is not a string (`2026: raised`), and .nan
// or .inf — and decodes them without complaint. A YAML config carrying one
// loaded, and then a response cache or budget listed at two stages became two
// instances that never saw each other's state, and GET /admin/config answered
// with an empty body. The refusal names where the value is, so the edit — quote
// the key, write the number — is one line.
func validateJSONForm(cfg Config) error {
	if _, err := json.Marshal(cfg); err != nil {
		return noJSONForm("", reflect.ValueOf(cfg), err)
	}
	return nil
}

// pluginJSONForm is validateJSONForm for one plugin entry, at its place in
// the plugin list.
func pluginJSONForm(i int, pc PluginConfig) error {
	if _, err := json.Marshal(pc.Config); err != nil {
		return noJSONForm(fmt.Sprintf("plugins[%d].config", i), reflect.ValueOf(pc.Config), err)
	}
	return nil
}

// noJSONForm reports the value under v, at path, that encoding/json refused
// with err. It is only asked once the encoder has failed, so it never refuses
// a config the encoder accepts; when it cannot place the value, the encoder's
// own error is the report.
func noJSONForm(path string, v reflect.Value, err error) error {
	if where := firstWithoutJSONForm(path, v); where != "" {
		return errors.New(where)
	}
	return fmt.Errorf("config has no JSON form: %w", err)
}

// firstWithoutJSONForm describes the first value under v that JSON cannot
// carry, or returns "" when it finds none. Struct fields are named as their
// json tags name them, so the path is the one an operator edits.
func firstWithoutJSONForm(path string, v reflect.Value) string {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return ""
		}
		return firstWithoutJSONForm(path, v.Elem())
	case reflect.Struct:
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			if where := firstWithoutJSONForm(joinPath(path, name), v.Field(i)); where != "" {
				return where
			}
		}
	case reflect.Map:
		keys := v.MapKeys()
		if v.Type().Key().Kind() != reflect.String {
			for _, key := range keys {
				if k := key; k.Kind() == reflect.Interface && !k.IsNil() && k.Elem().Kind() != reflect.String {
					return fmt.Sprintf("%s has a mapping key, %v, that is not a string; %s, whose keys are strings, so quote it", displayPath(path), k.Elem(), jsonFormNote)
				}
			}
		}
		slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
		for _, key := range keys {
			if where := firstWithoutJSONForm(joinPath(path, fmt.Sprint(key)), v.MapIndex(key)); where != "" {
				return where
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if where := firstWithoutJSONForm(fmt.Sprintf("%s[%d]", path, i), v.Index(i)); where != "" {
				return where
			}
		}
	case reflect.Float32, reflect.Float64:
		if f := v.Float(); math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Sprintf("%s is %v; %s, which has no spelling for a non-finite number", displayPath(path), f, jsonFormNote)
		}
	case reflect.Chan, reflect.Func, reflect.Complex64, reflect.Complex128, reflect.UnsafePointer:
		return fmt.Sprintf("%s is a %s; %s, which cannot carry one", displayPath(path), v.Kind(), jsonFormNote)
	}
	return ""
}

func joinPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func displayPath(path string) string {
	if path == "" {
		return "config"
	}
	return path
}
