# Spartan Radio

A minimal **Spartan protocol** audio streamer written in Go.

It serves a **single live radio stream** at `/radio`, streaming audio files from a directory in real time.
The music directory is scanned **recursively**, may be a **symlink**, and can contain MP3, Ogg, or both.

This is intentionally simple: no HTTP, no TLS, no transcoding, no metadata — just bytes over Spartan.

---

## Features

* 📡 **Spartan protocol** server (`spartan://`)
* 🎶 Live radio stream at `/radio`
* 📁 Recursive playlist generation (walks subdirectories)
* 🔗 Music directory can be a **symlink**
* 🎧 Supports:

  * MP3 only
  * Ogg/Vorbis only
  * MP3 + Ogg mixed (optional, with caveat)
* ⏱ Simple bitrate throttling (approximate, configurable)
* 🔄 Playlist rebuilt automatically between cycles

---

## Limitations (by design)

* One fixed MIME type per connection (Spartan requirement)
* When using mixed formats (`-format=both`), MIME must be generic
* No transcoding (files are streamed as-is)
* No ICY / metadata / track titles

---

## Build

```sh
go build -o spartan-radio
```

Or run directly:

```sh
go run .
```

---

## Usage

```sh
./spartan-radio [options]
```

### Command-line options

| Flag            | Default     | Description                                         |
| --------------- | ----------- | --------------------------------------------------- |
| `-music-dir`    | `./music`   | Directory containing audio files (can be a symlink) |
| `-port`         | `300`       | TCP port to listen on (Spartan default)             |
| `-host`         | `localhost` | Hostname used in the index page links               |
| `-format`       | `mp3`       | `mp3`, `ogg`, or `both`                             |
| `-bitrate-kbps` | `128`       | Approximate stream bitrate (throttling only)        |
| `-rescan`       | `10s`       | Delay when playlist is empty or rebuild fails       |
| `-mime`         | *(auto)*    | Override MIME type manually (advanced)              |

---

## Format modes

### MP3 only

```sh
./spartan-radio -format mp3
```

* Playlist includes only `.mp3`
* MIME: `audio/mpeg`
* Safest option for most players

---

### Ogg only

```sh
./spartan-radio -format ogg
```

* Playlist includes `.ogg` and `.oga`
* MIME: `audio/ogg`

---

### Mixed MP3 + Ogg

```sh
./spartan-radio -format both
```

* Playlist includes `.mp3`, `.ogg`, `.oga`
* MIME: `application/octet-stream`

⚠ **Important**
This works only if the client/player can **sniff the audio format from the byte stream**.
Many players cannot seamlessly switch formats mid-stream.

If you want full compatibility, use **one format only** or add transcoding.

---

## Directory scanning behavior

* The music directory itself may be a symlink
* Subdirectories are scanned recursively
* Symlinked subdirectories are followed
* Directory loops are detected and avoided
* Playlist order is lexicographical (sorted paths)

Example:

```text
music/
├── ambient/
│   ├── a.ogg
│   └── b.ogg
├── rock/
│   └── song.mp3
└── live -> /mnt/music/live-recordings
```

---

## Endpoints

### `/`

Index page (Gemtext):

```text
Spartan Radio

=> spartan://example.org:300/radio Tune in
```

### `/radio`

Live audio stream.

---

## Example
First find out the bitrate of your mp3s:

```
ffprobe music/21_car_radio.mp3 2>&1 | grep -E "Audio: mp3"
```

It'll give us something like:

```
./sprout-waves -format mp3 -bitrate-kbps 256
```


```sh
./sprout-waves -format mp3 -host radio.norayr.am -bitrate-kbps 320
```

Then open in a Spartan-capable client:

```text
spartan://radio.norayr.am/radio
```

With the player, do:

```
./swp -host radio.norayr.am -path /radio -player mpv
```

---

## Why Spartan?

* Simple text-based protocol
* No TLS or PKI
* Suitable for experimental networks (Yggdrasil, mesh, local-only)
* Fits well with Gemini-adjacent tooling

---

## License

GPL-3

---

