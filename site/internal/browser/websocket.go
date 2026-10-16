package browser

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
)

// wsConn is the client side of a WebSocket (RFC 6455), as much of it as
// WebDriver BiDi needs: text messages, fragments, pings and close.
type wsConn struct {
	c   net.Conn
	r   *bufio.Reader
	wmu sync.Mutex
}

func dialWS(addr, path string) (*wsConn, error) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	var k [16]byte
	rand.Read(k[:])
	key := base64.StdEncoding.EncodeToString(k[:])
	req := "GET " + path + " HTTP/1.1\r\nHost: " + addr + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		c.Close()
		return nil, err
	}
	r := bufio.NewReaderSize(c, 1<<20)
	res, err := http.ReadResponse(r, nil)
	if err != nil {
		c.Close()
		return nil, err
	}
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if res.StatusCode != http.StatusSwitchingProtocols || res.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		c.Close()
		return nil, fmt.Errorf("websocket handshake: %s", res.Status)
	}
	return &wsConn{c: c, r: r}, nil
}

func (w *wsConn) writeFrame(op byte, p []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	h := []byte{0x80 | op}
	switch n := len(p); {
	case n < 126:
		h = append(h, 0x80|byte(n))
	case n < 1<<16:
		h = append(h, 0x80|126)
		h = binary.BigEndian.AppendUint16(h, uint16(n))
	default:
		h = append(h, 0x80|127)
		h = binary.BigEndian.AppendUint64(h, uint64(n))
	}
	var mask [4]byte
	rand.Read(mask[:])
	h = append(h, mask[:]...)
	b := make([]byte, len(h)+len(p))
	copy(b, h)
	for i, c := range p {
		b[len(h)+i] = c ^ mask[i%4]
	}
	_, err := w.c.Write(b)
	return err
}

// WriteText sends one text message.
func (w *wsConn) WriteText(p []byte) error { return w.writeFrame(1, p) }

// ReadMessage returns the next text or binary message, answering pings.
func (w *wsConn) ReadMessage() ([]byte, error) {
	var msg []byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(w.r, h[:]); err != nil {
			return nil, err
		}
		fin, op := h[0]&0x80 != 0, h[0]&0x0f
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(w.r, b[:]); err != nil {
				return nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(w.r, b[:]); err != nil {
				return nil, err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		var mask [4]byte
		masked := h[1]&0x80 != 0
		if masked {
			if _, err := io.ReadFull(w.r, mask[:]); err != nil {
				return nil, err
			}
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(w.r, p); err != nil {
			return nil, err
		}
		if masked {
			for i := range p {
				p[i] ^= mask[i%4]
			}
		}
		switch op {
		case 8:
			w.writeFrame(8, nil)
			return nil, errors.New("websocket closed")
		case 9:
			w.writeFrame(10, p)
			continue
		case 10:
			continue
		}
		msg = append(msg, p...)
		if fin {
			return msg, nil
		}
	}
}

func (w *wsConn) Close() error { return w.c.Close() }
