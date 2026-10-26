package analyze

import (
	"math"
	"sort"
	"strings"

	"video-to-image/internal/domain"
)

// Drop reasons recorded by PostProcess (Drop.Reason).
const (
	DropUnparseableStart = "unparseable_start"
	DropUnparseableEnd   = "unparseable_end"
	DropOutOfRange       = "out_of_range"
	DropEndBeforeStart   = "end_before_start"
	DropTooShort         = "too_short"
	DropUnknownShot      = "unknown_shot_mapped_to_other" // the scene is kept as domain.OtherShotID
	DropMerged           = "merged"
	DropOverMaxScenes    = "over_max_scenes"
)

// MergeTouchSec: two ranges of the same shot whose gap is at most this long are merged
// as if they overlapped (model timestamps are only accurate to about a second anyway).
const MergeTouchSec = 0.05

// floatSlack absorbs binary rounding in length/gap comparisons (4.3 - 4.0 < 0.3 and
// 5.05 - 5.0 > 0.05 in float64).
const floatSlack = 1e-9

// candidate is a scene being post-processed; order and prio make every sort total.
type candidate struct {
	Scene
	order int // index in the model response
	prio  int // shot priority index; len(shots) for OtherShotID
}

// PostProcess parses, clamps, filters, maps unknown shots to "other", merges overlapping
// ranges with the same shotId, keeps at most Selection.MaxScenes (by relevance), and
// returns scenes ordered by start time with Idx assigned.
//
// Per scene, in this order: unparseable start/end → dropped; start ≥ duration →
// out_of_range; both ends clamped to [0, duration]; end ≤ start → end_before_start;
// shorter than MinSceneSec → too_short. A shotId matching no domain shot (compared
// case-insensitively after trimming) is kept as domain.OtherShotID and recorded as
// unknown_shot_mapped_to_other. Relevance is clamped to [0, 1] (NaN → 0). Reason is
// trimmed and capped at MaxSceneReasonRunes (see Truncate).
//
// Merging: ranges of the same shot that overlap or are at most MergeTouchSec apart
// become one range spanning both, with the higher relevance. RawStart comes from the
// scene that supplies the merged start, RawEnd from the one that supplies the merged
// end, and Reason from the most relevant scene (the earlier one on a tie). The absorbed
// (later-starting) scene is recorded as "merged".
//
// Then the MaxScenes most relevant scenes are kept (ties → earlier start) and the rest
// recorded as over_max_scenes. The result is deterministic for a given input. Drop.ShotID
// is what the model said for per-scene drops and the canonical shot ID for "merged" and
// "over_max_scenes". A NaN or negative durationSec is treated as 0 (every scene is out of
// range).
func PostProcess(resp *ModelResponse, durationSec float64, d *domain.Config) ([]Scene, []Drop) {
	if resp == nil || d == nil || len(resp.Scenes) == 0 {
		return nil, nil
	}
	if math.IsNaN(durationSec) || durationSec < 0 {
		durationSec = 0
	}

	shotPrio := make(map[string]int, len(d.Shots))
	shotByKey := make(map[string]string, len(d.Shots))
	for i, s := range d.Shots {
		shotPrio[s.ID] = i
		shotByKey[strings.ToLower(strings.TrimSpace(s.ID))] = s.ID
	}
	otherPrio := len(d.Shots)

	var drops []Drop
	drop := func(ms ModelScene, reason string) {
		drops = append(drops, Drop{RawStart: ms.Start, RawEnd: ms.End, ShotID: ms.ShotID, Reason: reason})
	}

	kept := make([]candidate, 0, len(resp.Scenes))
	for i, ms := range resp.Scenes {
		start, err := ParseTimestamp(ms.Start)
		if err != nil {
			drop(ms, DropUnparseableStart)
			continue
		}
		end, err := ParseTimestamp(ms.End)
		if err != nil {
			drop(ms, DropUnparseableEnd)
			continue
		}
		if start >= durationSec {
			drop(ms, DropOutOfRange)
			continue
		}
		start = clamp(start, 0, durationSec)
		end = clamp(end, 0, durationSec)
		if end <= start {
			drop(ms, DropEndBeforeStart)
			continue
		}
		if end-start < MinSceneSec-floatSlack {
			drop(ms, DropTooShort)
			continue
		}

		key := strings.ToLower(strings.TrimSpace(ms.ShotID))
		shotID, prio := domain.OtherShotID, otherPrio
		if id, ok := shotByKey[key]; ok {
			shotID, prio = id, shotPrio[id]
		} else if key != domain.OtherShotID {
			drop(ms, DropUnknownShot) // recorded, but the scene is kept as "other"
		}

		kept = append(kept, candidate{
			Scene: Scene{
				StartSec:  start,
				EndSec:    end,
				ShotID:    shotID,
				Relevance: clampRelevance(ms.Relevance),
				Reason:    Truncate(ms.Reason, MaxSceneReasonRunes),
				RawStart:  ms.Start,
				RawEnd:    ms.End,
			},
			order: i,
			prio:  prio,
		})
	}

	merged, mergeDrops := mergeSameShot(kept)
	drops = append(drops, mergeDrops...)

	if limit := d.Selection.MaxScenes; limit > 0 && len(merged) > limit {
		sort.SliceStable(merged, func(i, j int) bool {
			a, b := merged[i], merged[j]
			if a.Relevance != b.Relevance {
				return a.Relevance > b.Relevance
			}
			return byStart(a, b)
		})
		for _, c := range merged[limit:] {
			drops = append(drops, Drop{RawStart: c.RawStart, RawEnd: c.RawEnd, ShotID: c.ShotID, Reason: DropOverMaxScenes})
		}
		merged = merged[:limit]
	}

	sort.SliceStable(merged, func(i, j int) bool { return byStart(merged[i], merged[j]) })
	if len(merged) == 0 {
		return nil, drops
	}
	scenes := make([]Scene, len(merged))
	for i, c := range merged {
		c.Idx = i
		scenes[i] = c.Scene
	}
	return scenes, drops
}

