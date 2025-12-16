package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type Subscriber chan []byte

type Broadcaster struct {
	subs      map[Subscriber]bool
	addSub    chan Subscriber
	removeSub chan Subscriber
	broadcast chan []byte
	subCount  int64 // atomic
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		subs:      make(map[Subscriber]bool),
		addSub:    make(chan Subscriber),
		removeSub: make(chan Subscriber),
		broadcast: make(chan []byte, 4096),
	}
}

func (b *Broadcaster) Count() int64 {
	return atomic.LoadInt64(&b.subCount)
}

func (b *Broadcaster) Run() {
	for {
		select {
		case sub := <-b.addSub:
			b.subs[sub] = true
			atomic.AddInt64(&b.subCount, 1)

		case sub := <-b.removeSub:
			if _, ok := b.subs[sub]; ok {
				delete(b.subs, sub)
				close(sub)
				atomic.AddInt64(&b.subCount, -1)
			}

		case frame := <-b.broadcast:
			for sub := range b.subs {
				select {
				case sub <- frame:
				default:
					// Slow / stuck client: drop it
					delete(b.subs, sub)
					close(sub)
					atomic.AddInt64(&b.subCount, -1)
				}
			}
		}
	}
}

type throttle struct {
	targetBps int64
	start     time.Time
	written   int64
}

func newThrottle(kbps int) *throttle {
	if kbps <= 0 {
		kbps = 128
	}
	return &throttle{
		targetBps: int64(kbps) * 1000 / 8,
		start:     time.Now(),
	}
}

func (t *throttle) Pace(n int) {
	t.written += int64(n)
	elapsed := time.Since(t.start)
	if elapsed <= 0 {
		return
	}
	should := time.Duration(float64(t.written)/float64(t.targetBps)) * time.Second
	if should > elapsed {
		time.Sleep(should - elapsed)
	}
}

func resolveRoot(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path) // music dir itself may be a symlink
	if err != nil {
		return "", err
	}
	return filepath.Abs(real)
}

func allowedExts(format string) map[string]bool {
	switch strings.ToLower(format) {
	case "mp3":
		return map[string]bool{".mp3": true}
	case "ogg":
		return map[string]bool{".ogg": true, ".oga": true}
	case "both":
		return map[string]bool{".mp3": true, ".ogg": true, ".oga": true}
	default:
		return map[string]bool{".mp3": true}
	}
}

// Recursively walks root. Follows symlinked dirs too, but avoids cycles by tracking
// resolved real paths of visited directories.
func buildPlaylistRecursive(root string, exts map[string]bool) ([]string, error) {
	root = filepath.Clean(root)

	seenDirs := map[string]bool{}
	var out []string

	var walk func(dir string) error
	walk = func(dir string) error {
		realDir, err := filepath.EvalSymlinks(dir)
		if err == nil {
			if abs, e := filepath.Abs(realDir); e == nil {
				realDir = abs
			}
			if seenDirs[realDir] {
				return nil
			}
			seenDirs[realDir] = true
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}

		for _, e := range entries {
			full := filepath.Join(dir, e.Name())

			info, err := e.Info()
			if err != nil {
				continue
			}

			// Handle symlink entries by stat()'ing the target
			if info.Mode()&os.ModeSymlink != 0 {
				tinfo, err := os.Stat(full)
				if err != nil {
					continue
				}
				if tinfo.IsDir() {
					_ = walk(full)
					continue
				}
				ext := strings.ToLower(filepath.Ext(e.Name()))
				if exts[ext] {
					out = append(out, full)
				}
				continue
			}

			if info.IsDir() {
				_ = walk(full)
				continue
			}

			ext := strings.ToLower(filepath.Ext(e.Name()))
			if exts[ext] {
				out = append(out, full)
			}
		}

		return nil
	}

	_ = walk(root)

	sort.Strings(out)
	return out, nil
}

