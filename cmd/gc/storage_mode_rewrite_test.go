package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"gopkg.in/yaml.v3"
)

// This file pins the preservation rule for embedded Dolt scopes and the
// standing guard around it.
//
// ga-qi9km shipped these tests when canonicalization still rewrote an
// initialized embedded scope to server mode: the rewrite was load-bearing for a
// city whose bead store has to be a Dolt SERVER many processes open at once,
// but it re-points the ledger (server databases live in .beads/dolt, embedded
// ones in .beads/embeddeddolt/<db>) without moving a row, so it had to be
// announced. The ga-p9iuv architecture contract (2026-09-04) then settled the
// question the other way -- "existing direct local/remote, embedded, DoltLite,
// and proxied scopes remain authoritative and are not automatically converted",
// with "automatic embedded migration" explicitly out of scope -- so the rewrite
// is gone and an embedded scope keeps its mode through every door.
//
// The announcement stays, and so do these tests, in their inverted form: each
// one drives a door that used to flip the mode and proves the mode survives and
// nothing is printed. Should any canonicalization ever move a scope's storage
// mode again, the sink these tests watch is what makes it loud.

// embeddedScopeWithBeads builds a scope whose .beads/ is an embedded-mode bd
// workspace with a Dolt repository under it — what `bd init -p <prefix>` leaves
// behind, and what the live proof's city had before gc touched it.
//
// The Dolt repository is represented by the directory shape gc itself uses to
// recognize one (a `.dolt` subdirectory, the same test gc doctor's
// doltReposUnder applies). Standing up a real Dolt server to prove a
// path-and-JSON disagreement would test Dolt, not this.
func embeddedScopeWithBeads(t *testing.T, database string) string {
	t.Helper()
	scope := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, ".beads", "embeddeddolt", database, ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeScopeMetadata(t, scope, map[string]string{
		"database":      "dolt",
		"backend":       "dolt",
		"dolt_mode":     "embedded",
		"dolt_database": database,
	})
	return scope
}

