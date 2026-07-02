package playback

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/recordstore"
	"github.com/bluenviron/mediamtx/internal/test"
	"github.com/stretchr/testify/require"
)

// setSegmentEnd sets the file modification time to the given segment end,
// simulating what the recorder does when it closes (or flushes) a segment.
func setSegmentEnd(t testing.TB, fpath string, end time.Time) {
	err := os.Chtimes(fpath, end, end)
	require.NoError(t, err)
}

func TestOnFastList(t *testing.T) {
	start1 := time.Date(2008, 11, 7, 11, 22, 0, 500000000, time.Local)
	start2 := time.Date(2008, 11, 7, 11, 23, 2, 500000000, time.Local)
	start3 := time.Date(2009, 11, 7, 11, 23, 2, 500000000, time.Local)
	startGap := time.Date(2008, 11, 7, 11, 24, 2, 500000000, time.Local)

	for _, ca := range []string{
		"unfiltered",
		"filtered",
		"filtered and gap",
		"different init joined",
		"start after duration",
		"start before first",
	} {
		t.Run(ca, func(t *testing.T) {
			dir := t.TempDir()

			err := os.Mkdir(filepath.Join(dir, "mypath"), 0o755)
			require.NoError(t, err)

			seg1 := filepath.Join(dir, "mypath", "2008-11-07_11-22-00-500000.mp4")
			seg2 := filepath.Join(dir, "mypath", "2008-11-07_11-23-02-500000.mp4")
			seg3 := filepath.Join(dir, "mypath", "2009-11-07_11-23-02-500000.mp4")
			segGap := filepath.Join(dir, "mypath", "2008-11-07_11-24-02-500000.mp4")

			switch ca {
			case "unfiltered", "filtered", "start before first":
				writeSegment1(t, seg1)
				setSegmentEnd(t, seg1, start1.Add(62*time.Second))
				writeSegment2(t, seg2)
				setSegmentEnd(t, seg2, start2.Add(4*time.Second))
				writeSegment2(t, seg3)
				setSegmentEnd(t, seg3, start3.Add(4*time.Second))

			case "filtered and gap":
				writeSegment1(t, seg1)
				setSegmentEnd(t, seg1, start1.Add(62*time.Second))
				writeSegment2(t, segGap)
				setSegmentEnd(t, segGap, startGap.Add(4*time.Second))

			case "different init joined":
				writeSegment1(t, seg1)
				setSegmentEnd(t, seg1, start1.Add(62*time.Second))
				writeSegment3(t, seg2)
				setSegmentEnd(t, seg2, start2.Add(1*time.Second))

			case "start after duration":
				writeSegment1(t, seg1)
				setSegmentEnd(t, seg1, start1.Add(62*time.Second))
			}

			checked := false

			s := &Server{
				Address:      "127.0.0.1:9996",
				ReadTimeout:  conf.Duration(10 * time.Second),
				WriteTimeout: conf.Duration(10 * time.Second),
				PathConfs: map[string]*conf.Path{
					"mypath": {
						Name:         "mypath",
						RecordPath:   filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
						RecordFormat: conf.RecordFormatFMP4,
					},
				},
				AuthManager: &test.AuthManager{
					AuthenticateImpl: func(req *auth.Request) (string, *auth.Error) {
						require.Equal(t, conf.AuthActionPlayback, req.Action)
						checked = true
						return req.Credentials.User, nil
					},
				},
				Parent: test.NilLogger,
			}
			err = s.Initialize()
			require.NoError(t, err)
			defer s.Close()

			u, err := url.Parse("http://myuser:mypass@localhost:9996/fastlist")
			require.NoError(t, err)

			v := url.Values{}
			v.Set("path", "mypath")

			switch ca {
			case "filtered":
				v.Set("start", start1.Add(1*time.Second).Format(time.RFC3339Nano))
				v.Set("end", start3.Add(2*time.Second).Format(time.RFC3339Nano))

			case "filtered and gap":
				v.Set("start", start1.Add(80*time.Second).Format(time.RFC3339Nano))
				v.Set("end", start3.Add(2*time.Second).Format(time.RFC3339Nano))

			case "start after duration":
				v.Set("start", time.Date(2010, 11, 7, 11, 23, 20, 500000000, time.Local).Format(time.RFC3339Nano))

			case "start before first":
				v.Set("start", time.Date(2007, 11, 7, 11, 23, 20, 500000000, time.Local).Format(time.RFC3339Nano))
			}

			u.RawQuery = v.Encode()

			req, err := http.NewRequest(http.MethodGet, u.String(), nil)
			require.NoError(t, err)

			res, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer res.Body.Close()

			if ca == "start after duration" {
				require.Equal(t, http.StatusNotFound, res.StatusCode)
				return
			}

			require.Equal(t, http.StatusOK, res.StatusCode)

			var out any
			err = json.NewDecoder(res.Body).Decode(&out)
			require.NoError(t, err)

			switch ca {
			case "unfiltered", "start before first":
				require.Equal(t, []any{
					map[string]any{
						"duration": float64(66),
						"width":    float64(1920),
						"height":   float64(1080),
						"start":    start1.Format(time.RFC3339Nano),
						"url": "http://localhost:9996/get?duration=66&path=mypath&start=" +
							url.QueryEscape(start1.Format(time.RFC3339Nano)),
					},
					map[string]any{
						"duration": float64(4),
						"width":    float64(1920),
						"height":   float64(1080),
						"start":    start3.Format(time.RFC3339Nano),
						"url": "http://localhost:9996/get?duration=4&path=mypath&start=" +
							url.QueryEscape(start3.Format(time.RFC3339Nano)),
					},
				}, out)

			case "filtered":
				require.Equal(t, []any{
					map[string]any{
						"duration": float64(65),
						"width":    float64(1920),
						"height":   float64(1080),
						"start":    start1.Add(1 * time.Second).Format(time.RFC3339Nano),
						"url": "http://localhost:9996/get?duration=65&path=mypath&start=" +
							url.QueryEscape(start1.Add(1*time.Second).Format(time.RFC3339Nano)),
					},
					map[string]any{
						"duration": float64(2),
						"width":    float64(1920),
						"height":   float64(1080),
						"start":    start3.Format(time.RFC3339Nano),
						"url": "http://localhost:9996/get?duration=2&path=mypath&start=" +
							url.QueryEscape(start3.Format(time.RFC3339Nano)),
					},
				}, out)

			case "filtered and gap":
				require.Equal(t, []any{
					map[string]any{
						"duration": float64(4),
						"width":    float64(1920),
						"height":   float64(1080),
						"start":    startGap.Format(time.RFC3339Nano),
						"url": "http://localhost:9996/get?duration=4&path=mypath&start=" +
							url.QueryEscape(startGap.Format(time.RFC3339Nano)),
					},
				}, out)

			case "different init joined":
				// unlike /list, /fastlist joins by time adjacency alone,
				// so segments with different init data become a single span.
				require.Equal(t, []any{
					map[string]any{
						"duration": float64(63),
						"width":    float64(1920),
						"height":   float64(1080),
						"start":    start1.Format(time.RFC3339Nano),
						"url": "http://localhost:9996/get?duration=63&path=mypath&start=" +
							url.QueryEscape(start1.Format(time.RFC3339Nano)),
					},
				}, out)
			}

			require.True(t, checked)
		})
	}
}

