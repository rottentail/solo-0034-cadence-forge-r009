package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Store struct{ Root string }

func Open(root string) (*Store, error) {
	for _, dir := range []string{"objects", "staging", "renders", "deliveries"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			return nil, err
		}
	}
	return &Store{Root: root}, nil
}
func (s *Store) Object(digest string) (string, error) {
	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("invalid content digest")
	}
	return filepath.Join(s.Root, "objects", digest), nil
}
func (s *Store) Receive(r io.Reader, limit int64) (string, int64, error) {
	f, err := os.CreateTemp(filepath.Join(s.Root, "staging"), "upload-*")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(f.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, limit+1))
	if err != nil {
		f.Close()
		return "", n, err
	}
	if n == 0 || n > limit {
		f.Close()
		return "", n, fmt.Errorf("upload must contain 1..%d bytes", limit)
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return "", n, err
	}
	if err = f.Close(); err != nil {
		return "", n, err
	}
	digest := hex.EncodeToString(h.Sum(nil))
	dest, _ := s.Object(digest)
	if err = os.Link(f.Name(), dest); err != nil && !os.IsExist(err) {
		return "", n, err
	}
	if err = SyncDir(filepath.Dir(dest)); err != nil {
		return "", n, err
	}
	return digest, n, nil
}
func (s *Store) Staging(id string, attempt int) (string, error) {
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("invalid render id")
	}
	return os.MkdirTemp(filepath.Join(s.Root, "staging"), fmt.Sprintf("render-%s-%d-", id, attempt))
}
func (s *Store) RenderDir(id string, attempt int) (string, error) {
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("invalid render id")
	}
	return filepath.Join(s.Root, "renders", fmt.Sprintf("%s-%d", id, attempt)), nil
}
func SyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (s *Store) Promote(stage, id string, attempt int) error {
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			return fmt.Errorf("unexpected output directory")
		}
		f, err := os.OpenFile(filepath.Join(stage, e.Name()), os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		err = f.Sync()
		f.Close()
		if err != nil {
			return err
		}
	}
	if err = SyncDir(stage); err != nil {
		return err
	}
	dest, err := s.RenderDir(id, attempt)
	if err != nil {
		return err
	}
	if err = os.Rename(stage, dest); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(dest))
}

// DeliveryDir is the durable per-render directory that holds confirmed segments
// and the delivered caption file. It is deliberately not keyed by attempt: a
// retry resumes from the first boundary missing in the shared manifest, and the
// already-confirmed prefix stays exactly where it was published. Only the
// manifest records which files are part of the delivery.
func (s *Store) DeliveryDir(id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("invalid render id")
	}
	dir := filepath.Join(s.Root, "deliveries", id)
	if err := os.MkdirAll(filepath.Join(dir, "renditions"), 0700); err != nil {
		return "", err
	}
	return dir, nil
}

// PublishSegment moves one closed and inspected segment into the delivery tree.
// The rename is atomic within the data root. An already-published segment is
// never overwritten: a confirmed record must keep matching its file, so a
// conflicting re-publication is rejected rather than allowed to clobber it.
func (s *Store) PublishSegment(source, deliveryDir string, rendition string, index int) (string, error) {
	if !renditionNamePattern.MatchString(rendition) || index < 0 || index > 99999 {
		return "", fmt.Errorf("invalid segment identity")
	}
	dir := filepath.Join(deliveryDir, "renditions", rendition)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, fmt.Sprintf("segment-%05d.mp4", index))
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("segment %s/%d is already published", rendition, index)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Rename(source, dest); err != nil {
		return "", err
	}
	if err := SyncDir(dir); err != nil {
		return "", err
	}
	if err := SyncDir(filepath.Join(deliveryDir, "renditions")); err != nil {
		return "", err
	}
	return dest, nil
}

// PublishCaptions writes the delivered WebVTT beside the confirmed segments and
// makes it durable.
func (s *Store) PublishCaptions(deliveryDir, contents string) error {
	temp, err := os.CreateTemp(deliveryDir, ".captions-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err = temp.WriteString(contents); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(deliveryDir, CaptionFilename)); err != nil {
		return err
	}
	return SyncDir(deliveryDir)
}

const CaptionFilename = "captions.vtt"

var renditionNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
