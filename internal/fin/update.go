package fin

import (
	"archive/tar"
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kilianc/fin/internal/ui"
)

// ReleasesURL is where fin's releases live. Each release carries the
// universal macOS binary and its SHA256SUMS.
const ReleasesURL = "https://github.com/kilianc/fin/releases"

const releaseAsset = "fin-darwin-universal.tar.gz"

// updateCheckEvery is how often fin init, fin help and fin version look for
// a newer release. fin never installs one on its own: fin update does,
// when someone runs it.
const updateCheckEvery = 24 * time.Hour

var releaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// newerVersion reports whether latest is a later X.Y.Z than current.
func newerVersion(latest, current string) bool {
	if !releaseVersion.MatchString(latest) || !releaseVersion.MatchString(current) {
		return false
	}
	l, c := strings.Split(latest, "."), strings.Split(current, ".")
	for i := range 3 {
		a, _ := strconv.Atoi(l[i])
		b, _ := strconv.Atoi(c[i])
		if a != b {
			return a > b
		}
	}
	return false
}

func (a *App) releaseClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// latestRelease asks GitHub which release is latest, from where
// releases/latest redirects, so it needs no API call or token.
func (a *App) latestRelease(ctx context.Context, timeout time.Duration) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.ReleasesURL+"/latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := a.releaseClient(timeout).Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	tag := loc[strings.LastIndex(loc, "/")+1:]
	version := strings.TrimPrefix(tag, "v")
	if resp.StatusCode/100 != 3 || !releaseVersion.MatchString(version) {
		return "", fmt.Errorf("could not tell the latest release from %s (HTTP %d)", a.ReleasesURL, resp.StatusCode)
	}
	return version, nil
}

type updateCheck struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
}

// availableUpdate returns a newer release than this one, if there is one.
// It asks GitHub at most once a day, briefly, and stays quiet when it can't
// tell: a check must never slow down or break the command it rides on.
func (a *App) availableUpdate(ctx context.Context) string {
	if a.ReleasesURL == "" || !releaseVersion.MatchString(a.Version) || os.Getenv("FIN_NO_UPDATE_CHECK") != "" {
		return ""
	}
	path := filepath.Join(a.DataDir, "update-check.json")
	var c updateCheck
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if a.Now().Sub(c.CheckedAt) >= updateCheckEvery || c.CheckedAt.After(a.Now()) {
		c = updateCheck{CheckedAt: a.Now().UTC()}
		c.Latest, _ = a.latestRelease(ctx, 2*time.Second)
		if b, err := json.Marshal(c); err == nil && os.MkdirAll(a.DataDir, 0o700) == nil {
			_ = os.WriteFile(path, b, 0o600)
		}
	}
	if newerVersion(c.Latest, a.Version) {
		return c.Latest
	}
	return ""
}

// updateNotice is the one line fin init, fin help and fin version add when
// a newer release exists.
func updateNotice(latest string) string {
	return fmt.Sprintf("fin %s is available: run fin update", latest)
}

// cmdVersion prints "fin X.Y.Z" alone for scripts. A person at a terminal,
// or --json, also hears about a newer release.
func (a *App) cmdVersion(asJSON bool) int {
	current := cmp.Or(a.Version, "dev")
	if asJSON {
		body := map[string]any{"version": current, "latest": nil}
		if latest := a.availableUpdate(context.Background()); latest != "" {
			body["latest"], body["update"] = latest, "fin update"
		}
		return a.emit(&result{body: body})
	}
	fmt.Fprintf(a.Stdout, "fin %s\n", current)
	if a.human {
		if latest := a.availableUpdate(context.Background()); latest != "" {
			fmt.Fprintln(a.Stdout, ui.Muted.Render(updateNotice(latest)))
		}
	}
	return exitOK
}

// --- update ---

type updateView struct {
	Current string `json:"current"`
	Latest  string `json:"latest"`
	Updated bool   `json:"updated"`
	Path    string `json:"path,omitempty"`
}

