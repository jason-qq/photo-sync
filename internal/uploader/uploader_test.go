package uploader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"photo-sync/internal/api"
	"photo-sync/internal/apiclient"
	"photo-sync/internal/manifest"
	"photo-sync/internal/testserver"
)

// fakeRaw returns a minimal TIFF-headed file with an embedded JPEG whose
// color depends on seed, so different seeds give different hashes.
func fakeRaw(t *testing.T, seed int) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("II*\x00\x08\x00\x00\x00\x00\x00") // TIFF header, empty IFD0
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for i := range img.Pix {
		img.Pix[i] = uint8(seed * 40)
	}
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func writeFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

type harness struct {
	t      *testing.T
	env    *testserver.Env
	client *apiclient.Client
	dir    string
	out    bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	env := testserver.Start(t)
	dir := t.TempDir()
	for i, name := range []string{"DSC0001.ARW", "DSC0002.arw", "DSC0003.NEF"} {
		writeFile(t, dir, name, fakeRaw(t, i+1))
	}
	writeFile(t, dir, "notes.txt", []byte("ignored"))
	writeFile(t, dir, ".hidden.ARW", []byte("ignored"))
	return &harness{t: t, env: env, client: apiclient.New(env.URL, testserver.APIKey), dir: dir}
}

func (h *harness) run(force bool) Summary {
	h.t.Helper()
	h.out.Reset()
	sum, err := Run(context.Background(), Config{
		Dir: h.dir, Client: h.client, APIURL: h.env.URL, Date: "2026-11-09", Force: force, Workers: 2, Out: &h.out,
	})
	if err != nil {
		h.t.Fatalf("Run: %v\noutput:\n%s", err, h.out.String())
	}
	return sum
}

func (h *harness) manifest() *manifest.Manifest {
	h.t.Helper()
	m, err := manifest.Load(h.dir)
	if err != nil {
		h.t.Fatal(err)
	}
	return m
}

func expect(t *testing.T, got Summary, uploaded, skipped, failed int) {
	t.Helper()
	if got.Uploaded != uploaded || got.Skipped != skipped || got.Failed != failed {
		t.Errorf("summary = %d uploaded / %d skipped / %d failed, want %d / %d / %d",
			got.Uploaded, got.Skipped, got.Failed, uploaded, skipped, failed)
	}
}

func TestRunUploadsThenSkips(t *testing.T) {
	h := newHarness(t)

	first := h.run(false)
	expect(t, first, 3, 0, 0)

	m := h.manifest()
	if m.SessionID != first.SessionID || m.APIURL != h.env.URL || len(m.Photos) != 3 {
		t.Fatalf("manifest: %+v", m)
	}
	for name, e := range m.Photos {
		thumb := filepath.Join(h.env.BlobDir, "sessions", m.SessionID, e.PhotoID, "thumb.jpg")
		if _, err := os.Stat(thumb); err != nil {
			t.Errorf("%s: thumb not stored: %v", name, err)
		}
	}

	second := h.run(false)
	expect(t, second, 0, 3, 0)
	if second.SessionID != first.SessionID {
		t.Error("second run created a new session")
	}
}

func TestRunForce(t *testing.T) {
	h := newHarness(t)
	h.run(false)
	expect(t, h.run(true), 3, 0, 0)
	if !strings.Contains(h.out.String(), "re-uploaded") {
		t.Errorf("expected re-uploaded lines, got:\n%s", h.out.String())
	}
}

func TestRunChangedFile(t *testing.T) {
	h := newHarness(t)
	h.run(false)
	before := h.manifest().Photos["DSC0001.ARW"]

	writeFile(t, h.dir, "DSC0001.ARW", fakeRaw(t, 9))
	expect(t, h.run(false), 1, 2, 0)

	after := h.manifest().Photos["DSC0001.ARW"]
	if after.PhotoID == before.PhotoID || after.Hash == before.Hash {
		t.Errorf("manifest entry not updated: before %+v, after %+v", before, after)
	}
	if !strings.Contains(h.out.String(), "changed since last run") {
		t.Errorf("expected change note, got:\n%s", h.out.String())
	}
}