// TestOnFastListZeroDurationHeader checks that segments whose header duration
// was never written (crashed or in-progress recordings) are listed using the
// file modification time, without scanning the file parts.
func TestOnFastListZeroDurationHeader(t *testing.T) {
	dir := t.TempDir()

	err := os.Mkdir(filepath.Join(dir, "mypath"), 0o755)
	require.NoError(t, err)

	start := time.Date(2008, 11, 7, 11, 22, 0, 500000000, time.Local)
	fpath := filepath.Join(dir, "mypath", "2008-11-07_11-22-00-500000.mp4")

	// writeSegment1 does not write mvhd duration: the header reports 0,
	// like a segment that is still being recorded or was never closed.
	writeSegment1(t, fpath)
	setSegmentEnd(t, fpath, start.Add(120*time.Second))

	s := &Server{
		Address:      "127.0.0.1:9996",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		PathConfs: map[string]*conf.Path{
			"mypath": {
				Name:         "mypath",
				RecordPath:   filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
				RecordFormat: conf.RecordFormatFMP4,
			},
		},
		AuthManager: test.NilAuthManager,
		Parent:      test.NilLogger,
	}
	err = s.Initialize()
	require.NoError(t, err)
	defer s.Close()

	u, err := url.Parse("http://localhost:9996/fastlist")
	require.NoError(t, err)

	v := url.Values{}
	v.Set("path", "mypath")
	u.RawQuery = v.Encode()

	res, err := http.Get(u.String())
	require.NoError(t, err)
	defer res.Body.Close()

	require.Equal(t, http.StatusOK, res.StatusCode)

	var out any
	err = json.NewDecoder(res.Body).Decode(&out)
	require.NoError(t, err)

	require.Equal(t, []any{
		map[string]any{
			"duration": float64(120),
			"width":    float64(1920),
			"height":   float64(1080),
			"start":    start.Format(time.RFC3339Nano),
			"url": "http://localhost:9996/get?duration=120&path=mypath&start=" +
				url.QueryEscape(start.Format(time.RFC3339Nano)),
		},
	}, out)
}

// prepareBenchSegments creates count contiguous segments with header duration
// and mtime both set, so /list takes its cheapest route (header read, no part
// scan) and the comparison with /fastlist is fair.
func prepareBenchSegments(tb testing.TB, dir string, count int) *conf.Path {
	err := os.Mkdir(filepath.Join(dir, "mypath"), 0o755)
	require.NoError(tb, err)

	format := filepath.Join(dir, "mypath", "%Y-%m-%d_%H-%M-%S-%f.mp4")
	start := time.Date(2008, 11, 7, 11, 22, 0, 500000000, time.Local)
	const segDuration = 62 * time.Second

	for i := 0; i < count; i++ {
		segStart := start.Add(time.Duration(i) * segDuration)
		fpath := recordstore.Path{Start: segStart}.Encode(format)

		writeSegment1(tb, fpath)

		f, err := os.OpenFile(fpath, os.O_RDWR, 0o644)
		require.NoError(tb, err)
		err = writeDuration(f, segDuration)
		require.NoError(tb, err)
		err = f.Close()
		require.NoError(tb, err)

		setSegmentEnd(tb, fpath, segStart.Add(segDuration))
	}

	return &conf.Path{
		Name:         "mypath",
		RecordPath:   filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
		RecordFormat: conf.RecordFormatFMP4,
	}
}

func BenchmarkList(b *testing.B) {
	dir := b.TempDir()
	pathConf := prepareBenchSegments(b, dir, 500)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		segments, err := recordstore.FindSegments(pathConf, "mypath", nil, nil)
		require.NoError(b, err)
		_, err = parseAndConcatenate(conf.RecordFormatFMP4, segments)
		require.NoError(b, err)
	}
}

func BenchmarkFastList(b *testing.B) {
	dir := b.TempDir()
	pathConf := prepareBenchSegments(b, dir, 500)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		segments, err := recordstore.FindSegments(pathConf, "mypath", nil, nil)
		require.NoError(b, err)
		entries, err := fastListEntries(segments)
		require.NoError(b, err)
		require.NotEmpty(b, entries)
	}
}
