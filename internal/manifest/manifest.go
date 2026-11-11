// Package manifest reads and writes the uploader's per-folder state file,
// which ties a folder of raws to a session and maps each raw file (by path
// relative to the folder) to its photo ID and hash.
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// FileName is the manifest's name inside a session folder.
const FileName = ".review-manifest.json"

const currentVersion = 1

// Manifest is the uploader's state for one folder.
type Manifest struct {
	Version int `json:"version"`
	// APIURL is the API the session lives on, so a folder uploaded to one
	// environment isn't accidentally pointed at another.
	APIURL    string           `json:"apiUrl,omitempty"`
	SessionID string           `json:"sessionId,omitempty"`
	Photos    map[string]Entry `json:"photos"` // slash-separated path relative to the folder -> entry
}

// Entry records an uploaded raw.
type Entry struct {
	PhotoID string `json:"photoId"`
	Hash    string `json:"hash"`
}

// Load reads the manifest in dir. A missing manifest yields an empty one
// (SessionID == "").
func Load(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return &Manifest{Version: currentVersion, Photos: map[string]Entry{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest %s is corrupt: %w", FileName, err)
	}
	if m.Version != currentVersion {
		return nil, fmt.Errorf("manifest %s has unsupported version %d", FileName, m.Version)
	}
	if m.Photos == nil {
		m.Photos = map[string]Entry{}
	}
	return &m, nil
}

// Save writes the manifest to dir atomically (temp file + rename), so an
// interrupted save never leaves a truncated manifest.
func (m *Manifest) Save(dir string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, FileName+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after successful rename
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, FileName))
}
