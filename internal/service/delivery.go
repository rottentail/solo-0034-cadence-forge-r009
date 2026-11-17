package service

import (
	"cadenceforge/internal/domain"
	"cadenceforge/internal/encoding"
	"cadenceforge/internal/inspect"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// executeSegmented produces a multi-rendition delivery. The canonical boundary
// list is fixed before any encoding. The opening segment of every rendition is
// encoded first so each quality becomes switchable early; the remaining
// boundaries then fill cheapest-first. Each closed segment is inspected and
// committed to the shared manifest before encoding continues, so reviewers read
// a confirmed prefix while package recipients wait for manifest completion.
func (s *Service) executeSegmented(ctx context.Context, r domain.Render, attempt int, stage string) {
	if r.Delivery == nil {
		s.complete(r, nil, fmt.Errorf("segmented render has no delivery manifest"))
		return
	}
	deliveryDir, err := s.Blobs.DeliveryDir(r.ID)
	if err != nil {
		s.complete(r, nil, err)
		return
	}
	if err = s.Blobs.PublishCaptions(deliveryDir, r.Plan.VTT); err != nil {
		s.complete(r, nil, err)
		return
	}
	paths := map[string]string{}
	for id, source := range r.Sources {
		path, err := s.Blobs.Object(source.Digest)
		if err != nil {
			s.complete(r, nil, err)
			return
		}
		paths[id] = path
	}
	encoder := encoding.SegmentEncoder{Binary: s.Config.FFmpeg}
	ladder := domain.OrderedRenditions(r.Recipe)
	total := len(r.Delivery.Boundaries)
	// Scheduling always consults the durable manifest, never the snapshot taken
	// when the attempt started: a confirmed boundary is never re-encoded, which
	// is what makes progress resumable without republishing files.
	prefix := func(name string) (int, error) {
		st, err := s.Store.Snapshot()
		if err != nil {
			return 0, err
		}
		current, ok := st.Renders[r.ID]
		if !ok || current.Delivery == nil {
			return 0, fmt.Errorf("render delivery manifest disappeared")
		}
		return current.Delivery.RenditionPrefixFor(name), nil
	}
	// First pass: encode the opening segment of every rendition so each quality
	// becomes switchable within one segment's encode time, rather than making
	// reviewers wait for the lowest branch to finish before any alternative
	// exists. The cheapest branch still reaches a playable prefix first.
	for _, rendition := range ladder {
		if err := ctx.Err(); err != nil {
			s.complete(r, nil, err)
			return
		}
		start, err := prefix(rendition.Name)
		if err != nil {
			s.complete(r, nil, err)
			return
		}
		if start > 0 {
			continue
		}
		if err := s.encodeRange(ctx, r, attempt, deliveryDir, stage, paths, encoder, rendition, 0, 1); err != nil {
			s.complete(r, nil, err)
			return
		}
	}
	// Remaining passes: fill every branch from its confirmed prefix to the final
	// boundary. Cheapest first preserves the earliest possible full-quality
	// recovery and keeps only one branch's worth of staging alive at a time.
	for _, rendition := range ladder {
		if err := ctx.Err(); err != nil {
			s.complete(r, nil, err)
			return
		}
		start, err := prefix(rendition.Name)
		if err != nil {
			s.complete(r, nil, err)
			return
		}
		if start >= total {
			continue
		}
		if err := s.encodeRange(ctx, r, attempt, deliveryDir, stage, paths, encoder, rendition, start, total); err != nil {
			s.complete(r, nil, err)
			return
		}
	}
	s.completeSegmented(r)
}

// encodeRange publishes the canonical range [start, end) of one rendition,
// confirming each closed segment before the next is inspected.
func (s *Service) encodeRange(ctx context.Context, r domain.Render, attempt int, deliveryDir, stage string, paths map[string]string, encoder encoding.SegmentEncoder, rendition domain.Rendition, start, end int) error {
	renditionStage := filepath.Join(stage, rendition.Name)
	if err := os.MkdirAll(renditionStage, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(renditionStage)
	pattern := filepath.Join(renditionStage, "segment-%05d.mp4")
	ready := func(index int) error {
		staged := filepath.Join(renditionStage, fmt.Sprintf("segment-%05d.mp4", index))
		// If the boundary was confirmed after this range was scheduled, its
		// published file is authoritative. Drop the redundant staged copy
		// instead of republishing over a confirmed record.
		st, err := s.Store.Snapshot()
		if err != nil {
			return err
		}
		if current, ok := st.Renders[r.ID]; ok && current.Delivery != nil {
			if _, exists := current.Delivery.Confirmed(rendition.Name, index); exists {
				return os.Remove(staged)
			}
		}
		return s.confirmSegment(ctx, r, attempt, deliveryDir, rendition, index, staged)
	}
	return encoder.EncodeSegments(ctx, r, paths, rendition, start, end, pattern, ready)
}

// confirmSegment inspects one closed segment, publishes it durably and only then
// records it in the shared manifest. Encoding progress is never inferred from a
// file's existence by any reader.
func (s *Service) confirmSegment(ctx context.Context, r domain.Render, attempt int, deliveryDir string, rendition domain.Rendition, index int, path string) error {
	boundary, ok := r.Delivery.Boundary(index)
	if !ok {
		return fmt.Errorf("segment index %d outside the canonical boundaries", index)
	}
	record, err := inspect.Segment(ctx, s.Probe, path, r, rendition, boundary)
	if err != nil {
		return err
	}
	published, err := s.Blobs.PublishSegment(path, deliveryDir, rendition.Name, index)
	if err != nil {
		return err
	}
	if err = inspect.SyncSegment(published); err != nil {
		return err
	}
	now := time.Now().UTC()
	s.mutation.Lock()
	defer s.mutation.Unlock()
	announced := false
	err = s.Store.Update(func(st *domain.State) error {
		current, ok := st.Renders[r.ID]
		if !ok {
			return domain.Missing("render", r.ID)
		}
		if current.State != "running" || len(current.Attempts) != attempt {
			return domain.Conflict("render attempt is no longer active")
		}
		if current.Delivery == nil {
			return fmt.Errorf("active render lost its delivery manifest")
		}
		_, becameAvailable, err := current.Delivery.Confirm(record, now)
		if err != nil {
			return err
		}
		announced = becameAvailable
		current.UpdatedAt = now
		current.Delivery.Recompute(now)
		st.Renders[r.ID] = current
		st.Emit(r.ID, "render.segment.confirmed", fmt.Sprintf("%s/%05d", rendition.Name, index))
		if announced {
			st.Emit(r.ID, "render.rendition.available", rendition.Name)
		}
		return nil
	})
	if err == nil && announced {
		slog.Info("rendition available", "render", r.ID, "rendition", rendition.Name)
	}
	return err
}

// completeSegmented commits the final delivery state only when every expected
// segment of every rendition is present in the confirmed list. It never infers
// success from the existence of files on disk.
func (s *Service) completeSegmented(r domain.Render) {
	now := time.Now().UTC()
	s.mutation.Lock()
	var cause error
	err := s.Store.Update(func(st *domain.State) error {
		current, ok := st.Renders[r.ID]
		if !ok || current.State != "running" || len(current.Attempts) != len(r.Attempts) {
			return nil
		}
		if current.Delivery == nil {
			cause = fmt.Errorf("segmented render has no delivery manifest")
			return cause
		}
		current.Delivery.Recompute(now)
		if !current.Delivery.Complete {
			cause = fmt.Errorf("delivery manifest is not complete")
			return cause
		}
		if cause = s.verifyDeliveryFiles(current); cause != nil {
			return cause
		}
		current.UpdatedAt = now
		last := &current.Attempts[len(current.Attempts)-1]
		last.FinishedAt = &current.UpdatedAt
		last.Outcome = "succeeded"
		current.State = "succeeded"
		// Success for a segmented delivery is the complete confirmed manifest
		// plus re-verified files, not a single-file artifact record.
		current.Artifact = nil
		st.Renders[r.ID] = current
		st.Emit(r.ID, "render.succeeded", "all renditions complete and re-verified")
		return nil
	})
	if err != nil {
		if cause == nil {
			cause = err
		}
		s.completeLocked(r, nil, cause)
	}
	s.mutation.Unlock()
}