func (a *App) cmdUpdate(ctx context.Context, args []string) (*result, error) {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	check := fs.Bool("check", false, "only say whether a newer release exists")
	if _, err := parseArgs(fs, args); err != nil {
		return nil, err
	}
	if a.ReleasesURL == "" {
		return nil, newErr("UPDATE_ERROR", "this build of fin has no release source")
	}
	latest, err := a.latestRelease(ctx, 15*time.Second)
	if err != nil {
		return nil, newErr("UPDATE_ERROR", "%v", err)
	}
	view := updateView{Current: cmp.Or(a.Version, "dev"), Latest: latest}
	if !releaseVersion.MatchString(a.Version) {
		return nil, newErr("UPDATE_NOT_RELEASE", "this fin (%s) was not installed from a release, so fin update leaves it alone; reinstall from %s, or rebuild it the way you built it", view.Current, ReleasesURL)
	}
	if !newerVersion(latest, a.Version) {
		return &result{body: view, message: ui.Line(ui.Good, fmt.Sprintf("fin %s is the latest release", a.Version))}, nil
	}
	if *check {
		return &result{body: view, message: updateNotice(latest)}, nil
	}
	exe, err := a.executable()
	if err != nil {
		return nil, newErr("UPDATE_ERROR", "find this fin: %v", err)
	}
	binary, err := a.downloadRelease(ctx, latest)
	if err != nil {
		return nil, newErr("UPDATE_ERROR", "%v", err)
	}
	if err := replaceBinary(ctx, exe, binary, latest); err != nil {
		return nil, newErr("UPDATE_ERROR", "%v", err)
	}
	view.Updated, view.Path = true, exe
	_ = os.Remove(filepath.Join(a.DataDir, "update-check.json"))
	return &result{body: view, message: ui.Line(ui.Good, fmt.Sprintf("Updated fin %s → %s", a.Version, latest)) + ui.Muted.Render("  "+exe)}, nil
}

func (a *App) executable() (string, error) {
	if a.Executable != nil {
		return a.Executable()
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// downloadRelease fetches a release's archive and SHA256SUMS, checks one
// against the other, and returns the fin binary inside.
func (a *App) downloadRelease(ctx context.Context, version string) ([]byte, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	get := func(name string, limit int64) ([]byte, error) {
		url := fmt.Sprintf("%s/download/v%s/%s", a.ReleasesURL, version, name)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", name, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("download %s: HTTP %d", name, resp.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", name, err)
		}
		if int64(len(b)) > limit {
			return nil, fmt.Errorf("download %s: larger than expected", name)
		}
		return b, nil
	}
	sums, err := get("SHA256SUMS", 64<<10)
	if err != nil {
		return nil, err
	}
	want := ""
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && strings.TrimPrefix(f[1], "*") == releaseAsset {
			want = strings.ToLower(f[0])
		}
	}
	if want == "" {
		return nil, fmt.Errorf("SHA256SUMS for %s does not list %s", version, releaseAsset)
	}
	archive, err := get(releaseAsset, 512<<20)
	if err != nil {
		return nil, err
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("%s does not match its SHA256SUMS; nothing was changed", releaseAsset)
	}
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", releaseAsset, err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s has no fin binary", releaseAsset)
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", releaseAsset, err)
		}
		if h.Typeflag == tar.TypeReg && strings.TrimPrefix(h.Name, "./") == "fin" {
			return io.ReadAll(io.LimitReader(tr, 512<<20))
		}
	}
}

// replaceBinary writes the new fin beside the old one, checks that it runs
// and is the version expected, then renames it over the old one, so an
// interrupted update leaves the old fin in place.
func replaceBinary(ctx context.Context, exe string, binary []byte, version string) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".fin-update-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s (%v); reinstall from %s instead", exe, err, ReleasesURL)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(binary); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, tmp.Name(), "--version").Output()
	if err != nil || strings.TrimSpace(string(out)) != "fin "+version {
		return fmt.Errorf("the downloaded fin did not report version %s; nothing was changed", version)
	}
	return os.Rename(tmp.Name(), exe)
}
