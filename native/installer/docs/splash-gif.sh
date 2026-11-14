#!/usr/bin/env bash
# Build docs/splash.gif from every frame of the opening animation at
# 126 x 33 cells: 100 ms a frame, as the installer plays it, then a hold on
# the last one. Needs go, node (docs/render.mjs) and ffmpeg.
#
#   native/installer/docs/splash-gif.sh        from anywhere in the repository
set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# The same Go defaults as native/Makefile.
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}" GOPROXY="${GOPROXY:-direct}" GOSUMDB="${GOSUMDB:-off}"
SPLASH_FRAMES_DIR="$tmp/frames" go test -count=1 -run '^TestSplashFrames$' ./internal/ui/ >/dev/null
node docs/render.mjs "$tmp/frames/126x33" "$tmp/png" >/dev/null

# The concat demuxer takes each frame's duration; the last file is listed
# twice so its duration counts.
frames=("$tmp"/png/frame-*.png)
last="${frames[-1]}"
for f in "${frames[@]}"; do
  d=0.1
  [ "$f" = "$last" ] && d=2.5
  printf "file '%s'\nduration %s\n" "$f" "$d"
done > "$tmp/list.txt"
printf "file '%s'\n" "$last" >> "$tmp/list.txt"

# Crop the 1920x1080 page to the terminal's cells, then a palette sized for
# the art's few colors.
ffmpeg -y -loglevel error -f concat -safe 0 -i "$tmp/list.txt" \
  -vf "fps=10,crop=1592:870:164:129,scale=960:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=96:stats_mode=full[p];[b][p]paletteuse=dither=none" \
  -loop 0 docs/splash.gif
echo "native/installer/docs/splash.gif"
