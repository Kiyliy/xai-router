package proxy

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

type obsEvent struct {
	op      byte
	payload string
	size    int64
	clipped bool
}

func newTestObserver(max int) (*wsObserver, *[]obsEvent, *[]string) {
	var msgs []obsEvent
	var closes []string
	o := &wsObserver{maxInspect: max,
		onMessage: func(op byte, p []byte, size int64, clip bool) {
			msgs = append(msgs, obsEvent{op, string(p), size, clip})
		},
		onClose: func(code int, reason string) { closes = append(closes, fmt.Sprintf("%d %s", code, reason)) },
	}
	return o, &msgs, &closes
}

func TestWSObserverFrames(t *testing.T) {
	mask := []byte{9, 8, 7, 6}
	long := strings.Repeat("L", 300)    // 16-bit length
	huge := strings.Repeat("H", 70_000) // 64-bit length
	var stream []byte
	stream = append(stream, encodeFrame(true, opText, []byte(`{"a":1}`), nil)...)
	stream = append(stream, encodeFrame(false, opText, []byte("frag-"), mask)...)           // first fragment
	stream = append(stream, encodeFrame(true, opPing, []byte("p"), mask)...)                // control frame in between
	stream = append(stream, encodeFrame(true, opContinuation, []byte("ment"), mask)...)     // final fragment
	stream = append(stream, encodeFrame(true, opBinary, []byte(long), mask)...)             // 126 length
	stream = append(stream, encodeFrame(true, opText, []byte(huge), nil)...)                // 127 length, clipped
	stream = append(stream, encodeFrame(true, opText, nil, nil)...)                         // empty message
	stream = append(stream, encodeFrame(true, opClose, closeFrame(1001, "going"), mask)...) // close
	stream = append(stream, encodeFrame(true, opClose, nil, nil)...)                        // close without code
	stream = append(stream, encodeFrame(true, opClose, []byte{0x03, 0xe8, 0xff}, nil)...)   // invalid UTF-8 reason

	want := []obsEvent{
		{opText, `{"a":1}`, 7, false},
		{opText, "frag-ment", 9, false},
		{opBinary, long[:256], 300, true},
		{opText, huge[:256], 70_000, true},
		{opText, "", 0, false},
	}
	wantCloses := []string{"1001 going", "1005 ", "1000 "}
	for _, chunk := range []int{1, 3, 7, 64, len(stream)} {
		o, msgs, closes := newTestObserver(256)
		for i := 0; i < len(stream); i += chunk {
			o.Feed(stream[i:min(i+chunk, len(stream))])
		}
		if len(*msgs) != len(want) {
			t.Fatalf("chunk %d: %d messages", chunk, len(*msgs))
		}
		for i, w := range want {
			if (*msgs)[i] != w {
				t.Fatalf("chunk %d message %d: got %+v want %+v", chunk, i, (*msgs)[i], w)
			}
		}
		if strings.Join(*closes, "|") != strings.Join(wantCloses, "|") {
			t.Fatalf("chunk %d closes: %q", chunk, *closes)
		}
		if o.messages != int64(len(want)) || o.bytes != int64(len(stream)) {
			t.Fatalf("chunk %d counters: %d %d", chunk, o.messages, o.bytes)
		}
	}
}

func TestWSObserverInvalidControlFrame(t *testing.T) {
	o, msgs, _ := newTestObserver(64)
	bad := encodeFrame(true, opPing, bytes.Repeat([]byte("x"), 200), nil) // control frames must be <=125 bytes
	o.Feed(append(bad, encodeFrame(true, opText, []byte("after"), nil)...))
	if !o.broken || len(*msgs) != 0 {
		t.Fatalf("broken=%v msgs=%v", o.broken, *msgs)
	}
}

func TestWSObserverNoCallbacks(t *testing.T) {
	o := &wsObserver{maxInspect: 8}
	o.Feed(encodeFrame(true, opText, []byte("hello"), nil))
	o.Feed(encodeFrame(true, opClose, closeFrame(1000, ""), nil))
	if o.messages != 1 {
		t.Fatalf("messages %d", o.messages)
	}
}