func TestRunPerFileFailure(t *testing.T) {
	h := newHarness(t)
	writeFile(t, h.dir, "BROKEN.ARW", []byte("II*\x00 not a real raw"))

	sum := h.run(false)
	expect(t, sum, 3, 0, 1)
	if !strings.Contains(h.out.String(), "FAILED      BROKEN.ARW: preview:") {
		t.Errorf("expected failure line, got:\n%s", h.out.String())
	}
	if _, ok := h.manifest().Photos["BROKEN.ARW"]; ok {
		t.Error("failed file recorded in manifest")
	}
}

func TestRunUploadFailure(t *testing.T) {
	h := newHarness(t)
	// A file where the blob store needs a directory makes every PUT fail.
	writeFile(t, h.env.BlobDir, "sessions", []byte("blocker"))

	sum, err := Run(context.Background(), Config{
		Dir: h.dir, Client: h.client, APIURL: h.env.URL, Date: "2026-11-09", Workers: 2, Out: &h.out,
	})
	if err != nil {
		t.Fatal(err)
	}
	expect(t, sum, 0, 0, 3)
	out := h.out.String()
	if !strings.Contains(out, ": upload: thumb: ") && !strings.Contains(out, ": upload: preview: ") {
		t.Errorf("expected labelled upload failures, got:\n%s", out)
	}
	if n := len(h.manifest().Photos); n != 0 {
		t.Errorf("manifest has %d entries, want 0", n)
	}

	// Once storage works again, a rerun resumes the pending photos.
	os.Remove(filepath.Join(h.env.BlobDir, "sessions"))
	resumed := h.run(false)
	expect(t, resumed, 3, 0, 0)
}

func TestRunResumesPendingPhoto(t *testing.T) {
	h := newHarness(t)
	sum := h.run(false)

	// Simulate an interrupted upload: a new raw gets registered but never
	// uploaded, and isn't in the manifest.
	raw := fakeRaw(t, 7)
	writeFile(t, h.dir, "DSC0004.ARW", raw)
	hash := sha256.Sum256(raw)
	if _, _, err := h.client.RegisterPhoto(context.Background(), sum.SessionID,
		api.CreatePhotoRequest{Filename: "DSC0004.ARW", Hash: hex.EncodeToString(hash[:])}, false); err != nil {
		t.Fatal(err)
	}

	expect(t, h.run(false), 1, 3, 0)
	if !strings.Contains(h.out.String(), "re-uploaded DSC0004.ARW") {
		t.Errorf("expected resume, got:\n%s", h.out.String())
	}
}

func TestRunRejectsManifestFromOtherAPI(t *testing.T) {
	h := newHarness(t)
	h.run(false)

	_, err := Run(context.Background(), Config{Dir: h.dir, Client: h.client, APIURL: "https://prod.example", Out: &h.out})
	if err == nil || !strings.Contains(err.Error(), "was uploaded to") {
		t.Errorf("err = %v", err)
	}
}

func TestRunStopsOnBadKey(t *testing.T) {
	h := newHarness(t)
	h.run(false) // create the session with the good key

	// Force, as unchanged files would otherwise be skipped without asking
	// the API.
	bad := apiclient.New(h.env.URL, "wrong-key")
	h.out.Reset()
	sum, err := Run(context.Background(), Config{Dir: h.dir, Client: bad, APIURL: h.env.URL, Force: true, Workers: 1, Out: &h.out})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want 401", err)
	}
	if sum.Failed != 0 || strings.Contains(h.out.String(), "FAILED") {
		t.Errorf("expected no per-file failures, got %+v:\n%s", sum, h.out.String())
	}
}

// runOffline reruns the folder with a client whose every API call fails,
// so it only succeeds if no file needed the API.
func (h *harness) runOffline() (Summary, error) {
	h.t.Helper()
	h.out.Reset()
	bad := apiclient.New(h.env.URL, "wrong-key")
	return Run(context.Background(), Config{Dir: h.dir, Client: bad, APIURL: h.env.URL, Workers: 2, Out: &h.out})
}

