package extract

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"

	"video-to-image/internal/ids"
	"video-to-image/internal/media"
)

// Run executes Plan with ffmpeg and returns the candidates. The total never exceeds
// MaxCandidates.
//
// Ranges run sequentially (ffmpeg already uses every core to decode), one ffmpeg call
// per range writing WorkDir/cand/<scene>_%04d.jpg and WorkDir/small/<scene>_%04d.jpg,
// where <scene> is the 3-digit SceneIdx, suffixed with "-<plan index>" when several
// ranges share a SceneIdx. Candidate i of a range gets TimestampSec = PadStartSec +
// i/FPS clamped to [0, DurationSec] — the media.ExtractFrames sampling contract, exact
// to within one source frame.
//
// A range that yields no frames (it starts after the last decodable frame, e.g. when
// the container duration includes a longer audio track) is skipped and logged. If no
// range yields any frame the video is treated as undecodable and Run fails. If ffmpeg
// ever writes more frames than MaxCandidates in total, frames are dropped evenly
// within each range (never the tail), each range keeping at least one, and the
// dropped files are deleted. On error, every file Run wrote is removed; the caller
// still owns (and deletes) WorkDir.
func Run(ctx context.Context, ff *media.FFmpeg, in Input) (*Output, error) {
	switch {
	case ff == nil:
		return nil, errors.New("extract: nil ffmpeg")
	case in.SourcePath == "":
		return nil, errors.New("extract: empty source path")
	case in.WorkDir == "":
		return nil, errors.New("extract: empty work dir")
	}
	log := ff.Logger
	if log == nil {
		log = slog.Default()
	}

	plans := Plan(in)
	out := &Output{Plans: plans, Candidates: []Candidate{}}
	if len(plans) == 0 {
		log.Info("extract: no ranges to sample", "ranges", len(in.Ranges), "durationSec", in.DurationSec)
		return out, nil
	}
	candDir := filepath.Join(in.WorkDir, "cand")
	smallDir := filepath.Join(in.WorkDir, "small")
	for _, d := range []string{candDir, smallDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("extract: %w", err)
		}
	}

	names := fileStems(plans)
	var got []rangeFrames
	fail := func(err error) (*Output, error) {
		for _, r := range got {
			r.remove(0, len(r.full))
		}
		return nil, err
	}
	for k, p := range plans {
		if err := ctx.Err(); err != nil {
			return fail(fmt.Errorf("extract: %w", err))
		}
		res, err := ff.ExtractFrames(ctx, media.ExtractArgs{
			Input:        in.SourcePath,
			StartSec:     p.PadStartSec,
			DurationSec:  p.PadEndSec - p.PadStartSec,
			FPS:          p.FPS,
			FullPattern:  filepath.Join(candDir, names[k]+"_%04d.jpg"),
			SmallPattern: filepath.Join(smallDir, names[k]+"_%04d.jpg"),
			SmallWidth:   SmallWidth,
			FullQScale:   FullQScale,
			SmallQScale:  SmallQScale,
		})
		if err != nil {
			return fail(fmt.Errorf("extract: scene %d (%.2f–%.2f s): %w", p.SceneIdx, p.PadStartSec, p.PadEndSec, err))
		}
		n := min(len(res.Full), len(res.Small))
		if n == 0 {
			log.Warn("extract: range produced no frames; skipping",
				"scene", p.SceneIdx, "shot", p.ShotID, "padStartSec", p.PadStartSec, "padEndSec", p.PadEndSec, "fps", p.FPS)
			continue
		}
		if n != p.Expected {
			log.Debug("extract: frame count differs from plan", "scene", p.SceneIdx, "expected", p.Expected, "got", n)
		}
		got = append(got, rangeFrames{plan: k, full: res.Full[:n], small: res.Small[:n]})
	}
	if len(got) == 0 {
		return nil, fmt.Errorf("extract: no frames could be extracted from %d ranges of %s", len(plans), in.SourcePath)
	}

	maxCand := in.MaxCandidates
	if maxCand <= 0 {
		maxCand = DefaultMaxCandidates
	}
	if dropped := enforceCap(got, plans, maxCand); dropped > 0 {
		log.Warn("extract: more frames than the candidate cap; dropped evenly", "dropped", dropped, "max", maxCand)
	}

	for _, r := range got {
		p := plans[r.plan]
		for _, i := range r.keep {
			ts := p.PadStartSec + float64(i)/p.FPS
			out.Candidates = append(out.Candidates, Candidate{
				ID:           ids.NewCandidate(),
				SceneIdx:     p.SceneIdx,
				ShotID:       p.ShotID,
				Relevance:    p.Relevance,
				SceneReason:  p.Reason,
				TimestampSec: math.Min(math.Max(ts, 0), in.DurationSec),
				FullPath:     r.full[i],
				SmallPath:    r.small[i],
			})
		}
	}
	sort.SliceStable(out.Candidates, func(a, b int) bool {
		ca, cb := out.Candidates[a], out.Candidates[b]
		if ca.SceneIdx != cb.SceneIdx {
			return ca.SceneIdx < cb.SceneIdx
		}
		return ca.TimestampSec < cb.TimestampSec
	})
	return out, nil
}

