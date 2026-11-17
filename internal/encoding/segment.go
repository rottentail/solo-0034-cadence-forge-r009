package encoding

import (
	"cadenceforge/internal/domain"
	"cadenceforge/internal/media"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SegmentEncoder runs one encoding branch and emits the canonical boundaries of
// the frozen plan as independent segment files. A single FFmpeg process carries
// the whole composition, so every rendition shares one time base and one frame
// grid; the segment muxer cuts that one timeline at forced keyframes. The ready
// callback fires as each file is closed by the writer, which lets the service
// confirm front segments while the tail is still encoding.
type SegmentEncoder struct {
	Binary string
}

// SegmentArguments renders the composition and selects the canonical range
// [startIndex, endIndex) of the frozen timeline. Encoding a bounded range keeps
// both the prefix and the tail resumable at exact boundaries, and segment
// numbering stays in the canonical index space.
func SegmentArguments(render domain.Render, paths map[string]string, rendition domain.Rendition, startIndex, endIndex int, pattern string) ([]string, error) {
	r := render.Recipe
	boundaries := domain.SegmentBoundaries(render.Plan.DurationMS, r.SegmentMS)
	if len(boundaries) == 0 {
		return nil, fmt.Errorf("cannot segment an empty plan")
	}
	if startIndex < 0 || endIndex > len(boundaries) || startIndex >= endIndex {
		return nil, fmt.Errorf("encode range outside the canonical boundaries")
	}
	offsetMS := boundaries[startIndex].StartMS
	endMS := boundaries[endIndex-1].EndMS
	args := []string{"-nostdin", "-y", "-hide_banner", "-loglevel", "error", "-filter_complex_threads", "1"}
	filters := []string{}
	joins := []string{}
	for i, p := range render.Plan.Placements {
		path, ok := paths[p.SourceID]
		if !ok {
			return nil, fmt.Errorf("source object missing")
		}
		args = append(args, "-protocol_whitelist", "file,pipe", "-format_whitelist", media.Formats, "-i", path)
		audio := fmt.Sprintf("[%d:a:0]atrim=start=%s:end=%s,asetpts=PTS-STARTPTS,aresample=%d,aformat=sample_fmts=fltp:channel_layouts=stereo,volume=%.3fdB[a%d]", i, seconds(p.SourceInMS), seconds(p.SourceOutMS), r.SampleRate, p.GainDB, i)
		if !render.Sources[p.SourceID].Media.Audio {
			audio = fmt.Sprintf("anullsrc=r=%d:cl=stereo,atrim=duration=%s,asetpts=PTS-STARTPTS[a%d]", r.SampleRate, seconds(p.OutputOutMS-p.OutputInMS), i)
		}
		filters = append(filters, audio)
		filters = append(filters, fmt.Sprintf("[%d:v:0]trim=start=%s:end=%s,setpts=PTS-STARTPTS,scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=%d,format=yuv420p[v%d]", i, seconds(p.SourceInMS), seconds(p.SourceOutMS), r.Width, r.Height, r.Width, r.Height, r.FPS, i))
		joins = append(joins, fmt.Sprintf("[v%d][a%d]", i, i))
	}
	if len(joins) == 0 {
		return nil, fmt.Errorf("empty render")
	}
	filters = append(filters, strings.Join(joins, "")+fmt.Sprintf("concat=n=%d:v=1:a=1[vcat][acat]", len(joins)))
	if r.Normalize {
		filters = append(filters, fmt.Sprintf("[acat]loudnorm=I=%.2f:TP=-1.5:LRA=11,aresample=%d[anorm]", r.TargetLUFS, r.SampleRate))
	} else {
		filters = append(filters, "[acat]anull[anorm]")
	}
	filters = append(filters, fmt.Sprintf("[vcat]trim=start=%s:end=%s,setpts=PTS-STARTPTS,scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=%d,format=yuv420p[vout]", seconds(offsetMS), seconds(endMS), rendition.Width, rendition.Height, rendition.Width, rendition.Height, r.FPS))
	filters = append(filters, fmt.Sprintf("[anorm]atrim=start=%s:end=%s,asetpts=PTS-STARTPTS[aout]", seconds(offsetMS), seconds(endMS)))
	args = append(args, "-filter_complex", strings.Join(filters, ";"))
	// Cut at explicit canonical offsets relative to the range start so the
	// muxer's automatic cut times cannot drift from the shared boundary list,
	// including the merged tail segment.
	cuts := []string{}
	for _, boundary := range boundaries[startIndex+1 : endIndex] {
		cuts = append(cuts, seconds(boundary.StartMS-offsetMS))
	}
	args = append(args,
		"-map", "[vout]", "-map", "[aout]",
		"-c:v", "libx264", "-preset", "veryfast",
		"-b:v", fmt.Sprintf("%dk", rendition.VideoBitrateKbps),
		"-maxrate", fmt.Sprintf("%dk", rendition.VideoBitrateKbps*3/2),
		"-bufsize", fmt.Sprintf("%dk", rendition.VideoBitrateKbps*2),
		"-threads", "1",
		"-force_key_frames", "expr:gte(t,n_forced*"+seconds(r.SegmentMS)+")",
		"-c:a", "aac", "-b:a", fmt.Sprintf("%dk", rendition.AudioBitrateKbps),
		"-ar", fmt.Sprint(r.SampleRate),
		"-t", seconds(endMS-offsetMS),
		"-map_metadata", "-1",
		"-f", "segment")
	if len(cuts) > 0 {
		args = append(args, "-segment_times", strings.Join(cuts, ","))
	} else {
		// A single-boundary range needs no internal cut; the muxer still emits
		// exactly one file at the requested start number.
		args = append(args, "-segment_time", seconds(endMS-offsetMS))
	}
	args = append(args,
		"-segment_format", "mp4",
		"-segment_format_options", "movflags=+faststart",
		"-reset_timestamps", "1",
		"-segment_start_number", fmt.Sprint(startIndex),
		pattern)
	return args, nil
}

// EncodeSegments encodes the canonical range [startIndex, endIndex) and invokes
// ready for each segment file once the writer has closed it. A file is closed
// when a later file exists, or when the process ended cleanly.
func (e SegmentEncoder) EncodeSegments(ctx context.Context, render domain.Render, paths map[string]string, rendition domain.Rendition, startIndex, endIndex int, pattern string, ready func(index int) error) error {
	boundaries := domain.SegmentBoundaries(render.Plan.DurationMS, render.Recipe.SegmentMS)
	if len(boundaries) == 0 {
		return fmt.Errorf("cannot segment an empty plan")
	}
	if startIndex < 0 || endIndex > len(boundaries) || startIndex >= endIndex {
		return fmt.Errorf("encode range outside the canonical boundaries")
	}
	args, err := SegmentArguments(render, paths, rendition, startIndex, endIndex, pattern)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.Binary, args...)
	var stderr media.LimitedBuffer
	stderr.Limit = 16384
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	confirmed := startIndex - 1
	report := func(index int) error {
		for index > confirmed {
			next := confirmed + 1
			if next >= endIndex {
				break
			}
			if err := ready(next); err != nil {
				return err
			}
			confirmed = next
		}
		return nil
	}
	fail := func(cause error) error {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return fmt.Errorf("%w: %s", cause, strings.TrimSpace(stderr.String()))
	}
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			ticker.Stop()
			// The callback may move each closed file out of the staging
			// directory, so observe what the writer produced before invoking
			// it for the remaining boundaries.
			produced := map[int]bool{}
			if entries, readErr := os.ReadDir(filepath.Dir(pattern)); readErr == nil {
				for _, entry := range entries {
					index := -1
					if _, scanErr := fmt.Sscanf(entry.Name(), "segment-%05d.mp4", &index); scanErr == nil && index >= startIndex && index < endIndex {
						produced[index] = true
					}
				}
			}
			if err != nil {
				return fmt.Errorf("segment encode: %w: %s", err, strings.TrimSpace(stderr.String()))
			}
			for index := confirmed + 1; index < endIndex; index++ {
				if !produced[index] {
					return fmt.Errorf("segment encode ended before boundary %d", index)
				}
			}
			if rerr := report(endIndex - 1); rerr != nil {
				return fmt.Errorf("confirm segments: %w", rerr)
			}
			return nil
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-ticker.C:
			entries, readErr := os.ReadDir(filepath.Dir(pattern))
			if readErr != nil && !os.IsNotExist(readErr) {
				return fail(readErr)
			}
			highest := -1
			for _, entry := range entries {
				index := -1
				if _, scanErr := fmt.Sscanf(entry.Name(), "segment-%05d.mp4", &index); scanErr != nil || index < startIndex || index >= endIndex {
					continue
				}
				if index > highest {
					highest = index
				}
			}
			// The muxer closes segment N before it creates N+1. Only indices
			// strictly below the highest observed file are therefore provably
			// complete; the highest is still being written.
			if highest-1 >= startIndex {
				if rerr := report(highest - 1); rerr != nil {
					return fail(rerr)
				}
			}
		}
	}
}
