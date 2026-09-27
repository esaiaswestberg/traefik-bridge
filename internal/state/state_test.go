package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreSaveLoadAndPermissions(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "state"))
	want := &State{Version: 1, MasterID: "master-1", Slaves: map[string]Slave{}}
	if err := store.Save(want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "state", fileName))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("state file mode = %o, want 600", got)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.MasterID != want.MasterID || loaded.Version != want.Version {
		t.Errorf("Load() = %+v, want %+v", loaded, want)
	}

	want.MasterID = "master-2"
	if err := store.Save(want); err != nil {
		t.Fatalf("second Save() error = %v", err)
	}
	loaded, err = store.Load()
	if err != nil {
		t.Fatalf("second Load() error = %v", err)
	}
	if loaded.MasterID != "master-2" {
		t.Errorf("replaced state master ID = %q, want master-2", loaded.MasterID)
	}
}

func TestLoadRejectsInsecureStateFile(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save(&State{Version: 1}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fileName)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("Load() succeeded for a group/world-readable state file")
	}
}

func TestLoadRejectsInsecureStateDirectory(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save(&State{Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("Load() succeeded for a group/world-accessible state directory")
	}
}

func TestReconcilerStoreSaveLoadAndPermissions(t *testing.T) {
	dir := t.TempDir()
	store := NewReconcilerStore(dir)
	want := &ReconcilerState{Version: 1, Slaves: map[string]ReconcilerSlave{"slave": {EndpointHost: "slave.example", EndpointPort: 8444}}}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, reconcilerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("reconciler state file mode = %o, want 600", got)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Slaves["slave"].EndpointHost; got != "slave.example" {
		t.Errorf("endpoint host = %q", got)
	}
}
