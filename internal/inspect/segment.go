package inspect

import (
	"cadenceforge/internal/domain"
	"cadenceforge/internal/media"
	"context"
	"fmt"
	"math"
	"os"
)

// Segment verifies one closed rendition segment against the canonical boundary
// and the rendition's declared contract. A segment that fails any check is never
// written into the confirmed manifest.
func Segment(ctx context.Context, prober media.Prober, path string, render domain.Render, rendition domain.Rendition, boundary domain.SegmentBoundary) (domain.DeliverySegment, error) {
	if boundary.EndMS <= boundary.StartMS {
		return domain.DeliverySegment{}, fmt.Errorf("empty segment boundary")
	}
	m, err := prober.Probe(ctx, path)
	if err != nil {
		return domain.DeliverySegment{}, err
	}
	wantMS := boundary.EndMS - boundary.StartMS
	delta := m.DurationMS - wantMS
	tolerance := max(int64(100), int64(2000/render.Recipe.FPS))
	if math.Abs(float64(delta)) > float64(tolerance) {
		return domain.DeliverySegment{}, fmt.Errorf("segment %d duration delta %dms exceeds %dms", boundary.Index, delta, tolerance)
	}
	if !m.Video || !m.Audio {
		return domain.DeliverySegment{}, fmt.Errorf("segment %d lacks required streams", boundary.Index)
	}
	if m.Width != rendition.Width || m.Height != rendition.Height {
		return domain.DeliverySegment{}, fmt.Errorf("segment %d frame is %dx%d, want %dx%d", boundary.Index, m.Width, m.Height, rendition.Width, rendition.Height)
	}
	if m.SampleRate != render.Recipe.SampleRate {
		return domain.DeliverySegment{}, fmt.Errorf("segment %d sample rate mismatch", boundary.Index)
	}
	digest, n, err := Digest(path)
	if err != nil {
		return domain.DeliverySegment{}, err
	}
	if n <= 0 {
		return domain.DeliverySegment{}, fmt.Errorf("segment %d is empty", boundary.Index)
	}
	return domain.DeliverySegment{
		Rendition:  rendition.Name,
		Index:      boundary.Index,
		StartMS:    boundary.StartMS,
		EndMS:      boundary.EndMS,
		Filename:   domain.SegmentRelPath(rendition.Name, boundary.Index),
		Bytes:      n,
		Digest:     digest,
		Submission: render.FrozenSubmission,
	}, nil
}

// SyncSegment durably writes a segment file and its directory before the
// segment is registered as confirmed.
func SyncSegment(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return syncDir(path)
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
