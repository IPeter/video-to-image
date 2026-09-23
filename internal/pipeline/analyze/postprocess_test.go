package analyze

import (
	"fmt"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/domain"
)

const (
	f3q   = "front_three_quarter"
	side  = "side_profile"
	dash  = "interior_dashboard"
	other = domain.OtherShotID
)

// ms builds a model scene whose reason names its start, so merges are traceable.
func ms(start, end, shot string, rel float64) ModelScene {
	return ModelScene{Start: start, End: end, ShotID: shot, Reason: "at " + start, Relevance: rel}
}

func sc(start, end float64, shot string, rel float64, rawStart, rawEnd, reason string) Scene {
	return Scene{StartSec: start, EndSec: end, ShotID: shot, Relevance: rel, Reason: reason, RawStart: rawStart, RawEnd: rawEnd}
}

func drop(rawStart, rawEnd, shot, reason string) Drop {
	return Drop{RawStart: rawStart, RawEnd: rawEnd, ShotID: shot, Reason: reason}
}

func TestPostProcess(t *testing.T) {
	cases := []struct {
		name      string
		scenes    []ModelScene
		duration  float64 // 0 → 60
		maxScenes int     // 0 → testDomain's 5
		want      []Scene // Idx is checked separately (0..n-1)
		wantDrops []Drop
	}{
		{
			name:   "valid scene kept, reason trimmed",
			scenes: []ModelScene{{Start: "00:04", End: "00:09", ShotID: f3q, Reason: "  hero shot \n", Relevance: 0.9}},
			want:   []Scene{sc(4, 9, f3q, 0.9, "00:04", "00:09", "hero shot")},
		},
		{
			name:      "unparseable start",
			scenes:    []ModelScene{ms("soon", "00:09", f3q, 0.9)},
			wantDrops: []Drop{drop("soon", "00:09", f3q, DropUnparseableStart)},
		},
		{
			name:      "unparseable end",
			scenes:    []ModelScene{ms("00:04", "", f3q, 0.9)},
			wantDrops: []Drop{drop("00:04", "", f3q, DropUnparseableEnd)},
		},
		{
			name:      "both unparseable reports the start",
			scenes:    []ModelScene{ms("x", "y", f3q, 0.9)},
			wantDrops: []Drop{drop("x", "y", f3q, DropUnparseableStart)},
		},
		{
			name:      "start at duration is out of range",
			scenes:    []ModelScene{ms("01:00", "01:05", f3q, 0.9), ms("02:00", "02:05", side, 0.9)},
			wantDrops: []Drop{drop("01:00", "01:05", f3q, DropOutOfRange), drop("02:00", "02:05", side, DropOutOfRange)},
		},
		{
			name:   "end clamped to duration",
			scenes: []ModelScene{ms("00:55", "01:10", side, 0.8)},
			want:   []Scene{sc(55, 60, side, 0.8, "00:55", "01:10", "at 00:55")},
		},
		{
			name:     "fractional duration clamp",
			scenes:   []ModelScene{ms("00:30", "00:31", side, 0.8)},
			duration: 30.5,
			want:     []Scene{sc(30, 30.5, side, 0.8, "00:30", "00:31", "at 00:30")},
		},
		{
			name:      "end equal to start",
			scenes:    []ModelScene{ms("00:04", "00:04", f3q, 0.9)},
			wantDrops: []Drop{drop("00:04", "00:04", f3q, DropEndBeforeStart)},
		},
		{
			name:      "end before start",
			scenes:    []ModelScene{ms("00:09", "00:04", f3q, 0.9)},
			wantDrops: []Drop{drop("00:09", "00:04", f3q, DropEndBeforeStart)},
		},
		{
			name:      "too short",
			scenes:    []ModelScene{ms("00:04", "00:04.2", f3q, 0.9)},
			wantDrops: []Drop{drop("00:04", "00:04.2", f3q, DropTooShort)},
		},
		{
			name:      "too short after clamping to duration",
			scenes:    []ModelScene{ms("00:59.9", "01:30", f3q, 0.9)},
			wantDrops: []Drop{drop("00:59.9", "01:30", f3q, DropTooShort)},
		},
		{
			name:   "exactly MinSceneSec is kept despite float rounding",
			scenes: []ModelScene{ms("00:04", "00:04.3", f3q, 0.9)},
			want:   []Scene{sc(4, 4.3, f3q, 0.9, "00:04", "00:04.3", "at 00:04")},
		},
		{
			name:      "unknown shot is kept as other and recorded",
			scenes:    []ModelScene{ms("00:04", "00:09", "wheel_closeup", 0.7)},
			want:      []Scene{sc(4, 9, other, 0.7, "00:04", "00:09", "at 00:04")},
			wantDrops: []Drop{drop("00:04", "00:09", "wheel_closeup", DropUnknownShot)},
		},
		{
			name:      "empty shot is unknown",
			scenes:    []ModelScene{ms("00:04", "00:09", "", 0.7)},
			want:      []Scene{sc(4, 9, other, 0.7, "00:04", "00:09", "at 00:04")},
			wantDrops: []Drop{drop("00:04", "00:09", "", DropUnknownShot)},
		},
		{
			name: "shot ids match case-insensitively after trimming",
			scenes: []ModelScene{
				ms("00:04", "00:09", " Side_Profile ", 0.7),
				ms("00:20", "00:25", "OTHER", 0.6),
			},
			want: []Scene{
				sc(4, 9, side, 0.7, "00:04", "00:09", "at 00:04"),
				sc(20, 25, other, 0.6, "00:20", "00:25", "at 00:20"),
			},
		},
		{
			name: "relevance clamped",
			scenes: []ModelScene{
				ms("00:01", "00:02", f3q, 1.5),
				ms("00:10", "00:12", side, -0.2),
				ms("00:20", "00:22", dash, math.NaN()),
				ms("00:30", "00:32", other, math.Inf(1)),
			},
			want: []Scene{
				sc(1, 2, f3q, 1, "00:01", "00:02", "at 00:01"),
				sc(10, 12, side, 0, "00:10", "00:12", "at 00:10"),
				sc(20, 22, dash, 0, "00:20", "00:22", "at 00:20"),
				sc(30, 32, other, 1, "00:30", "00:32", "at 00:30"),
			},
		},
		{
			name:      "overlapping same shot merge: span both, max relevance, reason of the best",
			scenes:    []ModelScene{ms("00:04", "00:09", f3q, 0.6), ms("00:08", "00:12", f3q, 0.9)},
			want:      []Scene{sc(4, 12, f3q, 0.9, "00:04", "00:12", "at 00:08")},
			wantDrops: []Drop{drop("00:08", "00:12", f3q, DropMerged)},
		},
		{
			name:      "merge is order-independent and keeps the earlier reason on a tie",
			scenes:    []ModelScene{ms("00:08", "00:12", f3q, 0.7), ms("00:04", "00:09", f3q, 0.7)},
			want:      []Scene{sc(4, 12, f3q, 0.7, "00:04", "00:12", "at 00:04")},
			wantDrops: []Drop{drop("00:08", "00:12", f3q, DropMerged)},
		},
		{
			name:      "contained range does not shrink the end",
			scenes:    []ModelScene{ms("00:02", "00:10", side, 0.5), ms("00:03", "00:05", side, 0.8)},
			want:      []Scene{sc(2, 10, side, 0.8, "00:02", "00:10", "at 00:03")},
			wantDrops: []Drop{drop("00:03", "00:05", side, DropMerged)},
		},
		{
			name:      "touching within 0.05 s merges",
			scenes:    []ModelScene{ms("00:04", "00:05", f3q, 0.5), ms("00:05.05", "00:07", f3q, 0.5)},
			want:      []Scene{sc(4, 7, f3q, 0.5, "00:04", "00:07", "at 00:04")},
			wantDrops: []Drop{drop("00:05.05", "00:07", f3q, DropMerged)},
		},
		{
			name:   "gap of 0.1 s does not merge",
			scenes: []ModelScene{ms("00:04", "00:05", f3q, 0.5), ms("00:05.1", "00:07", f3q, 0.5)},
			want: []Scene{
				sc(4, 5, f3q, 0.5, "00:04", "00:05", "at 00:04"),
				sc(5.1, 7, f3q, 0.5, "00:05.1", "00:07", "at 00:05.1"),
			},
		},
		{
			name:   "overlapping different shots are kept apart",
			scenes: []ModelScene{ms("00:04", "00:09", f3q, 0.6), ms("00:05", "00:10", side, 0.9)},
			want: []Scene{
				sc(4, 9, f3q, 0.6, "00:04", "00:09", "at 00:04"),
				sc(5, 10, side, 0.9, "00:05", "00:10", "at 00:05"),
			},
		},
		{
			name: "chains merge transitively",
			scenes: []ModelScene{
				ms("00:00", "00:03", dash, 0.4),
				ms("00:04.5", "00:08", dash, 0.5),
				ms("00:02", "00:05", dash, 0.6),
			},
			want: []Scene{sc(0, 8, dash, 0.6, "00:00", "00:08", "at 00:02")},
			wantDrops: []Drop{
				drop("00:02", "00:05", dash, DropMerged),
				drop("00:04.5", "00:08", dash, DropMerged),
			},
		},
		{
			name: "unknown shots merge with other",
			scenes: []ModelScene{
				ms("00:10", "00:14", other, 0.3),
				ms("00:12", "00:16", "tail_lights", 0.5),
			},
			want: []Scene{sc(10, 16, other, 0.5, "00:10", "00:16", "at 00:12")},
			wantDrops: []Drop{
				drop("00:12", "00:16", "tail_lights", DropUnknownShot),
				drop("00:12", "00:16", other, DropMerged),
			},
		},
		{
			name: "over max scenes keeps the most relevant, ties go to the earlier start",
			scenes: []ModelScene{
				ms("00:50", "00:52", f3q, 0.9),
				ms("00:01", "00:03", side, 0.2),
				ms("00:10", "00:12", dash, 0.5),
				ms("00:20", "00:22", f3q, 0.5),
				ms("00:30", "00:32", other, 0.5),
				ms("00:40", "00:42", side, 0.8),
			},
			maxScenes: 4,
			want: []Scene{
				sc(10, 12, dash, 0.5, "00:10", "00:12", "at 00:10"),
				sc(20, 22, f3q, 0.5, "00:20", "00:22", "at 00:20"),
				sc(40, 42, side, 0.8, "00:40", "00:42", "at 00:40"),
				sc(50, 52, f3q, 0.9, "00:50", "00:52", "at 00:50"),
			},
			wantDrops: []Drop{
				drop("00:30", "00:32", other, DropOverMaxScenes),
				drop("00:01", "00:03", side, DropOverMaxScenes),
			},
		},
		{
			name: "merging happens before the max-scenes cap",
			scenes: []ModelScene{
				ms("00:01", "00:03", f3q, 0.9),
				ms("00:02", "00:04", f3q, 0.9),
				ms("00:10", "00:12", side, 0.8),
			},
			maxScenes: 2,
			want: []Scene{
				sc(1, 4, f3q, 0.9, "00:01", "00:04", "at 00:01"),
				sc(10, 12, side, 0.8, "00:10", "00:12", "at 00:10"),
			},
			wantDrops: []Drop{drop("00:02", "00:04", f3q, DropMerged)},
		},
		{
			name: "same start: shorter first, then shot priority",
			scenes: []ModelScene{
				ms("00:04", "00:09", side, 0.5),
				ms("00:04", "00:09", f3q, 0.5),
				ms("00:04", "00:06", dash, 0.5),
			},
			want: []Scene{
				sc(4, 6, dash, 0.5, "00:04", "00:06", "at 00:04"),
				sc(4, 9, f3q, 0.5, "00:04", "00:09", "at 00:04"),
				sc(4, 9, side, 0.5, "00:04", "00:09", "at 00:04"),
			},
		},
		{
			name: "identical duplicates merge into the first listed",
			scenes: []ModelScene{
				{Start: "00:04", End: "00:09", ShotID: f3q, Reason: "first", Relevance: 0.5},
				{Start: "0:04", End: "0:09", ShotID: f3q, Reason: "second", Relevance: 0.5},
			},
			want:      []Scene{sc(4, 9, f3q, 0.5, "00:04", "00:09", "first")},
			wantDrops: []Drop{drop("0:04", "0:09", f3q, DropMerged)},
		},
		{
			name: "output ordered by start time",
			scenes: []ModelScene{
				ms("00:40", "00:42", side, 0.8),
				ms("00:05", "00:07", dash, 0.3),
				ms("00:20", "00:22", f3q, 0.9),
			},
			want: []Scene{
				sc(5, 7, dash, 0.3, "00:05", "00:07", "at 00:05"),
				sc(20, 22, f3q, 0.9, "00:20", "00:22", "at 00:20"),
				sc(40, 42, side, 0.8, "00:40", "00:42", "at 00:40"),
			},
		},
		{
			name: "mixed drops are reported in model order",
			scenes: []ModelScene{
				ms("00:04", "00:09", f3q, 0.9),
				ms("bad", "00:09", side, 0.9),
				ms("00:10", "00:10.1", dash, 0.9),
				ms("09:00", "09:05", dash, 0.9),
			},
			want: []Scene{sc(4, 9, f3q, 0.9, "00:04", "00:09", "at 00:04")},
			wantDrops: []Drop{
				drop("bad", "00:09", side, DropUnparseableStart),
				drop("00:10", "00:10.1", dash, DropTooShort),
				drop("09:00", "09:05", dash, DropOutOfRange),
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := testDomain()
			if c.maxScenes > 0 {
				d.Selection.MaxScenes = c.maxScenes
			}
			dur := c.duration
			if dur == 0 {
				dur = 60
			}
			resp := &ModelResponse{Scenes: append([]ModelScene(nil), c.scenes...)}
			got, drops := PostProcess(resp, dur, d)

			for i := range c.want {
				c.want[i].Idx = i
			}
			assertScenes(t, c.want, got)
			assert.Equal(t, c.wantDrops, drops)
			// Printed form, because NaN != NaN.
			assert.Equal(t, fmt.Sprint(c.scenes), fmt.Sprint(resp.Scenes), "input must not be modified")
		})
	}
}

