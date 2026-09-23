# Fixture videos

Synthetic clips for manual runs (`make dev-fake`, then upload through the web app or
`curl`) and for the Playwright e2e test. They are generated, not committed:

```sh
make fixtures              # = ./scripts/gen-fixtures.sh
./scripts/gen-fixtures.sh /some/other/dir
FFMPEG=/opt/ffmpeg/bin/ffmpeg ./scripts/gen-fixtures.sh
```

The script needs `ffmpeg` with `libx264` and takes a few seconds. Its output is
byte-identical from run to run with the same ffmpeg/x264 build, so the SHA-256 of each
file (the recording/replay key of the recorded analyzer) stays stable on one machine.
Different ffmpeg versions produce different bytes, so do not hard-code hashes in tests.

Go unit tests do **not** use these files. They generate their own tiny fixtures into
`t.TempDir()` and skip when ffmpeg is missing.

## Files

Expected outcomes assume the `cars` domain limits (`configs/domains/cars.yaml`: 5–180 s,
short side ≥ 480 px) and the fake analyzer (`--analyzer=fake`).

| File | What it is | Expected outcome |
| --- | --- | --- |
| `car-walkaround-like.mp4` | 1280x720, 30 s, 30 fps, H.264 + AAC. testsrc2 background with a hue drift and a silver "car" box with two "wheels" that pans left↔right and bobs, so every frame differs. | Accepted; the job completes with images. The happy path. |
| `portrait-phone.mp4` | Upright 720x1280 content stored as 1280x720 coded frames with a −90° display matrix, the way phones record portrait. 10 s. | Accepted. ffprobe reports display size 720x1280 (`rotation: -90`); preview and extracted frames are portrait. |
| `reject-not-a-car.mp4` | 1280x720, 10 s of Conway's game of life (fixed seed). Technically valid. | Passes validation. The fake analyzer fails the gate for any file name containing `reject`, so the job ends `rejected`. A real model should reject it for its content. |
| `too-short.mp4` | 1280x720, 2 s. | Upload rejected with `TOO_SHORT`. |
| `low-res.mp4` | 320x240, 10 s. | Upload rejected with `RESOLUTION_TOO_LOW`. |
| `not-a-video.txt` | Plain text. | Upload rejected with `UNSUPPORTED_FORMAT` by magic-byte sniffing, before ffprobe runs. |

To check a file by hand:

```sh
ffprobe -v error -select_streams V:0 \
  -show_entries format=duration:stream=codec_name,width,height,avg_frame_rate:stream_side_data=rotation \
  -of compact=p=0 testdata/videos/portrait-phone.mp4
```
