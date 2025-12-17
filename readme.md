# Spartan Radio

A minimal **Spartan protocol** audio streamer written in Go.

It serves a **single live radio stream** at `/radio`, streaming audio files from a directory in real time.

It can play from a directory, or by using a playlist. It can also shuffle the playlist.

At first we were supporting MP3's because MP3's can be played from the middle of the byte stream. However, since Lagrange Gemini browser doesn't support MP3 streaming on IOS and Android (it works perfectly fine on Maemo-Leste with mobile UI), we deprecated MP3 streams and added more complex code to stream Ogg files.

Now we run ffmpeg with `-source wav` or `-source ogg`, ffmpeg encodes one ogg stream out of number of files, and our radio server broadcasts it.
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

## Build playlist

```
./build_playlist.sh ./music wav > playlist.txt
```

## Format modes

### For .wav sources

```
./spartan-waves -source wav -music-dir ./music -shuffle -playlist playlist.txt -host radio.norayr.am
```

### For .ogg sources

```
./spartan-waves -source ogg -music-dir ./music -shuffle -playlist playlist.txt -host radio.norayr.am
```
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


## License

GPL-3

---

