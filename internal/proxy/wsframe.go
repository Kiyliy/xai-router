package proxy

import (
	"encoding/binary"
	"unicode/utf8"
)

// WebSocket opcodes (RFC 6455).
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// wsObserver watches one direction of a WebSocket byte stream without
// altering it. It parses frames incrementally (frames and headers may be split
// across reads, payloads are never buffered beyond the inspection cap) and
// reports complete data messages and close frames.
type wsObserver struct {
	maxInspect int // bytes of each message kept for inspection

	hdr        []byte
	inFrame    bool
	fin        bool
	opcode     byte
	masked     bool
	mask       [4]byte
	maskPos    int
	payloadLen uint64
	remaining  uint64

	msgOp     byte // opcode of the message being assembled
	msg       []byte
	msgLen    int64
	msgClip   bool
	msgRunes  int64  // characters of a text message, counted past the inspection cap too
	ctrl      []byte // control frame payload (<=125 bytes)
	bytes     int64
	messages  int64
	broken    bool
	onMessage func(op byte, payload []byte, size int64, clipped bool)
	onClose   func(code int, reason string)
}

func (o *wsObserver) Feed(b []byte) {
	o.bytes += int64(len(b))
	for len(b) > 0 && !o.broken {
		if !o.inFrame {
			b = o.readHeader(b)
			continue
		}
		n := uint64(len(b))
		if n > o.remaining {
			n = o.remaining
		}
		o.consume(b[:n])
		b = b[n:]
		o.remaining -= n
		if o.remaining == 0 {
			o.endFrame()
		}
	}
}

// readHeader accumulates header bytes; returns the unconsumed input.
func (o *wsObserver) readHeader(b []byte) []byte {
	need := 2
	for {
		if len(o.hdr) >= 2 {
			need = 2
			switch o.hdr[1] & 0x7f {
			case 126:
				need += 2
			case 127:
				need += 8
			}
			if o.hdr[1]&0x80 != 0 {
				need += 4
			}
		}
		if len(o.hdr) >= need {
			break
		}
		if len(b) == 0 {
			return b
		}
		o.hdr = append(o.hdr, b[0])
		b = b[1:]
	}
	h := o.hdr
	o.fin = h[0]&0x80 != 0
	o.opcode = h[0] & 0x0f
	o.masked = h[1]&0x80 != 0
	pos := 2
	switch l := h[1] & 0x7f; l {
	case 126:
		o.payloadLen = uint64(binary.BigEndian.Uint16(h[2:4]))
		pos = 4
	case 127:
		o.payloadLen = binary.BigEndian.Uint64(h[2:10])
		pos = 10
	default:
		o.payloadLen = uint64(l)
	}
	if o.masked {
		copy(o.mask[:], h[pos:pos+4])
	}
	o.maskPos = 0
	o.remaining = o.payloadLen
	o.hdr = o.hdr[:0]
	o.inFrame = true
	if o.opcode >= 0x8 {
		if o.payloadLen > 125 { // invalid control frame: stop inspecting
			o.broken = true
			return b
		}
		o.ctrl = o.ctrl[:0]
	} else if o.opcode != opContinuation {
		o.msgOp, o.msg, o.msgLen, o.msgClip, o.msgRunes = o.opcode, o.msg[:0], 0, false, 0
	}
	if o.remaining == 0 {
		o.endFrame()
	}
	return b
}

func (o *wsObserver) consume(p []byte) {
	if o.opcode >= 0x8 {
		for _, c := range p {
			o.ctrl = append(o.ctrl, o.unmask(c))
		}
		return
	}
	o.msgLen += int64(len(p))
	text := o.msgOp == opText
	room := o.maxInspect - len(o.msg)
	if room <= 0 {
		// Past the inspection cap: keep the mask position in step, and keep
		// counting the characters of text messages (billed by length).
		o.msgClip = true
		if !text {
			o.maskPos += len(p)
			return
		}
		for _, c := range p {
			if o.unmask(c)&0xC0 != 0x80 {
				o.msgRunes++
			}
		}
		return
	}
	if len(p) > room {
		o.msgClip = true
	}
	for i, c := range p {
		v := o.unmask(c)
		if text && v&0xC0 != 0x80 {
			o.msgRunes++
		}
		if i < room {
			o.msg = append(o.msg, v)
		}
	}
}

func (o *wsObserver) unmask(c byte) byte {
	if !o.masked {
		return c
	}
	v := c ^ o.mask[o.maskPos&3]
	o.maskPos++
	return v
}

func (o *wsObserver) endFrame() {
	o.inFrame = false
	switch {
	case o.opcode == opClose:
		code, reason := 1005, ""
		if len(o.ctrl) >= 2 {
			code = int(binary.BigEndian.Uint16(o.ctrl[:2]))
			if utf8.Valid(o.ctrl[2:]) {
				reason = string(o.ctrl[2:])
			}
		}
		if o.onClose != nil {
			o.onClose(code, reason)
		}
	case o.opcode >= 0x8: // ping/pong
	case o.fin:
		o.messages++
		if o.onMessage != nil {
			o.onMessage(o.msgOp, o.msg, o.msgLen, o.msgClip)
		}
	}
}
