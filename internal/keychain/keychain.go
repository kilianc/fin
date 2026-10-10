// Package keychain stores secrets in the macOS login Keychain through
// /usr/bin/security. Values are passed on stdin, hex-encoded, so they never
// appear in a process argument list.
package keychain

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

const securityBin = "/usr/bin/security"

// ErrNotFound is returned when no Keychain entry exists for an account.
var ErrNotFound = errors.New("keychain entry not found")

// errSecItemNotFound is the exit status security(1) uses for a missing entry.
const errSecItemNotFound = 44

var validName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Keychain reads and writes generic passwords under one service name.
type Keychain struct {
	Service string
}

func (k Keychain) Get(account string) (string, error) {
	if err := k.check(account); err != nil {
		return "", err
	}
	out, err := exec.Command(securityBin, "find-generic-password", "-s", k.Service, "-a", account, "-w").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == errSecItemNotFound {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("keychain: read %s/%s: %w", k.Service, account, err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// Set creates or replaces the entry, then reads it back to confirm the write,
// because security -i does not reliably report failures in its exit status.
func (k Keychain) Set(account, value string) error {
	if err := k.check(account); err != nil {
		return err
	}
	cmd := exec.Command(securityBin, "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -X %s\n",
		k.Service, account, hex.EncodeToString([]byte(value))))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("keychain: write %s/%s: %w: %s", k.Service, account, err, strings.TrimSpace(string(out)))
	}
	got, err := k.Get(account)
	if err != nil {
		return fmt.Errorf("keychain: verify %s/%s: %w", k.Service, account, err)
	}
	if got != value {
		return fmt.Errorf("keychain: verify %s/%s: stored value does not match", k.Service, account)
	}
	return nil
}

// Delete removes the entry; a missing entry is not an error.
func (k Keychain) Delete(account string) error {
	if err := k.check(account); err != nil {
		return err
	}
	err := exec.Command(securityBin, "delete-generic-password", "-s", k.Service, "-a", account).Run()
	var exitErr *exec.ExitError
	if err != nil && !(errors.As(err, &exitErr) && exitErr.ExitCode() == errSecItemNotFound) {
		return fmt.Errorf("keychain: delete %s/%s: %w", k.Service, account, err)
	}
	return nil
}

func (k Keychain) check(account string) error {
	if !validName.MatchString(k.Service) || !validName.MatchString(account) {
		return fmt.Errorf("keychain: invalid service or account name %q/%q", k.Service, account)
	}
	return nil
}
