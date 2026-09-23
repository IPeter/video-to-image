#!/usr/bin/env bash
# gen-fixtures.sh — write deterministic synthetic fixture videos into testdata/videos/
# for manual runs (`make dev-fake`) and the Playwright e2e test. See
# testdata/videos/README.md for what each file exercises.
#
# Usage: scripts/gen-fixtures.sh [OUT_DIR]      (default: <repo>/testdata/videos)
#        FFMPEG=/path/to/ffmpeg scripts/gen-fixtures.sh
#
# Requires ffmpeg with libx264. Output is byte-identical across runs with the same
# ffmpeg/x264 build (fixed seeds, fixed thread count, bitexact muxing, no metadata).
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="${1:-$root/testdata/videos}"
ffmpeg_bin="${FFMPEG:-ffmpeg}"

die() { echo "gen-fixtures: $*" >&2; exit 1; }

command -v "$ffmpeg_bin" >/dev/null 2>&1 || die "'$ffmpeg_bin' not found on PATH (install ffmpeg or set FFMPEG)"
# Capture the list first: `ffmpeg ... | grep -q` under pipefail fails with SIGPIPE (141)
# on Linux when grep exits at the first match while ffmpeg is still writing.
encoders="$("$ffmpeg_bin" -hide_banner -encoders 2>/dev/null)" || die "'$ffmpeg_bin' cannot list encoders"
grep -q libx264 <<<"$encoders" || die "'$ffmpeg_bin' has no libx264 encoder"

mkdir -p "$out"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/v2i-fixtures.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

ff() { "$ffmpeg_bin" -y -hide_banner -nostdin -loglevel error "$@"; }

# Shared encoder settings. A fixed -threads count matters: x264's output depends on it.
enc=(
  -c:v libx264 -preset veryfast -crf 23 -pix_fmt yuv420p -g 60 -threads 4
  -c:a aac -b:a 96k -ac 2
  -fflags +bitexact -flags:v +bitexact -flags:a +bitexact -map_metadata -1
  -movflags +faststart
)

# testsrc_with_audio NAME WxH SECONDS FREQ — testsrc2 plus a sine tone.
testsrc_with_audio() {
  local name=$1 size=$2 secs=$3 freq=$4
  ff -f lavfi -i "testsrc2=size=${size}:rate=30:duration=${secs}" \
     -f lavfi -i "sine=frequency=${freq}:sample_rate=48000:duration=${secs}" \
     -map 0:v -map 1:a "${enc[@]}" -shortest "$out/$name"
}

echo "gen-fixtures: writing into $out"

# 1. car-walkaround-like.mp4 — 1280x720, 30 s. testsrc2 background with a slow hue
#    drift, plus a silver "car body" with two dark "wheels" that sweeps left↔right over
#    the clip (a walkaround pan) and bobs slightly (hand-held camera), so consecutive
#    frames always differ and different moments look different.
ff -f lavfi -i "testsrc2=size=1280x720:rate=30:duration=30" \
   -f lavfi -i "color=c=0xB8BCC2:size=520x200:rate=30:duration=30" \
   -f lavfi -i "color=c=0x1E1E1E:size=100x100:rate=30:duration=30" \
   -f lavfi -i "sine=frequency=220:sample_rate=48000:duration=30" \
   -filter_complex "
     [0:v]hue=h='6*t'[bg];
     [2:v]split[wa][wb];
     [bg][1:v]overlay=x='390+330*sin(2*PI*t/30)':y='380+12*sin(2*PI*t/4)':shortest=1[body];
     [body][wa]overlay=x='430+330*sin(2*PI*t/30)':y='530+12*sin(2*PI*t/4)':shortest=1[w1];
     [w1][wb]overlay=x='750+330*sin(2*PI*t/30)':y='530+12*sin(2*PI*t/4)':shortest=1[v]" \
   -map "[v]" -map 3:a "${enc[@]}" -shortest "$out/car-walkaround-like.mp4"

# 2. portrait-phone.mp4 — what a phone records in portrait: upright 720x1280 content,
#    stored as 1280x720 coded frames with a -90° display matrix (displays 720x1280).
ff -f lavfi -i "testsrc2=size=720x1280:rate=30:duration=10" \
   -f lavfi -i "sine=frequency=330:sample_rate=48000:duration=10" \
   -filter_complex "[0:v]transpose=cclock[v]" \
   -map "[v]" -map 1:a "${enc[@]}" -shortest "$tmp/portrait-coded.mp4"
if ! ff -display_rotation -90 -i "$tmp/portrait-coded.mp4" -map 0 -c copy \
        -fflags +bitexact -map_metadata -1 -movflags +faststart "$out/portrait-phone.mp4" 2>/dev/null; then
  # ffmpeg < 6.1 has no -display_rotation; the legacy (clockwise) rotate tag is
  # turned into the same display matrix by the mp4 muxer.
  ff -i "$tmp/portrait-coded.mp4" -map 0 -c copy -metadata:s:v:0 rotate=90 \
     -fflags +bitexact -movflags +faststart "$out/portrait-phone.mp4"
fi

# 3. reject-not-a-car.mp4 — 1280x720, 10 s of Conway's game of life (fixed seed).
#    Valid in every technical way; the fake analyzer rejects any file name containing
#    "reject", and a real model should reject the content.
#    The 320x180 grid is scaled up with nearest-neighbour so the cells compress well.
ff -f lavfi -i "life=size=320x180:rate=30:seed=42:ratio=0.1:mold=10:life_color=#3FBF3F:death_color=#402020:mold_color=#101030" \
   -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=10" \
   -filter_complex "[0:v]scale=1280:720:flags=neighbor[v]" \
   -map "[v]" -map 1:a -t 10 "${enc[@]}" "$out/reject-not-a-car.mp4"

# 4. too-short.mp4 — 2 s (cars minDurationSec is 5 → TOO_SHORT).
testsrc_with_audio too-short.mp4 1280x720 2 550

# 5. low-res.mp4 — 320x240, 10 s (short side 240 < minShortSidePx 480 → RESOLUTION_TOO_LOW).
testsrc_with_audio low-res.mp4 320x240 10 660

# 6. not-a-video.txt — plain text (sniffing → UNSUPPORTED_FORMAT).
cat >"$out/not-a-video.txt" <<'EOF'
This is a plain text file, not a video.
Uploading it must fail with UNSUPPORTED_FORMAT before ffprobe ever runs.
EOF

echo "gen-fixtures: done"
for f in car-walkaround-like.mp4 portrait-phone.mp4 reject-not-a-car.mp4 too-short.mp4 low-res.mp4 not-a-video.txt; do
  printf '  %-26s %8s bytes\n' "$f" "$(wc -c <"$out/$f" | tr -d ' ')"
done
