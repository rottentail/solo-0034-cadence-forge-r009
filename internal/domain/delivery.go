package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"time"
)

const (
	MinSegmentMS     = 1000
	MaxSegmentMS     = 6000
	MaxRenditions    = 4
	CaptionsFilename = "captions.vtt"
)

var renditionNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Rendition is one quality branch of a segmented delivery ladder. A recipe that
// declares renditions opts the render into segmented delivery; a recipe without
// them keeps the single-file artifact behaviour.
type Rendition struct {
	Name             string `json:"name"`
	Width            int    `json:"width"`
	Height           int    `json:"height"`
	VideoBitrateKbps int    `json:"video_bitrate_kbps"`
	AudioBitrateKbps int    `json:"audio_bitrate_kbps"`
}

// SegmentBoundary is the canonical time window shared by every rendition. The
// boundaries are derived once from the frozen plan duration and segment length;
// no consumer is permitted to infer them from the files present on disk.
type SegmentBoundary struct {
	Index   int   `json:"index"`
	StartMS int64 `json:"start_ms"`
	EndMS   int64 `json:"end_ms"`
}

// DeliverySegment is a segment that has been encoded, inspected and durably
// accepted into the manifest. A file that is not represented here is never
// exposed to reviewers or package recipients, even if it exists on disk.
type DeliverySegment struct {
	Rendition   string    `json:"rendition"`
	Index       int       `json:"index"`
	StartMS     int64     `json:"start_ms"`
	EndMS       int64     `json:"end_ms"`
	Filename    string    `json:"filename"`
	Bytes       int64     `json:"bytes"`
	Digest      string    `json:"digest"`
	Submission  string    `json:"submission"`
	ConfirmedAt time.Time `json:"confirmed_at"`
}

type RenditionDelivery struct {
	Name             string            `json:"name"`
	Width            int               `json:"width"`
	Height           int               `json:"height"`
	VideoBitrateKbps int               `json:"video_bitrate_kbps"`
	AudioBitrateKbps int               `json:"audio_bitrate_kbps"`
	State            string            `json:"state"`
	Segments         []DeliverySegment `json:"segments"`
}

// DeliveryManifest is the single confirmed segment list shared by progressive
// review and complete-package consumers. It is persisted with the render and is
// bound to the frozen submission.
type DeliveryManifest struct {
	SegmentMS            int64               `json:"segment_ms"`
	DurationMS           int64               `json:"duration_ms"`
	FrozenSubmission     string              `json:"frozen_submission"`
	FrozenSheetRevision  int                 `json:"frozen_sheet_revision"`
	FrozenRecipeRevision int                 `json:"frozen_recipe_revision"`
	CaptionsFile         string              `json:"captions_file"`
	CaptionsDigest       string              `json:"captions_digest"`
	Boundaries           []SegmentBoundary   `json:"boundaries"`
	Renditions           []RenditionDelivery `json:"renditions"`
	Complete             bool                `json:"complete"`
	CompletedAt          *time.Time          `json:"completed_at,omitempty"`
}

type PendingSegment struct {
	Rendition string `json:"rendition"`
	Index     int    `json:"index"`
	StartMS   int64  `json:"start_ms"`
	EndMS     int64  `json:"end_ms"`
}

// DeliveryEnabled reports whether a recipe asks for segmented multi-rendition
// delivery. WAV recipes never use it.
func DeliveryEnabled(recipe Recipe) bool {
	return recipe.Format == "mp4" && len(recipe.Renditions) > 0
}

// ValidateRenditions enforces the ladder contract for an MP4 recipe.
func ValidateRenditions(r Recipe) error {
	if len(r.Renditions) == 0 {
		if r.SegmentMS != 0 {
			return Invalid("segment_ms requires a rendition ladder")
		}
		return nil
	}
	if r.Format != "mp4" {
		return Invalid("rendition ladder is only supported for mp4")
	}
	if len(r.Renditions) > MaxRenditions {
		return Invalid("at most %d renditions are supported", MaxRenditions)
	}
	if r.SegmentMS < MinSegmentMS || r.SegmentMS > MaxSegmentMS || r.SegmentMS%100 != 0 {
		return Invalid("segment_ms must be 1000..6000 in 100ms steps")
	}
	names := map[string]bool{}
	for _, rendition := range r.Renditions {
		if !renditionNamePattern.MatchString(rendition.Name) {
			return Invalid("rendition name must match [a-z0-9][a-z0-9_-]{0,31}")
		}
		if names[rendition.Name] {
			return Invalid("rendition names must be unique")
		}
		names[rendition.Name] = true
		if rendition.Width < 64 || rendition.Height < 64 || rendition.Width > 1920 || rendition.Height > 1080 || rendition.Width%2 != 0 || rendition.Height%2 != 0 {
			return Invalid("rendition %s has invalid even dimensions", rendition.Name)
		}
		if rendition.Width > r.Width || rendition.Height > r.Height {
			return Invalid("rendition %s exceeds the recipe frame", rendition.Name)
		}
		if rendition.VideoBitrateKbps < 64 || rendition.VideoBitrateKbps > 20000 {
			return Invalid("rendition %s video_bitrate_kbps must be 64..20000", rendition.Name)
		}
		if rendition.AudioBitrateKbps < 32 || rendition.AudioBitrateKbps > 320 {
			return Invalid("rendition %s audio_bitrate_kbps must be 32..320", rendition.Name)
		}
	}
	return nil
}

