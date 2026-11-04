package main

import (
	"bytes"
	"compress/flate"
	"math/rand/v2"
	"strings"
	"sync"
)

// payload is a 16 KiB, JSON-like response body: compressible, like real
// responses, so the compression level changes both its size and its CPU cost.
var payload = func() []byte {
	r := rand.New(rand.NewPCG(7, 7))
	words := strings.Fields("tenant region shard origin cache hit miss evict batch latency p99 status ok error retry " +
		"user order item price currency quantity created updated id name value count total")
	var b bytes.Buffer
	for b.Len() < 16<<10 {
		b.WriteString(`{"`)
		b.WriteString(words[r.IntN(len(words))])
		b.WriteString(`":"`)
		for range 1 + r.IntN(4) {
			b.WriteString(words[r.IntN(len(words))])
			b.WriteByte(' ')
		}
		b.WriteString(`"},`)
	}
	return b.Bytes()
}()

// writers keeps one pool of flate writers per level, so encoding doesn't allocate a compressor per request.
var writers [flate.BestCompression + 1]sync.Pool

// encode compresses the value for key at level: the CPU work of a request.
func encode(key string, level int) []byte {
	var out bytes.Buffer
	w, _ := writers[level].Get().(*flate.Writer)
	if w == nil {
		w, _ = flate.NewWriter(&out, level)
	} else {
		w.Reset(&out)
	}
	w.Write([]byte(key))
	w.Write(payload)
	w.Close()
	writers[level].Put(w)
	return out.Bytes()
}
