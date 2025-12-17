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

  // Cached Ogg/Vorbis headers as raw Ogg pages bytes (Pattern A).
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

// ---------------- playlist / scanning ----------------

func resolveRoot(path string) (string, error) {
  real, err := filepath.EvalSymlinks(path) // music dir itself may be a symlink
  if err != nil {
    return "", err
  }
  return filepath.Abs(real)
}

func parsePlaylistLine(line string) string {
  line = strings.TrimSpace(line)
  line = strings.TrimPrefix(line, "\uFEFF")
  if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
    return ""
  }

  // Accept ffmpeg concat format: file 'path'
  if strings.HasPrefix(line, "file ") {
    rest := strings.TrimSpace(strings.TrimPrefix(line, "file"))
    rest = strings.TrimSpace(rest)
    if len(rest) >= 2 && ((rest[0] == '\'' && rest[len(rest)-1] == '\'') || (rest[0] == '"' && rest[len(rest)-1] == '"')) {
      rest = rest[1 : len(rest)-1]
    }
    // Undo common ffmpeg concat single-quote escape
    rest = strings.ReplaceAll(rest, `'\''`, `'`)
    return rest
  }

  return line
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

func wavExts() map[string]bool {
  return map[string]bool{
    ".wav":  true,
    ".wave": true,
  }
}