// assertScenes compares scenes with a small tolerance on the float fields.
func assertScenes(t *testing.T, want, got []Scene) {
	t.Helper()
	require.Len(t, got, len(want))
	for i := range want {
		w, g := want[i], got[i]
		assert.InDelta(t, w.StartSec, g.StartSec, 1e-9, "scene %d start", i)
		assert.InDelta(t, w.EndSec, g.EndSec, 1e-9, "scene %d end", i)
		w.StartSec, w.EndSec = g.StartSec, g.EndSec
		assert.Equal(t, w, g, "scene %d", i)
	}
}

func TestPostProcessEdgeInputs(t *testing.T) {
	d := testDomain()
	valid := &ModelResponse{Scenes: []ModelScene{ms("00:04", "00:09", f3q, 0.9)}}

	s, dr := PostProcess(nil, 60, d)
	assert.Nil(t, s)
	assert.Nil(t, dr)

	s, dr = PostProcess(&ModelResponse{}, 60, d)
	assert.Nil(t, s)
	assert.Nil(t, dr)

	s, dr = PostProcess(valid, 60, nil)
	assert.Nil(t, s)
	assert.Nil(t, dr)

	for _, dur := range []float64{0, -5, math.NaN()} {
		s, dr = PostProcess(valid, dur, d)
		assert.Empty(t, s, "duration %v", dur)
		assert.Equal(t, []Drop{drop("00:04", "00:09", f3q, DropOutOfRange)}, dr, "duration %v", dur)
	}

	// Everything dropped → nil scenes but the drops are reported.
	s, dr = PostProcess(&ModelResponse{Scenes: []ModelScene{ms("x", "y", f3q, 1)}}, 60, d)
	assert.Nil(t, s)
	assert.Len(t, dr, 1)
}