// A rerun skips files the manifest has with the same size and modification
// time without reading them or calling the API.
func TestRunSkipsUnchangedWithoutAPI(t *testing.T) {
	h := newHarness(t)
	h.run(false)
	for name, e := range h.manifest().Photos {
		if e.Size == 0 || e.ModTime.IsZero() {
			t.Errorf("%s: manifest entry missing size/modTime: %+v", name, e)
		}
	}

	sum, err := h.runOffline()
	if err != nil {
		t.Fatalf("rerun called the API: %v\n%s", err, h.out.String())
	}
	expect(t, sum, 0, 3, 0)
	if !strings.Contains(h.out.String(), "3 unchanged since last run") {
		t.Errorf("expected unchanged count, got:\n%s", h.out.String())
	}
}

// A file whose modification time changed (e.g. copied again) is checked
// against the API, but not re-uploaded if its content is the same.
func TestRunChecksTouchedFile(t *testing.T) {
	h := newHarness(t)
	h.run(false)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(h.dir, "DSC0001.ARW"), later, later); err != nil {
		t.Fatal(err)
	}

	if _, err := h.runOffline(); err == nil {
		t.Fatal("touched file wasn't checked against the API")
	}
	expect(t, h.run(false), 0, 3, 0) // same hash: already uploaded
	if _, err := h.runOffline(); err != nil {
		t.Fatalf("new modification time not recorded: %v", err)
	}
}

// Manifests written before size/modTime existed are filled in by the next
// run, after which reruns skip without the API.
func TestRunBackfillsOldManifest(t *testing.T) {
	h := newHarness(t)
	h.run(false)
	m := h.manifest()
	for name, e := range m.Photos {
		m.Photos[name] = manifest.Entry{PhotoID: e.PhotoID, Hash: e.Hash}
	}
	if err := m.Save(h.dir); err != nil {
		t.Fatal(err)
	}

	if _, err := h.runOffline(); err == nil {
		t.Fatal("old entries were trusted without size/modTime")
	}
	expect(t, h.run(false), 0, 3, 0)
	if _, err := h.runOffline(); err != nil {
		t.Fatalf("manifest not backfilled: %v", err)
	}
}

// Cameras start a new folder after file number 9999, so the same filename
// can appear in several subfolders. Each must be uploaded and tracked
// separately; hidden folders are skipped.
func TestRunRecursesIntoSubfolders(t *testing.T) {
	h := newHarness(t)
	for i, sub := range []string{"100MSDCF", "101MSDCF", ".trash"} {
		dir := filepath.Join(h.dir, "DCIM", sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, dir, "DSC00001.ARW", fakeRaw(t, i+4))
	}

	expect(t, h.run(false), 5, 0, 0) // 3 top-level + 2 in subfolders
	m := h.manifest()
	for _, key := range []string{"DSC0001.ARW", "DCIM/100MSDCF/DSC00001.ARW", "DCIM/101MSDCF/DSC00001.ARW"} {
		if _, ok := m.Photos[key]; !ok {
			t.Errorf("manifest missing %q: %v", key, m.Photos)
		}
	}
	if len(m.Photos) != 5 {
		t.Errorf("manifest has %d photos, want 5: %v", len(m.Photos), m.Photos)
	}

	// The server gets the relative path, so rollover folders sort in shot
	// order and same-named files stay distinguishable.
	photos, err := h.env.Store.ListPhotos(context.Background(), m.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range photos {
		got = append(got, p.Filename)
	}
	want := []string{"DCIM/100MSDCF/DSC00001.ARW", "DCIM/101MSDCF/DSC00001.ARW", "DSC0001.ARW", "DSC0002.arw", "DSC0003.NEF"}
	if !slices.Equal(got, want) {
		t.Errorf("server filenames = %v, want %v", got, want)
	}

	expect(t, h.run(false), 0, 5, 0)
}

func TestRunNoRaws(t *testing.T) {
	env := testserver.Start(t)
	_, err := Run(context.Background(), Config{Dir: t.TempDir(), Client: apiclient.New(env.URL, testserver.APIKey), APIURL: env.URL})
	if err == nil {
		t.Error("expected error for empty folder")
	}
}

func TestRunCancelled(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Session creation fails on a cancelled context; nothing is uploaded.
	if _, err := Run(ctx, Config{Dir: h.dir, Client: h.client, APIURL: h.env.URL, Out: &h.out}); err == nil {
		t.Error("expected cancellation error")
	}
}