// mergeSameShot merges overlapping or touching ranges that share a shot. The input is
// not modified.
func mergeSameShot(in []candidate) ([]candidate, []Drop) {
	if len(in) == 0 {
		return nil, nil
	}
	cs := append([]candidate(nil), in...)
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.prio != b.prio {
			return a.prio < b.prio
		}
		return byStart(a, b)
	})

	var out []candidate
	var drops []Drop
	cur := cs[0]
	reasonRel := cur.Relevance // relevance of the scene that supplied cur.Reason
	for _, next := range cs[1:] {
		if next.ShotID == cur.ShotID && next.StartSec-cur.EndSec <= MergeTouchSec+floatSlack {
			drops = append(drops, Drop{RawStart: next.RawStart, RawEnd: next.RawEnd, ShotID: next.ShotID, Reason: DropMerged})
			if next.EndSec > cur.EndSec {
				cur.EndSec, cur.RawEnd = next.EndSec, next.RawEnd
			}
			if next.Relevance > reasonRel {
				cur.Reason, reasonRel = next.Reason, next.Relevance
			}
			cur.Relevance = math.Max(cur.Relevance, next.Relevance)
			continue
		}
		out = append(out, cur)
		cur, reasonRel = next, next.Relevance
	}
	return append(out, cur), drops
}

// byStart orders by start, then end, then shot priority, then model order.
func byStart(a, b candidate) bool {
	switch {
	case a.StartSec != b.StartSec:
		return a.StartSec < b.StartSec
	case a.EndSec != b.EndSec:
		return a.EndSec < b.EndSec
	case a.prio != b.prio:
		return a.prio < b.prio
	default:
		return a.order < b.order
	}
}

func clamp(v, lo, hi float64) float64 { return math.Min(math.Max(v, lo), hi) }

// clampRelevance maps v into [0, 1]; NaN becomes 0.
func clampRelevance(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return clamp(v, 0, 1)
}

// MergeScenes adds extra scenes (e.g. the moments the contact-sheet pass located) to
// base: both are treated like one model answer, so ranges of the same shot that
// overlap or touch become one (see mergeSameShot: the higher relevance and its reason
// win) and at most Selection.MaxScenes are kept by relevance, the rest recorded as
// over_max_scenes. Scenes must already be post-processed (clamped, shot IDs
// canonical). The result is ordered by start with Idx reassigned; the inputs are not
// modified.
func MergeScenes(base, extra []Scene, d *domain.Config) ([]Scene, []Drop) {
	if d == nil || len(base)+len(extra) == 0 {
		return nil, nil
	}
	shotPrio := make(map[string]int, len(d.Shots))
	for i, s := range d.Shots {
		shotPrio[s.ID] = i
	}
	prioOf := func(id string) int {
		if p, ok := shotPrio[id]; ok {
			return p
		}
		return len(d.Shots)
	}
	cs := make([]candidate, 0, len(base)+len(extra))
	for _, list := range [][]Scene{base, extra} {
		for _, s := range list {
			cs = append(cs, candidate{Scene: s, order: len(cs), prio: prioOf(s.ShotID)})
		}
	}
	merged, drops := mergeSameShot(cs)
	if limit := d.Selection.MaxScenes; limit > 0 && len(merged) > limit {
		sort.SliceStable(merged, func(i, j int) bool {
			a, b := merged[i], merged[j]
			if a.Relevance != b.Relevance {
				return a.Relevance > b.Relevance
			}
			return byStart(a, b)
		})
		for _, c := range merged[limit:] {
			drops = append(drops, Drop{RawStart: c.RawStart, RawEnd: c.RawEnd, ShotID: c.ShotID, Reason: DropOverMaxScenes})
		}
		merged = merged[:limit]
	}
	sort.SliceStable(merged, func(i, j int) bool { return byStart(merged[i], merged[j]) })
	out := make([]Scene, len(merged))
	for i, c := range merged {
		c.Idx = i
		out[i] = c.Scene
	}
	return out, drops
}
