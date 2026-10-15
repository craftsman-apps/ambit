// Package connect holds what `ambit connect` and `ambit disconnect` need: where the key is stored, the state file,
// and the edits of the tools' config files.
package connect

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

// ErrNotFound means no key is stored for the address.
var ErrNotFound = errors.New("no key stored")

// Store keeps one key per gateway address (the normalised --url).
type Store interface {
	Get(url string) (string, error) // ErrNotFound when there is none
	Set(url, key string) error
	Delete(url string) error // no error when there is none
}

// Keychain is the operating system's store: macOS Keychain, Windows Credential Manager, Secret Service.
type Keychain struct {
	Service string
	Timeout time.Duration // Get: a locked collection must not hang `key print`, which tools run at every start
	// WriteTimeout is for Set and Delete during connect and disconnect. On a Linux desktop they show an unlock
	// dialog that a person has to answer.
	WriteTimeout time.Duration
	get          func(service, user string) (string, error)
	set          func(service, user, password string) error
	del          func(service, user string) error
}

func NewKeychain(service string) Keychain {
	return Keychain{Service: service, Timeout: 2 * time.Second, WriteTimeout: 60 * time.Second, get: keyring.Get, set: keyring.Set, del: keyring.Delete}
}

// within runs f and gives up after the timeout. The call keeps running in the background; the process ends soon.
func within[T any](timeout time.Duration, f func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() { v, err := f(); ch <- result{v, err} }()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-time.After(timeout):
		var zero T
		return zero, fmt.Errorf("the keychain did not answer within %s", timeout)
	}
}

func (k Keychain) Get(url string) (string, error) {
	v, err := within(k.Timeout, func() (string, error) { return k.get(k.Service, url) })
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	return v, err
}

func (k Keychain) Set(url, key string) error {
	_, err := within(k.WriteTimeout, func() (struct{}, error) { return struct{}{}, k.set(k.Service, url, key) })
	return err
}

func (k Keychain) Delete(url string) error {
	_, err := within(k.WriteTimeout, func() (struct{}, error) { return struct{}{}, k.del(k.Service, url) })
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// FileStore is the fallback: one file per address in Dir, readable by the current user only.
type FileStore struct{ Dir string }

// DefaultFileStore is <os.UserConfigDir>/ambit/keys, next to the local state store.
func DefaultFileStore() (FileStore, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return FileStore{}, err
	}
	return FileStore{Dir: filepath.Join(d, "ambit", "keys")}, nil
}

// Path is Dir/<first 16 hex digits of sha256(url)>: the address is not a file name.
func (s FileStore) Path(url string) string {
	h := sha256.Sum256([]byte(url))
	return filepath.Join(s.Dir, hex.EncodeToString(h[:])[:16])
}

func (s FileStore) Get(url string) (string, error) {
	b, err := os.ReadFile(s.Path(url))
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNotFound
	}
	return strings.TrimSpace(string(b)), err
}

func (s FileStore) Set(url, key string) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	return atomicWrite(s.Path(url), []byte(key+"\n"), 0o600)
}

func (s FileStore) Delete(url string) error {
	if err := os.Remove(s.Path(url)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Store names written to the state file.
const (
	StoreKeychain = "keychain"
	StoreFile     = "file"
)

// secretServiceMissing is the text of the errors go-keyring returns when no Secret Service runs (no D-Bus
// session: SSH, WSL without a desktop, containers). A locked collection is a different error and fails.
var secretServiceMissing = []string{"was not provided by any .service files", "ServiceUnknown", "session bus", "DBUS_SESSION_BUS_ADDRESS", "dbus:"}

func noSecretService(err error) bool {
	for _, s := range secretServiceMissing {
		if strings.Contains(err.Error(), s) {
			return true
		}
	}
	return false
}

// Put stores the key and returns the store that holds it, and a warning when the file fallback was used. mode is
// "keychain" (or empty) or "file". On macOS and Windows a keychain error is a failure, never a silent fallback:
// someone who clicked "Deny" did not ask for a plain file.
func Put(kc, file Store, goos, mode, url, key string) (store, warning string, err error) {
	switch mode {
	case "", StoreKeychain:
		err = kc.Set(url, key)
		if err == nil {
			return StoreKeychain, "", nil
		}
		if goos == "darwin" || goos == "windows" || !noSecretService(err) {
			return "", "", fmt.Errorf("the keychain refused the key (%w); unlock your keychain, or use --key-store file", err)
		}
	case StoreFile:
	default:
		return "", "", fmt.Errorf("--key-store must be keychain or file, not %q", mode)
	}
	if err := file.Set(url, key); err != nil {
		return "", "", err
	}
	if fsd, ok := file.(FileStore); ok && mode != StoreFile {
		return StoreFile, "no keychain found; the key is stored in " + fsd.Path(url) + ", readable only by you", nil
	}
	return StoreFile, "", nil
}
