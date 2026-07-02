package playback

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"time"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/recordstore"
	"github.com/gin-gonic/gin"
)

// fastStatSegment is a segment whose end time has been estimated
// from its file modification time, without opening the file.
// When produced by fastGroupSpans, it represents a contiguous run of
// segments and fpath is the path of the first segment of the run.
type fastStatSegment struct {
	fpath string
	start time.Time
	end   time.Time
}

// fastStatSegments estimates the end time of each segment from its file
// modification time. The recorder rewrites the header when a segment is
// closed, so for cleanly closed segments mtime matches the time recording
// stopped; for crashed or in-progress segments it matches the last flush.
// The estimate is wrong when mtimes were not preserved (recordings copied
// without timestamps) or when a segment was closed long after its last
// sample (stalled publisher): mtime is wall-clock time, not media time.
// Segments deleted between directory walk and stat are skipped; any other
// stat error is returned.
func fastStatSegments(segments []*recordstore.Segment) ([]fastStatSegment, error) {
	stats := make([]fastStatSegment, len(segments))
	errs := make([]error, len(segments))

	forEachBounded(len(segments), segmentIOConcurrency, func(i int) {
		info, err := os.Stat(segments[i].Fpath)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs[i] = err
			}
			return
		}

		end := info.ModTime()
		if end.Before(segments[i].Start) {
			end = segments[i].Start
		}

		stats[i] = fastStatSegment{
			fpath: segments[i].Fpath,
			start: segments[i].Start,
			end:   end,
		}
	})

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	out := stats[:0]
	for _, seg := range stats {
		if seg.fpath != "" {
			out = append(out, seg)
		}
	}
	return out, nil
}

// fastGroupSpans groups segments into contiguous spans by time adjacency
// alone, using the same tolerance as /list concatenation. Unlike /list,
// which compares fMP4 init data, this may join segments across a stream
// restart when the gap is below the tolerance; /get stops playing at such a
// boundary, so the join is harmless.
func fastGroupSpans(stats []fastStatSegment) []fastStatSegment {
	var spans []fastStatSegment

	for _, seg := range stats {
		if len(spans) > 0 {
			cur := &spans[len(spans)-1]
			if !seg.start.After(cur.end.Add(concatenationTolerance)) {
				if seg.end.After(cur.end) {
					cur.end = seg.end
				}
				continue
			}
		}

		spans = append(spans, seg)
	}

	return spans
}

// fastSpanResolution reads the video resolution from the header of the first
// segment of a span. Errors are not fatal: the entry is returned without
// resolution.
func fastSpanResolution(fpath string) (int, int) {
	f, err := os.Open(fpath)
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	init, _, _, err := segmentFMP4ReadHeader(f)
	if err != nil {
		return 0, 0
	}

	return videoResolution(init)
}

func fastListEntries(segments []*recordstore.Segment) ([]listEntry, error) {
	stats, err := fastStatSegments(segments)
	if err != nil {
		return nil, err
	}

	spans := fastGroupSpans(stats)

	entries := make([]listEntry, len(spans))

	forEachBounded(len(spans), segmentIOConcurrency, func(i int) {
		w, h := fastSpanResolution(spans[i].fpath)
		entries[i] = listEntry{
			Start:    spans[i].start,
			Duration: listEntryDuration(spans[i].end.Sub(spans[i].start)),
			Width:    w,
			Height:   h,
		}
	})

	return entries, nil
}

// onFastList is a faster variant of onList that never reads whole segment
// files: durations are estimated from file modification times and segments
// are concatenated by time adjacency, opening only the first file of each
// contiguous span (to read the video resolution). See fastStatSegments for
// the accuracy caveats of mtime-based durations.
func (s *Server) onFastList(ctx *gin.Context) {
	params, ok := s.parseListParams(ctx)
	if !ok {
		return
	}

	segments, ok := s.findSegments(ctx, params)
	if !ok {
		return
	}

	if params.pathConf.RecordFormat != conf.RecordFormatFMP4 {
		s.writeError(ctx, http.StatusInternalServerError, errMPEGTSNotSupported)
		return
	}

	entries, err := fastListEntries(segments)
	if err != nil {
		s.writeError(ctx, http.StatusInternalServerError, err)
		return
	}

	entries = trimEntries(entries, params.start, params.end)
	if len(entries) == 0 {
		s.writeError(ctx, http.StatusNotFound, recordstore.ErrNoSegmentsFound)
		return
	}

	s.fillEntryURLs(ctx, params.pathName, entries)

	ctx.JSON(http.StatusOK, entries)
}
