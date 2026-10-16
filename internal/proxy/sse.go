package proxy

import "bytes"

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
