package manifest

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadMissing(t *testing.T) {
	m, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if m.SessionID != "" || m.Photos == nil || m.Version != currentVersion {
		t.Errorf("unexpected empty manifest: %+v", m)
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := &Manifest{
		Version:   currentVersion,
		APIURL:    "http://localhost:8080",
		SessionID: "S1",
		Photos:    map[string]Entry{"DSC0001.ARW": {PhotoID: "P1", Hash: "abc"}},
	}
	if err := want.Save(dir); err != nil {
		t.Fatal(err)
	}
	// Saving again overwrites cleanly.
	want.Photos["DSC0002.ARW"] = Entry{PhotoID: "P2", Hash: "def"}
	if err := want.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("expected only the manifest in dir, found %d entries", len(entries))
	}
}

func TestLoadInvalid(t *testing.T) {
	for name, content := range map[string]string{
		"corrupt":     "{not json",
		"bad version": `{"version": 99, "photos": {}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o644)
			if _, err := Load(dir); err == nil {
				t.Error("Load succeeded")
			}
		})
	}
}
