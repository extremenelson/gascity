package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func writeEmbeddedConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// An embedded scope's config gains only the scope-local vocabulary: none of
// EnsureCanonicalConfig's endpoint, topology, or policy keys.
func TestEnsureEmbeddedScopeConfigWritesOnlyVocabulary(t *testing.T) {
	path := writeEmbeddedConfig(t, "sync.branch: main\n")
	changed, err := EnsureEmbeddedScopeConfig(fsys.OSFS{}, path, ConfigState{
		IssuePrefix:    "fr",
		EndpointOrigin: EndpointOriginInheritedCity,
		EndpointStatus: EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "3307",
		DoltUser:       "root",
		DoltMode:       "server",
		CustomTypes:    []string{"molecule", "convoy"},
	})
	if err != nil || !changed {
		t.Fatalf("EnsureEmbeddedScopeConfig = (%v, %v), want (true, nil)", changed, err)
	}
	got := readConfigFile(t, path)
	for _, want := range []string{"sync.branch: main", "issue_prefix: fr", "issue-prefix: fr", "types.custom: molecule,convoy"} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"gc.endpoint", "dolt.", "dolt:", "export.auto", "backup.enabled"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("config gained %q on an embedded scope:\n%s", forbidden, got)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want bd's 0600 preserved", info.Mode().Perm())
	}
}

// The endpoint mirror is gc's only when gc's marker says so; policy keys and a
// dolt.mode agreeing with metadata are never gc's to remove.
func TestEnsureEmbeddedScopeConfigScrubsOnlyGcOwnedKeys(t *testing.T) {
	for name, tc := range map[string]struct {
		in       string
		gone     []string
		retained []string
	}{
		"gc-stamped endpoint block": {
			in:       "gc.endpoint_origin: inherited_city\ngc.endpoint_status: verified\ndolt.mode: server\ndolt.host: 127.0.0.1\ndolt.port: 3307\ndolt.user: root\nbackup.enabled: false\nexport.auto: false\ndolt.auto-start: false\n",
			gone:     []string{"gc.endpoint_origin", "gc.endpoint_status", "dolt.mode", "dolt.host", "dolt.port", "dolt.user"},
			retained: []string{"backup.enabled: false", "export.auto: false", "dolt.auto-start: false"},
		},
		"no gc marker": {
			in:       "dolt.mode: embedded\ndolt.port: 3310\n",
			retained: []string{"dolt.mode: embedded", "dolt.port: 3310"},
		},
		"contradicting mode without marker": {
			in:       "dolt.mode: server\ndolt.host: 10.0.0.1\n",
			gone:     []string{"dolt.mode"},
			retained: []string{"dolt.host: 10.0.0.1"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeEmbeddedConfig(t, tc.in)
			if _, err := EnsureEmbeddedScopeConfig(fsys.OSFS{}, path, ConfigState{}); err != nil {
				t.Fatal(err)
			}
			got := readConfigFile(t, path)
			for _, key := range tc.gone {
				if strings.Contains(got, key+":") {
					t.Errorf("%s survived:\n%s", key, got)
				}
			}
			for _, want := range tc.retained {
				if !strings.Contains(got, want) {
					t.Errorf("%q was removed:\n%s", want, got)
				}
			}
			if changed, err := EnsureEmbeddedScopeConfig(fsys.OSFS{}, path, ConfigState{}); err != nil || changed {
				t.Fatalf("second pass = (%v, %v), want (false, nil)", changed, err)
			}
		})
	}
}

// bd's file is not repaired into gc's shape when it does not parse.
func TestEnsureEmbeddedScopeConfigLeavesUnparseableFileAlone(t *testing.T) {
	const malformed = "issue_prefix: fr\n  bad: [indent\n"
	path := writeEmbeddedConfig(t, malformed)
	changed, err := EnsureEmbeddedScopeConfig(fsys.OSFS{}, path, ConfigState{IssuePrefix: "fr", CustomTypes: []string{"molecule"}})
	if err != nil || changed {
		t.Fatalf("EnsureEmbeddedScopeConfig = (%v, %v), want (false, nil)", changed, err)
	}
	if got := readConfigFile(t, path); got != malformed {
		t.Fatalf("unparseable config was rewritten:\n%s", got)
	}
}
