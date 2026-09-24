import numpy as np
import pytest

from v2i_studio import masks

# ---- box ---------------------------------------------------------------------------------


@pytest.mark.parametrize(
    "raw, want",
    [
        ("0.1,0.2,0.9,0.8", (0.1, 0.2, 0.9, 0.8)),
        ("0,0,1,1", (0.0, 0.0, 1.0, 1.0)),
        ("1e-1,.2,0.9,0.80", (0.1, 0.2, 0.9, 0.8)),
    ],
)
def test_parse_box_accepts_plain_numbers(raw, want):
    assert masks.parse_box(raw) == pytest.approx(want)


@pytest.mark.parametrize(
    "raw",
    [
        None,
        "",
        "0.1,0.2,0.9",
        "0.1,0.2,0.9,0.8,0.5",
        "a,b,c,d",
        "nan,0,1,1",
        "0,0,inf,1",
        "0x1,0,1,1",
        " 0.1,0.2,0.9,0.8",
        "0.1,,0.9,0.8",
        "0.5,0.2,0.5,0.8",  # x0 == x1
        "0.6,0.2,0.5,0.8",  # x0 > x1
        "0.1,0.8,0.9,0.2",  # y0 > y1
        "-0.1,0.2,0.9,0.8",
        "0.1,0.2,1.01,0.8",
        "0.1,0.2,0.9,1.5",
    ],
)
def test_parse_box_rejects(raw):
    with pytest.raises(masks.InputError):
        masks.parse_box(raw)


def test_check_box_size():
    masks.check_box_size((0.1, 0.1, 0.2, 0.2), 160, 160)  # 16 px: ok
    with pytest.raises(masks.InputError):
        masks.check_box_size((0.1, 0.1, 0.2, 0.2), 150, 1000)


def test_crop_rect_pads_six_percent_and_clips():
    # box 800x300 px → pad 48 px left/right, 18 px top/bottom
    assert masks.crop_rect((0.1, 0.2, 0.9, 0.8), 1000, 500) == (52, 82, 948, 418)
    assert masks.crop_rect((0.0, 0.0, 1.0, 1.0), 640, 480) == (0, 0, 640, 480)
    assert masks.crop_rect((0.02, 0.5, 0.99, 0.99), 1000, 1000) == (0, 470, 1000, 1000)


def test_rescue_radius_is_04_percent_of_long_side():
    assert masks.rescue_radius(1115, 1920) == 8
    assert masks.rescue_radius(3840, 2160) == 15
    assert masks.rescue_radius(100, 200) == 2  # floor of 2 px


def test_birefnet_alpha_stretches_to_full_range():
    a = masks.birefnet_alpha(np.linspace(-3, 5, 100, dtype=np.float32).reshape(10, 10))
    assert a.dtype == np.uint8 and a.min() == 0 and a.max() == 255
    assert np.all(np.diff(a.ravel().astype(int)) >= 0)


# ---- SAM ---------------------------------------------------------------------------------


def _three_masks(areas):
    m = np.full((3, 10, 10), -1.0, np.float32)
    for k, a in enumerate(areas):
        m[k].ravel()[:a] = 1.0
    return m


def test_select_sam_output_takes_largest_among_near_best():
    m = _three_masks([10, 50, 30])
    assert masks.select_sam_output(m, np.array([0.90, 0.95, 0.94])) == 1
    # the largest mask scores too low; the larger of the two near the best wins
    assert masks.select_sam_output(m, np.array([0.95, 0.90, 0.94])) == 2


def test_sam_clean_drops_specks_fills_holes_keeps_big_parts():
    m = np.zeros((400, 400), np.uint8)
    m[100:300, 100:300] = 1  # the car
    m[150:160, 150:160] = 0  # enclosed hole → filled
    m[380:383, 10:13] = 1  # far speck → dropped
    m[97, 150] = 1  # 1-px island next to the car → dropped (< 2.5e-5 of the image)
    m[0:50, 350:400] = 1  # a large far part (> 1% of the image) → kept
    out = masks.sam_clean(m)
    assert out[155, 155] == 1
    assert out[381, 11] == 0
    assert out[97, 150] == 0
    assert out[0:50, 350:400].all()
    assert out[100:300, 100:300].all()
    assert m[155, 155] == 0  # input untouched


def test_sam_alpha_soft_edge_from_logits():
    lg = np.full((200, 200), -4.0, np.float32)
    lg[50:150, 50:150] = 4.0
    lg[90:100, 90:100] = -1.0  # hole inside → 255
    lg[5:7, 5:7] = 3.0  # far speck → 0
    lg[50, 50:150] = 0.5  # weakly positive edge row → soft, but ≥ 128
    a = masks.sam_alpha(lg)
    assert a[95, 95] == 255
    assert a[6, 6] == 0
    assert 128 <= a[50, 100] < 255
    assert a[100, 100] == 255 and a[180, 180] == 0


# ---- rescue union --------------------------------------------------------------------------

N = 1000  # image side; r = 4 px, min area = 20 px


def _rect(y0, y1, x0, x1, v=255):
    a = np.zeros((N, N), np.uint8)
    a[y0:y1, x0:x1] = v
    return a


def test_rescue_puts_back_a_region_birefnet_dropped():
    a_s = _rect(200, 800, 200, 800)
    a_b = a_s.copy()
    a_b[300:500, 300:500] = 0  # e.g. an open tailgate BiRefNet cut
    out, rescued, n = masks.rescue_union(a_b, a_s, masks.rescue_radius(N, N))
    assert n == 1
    assert out[300:500, 300:500].min() == 255
    assert rescued == pytest.approx(200 * 200 / N**2)
    assert np.array_equal(out, a_s)


