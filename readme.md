# Spartan Radio

A minimal **Spartan protocol** audio radio server written in Go.

It serves one continuous live stream at `/radio`. Source audio files are decoded
by `ffmpeg`, converted to a common PCM format, encoded as one continuous
Ogg/Vorbis stream, and broadcast to all connected listeners.

The server can either:

- scan a music directory recursively, or
- load an explicit playlist file.

## Features

- Spartan protocol server (`spartan://`)
- Live Ogg/Vorbis stream at `/radio`
- WAV and FLAC source files
- Recursive directory scanning
- Optional playlist file
- Optional shuffle mode
- Music directory may be a symlink
- Symlinked subdirectories are followed
- Directory loops are detected and avoided
- One continuous `ffmpeg` Ogg/Vorbis encoder
- Cached Vorbis headers for listeners joining mid-stream
- TCP keepalive and write deadlines for stale listener cleanup

## Supported source formats

The scanner and playlist loader accept these filename extensions,
case-insensitively:

- `.wav`
- `.wave`
- `.flac`

For example, `song.WAV`, `recording.Wave`, and `album.FLAC` are accepted.

MP3, OGG, OGA, Opus, and other formats are not selected by the server.

The outgoing radio stream is always:

```text
audio/ogg
```

encoded with Vorbis.

## Requirements

- Go
- `ffmpeg` built with the `libvorbis` encoder

Check that the encoder is available:

```sh
ffmpeg -encoders | grep libvorbis
```

## Build

```sh
go build -o spartan-radio
```

## Basic usage

```sh
./spartan-radio [options]
```

### Scan a directory directly

A playlist file is not required. The server can recursively scan the music
directory itself:

```sh
./spartan-radio \
  -music-dir ./music \
  -shuffle \
  -host radio.example.org \
  -port 300
```

Without `-shuffle`, files are played in lexicographical path order and the list
repeats.

With `-shuffle`, the list is shuffled for each playback cycle.

The directory is scanned again at the beginning of every cycle, so newly added
files can be picked up without restarting the server.

### Use an explicit playlist

```sh
./spartan-radio \
  -playlist ./playlist.txt \
  -shuffle \
  -host radio.example.org \
  -port 300
```

When `-playlist` is set, `-music-dir` scanning is not used.

The playlist is loaded again at the beginning of every playback cycle, so edits
take effect without restarting the server.

## Playlist format

The playlist may contain plain paths:

```text
music/ambient/first.wav
music/ambient/second.flac
```

or `ffmpeg` concat-style lines:

```text
file 'music/ambient/first.wav'
file 'music/ambient/second.flac'
```

Empty lines and lines beginning with `#` or `;` are ignored.

Relative paths are resolved relative to the playlist file's directory.

Missing files and unsupported extensions are skipped.

## Generate a WAV/FLAC playlist

A playlist is only needed when you want explicit ordering or a manually
maintained selection.

Example:

```sh
find -L ./music -type f \
  \( -iname '*.wav' -o -iname '*.wave' -o -iname '*.flac' \) \
  -print | sort |
awk '{
  gsub(/\047/, "'\''\\'\'''\''", $0)
  print "file \047" $0 "\047"
}' > playlist.txt
```

Then run:

```sh
./spartan-radio \
  -playlist ./playlist.txt \
  -shuffle \
  -host radio.example.org
```

## Command-line options

| Flag | Default | Description |
| --- | --- | --- |
| `-music-dir` | `./music` | Directory containing WAV/WAVE/FLAC files; may be a symlink |
| `-playlist` | empty | Playlist file; when set, directory scanning is disabled |
| `-shuffle` | `false` | Shuffle the file list for each playback cycle |
| `-port` | `300` | TCP listening port |
| `-host` | `localhost` | Hostname advertised in the index link |
| `-ffmpeg` | `ffmpeg` | Path to the `ffmpeg` executable |
| `-bitrate-kbps` | `192` | Vorbis target bitrate; set to `0` to use quality mode |
| `-vorbis-q` | `4` | Vorbis quality used when `-bitrate-kbps=0` |
| `-stream-name` | empty | Stream title used in Vorbis metadata and on the index page |
| `-rescan` | `10s` | Delay after an empty playlist or playlist loading error |

## Vorbis encoding modes

### Target bitrate

The default is 192 kbit/s:

```sh
./spartan-radio -music-dir ./music -bitrate-kbps 192
```

### Quality mode

Set the bitrate to zero to enable `ffmpeg -q:a` quality mode:

```sh
./spartan-radio \
  -music-dir ./music \
  -bitrate-kbps 0 \
  -vorbis-q 4
```

## Directory scanning behavior

- The music directory itself may be a symlink.
- Subdirectories are scanned recursively.
- Symlinked subdirectories are followed.
- Resolved directory paths are tracked to avoid symlink loops.
- Files are initially sorted lexicographically.
- Only WAV, WAVE, and FLAC extensions are included.

Example:

```text
music/
├── ambient/
│   ├── first.wav
│   └── second.flac
├── field-recordings/
│   └── morning.WAV
└── live -> /mnt/music/live-recordings
```

## Endpoints

### `/`

Returns a Gemtext index page:

```text
Spartan Radio (Vorbis over Spartan)

=> spartan://radio.example.org:300/radio Tune in
```

When `-stream-name` is supplied, it is used as the page title.

### `/radio`

Returns:

```text
2 audio/ogg
```

followed by the continuous Ogg/Vorbis audio stream.

## Listener handling

Each listener receives the cached Vorbis headers before current stream pages,
allowing a client to begin decoding after joining mid-stream.

The TCP connection uses keepalive probes, and each stream write has a deadline.
Dead, disconnected, or persistently stalled clients are removed from the active
listener set.

## Example

```sh
./spartan-radio \
  -music-dir /srv/spartan-waves/music \
  -shuffle \
  -host radio.norayr.am \
  -port 300 \
  -stream-name "Spartan Waves" \
  -bitrate-kbps 192
```

Open:

```text
spartan://radio.norayr.am:300/
```

## License

GPL-3.0
