package blob

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// LocalPathPrefix is where Local's Handler must be mounted, e.g.
//
//	mux.Handle("PUT "+blob.LocalPathPrefix+"{key...}", local)
const LocalPathPrefix = "/dev/blob/"

// MaxObjectSize caps a single uploaded object.
const MaxObjectSize = 25 << 20

// Local stores objects as files under a directory and issues HMAC-signed,
// expiring PUT URLs that its own handler verifies, mimicking S3 presigned
// URLs.
type Local struct {
	dir     string
	baseURL string
	secret  []byte
	now     func() time.Time
}

var _ Store = (*Local)(nil)

// NewLocal returns a Local storing objects under dir. baseURL is the
// externally reachable origin of the server mounting the handler, e.g.
// "http://localhost:8080".
func NewLocal(dir, baseURL string, secret []byte) *Local {
	return &Local{dir: dir, baseURL: baseURL, secret: secret, now: time.Now}
}

func (l *Local) PresignPut(_ context.Context, key string, ttl time.Duration) (string, error) {
	if !validKey(key) {
		return "", fmt.Errorf("blob: invalid key %q", key)
	}
	exp := l.now().Add(ttl).Unix()
	return fmt.Sprintf("%s%s%s?exp=%d&sig=%s", l.baseURL, LocalPathPrefix, key, exp, l.sign(http.MethodPut, key, exp)), nil
}

func (l *Local) Exists(_ context.Context, key string) (bool, error) {
	if !validKey(key) {
		return false, fmt.Errorf("blob: invalid key %q", key)
	}
	_, err := os.Stat(l.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// ServeHTTP handles signed PUT uploads. The object key is taken from the
// {key...} path wildcard.
func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	key := r.PathValue("key")
	exp, err := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
	if err != nil || !validKey(key) ||
		l.now().Unix() > exp ||
		!hmac.Equal([]byte(r.URL.Query().Get("sig")), []byte(l.sign(http.MethodPut, key, exp))) {
		http.Error(w, "invalid or expired signature", http.StatusForbidden)
		return
	}

	if err := l.write(key, http.MaxBytesReader(w, r.Body, MaxObjectSize)); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "object too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "write failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// write stores the object atomically (temp file + rename) so a partial
// upload never appears to exist.
func (l *Local) write(key string, body io.Reader) error {
	dst := l.path(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after successful rename
	if _, err := io.Copy(tmp, body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func (l *Local) path(key string) string {
	return filepath.Join(l.dir, filepath.FromSlash(key))
}

func (l *Local) sign(method, key string, exp int64) string {
	mac := hmac.New(sha256.New, l.secret)
	fmt.Fprintf(mac, "%s\n%s\n%d", method, key, exp)
	return hex.EncodeToString(mac.Sum(nil))
}

// validKey rejects empty, absolute, or traversing keys.
func validKey(key string) bool {
	return key != "" && filepath.IsLocal(filepath.FromSlash(key))
}
