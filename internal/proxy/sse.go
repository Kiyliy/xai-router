package proxy

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// sseParser incrementally splits a Server-Sent Events byte stream into events.
// It copes with events and lines split across arbitrary chunk boundaries.
type sseParser struct {
	buf     []byte
	event   string
	data    []byte
	hasData bool
	onEvent func(event string, data []byte)
}

func (p *sseParser) Write(b []byte) {
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		line := p.buf[:i]
		p.buf = p.buf[i+1:]
		if n := len(line); n > 0 && line[n-1] == '\r' {
			line = line[:n-1]
		}
		p.line(line)
	}
	// Bound memory if an upstream never sends a newline.
	if len(p.buf) > 8<<20 {
		p.buf = p.buf[:0]
	}
	if len(p.buf) == 0 && cap(p.buf) > 64<<10 {
		p.buf = nil
	}
}

func (p *sseParser) line(line []byte) {
	if len(line) == 0 {
		p.dispatch()
		return
	}
	if line[0] == ':' {
		return // comment / keep-alive
	}
	field, value := line, []byte(nil)
	if i := bytes.IndexByte(line, ':'); i >= 0 {
		field, value = line[:i], line[i+1:]
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
	}
	switch string(field) {
	case "event":
		p.event = string(value)
	case "data":
		if p.hasData {
			p.data = append(p.data, '\n')
		}
		p.data = append(p.data, value...)
		p.hasData = true
	}
}

func (p *sseParser) dispatch() {
	if p.hasData && p.onEvent != nil {
		p.onEvent(p.event, p.data)
	}
	p.event, p.data, p.hasData = "", p.data[:0], false
}

// Close flushes a trailing event that was not terminated by a blank line.
func (p *sseParser) Close() {
	if len(p.buf) > 0 {
		p.line(bytes.TrimRight(p.buf, "\r"))
		p.buf = nil
	}
	p.dispatch()
}

// capture keeps the head and tail of a byte stream within a size budget, so
// that both the first events and the final usage event of a long stream end
// up in the log.
type capture struct {
	limit     int
	head      []byte
	tail      []byte // ring buffer contents in order
	total     int
	truncated bool
}

func newCapture(limit int) *capture { return &capture{limit: limit} }

func (c *capture) Write(b []byte) {
	c.total += len(b)
	headLimit := c.limit * 3 / 4
	if room := headLimit - len(c.head); room > 0 {
		n := min(room, len(b))
		c.head = append(c.head, b[:n]...)
		b = b[n:]
	}
	if len(b) == 0 {
		return
	}
	c.truncated = c.truncated || c.limit-headLimit < len(c.tail)+len(b)
	c.tail = append(c.tail, b...)
	if tailLimit := c.limit - headLimit; len(c.tail) > tailLimit {
		c.tail = append(c.tail[:0], c.tail[len(c.tail)-tailLimit:]...)
	}
}

func (c *capture) Bytes() []byte {
	if !c.truncated {
		return append(c.head, c.tail...)
	}
	out := make([]byte, 0, len(c.head)+len(c.tail)+64)
	out = append(out, c.head...)
	out = append(out, []byte("\n\n…[stream truncated in log; "+itoa(c.total)+" bytes total]…\n\n")...)
	return append(out, c.tail...)
}

// messagesFixer repairs xAI's Anthropic-compatible /v1/messages streams,
// whose content_block_delta (and sometimes content_block_start/stop) events
// lack the "index" field that Anthropic SDKs require to assemble a message.
// It works on whole events, so it buffers at most one partial event.
type messagesFixer struct {
	buf  []byte
	cur  int
	next int
}

func (f *messagesFixer) Feed(b []byte) []byte {
	f.buf = append(f.buf, b...)
	var out []byte
	for {
		end, sep := eventEnd(f.buf)
		if end < 0 {
			break
		}
		out = append(out, f.fixBlock(f.buf[:end+sep])...)
		f.buf = f.buf[end+sep:]
	}
	return out
}

// Flush returns any trailing bytes not terminated by a blank line.
func (f *messagesFixer) Flush() []byte {
	out := f.buf
	f.buf = nil
	return out
}

func eventEnd(b []byte) (int, int) {
	i := bytes.Index(b, []byte("\n\n"))
	j := bytes.Index(b, []byte("\r\n\r\n"))
	switch {
	case i < 0 && j < 0:
		return -1, 0
	case j >= 0 && (i < 0 || j < i):
		return j, 4
	}
	return i, 2
}

func (f *messagesFixer) fixBlock(block []byte) []byte {
	start := bytes.Index(block, []byte("data: {"))
	if start < 0 {
		return block
	}
	jsonStart := start + len("data: ")
	lineEnd := bytes.IndexByte(block[jsonStart:], '\n')
	if lineEnd < 0 {
		lineEnd = len(block) - jsonStart
	}
	payload := bytes.TrimRight(block[jsonStart:jsonStart+lineEnd], "\r")
	var ev struct {
		Type  string `json:"type"`
		Index *int   `json:"index"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return block
	}
	var idx int
	switch ev.Type {
	case "content_block_start":
		if ev.Index != nil {
			f.cur, f.next = *ev.Index, *ev.Index+1
			return block
		}
		idx = f.next
		f.cur, f.next = idx, idx+1
	case "content_block_delta", "content_block_stop":
		if ev.Index != nil {
			return block
		}
		idx = f.cur
	default:
		return block
	}
	out := make([]byte, 0, len(block)+12)
	out = append(out, block[:jsonStart+1]...) // through the opening '{'
	out = append(out, `"index":`+strconv.Itoa(idx)+`,`...)
	return append(out, block[jsonStart+1:]...)
}