func readPlaylistFile(listPath string) ([]string, error) {
  f, err := os.Open(listPath)
  if err != nil {
    return nil, err
  }
  defer f.Close()

  baseDir := filepath.Dir(listPath)
  exts := wavExts()

  var out []string
  sc := bufio.NewScanner(f)
  for sc.Scan() {
    line := parsePlaylistLine(sc.Text())
    if line == "" {
      continue
    }

    p, ok := resolveExistingFile(line, baseDir)
    if !ok {
      log.Printf("playlist: skipping missing file: %s", line)
      continue
    }
    ext := strings.ToLower(filepath.Ext(p))
    if !exts[ext] {
      log.Printf("playlist: skipping non-wav file: %s", p)
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
func buildWavListFromDir(root string) ([]string, error) {
  root = filepath.Clean(root)
  exts := wavExts()

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

// ---------------- Ogg parsing for broadcasting + header cache ----------------

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
  // Vorbis header packet: [type]["vorbis"...]
  if len(pkt) >= 7 &&
    (pkt[0] == 0x01 || pkt[0] == 0x03 || pkt[0] == 0x05) &&
    bytes.Equal(pkt[1:7], []byte("vorbis")) {
    vh.gotPackets++
  }
}

func (vh *vorbisHeaderFinder) done() bool { return vh.gotPackets >= 3 }

// ---------------- ffmpeg encoder (single process) ----------------

type encoderConfig struct {
  ffmpegPath  string
  bitrateKbps int
  vorbisQ     int
  streamName  string
}

func startEncoder(cfg encoderConfig) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
  args := []string{
    "-hide_banner",
    "-loglevel", "warning",

    // Continuous input is concatenated WAVs on stdin.
    "-f", "s16le",
    "-ar", "44100",
    "-ac", "2",
    "-i", "pipe:0",
    "-vn",
    "-c:a", "libvorbis",
  }

  if cfg.bitrateKbps > 0 {
    args = append(args, "-b:a", fmt.Sprintf("%dk", cfg.bitrateKbps))
  } else {
    args = append(args, "-q:a", fmt.Sprintf("%d", cfg.vorbisQ))
  }

  // Constant stream metadata (Vorbis comments in header)
  if cfg.streamName != "" {
    args = append(args, "-metadata", fmt.Sprintf("title=%s", cfg.streamName))
  }

  args = append(args,
    "-f", "ogg",
    "pipe:1",
  )

  cmd := exec.Command(cfg.ffmpegPath, args...)
  cmd.Stderr = os.Stderr

  stdin, err := cmd.StdinPipe()
  if err != nil {
    return nil, nil, nil, err
  }
  stdout, err := cmd.StdoutPipe()
  if err != nil {
    return nil, nil, nil, err
  }

  if err := cmd.Start(); err != nil {
    return nil, nil, nil, err
  }
  return cmd, stdin, stdout, nil
}

func decodeWavToPCMAndWrite(ffmpegPath string, wavPath string, encStdin io.Writer) error {
  // Decode/resample to a stable PCM format that matches the encoder input.
  cmd := exec.Command(ffmpegPath,
    "-hide_banner", "-loglevel", "warning",
    // optional: pace decoding in realtime; helps “radio” feel
    "-re",
    "-i", wavPath,
    "-f", "s16le",
    "-ar", "44100",
    "-ac", "2",
    "pipe:1",
  )
  cmd.Stderr = os.Stderr
  out, err := cmd.StdoutPipe()
  if err != nil {
    return err
  }
  if err := cmd.Start(); err != nil {
    return err
  }

  _, copyErr := io.Copy(encStdin, out)
  waitErr := cmd.Wait()

  if copyErr != nil {
    return copyErr
  }
  return waitErr
}

// Feeds WAV files into encoder stdin forever (shuffle per cycle if enabled).
// If encoder stdin breaks, returns.
func feedWavForever(ffmpegPath string, stdin io.Writer, loadList func() ([]string, error), shuffle bool, rescanDelay time.Duration) {
  rng := rand.New(rand.NewSource(time.Now().UnixNano()))

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

    for _, p := range files {
      log.Printf("Now playing (wav input): %s", p)
      if err := decodeWavToPCMAndWrite(ffmpegPath, p, stdin); err != nil {
        log.Printf("decode/write failed: %v", err)
        return
      }
    }

    // loop again: rebuild list (so playlist edits take effect), reshuffle if enabled
  }
}

// Reads encoder stdout as Ogg pages, caches Vorbis headers once, broadcasts pages forever.
func broadcastFromEncoder(stdout io.Reader, b *Broadcaster) error {
  br := bufio.NewReaderSize(stdout, 256*1024)

  vh := &vorbisHeaderFinder{}
  var headerBuf bytes.Buffer
  headerSet := false

  for {
    page, err := readNextOggPage(br)
    if err != nil {
      return err
    }

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
}

// ---------------- Spartan handlers ----------------

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

func handleRequest(conn net.Conn, b *Broadcaster, host string, port int, streamName string) {
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
    title := "Spartan Radio (Vorbis over Spartan)"
    if streamName != "" {
      title = streamName
    }
    body := title + "\n\n" +
      "=> " + base + "/radio Tune in\n"
    fmt.Fprintf(conn, "2 text/gemini; charset=utf-8\r\n%s", body)

  case "/radio":
    handleRadio(conn, b)

  default:
    fmt.Fprintf(conn, "4 not found\r\n")
  }
}

func main() {
  musicDirFlag := flag.String("music-dir", "./music", "directory with .wav/.WAV files (can be a symlink)")
  playlistFlag := flag.String("playlist", "", "path to playlist text file (plain paths OR ffmpeg concat format). If set, music-dir scanning is not used.")
  shuffleFlag := flag.Bool("shuffle", false, "shuffle playlist each cycle")

  port := flag.Int("port", 300, "TCP port to listen on (Spartan default is 300)")
  host := flag.String("host", "localhost", "host name to advertise in index (spartan://HOST:PORT/...)")

  ffmpegFlag := flag.String("ffmpeg", "ffmpeg", "path to ffmpeg binary")

  // Output encoding knobs (Vorbis)
  bitrateKbps := flag.Int("bitrate-kbps", 192, "output Vorbis target bitrate kbps (ffmpeg -b:a). Set 0 to use -vorbis-q")
  vorbisQ := flag.Int("vorbis-q", 4, "output Vorbis quality (ffmpeg -q:a), used when -bitrate-kbps=0")

  streamName := flag.String("stream-name", "", "stream title metadata (Vorbis comment) and title shown in /")

  rescan := flag.Duration("rescan", 10*time.Second, "delay when playlist is empty or reload fails")

  flag.Parse()

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

  loadList := func() ([]string, error) {
    if *playlistFlag != "" {
      return readPlaylistFile(*playlistFlag)
    }
    return buildWavListFromDir(root)
  }

  b := NewBroadcaster()
  go b.Run()

  // Start one encoder ffmpeg and never restart it (unless it crashes).
  encCfg := encoderConfig{
    ffmpegPath:  *ffmpegFlag,
    bitrateKbps: *bitrateKbps,
    vorbisQ:     *vorbisQ,
    streamName:  *streamName,
  }

  cmd, stdin, stdout, err := startEncoder(encCfg)
  if err != nil {
    log.Fatalf("failed to start ffmpeg encoder: %v", err)
  }

  // Feed WAVs into encoder stdin forever (in background).
  go feedWavForever(*ffmpegFlag, stdin, loadList, *shuffleFlag, *rescan)

  // Broadcast encoder stdout (in background).
  go func() {
    err := broadcastFromEncoder(stdout, b)
    if err != nil && !errors.Is(err, io.EOF) {
      log.Printf("encoder stdout ended: %v", err)
    }
    // If encoder dies, exit the whole program (better than silently serving dead air).
    _ = cmd.Process.Kill()
    os.Exit(1)
  }()

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
  log.Printf("Output: audio/ogg (vorbis), shuffle=%v, ffmpeg=%s", *shuffleFlag, *ffmpegFlag)
  if *bitrateKbps > 0 {
    log.Printf("Vorbis bitrate: %dk", *bitrateKbps)
  } else {
    log.Printf("Vorbis quality: %d", *vorbisQ)
  }
  if *streamName != "" {
    log.Printf("Stream name: %s", *streamName)
  }

  for {
    conn, err := ln.Accept()
    if err != nil {
      log.Printf("accept error: %v", err)
      continue
    }
    go handleRequest(conn, b, *host, *port, *streamName)
  }
}

