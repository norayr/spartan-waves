package main

import (
	"bufio"
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
	"time"
)

type Subscriber chan []byte

type Broadcaster struct {
	subs       map[Subscriber]bool
	addSub     chan Subscriber
	removeSub  chan Subscriber
	broadcast  chan []byte
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		subs:      make(map[Subscriber]bool),
		addSub:    make(chan Subscriber),
		removeSub: make(chan Subscriber),
		broadcast: make(chan []byte, 1024),
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

func streamMp3Folder(folder string, b *Broadcaster) {
    for {
        files, _ := filepath.Glob(filepath.Join(folder, "*.mp3"))
        sort.Strings(files)
        if len(files) == 0 {
            time.Sleep(5 * time.Second)
            continue
        }

        for _, fpath := range files {
            log.Printf("Now playing: %s", filepath.Base(fpath))
            f, err := os.Open(fpath)
            if err != nil {
                log.Printf("Failed to open %s: %v", fpath, err)
                continue
            }

            reader := bufio.NewReader(f)
            frameTicker := time.NewTicker(26122 * time.Microsecond) // 26.122 ms
            defer frameTicker.Stop()

            for range frameTicker.C {
                frame, err := readNextMp3Frame(reader)
                if err != nil {
                    if err != io.EOF {
                        log.Printf("error reading frame: %v", err)
                    }
                    break
                }
                b.broadcast <- frame
            }

            f.Close()
        }
    }
}

func readNextMp3Frame(r *bufio.Reader) ([]byte, error) {
    for {
        b1, err := r.ReadByte()
        if err != nil {
            return nil, err
        }
        if b1 != 0xFF {
            continue
        }
        b2, err := r.ReadByte()
        if err != nil {
            return nil, err
        }
        if b2&0xE0 == 0xE0 {
            header := []byte{b1, b2}

            rest, err := r.Peek(2)
            if err != nil {
                return nil, err
            }
            header = append(header, rest...)
            r.Discard(2)

            size, err := mp3FrameSize(header)
            if err != nil {
                return nil, err
            }

            payload := make([]byte, size-4)
            _, err = io.ReadFull(r, payload)
            if err != nil {
                return nil, err
            }

            frame := append(header, payload...)
            return frame, nil
        }
    }
}

func mp3FrameSize(header []byte) (int, error) {
    bitrateIndex := (header[2] >> 4) & 0x0F
    sampleRateIndex := (header[2] >> 2) & 0x03
    padding := (header[2] >> 1) & 0x01

    bitrates := [...]int{
        0, 32, 40, 48, 56, 64, 80,
        96, 112, 128, 160, 192, 224, 256, 320,
    }
    sampleRates := [...]int{
        44100, 48000, 32000,
    }

    if bitrateIndex >= 15 || sampleRateIndex >= 3 {
        return 0, fmt.Errorf("invalid mp3 header")
    }

    bitrate := bitrates[bitrateIndex] * 1000
    sampleRate := sampleRates[sampleRateIndex]

    size := (144 * bitrate / sampleRate) + int(padding)

    return size, nil
}

func handleRadio(conn net.Conn, b *Broadcaster) {
	defer conn.Close()

	_, err := fmt.Fprintf(conn, "2 audio/mpeg\r\n")
	if err != nil {
		return
	}

	sub := make(Subscriber, 128)
	b.addSub <- sub
	defer func() { b.removeSub <- sub }()

	for frame := range sub {
		_, err := conn.Write(frame)
		if err != nil {
			return
		}
	}
}

func handleRequest(conn net.Conn, b *Broadcaster) {
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
		body := "Spartan Radio\n\n=> /radio Tune in to the live MP3 stream\n"
		fmt.Fprintf(conn, "2 text/gemini; charset=utf-8\r\n%s", body)
	case "/radio":
		handleRadio(conn, b)
	default:
		fmt.Fprintf(conn, "4 not found\r\n")
	}
}

func main() {
	musicDir := flag.String("music-dir", "./music", "directory with mp3 files")
	port := flag.Int("port", 300, "TCP port to listen on (Spartan default is 300)")
	flag.Parse()

	b := NewBroadcaster()
	go b.Run()
	go streamMp3Folder(*musicDir, b)

	addr := fmt.Sprintf(":%d", *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", addr, err)
	}
	log.Printf("Spartan Radio listening on spartan://localhost:%d/", *port)
	log.Printf("Serving MP3s from: %s", *musicDir)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}
		go handleRequest(conn, b)
	}
}

