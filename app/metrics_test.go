package main

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// TestMetricsFormat: every sample line is valid Prometheus text, each metric
// has HELP and TYPE, and the cluster label is on all of them.
func TestMetricsFormat(t *testing.T) {
	m := newMetrics("stg")
	m.secretEncryption.Store(1)
	rec := httptest.NewRecorder()
	m.write(rec)
	sample := regexp.MustCompile(`^(pickle_[a-z_]+)\{cluster="stg"(,[a-z]+="[^"]*")*\} -?[0-9]+$`)
	typed := map[string]bool{}
	samples := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n") {
		switch {
		case strings.HasPrefix(l, "# TYPE "):
			typed[strings.Fields(l)[2]] = true
		case strings.HasPrefix(l, "# HELP "):
		default:
			mm := sample.FindStringSubmatch(l)
			if mm == nil {
				t.Fatalf("bad sample line %q", l)
			}
			samples[mm[1]] = true
		}
	}
	for name := range samples {
		if !typed[name] {
			t.Errorf("%s has no TYPE", name)
		}
	}
	for _, name := range []string{"pickle_build_info", "pickle_start_time_seconds", "pickle_secret_encryption_enabled", "pickle_secret_values_encrypted_total", "pickle_secret_encryption_failures_total", "pickle_objects_tracked"} {
		if !samples[name] {
			t.Errorf("%s missing", name)
		}
	}
	if !strings.Contains(rec.Body.String(), `pickle_secret_encryption_enabled{cluster="stg"} 1`) {
		t.Error("encryption gauge not set")
	}
}
