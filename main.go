package main

import (
  "bufio"
  "bytes"
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
  hmu    sync.RWMutex
  header []byte
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

type throttle struct {
  targetBps int64
  start     time.Time
  written   int64
}

func newThrottle(kbps int) *throttle {
  // For “natural speed”, you must pace output.
  // Use ffprobe's audio bitrate (~499 for your Vorbis file).
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

func resolveExistingFile(p string, baseDir string) (string, bool) {
  // Expand relative paths relative to playlist file directory, if provided.
  if !filepath.IsAbs(p) && baseDir != "" {
    p = filepath.Join(baseDir, p)
  }
  // Clean + try eval symlinks to reduce duplicates
  p = filepath.Clean(p)

  // Stat (following symlink)
  if st, err := os.Stat(p); err == nil && !st.IsDir() {
    if abs, err := filepath.Abs(p); err == nil {
      p = abs
    }
    return p, true
  }
  return "", false
}

func readPlaylistFile(listPath string) ([]string, error) {
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
    // allow comments
    if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
      continue
    }
    // accept leading "./"
    line = strings.TrimPrefix(line, "\uFEFF") // strip BOM if any

    p, ok := resolveExistingFile(line, baseDir)
    if !ok {
      log.Printf("playlist: skipping missing file: %s", line)
      continue
    }
    ext := strings.ToLower(filepath.Ext(p))
    if ext != ".ogg" && ext != ".oga" {
      log.Printf("playlist: skipping non-ogg file: %s", p)
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
func buildOggListFromDir(root string) ([]string, error) {
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
    _, _ = r.ReadByte()
  }

  // Fixed header is 27 bytes
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

  // If not a continuation at page start, reset packet buffer.
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
  if len(pkt) >= 7 &&
    (pkt[0] == 0x01 || pkt[0] == 0x03 || pkt[0] == 0x05) &&
    bytes.Equal(pkt[1:7], []byte("vorbis")) {
    vh.gotPackets++
  }
}

func (vh *vorbisHeaderFinder) done() bool { return vh.gotPackets >= 3 }

// --- Streaming ---------------------------------------------------------------

func streamOgg(root string, playlistPath string, shuffle bool, b *Broadcaster, bitrateKbps int, rescanDelay time.Duration) {
  rng := rand.New(rand.NewSource(time.Now().UnixNano()))

  loadList := func() ([]string, error) {
    if playlistPath != "" {
      return readPlaylistFile(playlistPath)
    }
    return buildOggListFromDir(root)
  }

  for {
    files, err := loadList()
    if err != nil {
      log.Printf("playlist load error: %v", err)
      time.Sleep(rescanDelay)
      continue
    }
    if len(files) == 0 {
      time.Sleep(rescanDelay)
      continue
    }

    if shuffle {
      rng.Shuffle(len(files), func(i, j int) { files[i], files[j] = files[j], files[i] })
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

      // Pattern A: cache Vorbis headers as Ogg pages, and broadcast them too.
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

        b.broadcast <- page
        th.Pace(len(page))
      }

      if headerBuf.Len() > 0 && vh.done() {
        b.SetHeader(headerBuf.Bytes())
      } else {
        log.Printf("WARNING: could not confirm Vorbis headers for %s (late join may fail)", fpath)
      }

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
    // loop forever; reload list each cycle (so your playlist edits take effect)
  }
}

func handleRadio(conn net.Conn, b *Broadcaster) {
  defer conn.Close()

  if _, err := fmt.Fprintf(conn, "2 audio/ogg\r\n"); err != nil {
    return
  }

  // Pattern A: send cached Vorbis headers first for late joiners.
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
      "=> " + base + "/radio Tune in\n"
    fmt.Fprintf(conn, "2 text/gemini; charset=utf-8\r\n%s", body)

  case "/radio":
    handleRadio(conn, b)

  default:
    fmt.Fprintf(conn, "4 not found\r\n")
  }
}

func main() {
  musicDirFlag := flag.String("music-dir", "./music", "directory with .ogg/.oga files (can be a symlink)")
  playlistFlag := flag.String("playlist", "", "path to playlist text file (one path per line). If set, music-dir scanning is not used.")
  shuffleFlag := flag.Bool("shuffle", false, "shuffle playlist each cycle")
  port := flag.Int("port", 300, "TCP port to listen on (Spartan default is 300)")
  host := flag.String("host", "localhost", "host name to advertise in index (spartan://HOST:PORT/...)")

  // Kept for compatibility with your earlier CLI, but this build is ogg/vorbis only.
  format := flag.String("format", "ogg", "format to stream (only 'ogg' supported in this build)")
  bitrateKbps := flag.Int("bitrate-kbps", 192, "target stream bitrate (kbps). Use ffprobe audio bitrate (e.g. ~499)")
  rescan := flag.Duration("rescan", 10*time.Second, "delay when playlist is empty or rebuild fails")

  flag.Parse()

  if strings.ToLower(*format) != "ogg" {
    log.Fatalf("this build supports only -format ogg (Pattern A Vorbis headers). got: %q", *format)
  }
  if *bitrateKbps <= 0 {
    log.Fatalf("-bitrate-kbps must be > 0 (use ffprobe audio bitrate, e.g. ~499)")
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
  go streamOgg(root, *playlistFlag, *shuffleFlag, b, *bitrateKbps, *rescan)

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
  log.Printf("Mode: ogg/vorbis (Pattern A headers). shuffle=%v bitrate-kbps=%d", *shuffleFlag, *bitrateKbps)

  for {
    conn, err := ln.Accept()
    if err != nil {
      log.Printf("accept error: %v", err)
      continue
    }
    go handleRequest(conn, b, *host, *port)
  }
}

