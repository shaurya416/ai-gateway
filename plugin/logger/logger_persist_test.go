package logger

import (
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/plugin"
)

// A persist value that is not a boolean is refused, at startup and by
// `ferrogw validate`, rather than read as false.
//
// Read as false, it turned persistence off with nothing said: the gateway
// started, logged requests to stdout, and wrote no row — so the Request Logs
// page and every figure derived from it stayed empty. YAML hands over an
// unquoted `yes` or `on` as a string, and a ${VAR} reference always resolves to
// one.
func TestRequestLogger_RejectsNonBooleanPersist(t *testing.T) {
	for _, value := range []any{"yes", "on", "true", "${REQUEST_LOG_PERSIST}", 1} {
		config := map[string]any{"persist": value}

		l := &RequestLogger{}
		l.SetRequestLogWriter(&recordingWriter{})
		err := l.Init(config)
		if err == nil {
			t.Errorf("Init(persist: %#v) = nil, want an error naming the setting", value)
		} else if !strings.Contains(err.Error(), "persist") {
			t.Errorf("Init(persist: %#v) error %q does not name the setting", value, err)
		}

		if err := plugin.ValidateConfigFor("request-logger", config); err == nil {
			t.Errorf("ValidateConfigFor(persist: %#v) = nil, want the error Init gives", value)
		}
	}
}

// Every value persist could already be written as keeps working.
func TestRequestLogger_AcceptsBooleanOrAbsentPersist(t *testing.T) {
	for _, config := range []map[string]any{
		{"persist": true},
		{"persist": false},
		{"persist": nil},
		{},
		nil,
	} {
		l := &RequestLogger{}
		l.SetRequestLogWriter(&recordingWriter{})
		if err := l.Init(config); err != nil {
			t.Errorf("Init(%v) = %v, want nil", config, err)
		}
		if err := plugin.ValidateConfigFor("request-logger", config); err != nil {
			t.Errorf("ValidateConfigFor(%v) = %v, want nil", config, err)
		}
	}
}
