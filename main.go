package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Subscriber chan []byte

type Broadcaster struct {
	subs      map[Subscriber]bool
	addSub    chan Subscriber
	removeSub chan Subscriber
	broadcast chan []byte

	// Cached Ogg/Vorbis headers (as raw Ogg pages bytes). Sent to new subscribers
	// so they can decode even if they join mid-stream.
	hmu    sync.RWMutex
	header []byte
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		subs:      make(map[Subscriber]bool),
		addSub:    make(chan Subscriber),
		removeSub: make(chan Subscriber),
		broadcast: make(chan []byte, 4096),
	}
}

func (b *Broadcaster) Run() {
	for {
		select {
		case sub := <-b.addSub:
			b.subs[sub] = true

		case sub := <-b.removeSub:
			if _, ok := b.subs[sub]; ok {
				delete(b.subs, sub)
				close(sub)
			}

		case frame := <-b.broadcast:
			for sub := range b.subs {
				select {
				case sub <- frame:
				default:
					delete(b.subs, sub)
					close(sub)
				}
			}
		}
	}
}

func (b *Broadcaster) SetHeader(h []byte) {
	b.hmu.Lock()
	b.header = h
	b.hmu.Unlock()
}

func (b *Broadcaster) GetHeaderCopy() []byte {
	b.hmu.RLock()
	defer b.hmu.RUnlock()
	if len(b.header) == 0 {
		return nil
	}
	out := make([]byte, len(b.header))
	copy(out, b.header)
	return out
}

type throttle struct {
	targetBps int64
	start     time.Time
	written   int64
}

func newThrottle(kbps int) *throttle {
	// We want "natural playback speed", so require a positive bitrate.
	// Use the audio bitrate from ffprobe output (e.g., ~499 for your Vorbis file).
	if kbps <= 0 {
		kbps = 192
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

func buildPlaylistRecursive(root string) ([]string, error) {
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
				if ext == ".ogg" || ext == ".oga" {
					out = append(out, full)
				}
				continue
			}

			if info.IsDir() {
				_ = walk(full)
				continue
			}

			ext := strings.ToLower(filepath.Ext(e.Name()))
			if ext == ".ogg" || ext == ".oga" {
				out = append(out, full)
			}
		}
		return nil
	}

	_ = walk(root)
	sort.Strings(out)
	return out, nil
}

// --- Ogg page reader ---------------------------------------------------------

// Reads the next Ogg page (starts with "OggS") and returns the full page bytes.
func readNextOggPage(r *bufio.Reader) ([]byte, error) {
	// Find capture pattern "OggS"
	for {
		b, err := r.Peek(4)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(b, []byte("OggS")) {
			break
		}
		// Discard one byte and keep scanning
		_, _ = r.ReadByte()
	}

	// Now read fixed header (27 bytes)
	hdr := make([]byte, 27)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	if !bytes.Equal(hdr[:4], []byte("OggS")) {
		return nil, fmt.Errorf("ogg: lost sync (no OggS)")
	}

	segCount := int(hdr[26])
	segTable := make([]byte, segCount)
	if _, err := io.ReadFull(r, segTable); err != nil {
		return nil, err
	}

	bodyLen := 0
	for _, v := range segTable {
		bodyLen += int(v)
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}

	page := make([]byte, 0, 27+segCount+bodyLen)
	page = append(page, hdr...)
	page = append(page, segTable...)
	page = append(page, body...)
	return page, nil
}

type vorbisHeaderFinder struct {
	gotPackets int
	packetBuf  []byte
}

func (vh *vorbisHeaderFinder) feedPage(page []byte) {
	// Ogg page layout:
	// 0..26 header (27 bytes), [26]=page_segments, then segment table, then body.
	if len(page) < 27 {
		return
	}
	segCount := int(page[26])
	if len(page) < 27+segCount {
		return
	}
	hdrType := page[5]
	segTable := page[27 : 27+segCount]
	body := page[27+segCount:]

	// If continuation flag set, first packet continues from previous page.
	// If not set, we start fresh for packet boundaries at page start.
	if (hdrType & 0x01) == 0 {
		// Not a continuation at page start: packetBuf should be empty for new packets.
		// But if it isn't, drop it (corruption/edge case).
		vh.packetBuf = nil
	}

	offset := 0
	for _, lace := range segTable {
		n := int(lace)
		if offset+n > len(body) {
			return
		}
		vh.packetBuf = append(vh.packetBuf, body[offset:offset+n]...)
		offset += n

		// Packet ends when lacing value < 255
		if lace < 255 {
			vh.checkPacket(vh.packetBuf)
			vh.packetBuf = nil
		}
	}
}

func (vh *vorbisHeaderFinder) checkPacket(pkt []byte) {
	if vh.gotPackets >= 3 {
		return
	}
	// Vorbis header packet: [type][ "vorbis" ...]
	// type is 0x01, 0x03, 0x05 for the three header packets.
	if len(pkt) >= 7 && (pkt[0] == 0x01 || pkt[0] == 0x03 || pkt[0] == 0x05) && bytes.Equal(pkt[1:7], []byte("vorbis")) {
		vh.gotPackets++
	}
}

func (vh *vorbisHeaderFinder) done() bool {
	return vh.gotPackets >= 3
}

// --- Streaming ---------------------------------------------------------------

