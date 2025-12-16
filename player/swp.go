package main

import (
  "bufio"
  "flag"
  "fmt"
  "io"
  "log"
  "net"
  "os"
  "os/exec"
  "strings"
  "time"
)

func main() {
  host := flag.String("host", "localhost", "Spartan server host")
  port := flag.Int("port", 300, "Spartan server port")
  path := flag.String("path", "/radio", "path to stream (default /radio)")
  player := flag.String("player", "ffplay", "player command (ffplay|mpv|vlc). default: ffplay")
  flag.Parse()

  addr := fmt.Sprintf("%s:%d", *host, *port)
  conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
  if err != nil {
    log.Fatalf("connect failed: %v", err)
  }
  defer conn.Close()

  // Spartan request: "<method> <path> <content-length>\r\n"
  req := fmt.Sprintf("GET %s 0\r\n", *path)
  if _, err := conn.Write([]byte(req)); err != nil {
    log.Fatalf("send failed: %v", err)
  }

  br := bufio.NewReader(conn)
  hdr, err := br.ReadString('\n')
  if err != nil {
    log.Fatalf("read header failed: %v", err)
  }
  hdr = strings.TrimRight(hdr, "\r\n")

  if !strings.HasPrefix(hdr, "2 ") {
    // print error header and exit
    fmt.Fprintln(os.Stderr, "Server replied:", hdr)
    os.Exit(1)
  }

  mime := strings.TrimSpace(strings.TrimPrefix(hdr, "2 "))
  fmt.Fprintln(os.Stderr, "OK, MIME:", mime)

  // Launch a player that reads from stdin.
  var cmd *exec.Cmd
  switch *player {
  case "ffplay":
    // -nodisp: no video window; -autoexit: exit when stream ends
    cmd = exec.Command("ffplay", "-nodisp", "-autoexit", "-i", "-")
  case "mpv":
    cmd = exec.Command("mpv", "--no-video", "-")
  case "mplayer":
    cmd = exec.Command("mplayer", "-really-quiet", "-")
  case "vlc":
    // VLC reads stdin via "-" on some platforms; on others you may need "fd://0"
    cmd = exec.Command("vlc", "-")
  default:
    log.Fatalf("unknown player: %s (use ffplay|mpv|vlc)", *player)
  }

  cmd.Stdout = os.Stdout
  cmd.Stderr = os.Stderr
  in, err := cmd.StdinPipe()
  if err != nil {
    log.Fatalf("stdin pipe failed: %v", err)
  }

  if err := cmd.Start(); err != nil {
    log.Fatalf("player start failed: %v", err)
  }

  // Copy stream bytes to player stdin
  _, copyErr := io.Copy(in, br)
  _ = in.Close()

  // Wait for player to exit
  waitErr := cmd.Wait()

  if copyErr != nil && copyErr != io.EOF {
    log.Printf("stream ended with error: %v", copyErr)
  }
  if waitErr != nil {
    log.Printf("player exited with error: %v", waitErr)
  }
}