def test_rescue_raises_see_through_regions():
    a_s = _rect(200, 800, 200, 800)
    a_b = a_s.copy()
    a_b[300:500, 300:500] = 100  # semi-transparent body panel
    out, rescued, _ = masks.rescue_union(a_b, a_s, 4)
    assert out[300:500, 300:500].min() == 255
    assert rescued > 0


def test_rescue_ignores_thin_sam_fringe():
    a_b = _rect(200, 800, 200, 800)
    a_s = a_b.copy()
    a_s[197:200, 200:800] = 255  # 3 px background fringe (< 2r): BiRefNet's edge wins
    out, rescued, n = masks.rescue_union(a_b, a_s, 4)
    assert (n, rescued) == (0, 0.0)
    assert np.array_equal(out, a_b)


def test_rescue_ignores_components_below_min_area():
    a_s = _rect(200, 800, 200, 800)
    a_b = a_s.copy()
    a_b[500:504, 500:504] = 0  # 16 px < 2e-5 of the image
    out, rescued, n = masks.rescue_union(a_b, a_s, 4)
    assert (n, rescued) == (0, 0.0)
    assert out[502, 502] == 0


def test_rescue_grows_back_to_sam_outline_but_not_beyond():
    a_b = _rect(200, 800, 200, 500)
    a_s = _rect(200, 800, 200, 800)  # SAM also has the right half (e.g. a B-pillar block)
    out, _, _ = masks.rescue_union(a_b, a_s, 4)
    assert out[200:800, 200:800].min() == 255
    assert out[:, 800:].max() == 0 and out[:200].max() == 0


def test_rescue_never_lowers_alpha():
    rng = np.random.default_rng(7)
    a_b = rng.integers(0, 256, (N, N)).astype(np.uint8)
    a_s = _rect(100, 900, 100, 900)
    a_s[400:420] = 0
    out, _, _ = masks.rescue_union(a_b, a_s, 4)
    assert np.all(out >= a_b)


# ---- statistics ----------------------------------------------------------------------------

CONTRACT_KEYS = {
    "iou",
    "rescuedFraction",
    "coverage",
    "boxFill",
    "outsideBox",
    "secondComponent",
    "semiTransparent",
    "edgeContact",
}


def test_mask_stats_areas_and_components():
    H, W = 100, 200
    a = np.zeros((H, W), np.uint8)
    a[20:60, 40:120] = 255  # 3200 px, exactly the box
    a[20, 40:120] = 200  # soft but inside the mask
    a[60, 40:120] = 100  # soft, outside the mask (alpha < 128)
    a[80:85, 180:190] = 255  # detached second part, 50 px
    box = (0.2, 0.2, 0.6, 0.6)
    s, d = masks.mask_stats(a, a, a, box, 0.0123)
    assert set(s) == CONTRACT_KEYS
    assert s["coverage"] == pytest.approx(3250 / 20000)
    assert s["boxFill"] == pytest.approx(1.0)
    assert s["outsideBox"] == pytest.approx(50 / 3250, abs=1e-6)
    assert s["secondComponent"] == pytest.approx(50 / 20000)
    assert s["semiTransparent"] == pytest.approx(160 / 3250, abs=1e-6)
    assert s["rescuedFraction"] == 0.0123
    assert s["iou"] == 1.0
    assert d["components"] == 2
    assert d["maskBox"] == pytest.approx([0.2, 0.2, 0.95, 0.85])


def test_mask_stats_semi_transparent_counts_every_alpha_above_zero():
    a = np.zeros((100, 100), np.uint8)
    a[10:90, 10:90] = 255
    a[0, 0:10] = 1  # faint noise far from the car still counts (0 < alpha < 250)
    a[10, 10:20] = 250  # 250 is opaque
    s, _ = masks.mask_stats(a, a, a, (0.1, 0.1, 0.9, 0.9), 0.0)
    assert s["semiTransparent"] == pytest.approx(10 / 6400, abs=1e-6)


def test_mask_stats_edge_contact():
    H, W = 100, 200
    a = np.zeros((H, W), np.uint8)
    a[10:30, 0:5] = 255  # touches the left edge on 20 of 100 rows
    a[95:100, 50:100] = 255  # touches the bottom edge on 50 of 200 columns
    s, _ = masks.mask_stats(a, a, a, (0.0, 0.0, 1.0, 1.0), 0.0)
    assert s["edgeContact"] == {"left": 0.2, "right": 0.0, "top": 0.0, "bottom": 0.25}


def test_mask_stats_iou_between_models():
    a_b = np.zeros((100, 100), np.uint8)
    a_s = np.zeros((100, 100), np.uint8)
    a_b[0:50, 0:100] = 255  # 5000
    a_s[25:75, 0:100] = 255  # 5000, overlap 2500
    s, _ = masks.mask_stats(np.maximum(a_b, a_s), a_b, a_s, (0, 0, 1, 1), 0.0)
    assert s["iou"] == pytest.approx(2500 / 7500, abs=1e-6)


def test_mask_stats_empty_mask():
    a = np.zeros((50, 60), np.uint8)
    s, d = masks.mask_stats(a, a, a, (0.1, 0.1, 0.9, 0.9), 0.0)
    assert s["coverage"] == 0 and s["iou"] == 0 and s["secondComponent"] == 0
    assert d["maskBox"] is None and d["components"] == 0
