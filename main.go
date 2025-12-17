package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"os/exec"
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

	// Cached Ogg/Vorbis headers as raw Ogg pages bytes.
	hmu     sync.RWMutex
	header  []byte
	subCount int
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
			b.subCount++
			log.Printf("Listeners: %d", b.subCount)

		case sub := <-b.removeSub:
			if _, ok := b.subs[sub]; ok {
				delete(b.subs, sub)
				close(sub)
				b.subCount--
				log.Printf("Listeners: %d", b.subCount)
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

func resolveRoot(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path) // music dir itself may be a symlink
	if err != nil {
		return "", err
	}
	return filepath.Abs(real)
}

func resolveExistingFile(p string, baseDir string) (string, bool) {
	if !filepath.IsAbs(p) && baseDir != "" {
		p = filepath.Join(baseDir, p)
	}
	p = filepath.Clean(p)

	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		return p, true
	}
	return "", false
}

func allowedExtsForSource(source string) map[string]bool {
	switch strings.ToLower(source) {
	case "wav":
		return map[string]bool{".wav": true, ".wave": true}
	case "ogg":
		return map[string]bool{".ogg": true, ".oga": true}
	default:
		return map[string]bool{".wav": true, ".wave": true}
	}
}

func readPlaylistFile(listPath string, exts map[string]bool) ([]string, error) {
	f, err := os.Open(listPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	baseDir := filepath.Dir(listPath)

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		line = strings.TrimPrefix(line, "\uFEFF") // strip BOM if any

		p, ok := resolveExistingFile(line, baseDir)
		if !ok {
			log.Printf("playlist: skipping missing file: %s", line)
			continue
		}
		ext := strings.ToLower(filepath.Ext(p))
		if !exts[ext] {
			log.Printf("playlist: skipping non-matching file: %s", p)
			continue
		}
		out = append(out, p)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Recursively walks root. Follows symlinked dirs too, but avoids cycles by tracking
// resolved real paths of visited directories.
func buildListFromDir(root string, exts map[string]bool) ([]string, error) {
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

// --- Ogg page reader ---------------------------------------------------------

// Reads the next Ogg page (starts with "OggS") and returns the full page bytes.
func readNextOggPage(r *bufio.Reader) ([]byte, error) {
	for {
		b, err := r.Peek(4)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(b, []byte("OggS")) {
			break
		}
		_, _ = r.ReadByte()
	}

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

// Collects enough Ogg pages to include the 3 Vorbis header packets.
type vorbisHeaderFinder struct {
	gotPackets int
	packetBuf  []byte
}

func (vh *vorbisHeaderFinder) feedPage(page []byte) {
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

	if (hdrType & 0x01) == 0 {
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
	if len(pkt) >= 7 &&
		(pkt[0] == 0x01 || pkt[0] == 0x03 || pkt[0] == 0x05) &&
		bytes.Equal(pkt[1:7], []byte("vorbis")) {
		vh.gotPackets++
	}
}

func (vh *vorbisHeaderFinder) done() bool { return vh.gotPackets >= 3 }

// --- ffmpeg streaming --------------------------------------------------------

// Write ffmpeg concat file:
// file '/abs/path/one.wav'
func writeFFmpegConcatFile(paths []string) (string, error) {
	f, err := os.CreateTemp("", "sprout-waves-concat-*.txt")
	if err != nil {
		return "", err
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	for _, p := range paths {
		// Escape single quotes for ffmpeg concat syntax
		esc := strings.ReplaceAll(p, "'", `'\''`)
		if _, err := fmt.Fprintf(w, "file '%s'\n", esc); err != nil {
			return "", err
		}
	}
	if err := w.Flush(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

type ffmpegRunConfig struct {
	ffmpegPath   string
	source       string // wav|ogg
	playlistPath string
	root         string
	shuffle      bool
	rescanDelay  time.Duration

	// Encoding controls for the *output* Vorbis stream
	vorbisQ     int // used if bitrateKbps <= 0
	bitrateKbps int // if >0, use -b:a N k
}

func buildInputList(cfg ffmpegRunConfig, rng *rand.Rand) ([]string, error) {
	exts := allowedExtsForSource(cfg.source)

	var files []string
	var err error
	if cfg.playlistPath != "" {
		files, err = readPlaylistFile(cfg.playlistPath, exts)
	} else {
		files, err = buildListFromDir(cfg.root, exts)
	}
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}

	if cfg.shuffle {
		rng.Shuffle(len(files), func(i, j int) { files[i], files[j] = files[j], files[i] })
	}

	return files, nil
}

func buildFFmpegArgs(concatPath string, bitrateKbps int, vorbisQ int) []string {
	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-re",
		"-f", "concat",
		"-safe", "0",
		"-i", concatPath,
		"-vn",
		"-c:a", "libvorbis",
	}
	if bitrateKbps > 0 {
		args = append(args, "-b:a", fmt.Sprintf("%dk", bitrateKbps))
	} else {
		// Quality mode (VBR)
		if vorbisQ < 0 {
			vorbisQ = 4
		}
		args = append(args, "-q:a", fmt.Sprintf("%d", vorbisQ))
	}
	args = append(args, "-f", "ogg", "pipe:1")
	return args
}

func runFFmpegPump(cfg ffmpegRunConfig, b *Broadcaster) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	for {
		files, err := buildInputList(cfg, rng)
		if err != nil {
			log.Printf("playlist load error: %v", err)
			time.Sleep(cfg.rescanDelay)
			continue
		}
		if len(files) == 0 {
			time.Sleep(cfg.rescanDelay)
			continue
		}

		// Log first few items for sanity (don’t spam huge lists).
		log.Printf("Starting ffmpeg (%s source). Items: %d", cfg.source, len(files))
		if len(files) > 0 {
			log.Printf("Now playing (ffmpeg input): %s", files[0])
		}

		concatPath, err := writeFFmpegConcatFile(files)
		if err != nil {
			log.Printf("concat file error: %v", err)
			time.Sleep(cfg.rescanDelay)
			continue
		}

		args := buildFFmpegArgs(concatPath, cfg.bitrateKbps, cfg.vorbisQ)
		cmd := exec.Command(cfg.ffmpegPath, args...)
		cmd.Stderr = os.Stderr

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			_ = os.Remove(concatPath)
			log.Printf("ffmpeg stdout pipe error: %v", err)
			time.Sleep(cfg.rescanDelay)
			continue
		}

		if err := cmd.Start(); err != nil {
			_ = os.Remove(concatPath)
			log.Printf("ffmpeg start error: %v", err)
			time.Sleep(cfg.rescanDelay)
			continue
		}

		// We can remove the concat file after start; ffmpeg already opened it.
		_ = os.Remove(concatPath)

		// Read Ogg pages from ffmpeg stdout and broadcast them.
		br := bufio.NewReaderSize(stdout, 256*1024)
		vh := &vorbisHeaderFinder{}
		var headerBuf bytes.Buffer
		headerSet := false

		for {
			page, err := readNextOggPage(br)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					log.Printf("ffmpeg stream read error: %v", err)
				}
				break
			}

			// Cache the initial Vorbis header pages once per ffmpeg run (late-join support).
			if !headerSet {
				vh.feedPage(page)
				headerBuf.Write(page)
				if vh.done() {
					b.SetHeader(headerBuf.Bytes())
					headerSet = true
					log.Printf("Cached Vorbis headers: %d bytes", headerBuf.Len())
				}
			}

			b.broadcast <- page
		}

		// Ensure ffmpeg process is reaped.
		_ = cmd.Wait()
		log.Printf("ffmpeg exited; restarting after %s", cfg.rescanDelay)
		time.Sleep(cfg.rescanDelay)
	}
}

// --- Spartan handlers --------------------------------------------------------

func handleRadio(conn net.Conn, b *Broadcaster) {
	remote := conn.RemoteAddr().String()
	log.Printf("Listener connected: %s", remote)
	defer func() {
		log.Printf("Listener disconnected: %s", remote)
		_ = conn.Close()
	}()

	if _, err := fmt.Fprintf(conn, "2 audio/ogg\r\n"); err != nil {
		return
	}

	// Send cached Vorbis headers first (late join can decode).
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
		body := "Spartan Radio (Vorbis over Spartan)\n\n" +
			"=> " + base + "/radio Tune in\n"
		fmt.Fprintf(conn, "2 text/gemini; charset=utf-8\r\n%s", body)

	case "/radio":
		handleRadio(conn, b)

	default:
		fmt.Fprintf(conn, "4 not found\r\n")
	}
}

func main() {
	musicDirFlag := flag.String("music-dir", "./music", "directory with audio files (can be a symlink)")
	playlistFlag := flag.String("playlist", "", "path to playlist text file (one path per line). If set, music-dir scanning is not used.")
	shuffleFlag := flag.Bool("shuffle", false, "shuffle playlist each cycle")

	sourceFlag := flag.String("source", "ogg", "input source type: ogg|wav (output is always Vorbis Ogg stream)")
	ffmpegFlag := flag.String("ffmpeg", "ffmpeg", "path to ffmpeg binary")

	port := flag.Int("port", 300, "TCP port to listen on (Spartan default is 300)")
	host := flag.String("host", "localhost", "host name to advertise in index (spartan://HOST:PORT/...)")

	// Keep bitrate-kbps as a useful knob: it becomes ffmpeg output bitrate target.
	// (If you prefer VBR quality mode, set -bitrate-kbps 0 and use -vorbis-q.)
	bitrateKbps := flag.Int("bitrate-kbps", 192, "output Vorbis target bitrate kbps (ffmpeg -b:a). Set 0 to use -vorbis-q instead.")
	vorbisQ := flag.Int("vorbis-q", 4, "output Vorbis quality (ffmpeg -q:a), used when -bitrate-kbps=0")
	rescan := flag.Duration("rescan", 10*time.Second, "delay when playlist is empty or rebuild fails / ffmpeg restarts")

	flag.Parse()

	src := strings.ToLower(*sourceFlag)
	if src != "ogg" && src != "wav" {
		log.Fatalf("invalid -source=%q (use ogg|wav)", *sourceFlag)
	}

	root := ""
	var err error
	if *playlistFlag == "" {
		root, err = resolveRoot(*musicDirFlag)
		if err != nil {
			log.Fatalf("failed to resolve music-dir %q: %v", *musicDirFlag, err)
		}
	} else {
		// Resolve playlist path to absolute for stable base dir resolution.
		if abs, e := filepath.Abs(*playlistFlag); e == nil {
			*playlistFlag = abs
		}
	}

	b := NewBroadcaster()
	go b.Run()

	// Start continuous ffmpeg → broadcaster pump
	cfg := ffmpegRunConfig{
		ffmpegPath:   *ffmpegFlag,
		source:       src,
		playlistPath: *playlistFlag,
		root:         root,
		shuffle:      *shuffleFlag,
		rescanDelay:  *rescan,
		vorbisQ:      *vorbisQ,
		bitrateKbps:  *bitrateKbps,
	}
	go runFFmpegPump(cfg, b)

	addr := fmt.Sprintf(":%d", *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", addr, err)
	}

	log.Printf("Spartan Radio listening on spartan://%s:%d/", *host, *port)
	if *playlistFlag != "" {
		log.Printf("Playlist file: %s", *playlistFlag)
	} else {
		log.Printf("Serving from (resolved): %s", root)
	}
	log.Printf("Input source: %s, shuffle=%v, ffmpeg=%s", src, *shuffleFlag, *ffmpegFlag)
	if *bitrateKbps > 0 {
		log.Printf("Output: audio/ogg (vorbis), bitrate-kbps=%d", *bitrateKbps)
	} else {
		log.Printf("Output: audio/ogg (vorbis), vorbis-q=%d", *vorbisQ)
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}
		go handleRequest(conn, b, *host, *port)
	}
}
