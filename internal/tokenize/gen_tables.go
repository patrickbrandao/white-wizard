//go:build ignore

// gen_tables converts the official tiktoken rank files (o200k_base,
// cl100k_base) into the compact tables embedded by this package (ADR 0053):
// a header "WWZBPE1\n", the token count as uvarint and, for every rank from
// 0 on, the token length as uvarint followed by its bytes; the whole thing
// gzip-compressed. The source must match the sha256 tiktoken publishes, and
// its ranks must be 0..n-1 without gaps.
//
// Usage (from internal/tokenize, through go generate):
//
//	go run gen_tables.go                  # downloads from openaipublic
//	go run gen_tables.go -src DIR         # reads DIR/<name>.tiktoken
//
// The network is used only here, never at run time.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// sources are the official files and the sha256 tiktoken checks them
// against (tiktoken_ext/openai_public.py, MIT).
var sources = []struct{ name, url, sha256 string }{
	{"o200k_base", "https://openaipublic.blob.core.windows.net/encodings/o200k_base.tiktoken", "446a9538cb6c348e3516120d7c08b09f57c36495e2acfffe59a5bf8b0cfb1a2d"},
	{"cl100k_base", "https://openaipublic.blob.core.windows.net/encodings/cl100k_base.tiktoken", "223921b76ee99bde995b7ff738513eef100fb51d18c93597a113bcffe865b2a7"},
}

func main() {
	src := flag.String("src", "", "directory with <name>.tiktoken (empty: download)")
	out := flag.String("out", "data", "output directory")
	flag.Parse()
	for _, s := range sources {
		raw, err := read(*src, s.name, s.url)
		if err != nil {
			log.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); got != s.sha256 {
			log.Fatalf("%s: sha256 %s, want %s", s.name, got, s.sha256)
		}
		table, n, err := compact(raw)
		if err != nil {
			log.Fatalf("%s: %v", s.name, err)
		}
		var gz bytes.Buffer
		w, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
		w.Write(table)
		if err := w.Close(); err != nil {
			log.Fatal(err)
		}
		path := filepath.Join(*out, s.name+".bin.gz")
		if err := os.WriteFile(path, gz.Bytes(), 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s: %d tokens, %d bytes compact, %d bytes gzip -> %s\n", s.name, n, len(table), gz.Len(), path)
	}
}

func read(dir, name, url string) ([]byte, error) {
	if dir != "" {
		return os.ReadFile(filepath.Join(dir, name+".tiktoken"))
	}
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func compact(raw []byte) ([]byte, int, error) {
	var out bytes.Buffer
	out.WriteString("WWZBPE1\n")
	var body bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	n := 0
	var tmp [binary.MaxVarintLen64]byte
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		b64, rankStr, ok := strings.Cut(line, " ")
		if !ok {
			return nil, 0, fmt.Errorf("line %d: %q", n+1, line)
		}
		tok, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, 0, fmt.Errorf("line %d: %w", n+1, err)
		}
		rank, err := strconv.Atoi(rankStr)
		if err != nil || rank != n {
			return nil, 0, fmt.Errorf("line %d: rank %q, want %d", n+1, rankStr, n)
		}
		body.Write(tmp[:binary.PutUvarint(tmp[:], uint64(len(tok)))])
		body.Write(tok)
		n++
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	out.Write(tmp[:binary.PutUvarint(tmp[:], uint64(n))])
	out.Write(body.Bytes())
	return out.Bytes(), n, nil
}
