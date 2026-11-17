package service

import (
	"archive/zip"
	"cadenceforge/internal/domain"
	"cadenceforge/internal/inspect"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// DeliveryView is the reviewer-facing projection of a segmented render. It
// exposes only confirmed segments, grouped into a playable prefix per rendition.
// A rendition that has no confirmed front segment is reported as pending and is
// never offered as switchable.
type DeliveryView struct {
	RenderID         string                   `json:"render_id"`
	State            string                   `json:"state"`
	FrozenSubmission string                   `json:"frozen_submission"`
	DurationMS       int64                    `json:"duration_ms"`
	SegmentMS        int64                    `json:"segment_ms"`
	Complete         bool                     `json:"complete"`
	Captions         string                   `json:"captions"`
	Boundaries       []domain.SegmentBoundary `json:"boundaries"`
	Renditions       []RenditionAvailability  `json:"renditions"`
	Pending          []domain.PendingSegment  `json:"pending"`
}

type RenditionAvailability struct {
	Name        string                   `json:"name"`
	Width       int                      `json:"width"`
	Height      int                      `json:"height"`
	State       string                   `json:"state"`
	Playable    bool                     `json:"playable"`
	PrefixCount int                      `json:"prefix_count"`
	PrefixEndMS int64                    `json:"prefix_end_ms"`
	Segments    []domain.DeliverySegment `json:"segments"`
}

// Delivery returns the confirmed delivery view for one render. Only records in
// the manifest are surfaced; unregistered files are ignored even if present.
func (s *Service) Delivery(id string) (DeliveryView, error) {
	st, err := s.Store.Snapshot()
	if err != nil {
		return DeliveryView{}, err
	}
	r, ok := st.Renders[id]
	if !ok {
		return DeliveryView{}, domain.Missing("render", id)
	}
	if r.Delivery == nil {
		return DeliveryView{}, domain.Conflict("render does not use segmented delivery")
	}
	view := DeliveryView{
		RenderID:         r.ID,
		State:            r.State,
		FrozenSubmission: r.FrozenSubmission,
		DurationMS:       r.Delivery.DurationMS,
		SegmentMS:        r.Delivery.SegmentMS,
		Complete:         r.Delivery.Complete,
		Captions:         r.Plan.VTT,
		Boundaries:       r.Delivery.Boundaries,
		Pending:          r.Delivery.PendingSegments(),
	}
	for _, rendition := range r.Delivery.Renditions {
		prefix := r.Delivery.PrefixSegments(rendition.Name)
		entry := RenditionAvailability{
			Name:        rendition.Name,
			Width:       rendition.Width,
			Height:      rendition.Height,
			State:       rendition.State,
			Playable:    len(prefix) > 0,
			PrefixCount: len(prefix),
			Segments:    prefix,
		}
		if len(prefix) > 0 {
			entry.PrefixEndMS = prefix[len(prefix)-1].EndMS
		}
		view.Renditions = append(view.Renditions, entry)
	}
	return view, nil
}

// SegmentPath resolves one confirmed segment for download. It refuses any
// rendition/index pair that is not in the manifest, so a partially written or
// orphaned file can never be served.
func (s *Service) SegmentPath(id, rendition string, index int) (domain.Render, string, error) {
	st, err := s.Store.Snapshot()
	if err != nil {
		return domain.Render{}, "", err
	}
	r, ok := st.Renders[id]
	if !ok {
		return r, "", domain.Missing("render", id)
	}
	if r.Delivery == nil {
		return r, "", domain.Conflict("render does not use segmented delivery")
	}
	segment, ok := r.Delivery.Confirmed(rendition, index)
	if !ok {
		return r, "", domain.Conflict("segment is not in the confirmed list")
	}
	if r.State == "cancelled" {
		return r, "", domain.Conflict("cancelled render cannot expose segments")
	}
	dir, err := s.Blobs.DeliveryDir(r.ID)
	if err != nil {
		return r, "", err
	}
	path := filepath.Join(dir, segment.Filename)
	digest, n, err := inspect.Digest(path)
	if err != nil {
		return r, "", domain.Conflict(fmt.Sprintf("confirmed segment %s/%d is missing", rendition, index))
	}
	if digest != segment.Digest || n != segment.Bytes {
		return r, "", domain.Conflict(fmt.Sprintf("confirmed segment %s/%d changed after confirmation", rendition, index))
	}
	return r, path, nil
}

// verifySegmented re-checks a complete segmented delivery: the manifest must be
// complete and every confirmed segment and caption must still match its committed
// digest. It is the verification gate for POST /verify.
func (s *Service) verifySegmented(id string) (domain.Inspection, error) {
	st, err := s.Store.Snapshot()
	if err != nil {
		return domain.Inspection{}, err
	}
	r, ok := st.Renders[id]
	if !ok {
		return domain.Inspection{}, domain.Missing("render", id)
	}
	if r.Delivery == nil {
		return domain.Inspection{}, domain.Conflict("render does not use segmented delivery")
	}
	if r.State != "succeeded" || !r.Delivery.Complete {
		return domain.Inspection{}, domain.Conflict("delivery is not complete")
	}
	if err := s.verifyDeliveryFiles(r); err != nil {
		return domain.Inspection{}, err
	}
	first := r.Delivery.Renditions[0]
	return domain.Inspection{
		Media:  domain.Media{Video: true, Audio: true, Width: first.Width, Height: first.Height, SampleRate: r.Recipe.SampleRate, DurationMS: r.Delivery.DurationMS},
		Digest: r.FrozenSubmission,
	}, nil
}

// verifyDeliveryFiles checks that every confirmed segment file still matches its
// committed digest and that the caption file matches the frozen plan. It is the
// gate before any package is declared deliverable.
func (s *Service) verifyDeliveryFiles(r domain.Render) error {
	dir, err := s.Blobs.DeliveryDir(r.ID)
	if err != nil {
		return err
	}
	captions, err := os.ReadFile(filepath.Join(dir, r.Delivery.CaptionsFile))
	if err != nil {
		return domain.Conflict("caption delivery is missing")
	}
	if domain.VTTDigest(string(captions)) != r.Delivery.CaptionsDigest {
		return domain.Conflict("caption delivery does not match the frozen plan")
	}
	for _, rendition := range r.Delivery.Renditions {
		for _, segment := range rendition.Segments {
			path := filepath.Join(dir, segment.Filename)
			digest, n, err := inspect.Digest(path)
			if err != nil {
				return domain.Conflict(fmt.Sprintf("confirmed segment %s/%d is missing", rendition.Name, segment.Index))
			}
			if digest != segment.Digest || n != segment.Bytes {
				return domain.Conflict(fmt.Sprintf("confirmed segment %s/%d changed after confirmation", rendition.Name, segment.Index))
			}
		}
	}
	return nil
}

// DeliveryBundle writes a complete package: every rendition's segments in order,
// the shared caption file and a manifest. It refuses to emit a successful
// archive unless the manifest is complete and every confirmed file re-verifies,
// so a recipient can never receive a set with missing or mismatched pieces.
func (s *Service) DeliveryBundle(ctx context.Context, id string, dest io.Writer) error {
	st, err := s.Store.Snapshot()
	if err != nil {
		return err
	}
	r, ok := st.Renders[id]
	if !ok {
		return domain.Missing("render", id)
	}
	if r.Delivery == nil {
		return domain.Conflict("render does not use segmented delivery")
	}
	if r.State != "succeeded" || !r.Delivery.Complete {
		return domain.Conflict("delivery is not complete; package is unavailable")
	}
	if err := s.verifyDeliveryFiles(r); err != nil {
		return err
	}
	dir, err := s.Blobs.DeliveryDir(r.ID)
	if err != nil {
		return err
	}
	z := zip.NewWriter(dest)
	closeWith := func(cause error) error {
		if closeErr := z.Close(); cause == nil {
			return closeErr
		}
		return cause
	}
	for _, rendition := range r.Delivery.Renditions {
		for _, segment := range rendition.Segments {
			if err := addFile(z, filepath.Join(dir, segment.Filename), segment.Filename); err != nil {
				return closeWith(err)
			}
		}
	}
	if err := addFile(z, filepath.Join(dir, r.Delivery.CaptionsFile), r.Delivery.CaptionsFile); err != nil {
		return closeWith(err)
	}
	manifest, err := json.MarshalIndent(bundleManifest(r), "", "  ")
	if err != nil {
		return closeWith(err)
	}
	w, err := z.Create("delivery.manifest.json")
	if err != nil {
		return closeWith(err)
	}
	if _, err = w.Write(manifest); err != nil {
		return closeWith(err)
	}
	return z.Close()
}

type bundleDocument struct {
	Submission string                   `json:"submission"`
	Sheet      domain.Sheet             `json:"sheet"`
	Recipe     domain.Recipe            `json:"recipe"`
	Plan       domain.Plan              `json:"plan"`
	Delivery   *domain.DeliveryManifest `json:"delivery"`
}

func bundleManifest(r domain.Render) bundleDocument {
	return bundleDocument{Submission: r.FrozenSubmission, Sheet: r.Sheet, Recipe: r.Recipe, Plan: r.Plan, Delivery: r.Delivery}
}

func addFile(z *zip.Writer, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w, err := z.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}
