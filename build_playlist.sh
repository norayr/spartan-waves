#!/usr/bin/env bash

# Usage:
# ./build_playlist.sh ./music wav > playlist.txt

set -euo pipefail

MUSIC_DIR="${1:-./music}"
FORMAT="${2:-mp3}" # mp3|ogg|both|wav

case "$FORMAT" in
  mp3)
    find -L "$MUSIC_DIR" -type f -iname '*.mp3' -print | sort
    ;;
  ogg)
    find -L "$MUSIC_DIR" -type f \( -iname '*.ogg' -o -iname '*.oga' \) -print | sort
    ;;
  both)
    find -L "$MUSIC_DIR" -type f \( -iname '*.mp3' -o -iname '*.ogg' -o -iname '*.oga' \) -print | sort
    ;;
  wav)
    find -L "$MUSIC_DIR" -type f \( -iname '*.wav' -o -iname '*.wave' \) -print | sort \
      | awk '{
          gsub(/\047/, "'\''\\'\'''\''", $0);
          print "file \047" $0 "\047"
        }'
    ;;
  *)
    echo "bad format"; exit 2
    ;;
esac
