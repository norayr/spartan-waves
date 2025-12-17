# Spartan Radio

A minimal **Spartan protocol** audio streamer written in Go.

It serves a **single live radio stream** at `/radio`, streaming audio files from a directory in real time.

It can play from a directory, or by using a playlist. It can also shuffle the playlist.

At first we were supporting MP3's because MP3's can be played from the middle of the byte stream. However, since Lagrange Gemini browser doesn't support MP3 streaming on IOS and Android (it works perfectly fine on Maemo-Leste with mobile UI), we deprecated MP3 streams and added more complex code to stream Ogg files.

---

## Features

* 📡 **Spartan protocol** server (`spartan://`)
* 🎶 Live radio stream at `/radio`
* 📁 Recursive playlist generation (walks subdirectories)
* 🔗 Music directory can be a **symlink**
* 🎧 Supports:

  * Ogg/Vorbis only
* ⏱ Simple bitrate throttling (approximate, configurable)

---

## Limitations (by design)

* No transcoding (files are streamed as-is)
* No ICY / metadata / track titles

---

## Build

```sh
go build -o spartan-radio
```

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
| `-bitrate-kbps` | `128`       | Approximate stream bitrate (throttling only)        |
| `-playlist`     | `somefile.txt` | Playlist       |
| `-shuffle`      |  | Shuffle the playlist       |

---

## Format modes

### Now Ogg only

```sh
./spartan-radio -format ogg
```

`-format` preserved for compatibility. Maybe removed later.

* MIME: `audio/ogg`

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
│   └── song.ogg
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
ffprobe music/radio.ogg 2>&1"
```

It'll give us something like:

```
Input #0, ogg, from '/amp/sounds/recordings/performances/2025-12-13-anonradio/REC002.ogg':
  Duration: 01:04:57.24, start: 0.000000, bitrate: 444 kb/s
  Stream #0:0: Audio: vorbis, 44100 Hz, stereo, fltp, 499 kb/s
      Metadata:
        encoder         : Lavc61.19.101 libvorbis
        artist          : inky from the tape
        title           : 2025-12-13-anonradio
        genre           : Electronic

```


```sh
./spartan-waves -format ogg -playlist playlist_ogg.txt -bitrate-kbps 499 -host radio.norayr.am -shuffle
```

Then open in a Spartan-capable client:

```text
spartan://radio.norayr.am/radio
```

With the player, do:

```
./swp -host radio.norayr.am -path /radio -player mpv
```

However, instead of player you can just do:

for mp3 stream

```
echo 'radio.norayr.am /radio 0' |nc norayr.am 300 |sox -tmp3 - -d
```

for ogg stream:
```
echo 'radio.norayr.am /radio 0' | nc norayr.am 300 | sox -V0 -togg  -  -d
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