func readScopeDoltMode(t *testing.T, scope string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(scope, ".beads", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		DoltMode string `json:"dolt_mode"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	return meta.DoltMode
}

// captureStorageModeChanges redirects the sink the canonicalization announces
// storage-mode changes on and returns the buffer holding them.
func captureStorageModeChanges(t *testing.T) *bytes.Buffer {
	t.Helper()
	orig := storageModeChangeSink
	buf := &bytes.Buffer{}
	storageModeChangeSink = buf
	t.Cleanup(func() { storageModeChangeSink = orig })
	return buf
}

// emptyBdRunner answers every bd invocation with `[]` and exit 0.
func emptyBdRunner(_, _ string, _ ...string) ([]byte, error) { return []byte("[]"), nil }

// TestCanonicalizingAnEmbeddedScopeKeepsItsModeAndStaysSilent ensures an existing
// embedded scope is left untouched and emits no misleading migration notice.
func TestCanonicalizingAnEmbeddedScopeKeepsItsModeAndStaysSilent(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jc")
	notices := captureStorageModeChanges(t)

	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc", "proxied-server"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}

	if mode := readScopeDoltMode(t, scope); mode != "embedded" {
		t.Fatalf("dolt_mode = %q after canonicalization, want embedded", mode)
	}
	if notices.Len() != 0 {
		t.Fatalf("preserved embedded scope emitted a storage-mode notice: %q", notices.String())
	}
}

// TestAPreservedEmbeddedScopeNeedsNoRecoveryGuidance is the operator-guidance
// half of ga-qi9km, inverted by the ga-p9iuv contract. The guidance existed
// because canonicalization moved the ledger; nothing moves it now, so the
// correct amount of guidance is none — and the two ways it used to be worse
// than useless are the two things this still refuses to emit.
//
// The first is advice gc itself undoes: "point metadata.json back at the
// embedded database" works until the next boot of a gc that re-canonicalizes,
// leaving the operator in a loop.
//
// The second is overstating what is on disk. `bd init` creates the embedded
// repository before a single bead exists, so "holds a Dolt bead database" is
// all that is knowable — a claim that it holds ROWS is one gc cannot make
// without opening it.
//
// `gc doctor`'s bd-split-store check is asserted here rather than restated: it
// must be quiet about an untouched embedded scope too, or the two would send an
// operator in different directions about the same two directories.
func TestAPreservedEmbeddedScopeNeedsNoRecoveryGuidance(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jc")
	notices := captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc", "proxied-server"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	notice := notices.String()

	if notice != "" {
		t.Fatalf("preserved embedded scope emitted an announcement: %q", notice)
	}
	// Not a restatement: the check the announcement names is run against the
	// scope the announcement was printed for, so a drift in either text or a
	// regression that makes the check silent on this shape fails here.
	result := doctor.NewBDSplitStoreCheck(scope).Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("gc doctor's bd-split-store check reports %v (%q) for an unchanged embedded scope", result.Status, result.Message)
	}
	if result.FixHint != "" {
		t.Errorf("gc doctor emitted split-store guidance for unchanged scope: %q", result.FixHint)
	}
	for _, forbidden := range []string{
		// Naming this edit as a recovery sends the operator round a loop.
		`"dolt_mode": "embedded"`,
		"point .beads/metadata.json back",
		// Nothing on disk supports these.
		"lost", "deleted", "corrupt",
	} {
		if strings.Contains(strings.ToLower(notice), strings.ToLower(forbidden)) {
			t.Errorf("the announcement claims %q, which gc either cannot know or immediately reverts: %q", forbidden, notice)
		}
	}
}

// TestNoDoorFlipsAnEmbeddedScopesStorageMode closes the gap a per-command rule
// always has.
//
// `gc rig set-endpoint` and `gc beads city use-managed`/`use-external` reach
// their own canonicalizers (requireCanonicalizedScopeMetadata for the scope the
// command names, canonicalizeScopeMetadataIfPresent for the inherited rigs a
// city endpoint change sweeps along, both in cmd_rig_endpoint.go) rather than
// the init one, and each used to perform the identical embedded→server rewrite.
// Preservation that depends on which command the operator happened to run — or
// on which of the two endpoint doors the scope arrived through — is no
// preservation at all, so every door is driven here.
func TestNoDoorFlipsAnEmbeddedScopesStorageMode(t *testing.T) {
	for name, canonicalize := range map[string]func(scope string) error{
		"init path": func(scope string) error {
			return ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc", "proxied-server")
		},
		"endpoint path, named scope": func(scope string) error {
			return requireCanonicalizedScopeMetadata(fsys.OSFS{}, scope, scope)
		},
		"endpoint path, inherited rig": func(scope string) error {
			return canonicalizeScopeMetadataIfPresent(fsys.OSFS{}, scope, scope)
		},
	} {
		t.Run(name, func(t *testing.T) {
			scope := embeddedScopeWithBeads(t, "jc")
			notices := captureStorageModeChanges(t)
			if err := canonicalize(scope); err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			if mode := readScopeDoltMode(t, scope); mode != "embedded" {
				t.Fatalf("dolt_mode = %q, want embedded", mode)
			}
			if notices.Len() != 0 {
				t.Fatalf("preserved embedded scope emitted notice: %q", notices.String())
			}
		})
	}
}

// TestCanonicalizingAnAlreadyCanonicalScopeIsSilent keeps the signal worth
// something. Every boot re-canonicalizes every scope; a line per scope per boot
// is a line nobody reads, and the one that matters would arrive inside it.
func TestCanonicalizingAnAlreadyCanonicalScopeIsSilent(t *testing.T) {
	for name, meta := range map[string]map[string]string{
		"already server":   {"database": "dolt", "backend": "dolt", "dolt_mode": "server", "dolt_database": "jc"},
		"no mode recorded": {"database": "dolt", "backend": "dolt", "dolt_database": "jc"},
	} {
		t.Run(name, func(t *testing.T) {
			scope := t.TempDir()
			writeScopeMetadata(t, scope, meta)
			notices := captureStorageModeChanges(t)

			if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc", "proxied-server"); err != nil {
				t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
			}
			if notices.Len() != 0 {
				t.Fatalf("a canonical scope announced a storage-mode change: %q", notices.String())
			}
		})
	}
}

// TestPreservingTheStorageModeNeverChangesWhatAReadAnswers is the preservation
// proof for an existing embedded scope. Canonicalization must not silently
// migrate it to proxied/direct server mode, and reads remain unchanged.
//
// A scope whose metadata already names embedded storage continues answering
// `[]` with nil on every read shape. Existing installs are never auto-converted.
func TestPreservingTheStorageModeNeverChangesWhatAReadAnswers(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jc")
	captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc", "proxied-server"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	if mode := readScopeDoltMode(t, scope); mode != "embedded" {
		t.Fatalf("dolt_mode = %q after canonicalization, want embedded", mode)
	}

	var notices bytes.Buffer
	store := beads.NewBdStore(scope, emptyBdRunner, beads.WithBdStoreNoticeSink(&notices))
	for _, name := range []string{"List", "Ready", "Children"} {
		reads := map[string]func() ([]beads.Bead, error){
			"List":     func() ([]beads.Bead, error) { return store.List(beads.ListQuery{AllowScan: true}) },
			"Ready":    func() ([]beads.Bead, error) { return store.Ready() },
			"Children": func() ([]beads.Bead, error) { return store.Children("jc-1") },
		}
		t.Run(name, func(t *testing.T) {
			got, err := reads[name]()
			if err != nil || len(got) != 0 {
				t.Fatalf("%s = (%d beads, %v), want (0, nil)", name, len(got), err)
			}
		})
	}
	// No mode changed, so no storage-mode or read-time notice is emitted.
	if notices.Len() != 0 {
		t.Fatalf("unexpected read-time notice for preserved embedded scope: %q", notices.String())
	}
}

// TestAnEmptyReadIsNotEvidenceTheScopeIsReadingTheWrongDatabase is the first of
// the two cases that keep a read-time REFUSAL out of this change, and it is the
// one a populated city hits every minute.
//
// A refusal keyed on the presence of a second Dolt directory fires on the
// RESULT of one call, not on the store: `Ready()` returning zero rows is the
// steady state of an idle city and of every assignee-scoped probe, and the
// filtered reads below are answered by a store bd just handed rows for. A city
// that migrated deliberately and kept the old directory — which is the state
// `gc doctor`'s own fix hint tells operators to sit in — would have every one
// of these turn into an error, and `federateBeadLegs` aborts the whole
// federation on any leg error, so `gc ready` exits non-zero for the city and
// every worker's generated work query fails with it.
//
// It is also the acceptance case for the notice ga-clsfl ships: this store is
// demonstrably populated, so it must be answered AND left alone — no error, and
// nothing printed. The notice decides per STORE (has this store ever handed
// back a row?), which is why the first List below immunizes every read after
// it.
//
// Red before ga-clsfl's predecessor, on a scope with metadata pointing at the
// server store and a retained .beads/embeddeddolt/jc:
//
//	List  = (1 beads, <nil>)                    ← the active store is populated
//	Ready = (0 beads, bead store read returned empty while an unread bead database sits beside it…)
func TestAnEmptyReadIsNotEvidenceTheScopeIsReadingTheWrongDatabase(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jc")
	captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc", "proxied-server"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	answering := func(_, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "ready" {
			return []byte(`[]`), nil
		}
		return []byte(`[{"id":"jc-1","title":"real row","status":"open","assignee":"alice"}]`), nil
	}
	var notices bytes.Buffer
	store := beads.NewBdStore(scope, answering, beads.WithBdStoreNoticeSink(&notices))

	got, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil || len(got) != 1 {
		t.Fatalf("List = (%d beads, %v), want (1, nil): this store is demonstrably the populated one", len(got), err)
	}
	t.Cleanup(func() {
		if notices.Len() != 0 {
			t.Errorf("a demonstrably populated store printed the unread-store notice: %q", notices.String())
		}
	})
	for name, read := range map[string]func() ([]beads.Bead, error){
		// The frontier is empty because nothing is claimable right now.
		"empty frontier on a populated store": func() ([]beads.Bead, error) { return store.Ready() },
		// bd answered with a row; the in-process assignee filter dropped it.
		"per-assignee frontier": func() ([]beads.Bead, error) {
			return store.Ready(beads.ReadyQuery{Assignee: "demo/worker"})
		},
		// bd answered with a row; the wisp-tier filter dropped it.
		"wisp tier over issue rows": func() ([]beads.Bead, error) {
			return store.Ready(beads.ReadyQuery{TierMode: beads.TierWisps})
		},
		// A leaf really has no children, and 26 non-test call sites walk them.
		"children of a leaf": func() ([]beads.Bead, error) { return store.Children("jc-9") },
		// An empty inbox is the normal state of a mail poll.
		"mail poll with no mail": func() ([]beads.Bead, error) {
			return store.List(beads.ListQuery{Type: "message", Status: "open", Assignee: "demo/worker"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := read()
			if err != nil {
				t.Fatalf("read returned %d beads and err = %v; an empty answer from a demonstrably populated store is a real answer, and refusing it fails `gc ready` for the whole city", len(got), err)
			}
		})
	}
}

// TestAdoptingAFreshlyInitializedWorkspaceStillReads verifies that an existing
// embedded workspace remains readable during adoption without migration noise.
func TestAdoptingAFreshlyInitializedWorkspaceStillReads(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jkq") // `bd init -p jkq`: empty repo, embedded mode
	notices := captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jkq", "proxied-server"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	if notices.Len() != 0 {
		t.Fatalf("adopting an embedded workspace emitted a storage-mode notice: %q", notices.String())
	}

	var readNotices bytes.Buffer
	store := beads.NewBdStore(scope, emptyBdRunner, beads.WithBdStoreNoticeSink(&readNotices))
	// The readiness gate `gc rig add` and `gc start` block adoption on.
	slept := 0
	if err := verifyCanonicalBdScopeStoreReady(store, func(time.Duration) { slept++ }); err != nil {
		t.Fatalf("verifyCanonicalBdScopeStoreReady = %v, want nil: a rig with no beads yet is not a broken rig", err)
	}
	if slept != 0 {
		t.Fatalf("adoption slept %d time(s) before succeeding; the gate must pass on the first attempt", slept)
	}
	if got, err := store.Ready(); err != nil || len(got) != 0 {
		t.Fatalf("Ready = (%d beads, %v), want (0, nil)", len(got), err)
	}
	if readNotices.Len() != 0 {
		t.Fatalf("unexpected read-time notice for preserved embedded scope: %q", readNotices.String())
	}
}

// TestTheStorageModeAnnouncementIsNotSplitStoreSpecific verifies that a
// single-store embedded city is also preserved without migration output.
func TestTheStorageModeAnnouncementIsNotSplitStoreSpecific(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "hq")
	if _, present := beads.BeadDatabaseDirForDoltMode(scope, "server", "hq"); present {
		t.Fatal("the fixture has a server database; nothing has been rewritten yet")
	}

	notices := captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "hq", "proxied-server"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	if notices.Len() != 0 {
		t.Fatalf("unexpected rewrite notice on a single-store city: %q", notices.String())
	}
	_, present := beads.BeadDatabaseDirForDoltMode(scope, "embedded", "hq")
	if !present {
		t.Fatal("the embedded database gc stopped reading is no longer resolvable")
	}
}

// TestTheThreeMessagesAboutOneUnreadDatabaseAgree verifies that unchanged
// embedded storage produces no contradictory split-store guidance.
func TestTheThreeMessagesAboutOneUnreadDatabaseAgree(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jc")
	announcement := captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc", "proxied-server"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	var readNotice bytes.Buffer
	store := beads.NewBdStore(scope, emptyBdRunner, beads.WithBdStoreNoticeSink(&readNotice))
	if _, err := store.Ready(); err != nil {
		t.Fatalf("Ready() error = %v, want nil", err)
	}
	diagnostic := doctor.NewBDSplitStoreCheck(scope).Run(&doctor.CheckContext{})
	if diagnostic.Status != doctor.StatusOK {
		t.Fatalf("gc doctor reports %v (%q) for an unchanged embedded scope", diagnostic.Status, diagnostic.Message)
	}

	messages := map[string]string{
		"flip-time announcement": announcement.String(),
		"read-time notice":       readNotice.String(),
		"gc doctor fix hint":     diagnostic.FixHint,
	}
	for name, msg := range messages {
		if msg != "" && name != "gc doctor fix hint" {
			t.Errorf("%s unexpectedly emitted split-store guidance: %q", name, msg)
		}
	}
	// Doctor prescribes the state the notice fires in, so it has to name the
	// way to live in that state quietly.
	if diagnostic.FixHint != "" || readNotice.Len() != 0 {
		t.Fatalf("unchanged embedded scope emitted guidance: doctor=%q read=%q", diagnostic.FixHint, readNotice.String())
	}
	// And doctor has to promise the bound the guard actually holds. The guard
	// memoizes per SCOPE PATH inside one process, and cmd/gc builds a throwaway
	// bd store per request on the paths internal/api reads through — so "a
	// one-time notice" was false there, at status-rebuild rate, in a tree with
	// a documented log-flood history. Telling an operator to expect less noise
	// than they will get is the same class of false statement as telling them
	// rows are gone.
}

// legacyManagedCityWithEmbeddedRig builds the shape the config.yaml half of the
// ga-p9iuv contract has to hold on: a legacy GC-managed direct city (server
// metadata, managed_city endpoint) with a rig registered in it whose own
// metadata.json records embedded Dolt storage. rigConfig is the rig's
// .beads/config.yaml as bd — or an earlier gc — left it.
func legacyManagedCityWithEmbeddedRig(t *testing.T, rigConfig string) (string, string, *config.City) {
	t.Helper()
	cityPath := t.TempDir()
	writeScopeMetadata(t, cityPath, map[string]string{
		"database":      "dolt",
		"backend":       "dolt",
		"dolt_mode":     "server",
		"dolt_database": "hq",
	})
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: hq\nissue-prefix: hq\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads", "embeddeddolt", "fr", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeScopeMetadata(t, rigPath, map[string]string{
		"database":      "dolt",
		"backend":       "dolt",
		"dolt_mode":     "embedded",
		"dolt_database": "fr",
	})
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(rigConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "legacy-city"},
		Rigs:      []config.Rig{{Name: "frontend", Path: rigPath, Prefix: "fr"}},
	}
	return cityPath, rigPath, cfg
}

func readScopeConfigKeys(t *testing.T, scope string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(scope, ".beads", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse config.yaml: %v\n%s", err, data)
	}
	keys := map[string]string{}
	var walk func(prefix string, node map[string]any)
	walk = func(prefix string, node map[string]any) {
		for k, v := range node {
			if nested, ok := v.(map[string]any); ok {
				walk(prefix+k+".", nested)
				continue
			}
			keys[prefix+k] = fmt.Sprint(v)
		}
	}
	walk("", doc)
	return keys
}

// embeddedScopeConfigDoors are the lifecycle doors that canonicalize a rig's
// .beads/config.yaml without the operator naming the rig: the boot/doctor/
// reload normalization (which also runs the port-file sync), the per-scope init
// normalization `gc rig add` and `gc init` run, and the per-scope write
// `gc beads city migrate-proxied` performs.
func embeddedScopeConfigDoors(cityPath, rigPath string, cfg *config.City) map[string]func() error {
	return map[string]func() error{
		"boot normalization": func() error {
			return normalizeCanonicalBdScopeFiles(cityPath, cfg, io.Discard)
		},
		"init normalization": func() error {
			return normalizeCanonicalBdScopeFilesForInit(cityPath, rigPath, "fr", "")
		},
		"direct scope write": func() error {
			return normalizeScopeDoltConfig(rigPath, contract.ConfigState{
				IssuePrefix:    "fr",
				EndpointOrigin: contract.EndpointOriginInheritedCity,
				EndpointStatus: contract.EndpointStatusVerified,
				DoltMode:       "server",
			})
		},
	}
}

// TestNoDoorStampsServerTopologyIntoAnEmbeddedScopesConfig is the config.yaml
// half of TestNoDoorFlipsAnEmbeddedScopesStorageMode. Keeping metadata.json at
// embedded while writing `dolt.mode: server` and a gc endpoint origin beside it
// records a topology claim ensureCanonicalScopeMetadata then refuses to act on
// — and that any other reader of config.yaml acts on. It also turns off
// policies (bd's own backup, JSONL export) on a scope gc does not back up or
// serve, and leaves a git-tracked file dirty on every pass.
func TestNoDoorStampsServerTopologyIntoAnEmbeddedScopesConfig(t *testing.T) {
	const bdInitConfig = "issue_prefix: fr\nissue-prefix: fr\nsync.branch: main\n"
	gcOwnedOrPolicy := []string{
		"gc.endpoint_origin", "gc.endpoint_status",
		"dolt.mode", "dolt.host", "dolt.port", "dolt.user", "dolt.socket",
		"dolt.auto-start", "dolt.disable-event-flush",
		"export.auto", "backup.enabled",
	}
	for _, name := range []string{"boot normalization", "init normalization", "direct scope write"} {
		t.Run(name, func(t *testing.T) {
			cityPath, rigPath, cfg := legacyManagedCityWithEmbeddedRig(t, bdInitConfig)
			if err := os.WriteFile(filepath.Join(rigPath, ".beads", "issues.jsonl"), []byte("{}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			captureStorageModeChanges(t)
			if err := embeddedScopeConfigDoors(cityPath, rigPath, cfg)[name](); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if mode := readScopeDoltMode(t, rigPath); mode != "embedded" {
				t.Fatalf("dolt_mode = %q, want embedded", mode)
			}
			keys := readScopeConfigKeys(t, rigPath)
			for _, key := range gcOwnedOrPolicy {
				if value, ok := keys[key]; ok {
					t.Errorf("config.yaml gained %s: %s on an embedded scope: %v", key, value, keys)
				}
			}
			for key, want := range map[string]string{"issue_prefix": "fr", "issue-prefix": "fr", "sync.branch": "main"} {
				if keys[key] != want {
					t.Errorf("config.yaml %s = %q, want %q: %v", key, keys[key], want, keys)
				}
			}
			if !strings.Contains(keys["types.custom"], "molecule") {
				t.Errorf("config.yaml types.custom = %q, want gc's required vocabulary registered", keys["types.custom"])
			}
			if _, err := os.Stat(filepath.Join(rigPath, ".beads", "issues.jsonl")); err != nil {
				t.Errorf("issues.jsonl on an embedded scope was removed: %v", err)
			}
		})
	}
}

// TestAnAlreadyStampedEmbeddedScopeConvergesOffGcsEndpointClaims covers the
// rigs an earlier gc already stamped. The keys gc provably owns — its own
// gc.endpoint_* namespace, and a dolt.mode that contradicts the embedded
// metadata.json beside it — are removed, together with the dolt.host/port/user
// mirror when gc's endpoint marker proves gc wrote that block. The policy keys
// (backup, export, auto-start, event flush) cannot be told apart from an
// operator's own setting, so they are left exactly as found.
func TestAnAlreadyStampedEmbeddedScopeConvergesOffGcsEndpointClaims(t *testing.T) {
	const stamped = "issue_prefix: fr\nissue-prefix: fr\nsync.branch: main\n" +
		"gc.endpoint_origin: inherited_city\ngc.endpoint_status: verified\n" +
		"dolt.mode: server\ndolt.host: 127.0.0.1\ndolt.port: 3307\ndolt.user: root\n" +
		"dolt.auto-start: false\ndolt:\n  disable-event-flush: true\n" +
		"export.auto: false\nbackup.enabled: false\n" +
		"types.custom: molecule,convoy,message,event,gate,merge-request,agent,role,rig,session,spec,convergence,step,startup-health-episode,custom-extra\n"
	for _, name := range []string{"boot normalization", "init normalization", "direct scope write"} {
		t.Run(name, func(t *testing.T) {
			cityPath, rigPath, cfg := legacyManagedCityWithEmbeddedRig(t, stamped)
			captureStorageModeChanges(t)
			door := embeddedScopeConfigDoors(cityPath, rigPath, cfg)[name]
			if err := door(); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			keys := readScopeConfigKeys(t, rigPath)
			for _, key := range []string{"gc.endpoint_origin", "gc.endpoint_status", "dolt.mode", "dolt.host", "dolt.port", "dolt.user"} {
				if value, ok := keys[key]; ok {
					t.Errorf("config.yaml still carries gc-authored %s: %s: %v", key, value, keys)
				}
			}
			for key, want := range map[string]string{
				"issue_prefix":             "fr",
				"sync.branch":              "main",
				"dolt.auto-start":          "false",
				"dolt.disable-event-flush": "true",
				"export.auto":              "false",
				"backup.enabled":           "false",
			} {
				if keys[key] != want {
					t.Errorf("config.yaml %s = %q, want %q left as found: %v", key, keys[key], want, keys)
				}
			}
			if !strings.HasSuffix(keys["types.custom"], ",custom-extra") {
				t.Errorf("types.custom = %q, want operator extension preserved", keys["types.custom"])
			}
			if mode := readScopeDoltMode(t, rigPath); mode != "embedded" {
				t.Fatalf("dolt_mode = %q, want embedded", mode)
			}

			// Converged means the next pass writes nothing.
			before, err := os.ReadFile(filepath.Join(rigPath, ".beads", "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if err := door(); err != nil {
				t.Fatalf("%s (second pass): %v", name, err)
			}
			after, err := os.ReadFile(filepath.Join(rigPath, ".beads", "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("second pass rewrote a converged config.yaml:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// TestAnEmbeddedScopesOwnDoltModeSurvives keeps the scrub honest: a dolt.mode
// that agrees with metadata.json is not gc's claim to remove, and without a gc
// endpoint marker the endpoint keys are not provably gc's either.
func TestAnEmbeddedScopesOwnDoltModeSurvives(t *testing.T) {
	cityPath, rigPath, cfg := legacyManagedCityWithEmbeddedRig(t, "issue_prefix: fr\nissue-prefix: fr\ndolt.mode: embedded\ndolt.port: 3310\n")
	captureStorageModeChanges(t)
	if err := normalizeCanonicalBdScopeFiles(cityPath, cfg, io.Discard); err != nil {
		t.Fatalf("normalizeCanonicalBdScopeFiles: %v", err)
	}
	keys := readScopeConfigKeys(t, rigPath)
	if keys["dolt.mode"] != "embedded" {
		t.Errorf("dolt.mode = %q, want the scope's own embedded value kept: %v", keys["dolt.mode"], keys)
	}
	if keys["dolt.port"] != "3310" {
		t.Errorf("dolt.port = %q, want an unmarked operator key kept: %v", keys["dolt.port"], keys)
	}
}

// TestACityEndpointChangeLeavesAnEmbeddedRigsConfigAlone is the city-endpoint
// sweep door (`gc beads city use-external`/`use-managed`): the rigs it carries
// along inherit the city's server, and an embedded rig has no server to
// inherit. Sweeping it would stamp inherited_city, dolt.host/port and
// dolt.mode: server into the rig's config.yaml while its metadata stays
// embedded, so the plan leaves it out of the update.
func TestACityEndpointChangeLeavesAnEmbeddedRigsConfigAlone(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityDir := t.TempDir()
	rigDir := embeddedScopeWithBeads(t, "fe")
	const bdInitConfig = "issue_prefix: fe\nissue-prefix: fe\n"
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(bdInitConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCityEndpointCityConfigWithCompat(t, cityDir, config.DoltConfig{Host: "old-city.example.com", Port: 3306}, []config.Rig{
		{Name: "frontend", Path: rigDir, Prefix: "fe"},
	})
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointCanonicalConfig(t, cityDir, contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginCityCanonical, EndpointStatus: contract.EndpointStatusVerified, DoltHost: "old-city.example.com", DoltPort: "3306"})
	captureStorageModeChanges(t)

	var stdout, stderr bytes.Buffer
	code := doBeadsCityEndpoint(fsys.OSFS{}, cityDir, cityEndpointOptions{External: true, Host: "db.example.com", Port: "4406", AdoptUnverified: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doBeadsCityEndpoint() = %d, want 0; stderr = %s", code, stderr.String())
	}
	got, err := os.ReadFile(filepath.Join(rigDir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != bdInitConfig {
		t.Fatalf("embedded rig config.yaml was rewritten by the city endpoint sweep:\n%s", got)
	}
	if mode := readScopeDoltMode(t, rigDir); mode != "embedded" {
		t.Fatalf("dolt_mode = %q, want embedded", mode)
	}
}
