package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const currentVersion = 1

type State struct {
	Version        int       `json:"version"`
	SchoolOrigin   string    `json:"school_origin"`
	DeviceID       string    `json:"device_id"`
	DeviceName     string    `json:"device_name"`
	WebSocketURL   string    `json:"websocket_url"`
	LocalBaseURL   string    `json:"local_base_url"`
	PairedAt       time.Time `json:"paired_at"`
	TokenInKeyring bool      `json:"token_in_keyring"`
	DeviceToken    string    `json:"device_token,omitempty"`
}

type RuntimeStatus struct {
	Connected       bool      `json:"connected"`
	DeviceID        string    `json:"device_id,omitempty"`
	LastConnectedAt time.Time `json:"last_connected_at,omitempty"`
	LastEventAt     time.Time `json:"last_event_at"`
	LastError       string    `json:"last_error,omitempty"`
}

type Keyring interface {
	Store(deviceID, token string) error
	Load(deviceID string) (string, error)
	Delete(deviceID string) error
}

type SecretToolKeyring struct {
	Path string
}

func (keyring SecretToolKeyring) executable() (string, error) {
	if keyring.Path != "" {
		return keyring.Path, nil
	}
	path, err := exec.LookPath("secret-tool")
	if err != nil {
		return "", errors.New("secret-tool is not installed")
	}
	return path, nil
}

func (keyring SecretToolKeyring) Store(deviceID, token string) error {
	path, err := keyring.executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "store", "--label=EduKod Local Companion", "service", "edukod-companion", "device", deviceID)
	command.Stdin = strings.NewReader(token)
	command.Stdout = io.Discard
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("secret-tool store failed: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (keyring SecretToolKeyring) Load(deviceID string) (string, error) {
	path, err := keyring.executable()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "lookup", "service", "edukod-companion", "device", deviceID)
	var output bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &output
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("secret-tool lookup failed: %s", strings.TrimSpace(stderr.String()))
	}
	token := strings.TrimSpace(output.String())
	if token == "" {
		return "", errors.New("device credential is missing from the keyring")
	}
	return token, nil
}

func (keyring SecretToolKeyring) Delete(deviceID string) error {
	path, err := keyring.executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "clear", "service", "edukod-companion", "device", deviceID)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Run()
}

type Store struct {
	Directory      string
	Keyring        Keyring
	RequireKeyring bool
}

func DefaultDirectory() (string, error) {
	if root := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); root != "" {
		return filepath.Join(root, "edukod-companion"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "edukod-companion"), nil
}

func NewStore(directory string) (*Store, error) {
	if directory == "" {
		var err error
		directory, err = DefaultDirectory()
		if err != nil {
			return nil, err
		}
	}
	return &Store{Directory: filepath.Clean(directory), Keyring: SecretToolKeyring{}}, nil
}

func (store *Store) statePath() string {
	return filepath.Join(store.Directory, "config.json")
}

func (store *Store) statusPath() string {
	return filepath.Join(store.Directory, "status.json")
}

func ensurePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("companion config directory must be a real directory")
	}
	if info.Mode().Perm()&0077 != 0 {
		if err := os.Chmod(path, 0700); err != nil {
			return err
		}
	}
	return nil
}

func atomicWrite(path string, payload []byte) error {
	directory := filepath.Dir(path)
	if err := ensurePrivateDirectory(directory); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to write through a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".edukod-companion-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

func (store *Store) Save(state State, token string) error {
	if state.DeviceID == "" || token == "" {
		return errors.New("device id and credential are required")
	}
	state.Version = currentVersion
	state.DeviceToken = ""
	state.TokenInKeyring = false
	var keyringErr error
	if store.Keyring != nil {
		keyringErr = store.Keyring.Store(state.DeviceID, token)
	}
	if store.Keyring != nil && keyringErr == nil {
		state.TokenInKeyring = true
	} else if store.RequireKeyring {
		return fmt.Errorf("store device credential in Secret Service: %w", keyringErr)
	} else {
		state.DeviceToken = token
	}
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if err := atomicWrite(store.statePath(), payload); err != nil {
		if state.TokenInKeyring && store.Keyring != nil {
			_ = store.Keyring.Delete(state.DeviceID)
		}
		return err
	}
	return nil
}

func (store *Store) Exists() (bool, error) {
	_, err := os.Lstat(store.statePath())
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (store *Store) Load() (State, string, error) {
	var state State
	info, err := os.Lstat(store.statePath())
	if err != nil {
		return state, "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return state, "", errors.New("companion config must be a regular file")
	}
	if info.Mode().Perm()&0077 != 0 {
		return state, "", errors.New("companion config permissions must be 0600")
	}
	payload, err := os.ReadFile(store.statePath())
	if err != nil {
		return state, "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return state, "", err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return state, "", errors.New("companion config contains trailing data")
	}
	if state.Version != currentVersion || state.DeviceID == "" || state.WebSocketURL == "" || state.LocalBaseURL == "" {
		return state, "", errors.New("companion config is incomplete or unsupported")
	}
	if state.TokenInKeyring {
		if store.Keyring == nil {
			return state, "", errors.New("device credential requires an unavailable keyring")
		}
		token, err := store.Keyring.Load(state.DeviceID)
		return state, token, err
	}
	if state.DeviceToken == "" {
		return state, "", errors.New("device credential is missing")
	}
	return state, state.DeviceToken, nil
}

func (store *Store) Delete() error {
	state, _, loadErr := store.Load()
	if loadErr == nil && state.TokenInKeyring && store.Keyring != nil {
		_ = store.Keyring.Delete(state.DeviceID)
	}
	for _, path := range []string{store.statePath(), store.statusPath()} {
		if info, err := os.Lstat(path); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return errors.New("refusing to delete a symlink")
			}
			if err := os.Remove(path); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (store *Store) SaveStatus(status RuntimeStatus) error {
	status.LastEventAt = time.Now().UTC()
	if len(status.LastError) > 500 {
		status.LastError = status.LastError[:500]
	}
	payload, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(store.statusPath(), append(payload, '\n'))
}

func (store *Store) LoadStatus() (RuntimeStatus, error) {
	var status RuntimeStatus
	payload, err := os.ReadFile(store.statusPath())
	if err != nil {
		return status, err
	}
	err = json.Unmarshal(payload, &status)
	return status, err
}
