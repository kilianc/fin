// Command fin reads bank and brokerage data from Plaid. See README.md.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"charm.land/lipgloss/v2"
	"golang.org/x/term"

	"github.com/kilianc/fin/internal/fin"
	"github.com/kilianc/fin/internal/keychain"
	"github.com/kilianc/fin/internal/plaid"
	"github.com/kilianc/fin/internal/state"
	"github.com/kilianc/fin/internal/store"
	"github.com/kilianc/fin/internal/ui"
)

// version is set by scripts/build.sh with -ldflags "-X main.version=X.Y.Z".
var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	statePath, err := state.DefaultPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "{\"error\": {\"code\": \"STATE_ERROR\", \"message\": %q}}\n", err.Error())
		return 1
	}
	dataDir, err := store.DefaultDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "{\"error\": {\"code\": \"STORE_ERROR\", \"message\": %q}}\n", err.Error())
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	isTTY := func(f *os.File) func() bool {
		return func() bool { return term.IsTerminal(int(f.Fd())) }
	}
	if isTTY(os.Stdin)() && isTTY(os.Stdout)() {
		ui.SetDarkBackground(lipgloss.HasDarkBackground(os.Stdin, os.Stdout))
	}
	// Plaid's CLI keeps its config in the OS config dir: ~/Library/Application
	// Support on macOS, ~/.config on Linux.
	configDir, _ := os.UserConfigDir()
	app := &fin.App{
		Version:        version,
		EnvVar:         os.Getenv("PLAID_ENV"),
		StatePath:      statePath,
		DataDir:        dataDir,
		PlaidCLIConfig: filepath.Join(configDir, "plaid-cli", "config.json"),
		Secrets:        keychain.Keychain{Service: "fin"},
		NewPlaid: func(env plaid.Env, clientID, secret string) fin.Plaid {
			return plaid.NewClient(env, clientID, secret)
		},
		Stdin:            os.Stdin,
		Stdout:           os.Stdout,
		Stderr:           os.Stderr,
		Now:              time.Now,
		IsTerminal:       isTTY(os.Stdin),
		StdoutIsTerminal: isTTY(os.Stdout),
		StderrIsTerminal: isTTY(os.Stderr),
		Width: func() int {
			for _, f := range []*os.File{os.Stdout, os.Stderr} {
				if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 0 {
					return w
				}
			}
			return 80
		},
		ReadLine: func(prompt string) (string, error) {
			return ui.Prompt(os.Stdin, os.Stderr, prompt, "", false)
		},
		ReadSecret: func(prompt string) (string, error) {
			return ui.Prompt(os.Stdin, os.Stderr, prompt, "paste, then press Enter", true)
		},
		OpenURL: func(url string) error { return exec.Command("/usr/bin/open", url).Run() },
		Copy: func(text string) error {
			cmd := exec.Command("/usr/bin/pbcopy")
			cmd.Stdin = strings.NewReader(text)
			return cmd.Run()
		},
		PollInterval: 3 * time.Second,
		Debug:        os.Getenv("FIN_DEBUG") != "",
	}
	return app.Run(ctx, os.Args[1:])
}
