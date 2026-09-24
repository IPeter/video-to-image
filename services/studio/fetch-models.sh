#!/usr/bin/env bash
# Fills services/studio/models (git-ignored) with the sidecar's model files and verifies
# every file against models.sha256. Files already present with the right hash are kept.
#
#   services/studio/fetch-models.sh            # fetch + verify
#   services/studio/fetch-models.sh --check    # verify only (exit 1 on a missing/bad file)
#
# Sources, in order:
#   1. the research spike's copies (STUDIO_SPIKE_MODELS, default
#      data/research/studio-spike/models) when present;
#   2. SAM 2.1 base-plus from Hugging Face (onnx-community/sam2.1-hiera-base-plus-ONNX)
#      at the pinned revision below;
#   3. BiRefNet: STUDIO_BIREFNET_URL (a copy of our own export, e.g. in MinIO). There is
#      no public download of the DeformConv re-export; tools/README.md shows how to
#      reproduce it from the MIT weights (needs torch, one-off, not part of the image).
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
dest=${STUDIO_MODELS_DIR:-$here/models}
spike=${STUDIO_SPIKE_MODELS:-$here/../../data/research/studio-spike/models}
hf_repo=onnx-community/sam2.1-hiera-base-plus-ONNX
hf_rev=bab18593f44e652f04cf18b60b3690f60e8996b0
sam_dir=sam2.1-hiera-base-plus
check_only=false
[[ ${1:-} == --check ]] && check_only=true

sha() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

copy() { # clone on APFS when possible (no extra disk), else a plain copy
  cp -c "$1" "$2" 2>/dev/null || cp "$1" "$2"
}

fail=0
while read -r want name; do
  [[ -z ${want:-} || $want == \#* ]] && continue
  out=$dest/$name
  if [[ -f $out && $(sha "$out") == "$want" ]]; then
    echo "ok       $name"
    continue
  fi
  if $check_only; then
    echo "MISSING  $name (or wrong sha256)" >&2
    fail=1
    continue
  fi
  mkdir -p "$(dirname "$out")"
  tmp=$out.part
  rm -f "$tmp"
  case $name in
    birefnet-*)
      if [[ -f $spike/$name ]]; then
        copy "$spike/$name" "$tmp"
      elif [[ -n ${STUDIO_BIREFNET_URL:-} ]]; then
        curl -fsSL --retry 3 -o "$tmp" "$STUDIO_BIREFNET_URL"
      else
        echo "no source for $name: set STUDIO_BIREFNET_URL or re-export it (tools/README.md)" >&2
        exit 1
      fi
      ;;
    "$sam_dir"/*)
      f=${name#"$sam_dir"/}
      if [[ -f $spike/sam-base-plus/onnx/$f ]]; then
        copy "$spike/sam-base-plus/onnx/$f" "$tmp"
      else
        curl -fsSL --retry 3 -o "$tmp" "https://huggingface.co/$hf_repo/resolve/$hf_rev/onnx/$f"
      fi
      ;;
    *)
      echo "unknown model file $name in models.sha256" >&2
      exit 1
      ;;
  esac
  got=$(sha "$tmp")
  if [[ $got != "$want" ]]; then
    rm -f "$tmp"
    echo "sha256 mismatch for $name: got $got, want $want" >&2
    exit 1
  fi
  mv "$tmp" "$out"
  echo "fetched  $name"
done <"$here/models.sha256"
exit $fail
