package fin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// releases serves a fake GitHub releases page: latest redirects to
// version, whose archive holds a fin that reports reports.
type releases struct {
	version, reports string
	badSum           bool
	latestCalls      atomic.Int32
}

func (r *releases) serve(t *testing.T) string {
	t.Helper()
	var archive bytes.Buffer
	zw := gzip.NewWriter(&archive)
	tw := tar.NewWriter(zw)
	script := []byte("#!/bin/sh\necho 'fin " + r.reports + "'\n")
	if err := tw.WriteHeader(&tar.Header{Name: "fin", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Write(script)
	tw.Close()
	zw.Close()
	sum := sha256.Sum256(archive.Bytes())
	if r.badSum {
		sum[0] ^= 0xff
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/latest":
			r.latestCalls.Add(1)
			http.Redirect(w, req, "/tag/v"+r.version, http.StatusFound)
		case "/download/v" + r.version + "/SHA256SUMS":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), releaseAsset)
		case "/download/v" + r.version + "/" + releaseAsset:
			w.Write(archive.Bytes())
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func updateApp(t *testing.T, r *releases) (*testApp, string) {
	t.Helper()
	ta := newTestApp(t)
	ta.Version, ta.ReleasesURL = "0.4.1", r.serve(t)
	exe := filepath.Join(t.TempDir(), "fin")
	if err := os.WriteFile(exe, []byte("old fin"), 0o755); err != nil {
		t.Fatal(err)
	}
	ta.Executable = func() (string, error) { return exe, nil }
	return ta, exe
}

func TestUpdateReplacesFinWithTheCheckedRelease(t *testing.T) {
	ta, exe := updateApp(t, &releases{version: "0.5.0", reports: "0.5.0"})
	code, body := ta.run(t, "update", "--check", "--json")
	if b, _ := os.ReadFile(exe); code != exitOK || body["latest"] != "0.5.0" || body["updated"] != false || string(b) != "old fin" {
		t.Fatalf("check exit=%d body=%v", code, body)
	}
	code, body = ta.run(t, "update", "--json")
	if code != exitOK || body["updated"] != true || body["path"] != exe {
		t.Fatalf("update exit=%d body=%v stderr=%s", code, body, ta.stderr)
	}
	if b, _ := os.ReadFile(exe); !bytes.Contains(b, []byte("echo 'fin 0.5.0'")) {
		t.Errorf("binary not replaced: %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".fin-update-*")); len(left) != 0 {
		t.Errorf("left temp files: %v", left)
	}
}

func TestUpdateChangesNothingUnlessTheReleaseChecksOut(t *testing.T) {
	for name, r := range map[string]*releases{
		"checksum": {version: "0.5.0", reports: "0.5.0", badSum: true},
		"version":  {version: "0.5.0", reports: "0.4.9"},
	} {
		ta, exe := updateApp(t, r)
		code, _ := ta.run(t, "update", "--json")
		if e := ta.stderrJSON(t); code != exitError || e["code"] != "UPDATE_ERROR" {
			t.Errorf("%s: exit=%d error=%v", name, code, e)
		}
		if b, _ := os.ReadFile(exe); string(b) != "old fin" {
			t.Errorf("%s: binary changed to %q", name, b)
		}
	}
}

func TestUpdateLeavesSourceBuildsAndCurrentReleasesAlone(t *testing.T) {
	ta, exe := updateApp(t, &releases{version: "0.4.1", reports: "0.4.1"})
	code, body := ta.run(t, "update", "--json")
	if b, _ := os.ReadFile(exe); code != exitOK || body["updated"] != false || string(b) != "old fin" {
		t.Errorf("up to date: exit=%d body=%v", code, body)
	}
	ta.Version = "dev"
	code, _ = ta.run(t, "update", "--json")
	if e := ta.stderrJSON(t); code != exitError || e["code"] != "UPDATE_NOT_RELEASE" {
		t.Errorf("dev build: exit=%d error=%v", code, e)
	}
}

func TestNewReleaseNoticeAsksGitHubOnceADay(t *testing.T) {
	r := &releases{version: "0.5.0", reports: "0.5.0"}
	ta, _ := updateApp(t, r)
	code, body := ta.run(t, "init", "--json")
	if u, _ := body["update"].(map[string]any); code != exitOK || u["latest"] != "0.5.0" || u["command"] != "fin update" {
		t.Fatalf("init update=%v", body["update"])
	}
	if code, body := ta.run(t, "version", "--json"); code != exitOK || body["latest"] != "0.5.0" {
		t.Errorf("version=%v", body)
	}
	ta.stdout.Reset()
	ta.Run(context.Background(), []string{"version"})
	if ta.stdout.String() != "fin 0.4.1\n" {
		t.Errorf("plain version for scripts = %q", ta.stdout.String())
	}
	if n := r.latestCalls.Load(); n != 1 {
		t.Errorf("asked GitHub %d times within a day", n)
	}
	ta.Now = func() time.Time { return testNow.Add(25 * time.Hour) }
	ta.run(t, "version", "--json")
	if n := r.latestCalls.Load(); n != 2 {
		t.Errorf("did not ask again after a day: %d", n)
	}
	r.version = "0.4.1"
	ta.Now = func() time.Time { return testNow.Add(50 * time.Hour) }
	if _, body := ta.run(t, "init", "--json"); body["update"] != nil {
		t.Errorf("notice for the version already installed: %v", body["update"])
	}
}