// Playlist file: one path per line. Empty lines and lines starting with # are ignored.
// Relative paths are resolved relative to the playlist file directory.
func readPlaylistFile(plPath string) ([]string, error) {
	f, err := os.Open(plPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	baseDir := filepath.Dir(plPath)

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !filepath.IsAbs(line) {
			line = filepath.Join(baseDir, line)
		}
		line = filepath.Clean(line)
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func maybeShuffle(files []string, shuffle bool) {
	if !shuffle || len(files) <= 1 {
		return
	}
	rand.Shuffle(len(files), func(i, j int) { files[i], files[j] = files[j], files[i] })
}

// Streams files forever. Playlist is rebuilt each cycle to pick up changes.
func streamForever(root string, exts map[string]bool, playlistPath string, shuffle bool, b *Broadcaster, bitrateKbps int, rescanDelay time.Duration) {
	for {
		var files []string
		var err error

		if playlistPath != "" {
			files, err = readPlaylistFile(playlistPath)
		} else {
			files, err = buildPlaylistRecursive(root, exts)
		}

		if err != nil {
			log.Printf("playlist error: %v", err)
			time.Sleep(rescanDelay)
			continue
		}
		if len(files) == 0 {
			time.Sleep(rescanDelay)
			continue
		}

		maybeShuffle(files, shuffle)

		for _, fpath := range files {
			log.Printf("Now playing: %s", fpath)

			f, err := os.Open(fpath)
			if err != nil {
				log.Printf("open failed: %v", err)
				continue
			}

			br := bufio.NewReaderSize(f, 64*1024)
			th := newThrottle(bitrateKbps)
			buf := make([]byte, 16*1024)

			for {
				n, err := br.Read(buf)
				if n > 0 {
					chunk := make([]byte, n)
					copy(chunk, buf[:n])
					b.broadcast <- chunk
					th.Pace(n)
				}
				if err != nil {
					if err != io.EOF {
						log.Printf("read error: %v", err)
					}
					break
				}
			}

			_ = f.Close()
		}
	}
}

func handleRadio(conn net.Conn, b *Broadcaster, mime string) {
	remote := conn.RemoteAddr().String()
	log.Printf("RADIO CONNECT from %s (listeners=%d)", remote, b.Count()+1)

	defer func() {
		// Note: listener count decremented via broadcaster removeSub,
		// so log after we request removal.
		log.Printf("RADIO DISCONNECT from %s (listeners=%d)", remote, b.Count()-1)
		_ = conn.Close()
	}()

	_, err := fmt.Fprintf(conn, "2 %s\r\n", mime)
	if err != nil {
		return
	}

	sub := make(Subscriber, 256)
	b.addSub <- sub
	defer func() { b.removeSub <- sub }()

	for chunk := range sub {
		_, err := conn.Write(chunk)
		if err != nil {
			return
		}
	}
}

func handleRequest(conn net.Conn, b *Broadcaster, host string, port int, mime string) {
	reader := bufio.NewReader(conn)

	line, err := reader.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return
	}
	line = strings.TrimRight(line, "\r\n")

	parts := strings.Split(line, " ")
	if len(parts) != 3 {
		fmt.Fprintf(conn, "4 malformed request line\r\n")
		_ = conn.Close()
		return
	}

	path := parts[1]
	lenStr := parts[2]

	contentLen, err := strconv.Atoi(lenStr)
	if err != nil || contentLen < 0 {
		fmt.Fprintf(conn, "4 invalid content-length\r\n")
		_ = conn.Close()
		return
	}

	if contentLen > 0 {
		_, err = io.CopyN(io.Discard, reader, int64(contentLen))
		if err != nil {
			fmt.Fprintf(conn, "5 error reading request body\r\n")
			_ = conn.Close()
			return
		}
	}

	switch path {
	case "/", "/index.gmi", "/index.txt":
		base := fmt.Sprintf("spartan://%s:%d", host, port)
		body := "Spartan Radio\n\n" +
			"=> " + base + "/radio Tune in\n"
		fmt.Fprintf(conn, "2 text/gemini; charset=utf-8\r\n%s", body)
		_ = conn.Close()

	case "/radio":
		// handleRadio will close conn
		handleRadio(conn, b, mime)

	default:
		fmt.Fprintf(conn, "4 not found\r\n")
		_ = conn.Close()
	}
}

func main() {
	rand.Seed(time.Now().UnixNano())

	musicDirFlag := flag.String("music-dir", "./music", "directory with audio files (can be a symlink)")
	playlistPath := flag.String("playlist", "", "path to playlist file (one file path per line); if set, music-dir scanning is not used")
	buildPlaylist := flag.Bool("build-playlist", false, "print playlist (based on -music-dir/-format) to stdout and exit")
	shuffle := flag.Bool("shuffle", false, "shuffle playback order each cycle")
	port := flag.Int("port", 300, "TCP port to listen on (Spartan default is 300)")
	host := flag.String("host", "localhost", "host name to advertise in index (spartan://HOST:PORT/...)")
	format := flag.String("format", "mp3", "mp3|ogg|both (controls which files are in the playlist)")
	bitrateKbps := flag.Int("bitrate-kbps", 128, "approx stream bitrate throttle (kbps)")
	rescan := flag.Duration("rescan", 10*time.Second, "delay when playlist is empty or rebuild fails")
	mimeOverride := flag.String("mime", "", "override MIME for /radio (advanced)")
	logListenersEvery := flag.Duration("log-listeners", 0, "if >0, periodically log current listener count (e.g. 30s, 5m)")
	flag.Parse()

	if *bitrateKbps <= 0 {
		log.Fatalf("-bitrate-kbps must be > 0")
	}

	root, err := resolveRoot(*musicDirFlag)
	if err != nil {
		log.Fatalf("failed to resolve music-dir %q: %v", *musicDirFlag, err)
	}

	f := strings.ToLower(*format)
	if f != "mp3" && f != "ogg" && f != "both" {
		log.Fatalf("invalid -format=%q (use mp3|ogg|both)", *format)
	}

	exts := allowedExts(f)

	if *buildPlaylist {
		files, err := buildPlaylistRecursive(root, exts)
		if err != nil {
			log.Fatalf("playlist error: %v", err)
		}
		for _, p := range files {
			fmt.Println(p)
		}
		return
	}

	mime := ""
	if *mimeOverride != "" {
		mime = *mimeOverride
	} else {
		switch f {
		case "mp3":
			mime = "audio/mpeg"
		case "ogg":
			mime = "audio/ogg"
		case "both":
			// Mixing mp3+ogg in one stream means one fixed MIME can't be correct.
			// This works only if the player sniffs the format from bytes.
			mime = "application/octet-stream"
			log.Printf("WARNING: -format=both mixes MP3+Ogg; using MIME %q for /radio", mime)
		}
	}

	b := NewBroadcaster()
	go b.Run()

	if *logListenersEvery > 0 {
		go func() {
			t := time.NewTicker(*logListenersEvery)
			defer t.Stop()
			for range t.C {
				log.Printf("listeners=%d", b.Count())
			}
		}()
	}

	go streamForever(root, exts, *playlistPath, *shuffle, b, *bitrateKbps, *rescan)

	addr := fmt.Sprintf(":%d", *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", addr, err)
	}
	log.Printf("Spartan Radio listening on spartan://%s:%d/", *host, *port)
	log.Printf("Serving from (resolved): %s", root)
	log.Printf("Format: %s (exts=%v), MIME: %s", f, exts, mime)
	if *playlistPath != "" {
		log.Printf("Playlist: %s (shuffle=%v)", *playlistPath, *shuffle)
	} else {
		log.Printf("Playlist: scanned from -music-dir (shuffle=%v)", *shuffle)
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}
		go handleRequest(conn, b, *host, *port, mime)
	}
}