// The result must not depend on the order the model listed the scenes in.
func TestPostProcessDeterministic(t *testing.T) {
	d := testDomain()
	d.Selection.MaxScenes = 4
	base := []ModelScene{
		ms("00:01", "00:04", f3q, 0.9),
		ms("00:03", "00:06", f3q, 0.4),
		ms("00:10", "00:13", side, 0.7),
		ms("00:12", "00:15", dash, 0.7),
		ms("00:20", "00:25", other, 0.2),
		ms("00:30", "00:31", "mystery", 0.6),
		ms("00:40", "00:44", side, 0.55),
		ms("00:50", "00:52", dash, 0.1),
	}
	want, _ := PostProcess(&ModelResponse{Scenes: base}, 60, d)
	require.Len(t, want, 4)

	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 50; i++ {
		perm := append([]ModelScene(nil), base...)
		rng.Shuffle(len(perm), func(a, b int) { perm[a], perm[b] = perm[b], perm[a] })
		got, _ := PostProcess(&ModelResponse{Scenes: perm}, 60, d)
		require.Equal(t, want, got, "permutation %d", i)
	}
	for i, s := range want {
		assert.Equal(t, i, s.Idx)
		if i > 0 {
			assert.LessOrEqual(t, want[i-1].StartSec, s.StartSec)
		}
	}
}