func streamOggFolder(root string, b *Broadcaster, bitrateKbps int, rescanDelay time.Duration) {
	for {
		files, err := buildPlaylistRecursive(root)
		if err != nil {
			log.Printf("playlist error: %v", err)
			time.Sleep(rescanDelay)
			continue
		}
		if len(files) == 0 {
			time.Sleep(rescanDelay)
			continue
		}

		for _, fpath := range files {
			log.Printf("Now playing: %s", fpath)

			f, err := os.Open(fpath)
			if err != nil {
				log.Printf("open failed: %v", err)
				continue
			}

			r := bufio.NewReaderSize(f, 256*1024)
			th := newThrottle(bitrateKbps)

			// Build and cache Vorbis headers as Ogg pages, Icecast-style.
			// We also broadcast these pages to current listeners.
			var headerBuf bytes.Buffer
			vh := &vorbisHeaderFinder{}

			for !vh.done() {
				page, err := readNextOggPage(r)
				if err != nil {
					if err != io.EOF {
						log.Printf("ogg read error (headers): %v", err)
					}
					break
				}
				vh.feedPage(page)
				headerBuf.Write(page)

				// Broadcast header pages too (listeners already connected should hear from start).
				b.broadcast <- page
				th.Pace(len(page))
			}

			// Publish header cache for late joiners.
			if headerBuf.Len() > 0 && vh.done() {
				b.SetHeader(headerBuf.Bytes())
			} else {
				// If we couldn't extract headers, keep whatever header was previously set.
				log.Printf("WARNING: could not confirm Vorbis headers for %s (late join may fail)", fpath)
			}

			// Stream remaining pages
			for {
				page, err := readNextOggPage(r)
				if err != nil {
					if err != io.EOF {
						log.Printf("ogg read error: %v", err)
					}
					break
				}
				b.broadcast <- page
				th.Pace(len(page))
			}

			_ = f.Close()
		}
	}
}

func handleRadio(conn net.Conn, b *Broadcaster) {
	defer conn.Close()

	// Spartan success + MIME
	if _, err := fmt.Fprintf(conn, "2 audio/ogg\r\n"); err != nil {
		return
	}

	// Icecast-like: send cached headers first, so client can decode even mid-stream.
	if hdr := b.GetHeaderCopy(); len(hdr) > 0 {
		if _, err := conn.Write(hdr); err != nil {
			return
		}
	}

	sub := make(Subscriber, 512)
	b.addSub <- sub
	defer func() { b.removeSub <- sub }()

	for page := range sub {
		if _, err := conn.Write(page); err != nil {
			return
		}
	}
}

func handleRequest(conn net.Conn, b *Broadcaster, host string, port int) {
	defer conn.Close()

	reader := bufio.NewReader(conn)

	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	line = strings.TrimRight(line, "\r\n")

	parts := strings.Split(line, " ")
	if len(parts) != 3 {
		fmt.Fprintf(conn, "4 malformed request line\r\n")
		return
	}

	path := parts[1]
	lenStr := parts[2]

	contentLen, err := strconv.Atoi(lenStr)
	if err != nil || contentLen < 0 {
		fmt.Fprintf(conn, "4 invalid content-length\r\n")
		return
	}

	if contentLen > 0 {
		_, err = io.CopyN(io.Discard, reader, int64(contentLen))
		if err != nil {
			fmt.Fprintf(conn, "5 error reading request body\r\n")
			return
		}
	}

	switch path {
	case "/", "/index.gmi", "/index.txt":
		base := fmt.Sprintf("spartan://%s:%d", host, port)
		body := "Spartan Radio (Ogg Vorbis)\n\n" +
			"=> " + base + "/radio Tune in (audio/ogg)\n"
		fmt.Fprintf(conn, "2 text/gemini; charset=utf-8\r\n%s", body)

	case "/radio":
		handleRadio(conn, b)

	default:
		fmt.Fprintf(conn, "4 not found\r\n")
	}
}

func main() {
	musicDirFlag := flag.String("music-dir", "./music", "directory with .ogg/.oga files (can be a symlink)")
	port := flag.Int("port", 300, "TCP port to listen on (Spartan default is 300)")
	host := flag.String("host", "localhost", "host name to advertise in index (spartan://HOST:PORT/...)")
	bitrateKbps := flag.Int("bitrate-kbps", 192, "target stream bitrate (kbps). Use ffprobe's audio bitrate (e.g. ~499 for your REC002.ogg)")
	rescan := flag.Duration("rescan", 10*time.Second, "delay when playlist is empty or rebuild fails")
	flag.Parse()

	root, err := resolveRoot(*musicDirFlag)
	if err != nil {
		log.Fatalf("failed to resolve music-dir %q: %v", *musicDirFlag, err)
	}

	b := NewBroadcaster()
	go b.Run()
	go streamOggFolder(root, b, *bitrateKbps, *rescan)

	addr := fmt.Sprintf(":%d", *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", addr, err)
	}

	log.Printf("Spartan Radio listening on spartan://%s:%d/", *host, *port)
	log.Printf("Serving from (resolved): %s", root)
	log.Printf("Ogg/Vorbis mode. bitrate-kbps=%d", *bitrateKbps)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}
		go handleRequest(conn, b, *host, *port)
	}
}