// MinSegmentTailMS is the shortest final segment we are willing to encode. A
// shorter remainder is merged into the preceding boundary so no degenerate
// segment is ever placed in the shared list.
const MinSegmentTailMS = 250

func SegmentBoundaries(durationMS, segmentMS int64) []SegmentBoundary {
	out := []SegmentBoundary{}
	if durationMS <= 0 || segmentMS <= 0 {
		return out
	}
	for start, index := int64(0), 0; start < durationMS; start, index = start+segmentMS, index+1 {
		end := start + segmentMS
		if end > durationMS {
			end = durationMS
		}
		out = append(out, SegmentBoundary{Index: index, StartMS: start, EndMS: end})
	}
	if len(out) > 1 && out[len(out)-1].EndMS-out[len(out)-1].StartMS < MinSegmentTailMS {
		out[len(out)-2].EndMS = out[len(out)-1].EndMS
		out = out[:len(out)-1]
	}
	for i := range out {
		out[i].Index = i
	}
	return out
}

// OrderedRenditions returns the ladder from lowest to highest quality. Encoding
// and the manifest both use this order so front segments of the cheapest branch
// are confirmed first.
func OrderedRenditions(recipe Recipe) []Rendition {
	out := append([]Rendition(nil), recipe.Renditions...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Height != out[j].Height {
			return out[i].Height < out[j].Height
		}
		if out[i].Width != out[j].Width {
			return out[i].Width < out[j].Width
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func SegmentRelPath(rendition string, index int) string {
	return fmt.Sprintf("renditions/%s/segment-%05d.mp4", rendition, index)
}

func VTTDigest(vtt string) string {
	sum := sha256.Sum256([]byte(vtt))
	return hex.EncodeToString(sum[:])
}

// SubmissionDigest binds the frozen timeline, recipe ladder, source identities
// and caption text into one value. Every confirmed segment repeats it, so a
// reviewer or package recipient can prove a segment belongs to the original
// submission even after later sheet revisions exist.
func SubmissionDigest(sheet Sheet, recipe Recipe, sources map[string]Source, plan Plan) string {
	payload := struct {
		Sheet   Sheet             `json:"sheet"`
		Recipe  Recipe            `json:"recipe"`
		Sources map[string]Source `json:"sources"`
		Plan    Plan              `json:"plan"`
	}{Sheet: sheet, Recipe: recipe, Sources: sources, Plan: plan}
	data, err := json.Marshal(payload)
	if err != nil {
		// All members are plain value types; marshalling cannot fail in practice.
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// NewDeliveryManifest builds the empty confirmed manifest for a frozen render.
func NewDeliveryManifest(render Render) (*DeliveryManifest, error) {
	if !DeliveryEnabled(render.Recipe) {
		return nil, fmt.Errorf("recipe does not declare a rendition ladder")
	}
	if render.Plan.DurationMS <= 0 {
		return nil, fmt.Errorf("cannot segment an empty plan")
	}
	manifest := &DeliveryManifest{
		SegmentMS:            render.Recipe.SegmentMS,
		DurationMS:           render.Plan.DurationMS,
		FrozenSubmission:     render.FrozenSubmission,
		FrozenSheetRevision:  render.Sheet.Revision,
		FrozenRecipeRevision: render.Recipe.Revision,
		CaptionsFile:         CaptionsFilename,
		CaptionsDigest:       VTTDigest(render.Plan.VTT),
		Boundaries:           SegmentBoundaries(render.Plan.DurationMS, render.Recipe.SegmentMS),
		Renditions:           []RenditionDelivery{},
	}
	for _, rendition := range OrderedRenditions(render.Recipe) {
		manifest.Renditions = append(manifest.Renditions, RenditionDelivery{
			Name:             rendition.Name,
			Width:            rendition.Width,
			Height:           rendition.Height,
			VideoBitrateKbps: rendition.VideoBitrateKbps,
			AudioBitrateKbps: rendition.AudioBitrateKbps,
			State:            "pending",
			Segments:         []DeliverySegment{},
		})
	}
	return manifest, nil
}

func (manifest *DeliveryManifest) Rendition(name string) *RenditionDelivery {
	for i := range manifest.Renditions {
		if manifest.Renditions[i].Name == name {
			return &manifest.Renditions[i]
		}
	}
	return nil
}

func (manifest *DeliveryManifest) Boundary(index int) (SegmentBoundary, bool) {
	if index < 0 || index >= len(manifest.Boundaries) {
		return SegmentBoundary{}, false
	}
	return manifest.Boundaries[index], true
}

func (manifest *DeliveryManifest) Confirmed(name string, index int) (DeliverySegment, bool) {
	rendition := manifest.Rendition(name)
	if rendition == nil {
		return DeliverySegment{}, false
	}
	for _, segment := range rendition.Segments {
		if segment.Index == index {
			return segment, true
		}
	}
	return DeliverySegment{}, false
}

// Confirm inserts a newly inspected segment, keeping records sorted by index.
// It validates the record against the canonical boundary and the frozen
// submission. The second result reports whether this confirmation made a
// rendition playable for the first time.
func (manifest *DeliveryManifest) Confirm(segment DeliverySegment, now time.Time) (bool, bool, error) {
	rendition := manifest.Rendition(segment.Rendition)
	if rendition == nil {
		return false, false, fmt.Errorf("unknown rendition %q in segment record", segment.Rendition)
	}
	boundary, ok := manifest.Boundary(segment.Index)
	if !ok {
		return false, false, fmt.Errorf("segment index outside the canonical boundaries")
	}
	if segment.StartMS != boundary.StartMS || segment.EndMS != boundary.EndMS {
		return false, false, fmt.Errorf("segment boundaries do not match the confirmed list")
	}
	if segment.Submission != manifest.FrozenSubmission {
		return false, false, fmt.Errorf("segment does not belong to the frozen submission")
	}
	if segment.Filename != SegmentRelPath(segment.Rendition, segment.Index) {
		return false, false, fmt.Errorf("segment filename does not match its rendition and index")
	}
	segment.ConfirmedAt = now.UTC()
	for _, existing := range rendition.Segments {
		if existing.Index == segment.Index {
			return false, false, nil
		}
	}
	before := RenditionPrefix(*rendition) > 0
	insertAt := sort.Search(len(rendition.Segments), func(i int) bool {
		return rendition.Segments[i].Index >= segment.Index
	})
	rendition.Segments = append(rendition.Segments, DeliverySegment{})
	copy(rendition.Segments[insertAt+1:], rendition.Segments[insertAt:])
	rendition.Segments[insertAt] = segment
	manifest.Recompute(now)
	return true, !before && RenditionPrefix(*rendition) > 0, nil
}

// RenditionPrefix reports the contiguous confirmed prefix that a reviewer can
// actually play. A segment confirmed past a gap is not playable.
func RenditionPrefix(rendition RenditionDelivery) int {
	prefix := 0
	for _, segment := range rendition.Segments {
		if segment.Index != prefix {
			break
		}
		prefix++
	}
	return prefix
}

func renditionState(rendition *RenditionDelivery, expected int) string {
	if expected > 0 && len(rendition.Segments) == expected && RenditionPrefix(*rendition) == expected {
		return "complete"
	}
	if RenditionPrefix(*rendition) > 0 {
		return "available"
	}
	return "pending"
}

// Recompute refreshes derived rendition and completion state from the confirmed
// segment records.
func (manifest *DeliveryManifest) Recompute(now time.Time) {
	expected := len(manifest.Boundaries)
	complete := expected > 0
	for i := range manifest.Renditions {
		rendition := &manifest.Renditions[i]
		sort.SliceStable(rendition.Segments, func(a, b int) bool {
			return rendition.Segments[a].Index < rendition.Segments[b].Index
		})
		rendition.State = renditionState(rendition, expected)
		if rendition.State != "complete" {
			complete = false
		}
	}
	manifest.Complete = complete
	if complete {
		if manifest.CompletedAt == nil {
			stamp := now.UTC()
			manifest.CompletedAt = &stamp
		}
	} else {
		manifest.CompletedAt = nil
	}
}

// PendingSegments lists the expected segments that have not been confirmed yet.
func (manifest *DeliveryManifest) PendingSegments() []PendingSegment {
	out := []PendingSegment{}
	for i := range manifest.Renditions {
		rendition := &manifest.Renditions[i]
		confirmed := map[int]bool{}
		for _, segment := range rendition.Segments {
			confirmed[segment.Index] = true
		}
		for _, boundary := range manifest.Boundaries {
			if !confirmed[boundary.Index] {
				out = append(out, PendingSegment{Rendition: rendition.Name, Index: boundary.Index, StartMS: boundary.StartMS, EndMS: boundary.EndMS})
			}
		}
	}
	return out
}

// PrefixSegments returns the playable contiguous prefix for one rendition. An
// unavailable rendition yields an empty list, never a null.
func (manifest *DeliveryManifest) PrefixSegments(name string) []DeliverySegment {
	out := []DeliverySegment{}
	rendition := manifest.Rendition(name)
	if rendition == nil {
		return out
	}
	n := RenditionPrefix(*rendition)
	return append(out, rendition.Segments[:n]...)
}

// RenditionPrefixFor reports the contiguous confirmed prefix for a rendition.
func (manifest *DeliveryManifest) RenditionPrefixFor(name string) int {
	rendition := manifest.Rendition(name)
	if rendition == nil {
		return 0
	}
	return RenditionPrefix(*rendition)
}

// ValidateDelivery checks that a persisted manifest still describes exactly the
// canonical ladder and boundaries of its frozen render, that every confirmed
// record is well formed, and that derived flags agree with the records. It is
// applied on journal load and on every durable update, so a foreign or damaged
// manifest can neither load nor be committed.
func ValidateDelivery(r Render) error {
	manifest := r.Delivery
	if !DeliveryEnabled(r.Recipe) {
		if manifest != nil {
			return fmt.Errorf("delivery manifest present without a rendition ladder")
		}
		return nil
	}
	if manifest == nil {
		return fmt.Errorf("segmented render lacks a delivery manifest")
	}
	if manifest.FrozenSubmission != r.FrozenSubmission {
		return fmt.Errorf("delivery manifest is bound to a different submission")
	}
	if manifest.FrozenSheetRevision != r.Sheet.Revision || manifest.FrozenRecipeRevision != r.Recipe.Revision {
		return fmt.Errorf("delivery manifest is bound to different frozen revisions")
	}
	if manifest.DurationMS != r.Plan.DurationMS || manifest.SegmentMS != r.Recipe.SegmentMS {
		return fmt.Errorf("delivery manifest duration or segment length drifted")
	}
	expected := SegmentBoundaries(r.Plan.DurationMS, r.Recipe.SegmentMS)
	if len(expected) != len(manifest.Boundaries) {
		return fmt.Errorf("delivery boundary count drifted")
	}
	for i := range expected {
		if expected[i] != manifest.Boundaries[i] {
			return fmt.Errorf("delivery boundary %d drifted", i)
		}
	}
	ladder := OrderedRenditions(r.Recipe)
	if len(ladder) != len(manifest.Renditions) {
		return fmt.Errorf("delivery rendition count drifted")
	}
	for i, rendition := range ladder {
		delivery := manifest.Renditions[i]
		if delivery.Name != rendition.Name || delivery.Width != rendition.Width || delivery.Height != rendition.Height ||
			delivery.VideoBitrateKbps != rendition.VideoBitrateKbps || delivery.AudioBitrateKbps != rendition.AudioBitrateKbps {
			return fmt.Errorf("delivery rendition %d does not match the frozen ladder", i)
		}
		seen := map[int]bool{}
		for j, segment := range delivery.Segments {
			if segment.Rendition != delivery.Name {
				return fmt.Errorf("segment rendition mismatch")
			}
			if segment.Index < 0 || segment.Index >= len(expected) || seen[segment.Index] {
				return fmt.Errorf("segment index invalid or duplicated")
			}
			seen[segment.Index] = true
			if theBoundary := expected[segment.Index]; segment.StartMS != theBoundary.StartMS || segment.EndMS != theBoundary.EndMS {
				return fmt.Errorf("segment boundaries do not match the canonical list")
			}
			if segment.Submission != r.FrozenSubmission {
				return fmt.Errorf("segment is bound to a different submission")
			}
			if segment.Filename != SegmentRelPath(delivery.Name, segment.Index) {
				return fmt.Errorf("segment filename does not match its rendition and index")
			}
			if len(segment.Digest) != 64 || segment.Bytes <= 0 || segment.ConfirmedAt.IsZero() {
				return fmt.Errorf("segment record is incomplete")
			}
			if j > 0 && delivery.Segments[j-1].Index >= segment.Index {
				return fmt.Errorf("segments are not ordered by index")
			}
		}
		state := renditionState(&manifest.Renditions[i], len(expected))
		if delivery.State != state {
			return fmt.Errorf("rendition state disagrees with confirmed segments")
		}
	}
	complete := true
	for i := range manifest.Renditions {
		if manifest.Renditions[i].State != "complete" {
			complete = false
		}
	}
	if manifest.Complete != complete {
		return fmt.Errorf("completion flag disagrees with confirmed segments")
	}
	if manifest.Complete != (manifest.CompletedAt != nil) {
		return fmt.Errorf("completion timestamp disagrees with completion flag")
	}
	if manifest.CaptionsFile != CaptionsFilename || manifest.CaptionsDigest != VTTDigest(r.Plan.VTT) {
		return fmt.Errorf("delivery captions do not match the frozen plan")
	}
	return nil
}
