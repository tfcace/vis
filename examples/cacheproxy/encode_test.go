package main

import (
	"bytes"
	"compress/flate"
	"io"
	"strconv"
	"testing"
)

func TestEncodeRoundTrips(t *testing.T) {
	for _, level := range []int{1, 6, 9} {
		got, err := io.ReadAll(flate.NewReader(bytes.NewReader(encode("k1", level))))
		if err != nil || !bytes.Equal(got, append([]byte("k1"), payload...)) {
			t.Fatalf("level %d: round trip failed: %v", level, err)
		}
	}
}

func BenchmarkEncode(b *testing.B) {
	for _, level := range []int{1, 6, 9} {
		b.Run(strconv.Itoa(level), func(b *testing.B) {
			for b.Loop() {
				encode("k1", level)
			}
		})
	}
}
