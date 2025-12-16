#!/usr/bin/env bash
set -euo pipefail

# use like
# ./build_playlist.sh ./music mp3 > playlist.txt


MUSIC_DIR="${1:-./music}"
FORMAT="${2:-mp3}" # mp3|ogg|both

case "$FORMAT" in
  mp3)  REGEX='.*\.mp3$' ;;
  ogg)  REGEX='.*\.(ogg|oga)$' ;;
  both) REGEX='.*\.(mp3|ogg|oga)$' ;;
  *) echo "bad format"; exit 2 ;;
esac

find -L ${MUSIC_DIR} -type f -iname '*.mp3' | sort


