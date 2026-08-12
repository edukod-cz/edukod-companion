package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeKeyring struct {
	token    string
	storeErr error
	deleted  bool
}

func (keyring *fakeKeyring) Store(_ string, token string) error {
	if keyring.storeErr != nil {
		return keyring.storeErr
	}
	keyring.token = token
	return nil
}

func (keyring *fakeKeyring) Load(_ string) (string, error) {
	if keyring.token == "" {
		return "", errors.New("missing")
	}
	return keyring.token, nil
}

func (keyring *fakeKeyring) Delete(_ string) error {
	keyring.deleted = true
	keyring.token = ""
	return nil
}

func testState() State {
	return State{
		SchoolOrigin: "https://school.example.test",
		DeviceID:     "device-1",
		DeviceName:   "AI workstation",
		WebSocketURL: "wss://school.example.test/api/ai/companion/v1/ws",
		LocalBaseURL: "http://127.0.0.1:11434/v1",
		PairedAt:     time.Now().UTC(),
	}
}

func TestSaveUsesKeyringAndPrivateConfig(t *testing.T) {
	directory := t.TempDir() + "/config"
	keyring := &fakeKeyring{}
	store := &Store{Directory: directory, Keyring: keyring}
	if err := store.Save(testState(), "secret-token"); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(filepath.Join(directory, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) == "" || contains(string(payload), "secret-token") {
		t.Fatal("keyring-backed config must not contain the token")
	}
	info, err := os.Stat(filepath.Join(directory, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
	_, token, err := store.Load()
	if err != nil || token != "secret-token" {
		t.Fatalf("Load token = %q, %v", token, err)
	}
}

func TestSaveFallsBackToPrivateFile(t *testing.T) {
	directory := t.TempDir() + "/config"
	store := &Store{Directory: directory, Keyring: &fakeKeyring{storeErr: errors.New("unavailable")}}
	if err := store.Save(testState(), "fallback-token"); err != nil {
		t.Fatal(err)
	}
	_, token, err := store.Load()
	if err != nil || token != "fallback-token" {
		t.Fatalf("Load token = %q, %v", token, err)
	}
}

func TestSaveCanRequireKeyring(t *testing.T) {
	directory := t.TempDir() + "/config"
	store := &Store{
		Directory:      directory,
		Keyring:        &fakeKeyring{storeErr: errors.New("locked")},
		RequireKeyring: true,
	}
	if err := store.Save(testState(), "device-token"); err == nil {
		t.Fatal("Save silently fell back despite requiring the keyring")
	}
	exists, err := store.Exists()
	if err != nil || exists {
		t.Fatalf("Exists = %v, %v; failed save must not create config", exists, err)
	}
}

func TestLoadRejectsLoosePermissions(t *testing.T) {
	directory := t.TempDir() + "/config"
	store := &Store{Directory: directory, Keyring: &fakeKeyring{storeErr: errors.New("unavailable")}}
	if err := store.Save(testState(), "fallback-token"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "config.json")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load(); err == nil {
		t.Fatal("Load accepted world-readable credentials")
	}
}

func contains(value, part string) bool {
	for index := 0; index+len(part) <= len(value); index++ {
		if value[index:index+len(part)] == part {
			return true
		}
	}
	return false
}