// rangeFrames are the files one range produced. keep lists the frame indices that
// survive the cap (all of them unless enforceCap dropped some); a frame's index, not
// its position in keep, determines its timestamp.
type rangeFrames struct {
	plan        int
	full, small []string
	keep        []int
}

func (r rangeFrames) remove(from, to int) {
	for i := from; i < to; i++ {
		_ = os.Remove(r.full[i])
		_ = os.Remove(r.small[i])
	}
}

// enforceCap fills every range's keep list, dropping frames evenly so the total is
// at most maxCand, deletes the dropped files and returns how many were dropped.
func enforceCap(got []rangeFrames, plans []RangePlan, maxCand int) int {
	counts := make([]int, len(got))
	rel := make([]float64, len(got))
	for i, r := range got {
		counts[i] = len(r.full)
		rel[i] = plans[r.plan].Relevance
	}
	quotas := allocate(counts, rel, maxCand)
	dropped := 0
	for i := range got {
		r := &got[i]
		r.keep = evenIndices(counts[i], quotas[i])
		kept := make(map[int]bool, len(r.keep))
		for _, k := range r.keep {
			kept[k] = true
		}
		for j := range counts[i] {
			if !kept[j] {
				r.remove(j, j+1)
				dropped++
			}
		}
	}
	return dropped
}

// allocate splits maxCand frames across ranges proportionally to counts (largest
// remainder), never more than a range has and at least one per range. When there are
// more ranges than maxCand, the most relevant ranges get one frame each and the rest
// none.
func allocate(counts []int, relevance []float64, maxCand int) []int {
	total := 0
	for _, c := range counts {
		total += c
	}
	quotas := make([]int, len(counts))
	if total <= maxCand {
		copy(quotas, counts)
		return quotas
	}
	nonEmpty := 0
	for _, c := range counts {
		if c > 0 {
			nonEmpty++
		}
	}
	if maxCand <= 0 {
		return quotas
	}
	if nonEmpty > maxCand {
		order := make([]int, 0, nonEmpty)
		for i, c := range counts {
			if c > 0 {
				order = append(order, i)
			}
		}
		sort.SliceStable(order, func(a, b int) bool { return relevance[order[a]] > relevance[order[b]] })
		for _, i := range order[:maxCand] {
			quotas[i] = 1
		}
		return quotas
	}

	type rem struct {
		i    int
		frac float64
	}
	rems := make([]rem, 0, len(counts))
	sum := 0
	for i, c := range counts {
		if c == 0 {
			continue
		}
		exact := float64(c) * float64(maxCand) / float64(total)
		q := min(c, max(1, int(math.Floor(exact))))
		quotas[i] = q
		sum += q
		rems = append(rems, rem{i, exact - float64(q)})
	}
	// The one-frame minimum can overshoot: take back from the largest quotas.
	for sum > maxCand {
		big := -1
		for i, q := range quotas {
			if q > 1 && (big < 0 || q > quotas[big]) {
				big = i
			}
		}
		quotas[big]--
		sum--
	}
	// Hand out the rest by largest remainder.
	sort.SliceStable(rems, func(a, b int) bool { return rems[a].frac > rems[b].frac })
	for sum < maxCand {
		progressed := false
		for _, r := range rems {
			if sum == maxCand {
				break
			}
			if quotas[r.i] < counts[r.i] {
				quotas[r.i]++
				sum++
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	return quotas
}

// evenIndices picks q of n indices spread evenly over [0, n): the centre of each of
// q equal buckets. q ≥ n returns all indices.
func evenIndices(n, q int) []int {
	if q >= n {
		all := make([]int, n)
		for i := range all {
			all[i] = i
		}
		return all
	}
	out := make([]int, q)
	for j := range q {
		out[j] = int(math.Floor((float64(j) + 0.5) * float64(n) / float64(q)))
	}
	return out
}

// fileStems names each plan's frames: the 3-digit scene index, plus "-<plan index>"
// when several plans share a scene index, so patterns never collide.
func fileStems(plans []RangePlan) []string {
	seen := make(map[int]int, len(plans))
	for _, p := range plans {
		seen[p.SceneIdx]++
	}
	names := make([]string, len(plans))
	for k, p := range plans {
		names[k] = fmt.Sprintf("%03d", p.SceneIdx)
		if seen[p.SceneIdx] > 1 {
			names[k] += fmt.Sprintf("-%02d", k)
		}
	}
	return names
}
