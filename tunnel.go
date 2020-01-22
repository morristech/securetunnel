package securetunnel

import (
	"encoding/binary"
	"fmt"
	io "io"
	"net/http"
	strings "strings"
	"sync"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/go-hclog"
	"github.com/pkg/errors"
)

type Tunnel struct {
	source bool
	conn   *websocket.Conn
	id     string

	partial []byte

	buf []byte

	readRest []byte

	mu            sync.Mutex
	statusWaiting chan *Message
}

var (
	ErrAuthentication = errors.New("invalid authentication")
	ErrProtocolError  = errors.New("protocol error")
	ErrMessageTooBig  = errors.New("message too big")
)

func openMode(host, token, mode string) (*Tunnel, error) {
	var dialer websocket.Dialer

	proto := "wss"

	if host == "localhost" || strings.HasPrefix(host, "localhost:") {
		proto = "ws"
	}

	url := fmt.Sprintf("%s://%s/tunnel?role=%s", proto, host, mode)

	hclog.L().Info("connecting to server", "url", url)

	headers := http.Header{}

	headers.Add("access-token", token)

	sleepDur := time.Second

	for {
		conn, resp, err := dialer.Dial(url, headers)
		if err != nil {
			if resp == nil {
				return nil, err
			}

			reason := resp.Header.Get("X-Status-Reason")
			return nil, errors.Wrapf(err, "error attempting to dial connection: %s", reason)
		}

		if resp.StatusCode >= 400 && resp.StatusCode <= 499 {
			return nil, errors.Wrapf(ErrAuthentication, "unable to connect to endpoint. status=%d", resp.StatusCode)
		}

		if resp.StatusCode >= 500 && resp.StatusCode <= 599 {
			time.Sleep(sleepDur)

			sleepDur *= 2
			continue
		}

		cid := resp.Header.Get("channel-id")

		tun := &Tunnel{
			source: true,
			conn:   conn,
			id:     cid,
			buf:    make([]byte, 8196),
		}

		return tun, nil
	}
}

func Open(token string) (*Tunnel, error) {
	t, _, _, err := DecodeToken(token)
	if err != nil {
		return nil, err
	}

	spew.Dump(t)

	switch t.Mode {
	case SOURCE:
		return openSource(t.Host, token)
	case DESTINATION:
		return openDestination(t.Host, token)
	default:
		return nil, errors.Wrapf(ErrAuthentication, "bad token")
	}
}

func openSource(host, token string) (*Tunnel, error) {
	tun, err := openMode(host, token, "source")
	if err != nil {
		return nil, err
	}

	// Wait for the connected message, meaning destination is connected.
	msg, err := tun.getMessage()
	if err != nil {
		tun.Close()
		return nil, err
	}

	if msg.Type != CONNECTED {
		return nil, errors.Wrapf(ErrProtocolError, "initial message wasn't connected")
	}

	var rmsg Message
	rmsg.Type = SESSION_START

	err = tun.sendMessage(&rmsg)
	if err != nil {
		tun.Close()
		return nil, err
	}

	msg, err = tun.getMessage()
	if err != nil {
		tun.Close()
		return nil, err
	}

	if msg.Type != SESSION_CONT {
		return nil, errors.Wrapf(ErrProtocolError, "session wasn't continued by destination")
	}

	return tun, nil
}

func openDestination(host, token string) (*Tunnel, error) {
	tun, err := openMode(host, token, "destination")
	if err != nil {
		return nil, err
	}

	msg, err := tun.getMessage()
	if err != nil {
		tun.Close()
		return nil, err
	}

	if msg.Type != SESSION_START {
		return nil, errors.Wrapf(ErrProtocolError, "did not receive stream start")
	}

	var rmsg Message
	rmsg.Type = SESSION_CONT

	err = tun.sendMessage(&rmsg)
	if err != nil {
		tun.Close()
		return nil, err
	}

	return tun, nil
}

const maxSize = 131076

func (t *Tunnel) sendMessage(msg *Message) error {
	sz := msg.Size()

	if sz > len(t.buf) {
		t.buf = make([]byte, sz+512)
	}

	n, err := msg.MarshalTo(t.buf[2:])
	if err != nil {
		return err
	}

	pktSize := n + 2

	if pktSize >= maxSize {
		return ErrMessageTooBig
	}

	binary.BigEndian.PutUint16(t.buf, uint16(n))

	return t.conn.WriteMessage(websocket.BinaryMessage, t.buf[:pktSize])
}

func (t *Tunnel) getMessage() (*Message, error) {
	var (
		data []byte
		err  error
	)

	if len(t.partial) == 0 {
		_, data, err = t.conn.ReadMessage()
		if err != nil {
			return nil, err
		}
	} else {
		data = t.partial
	}

	sz := binary.BigEndian.Uint16(data)
	data = data[2:]

	msgData := data[:sz]
	data = data[sz:]

	if len(data) > 0 {
		t.partial = data
	}

	var msg Message

	err = msg.Unmarshal(msgData)
	if err != nil {
		return nil, err
	}

	return &msg, nil
}

func (t *Tunnel) Close() error {
	return t.conn.Close()
}

func (t *Tunnel) RequestStatus() (*Status, error) {
	t.mu.Lock()

	c := make(chan *Message, 1)

	t.statusWaiting = c

	var msg Message
	msg.Type = STATUS_REQUEST

	t.sendMessage(&msg)

	t.mu.Unlock()

	resp := <-c

	var stat Status

	err := stat.Unmarshal(resp.Payload)
	if err != nil {
		return nil, err
	}

	return &stat, nil
}

const maxPayloadSize = maxSize - 64

func (t *Tunnel) Read(buf []byte) (int, error) {
	var data []byte

	if len(t.readRest) > 0 {
		data = t.readRest
	} else {
	retry:
		msg, err := t.getMessage()
		if err != nil {
			return 0, err
		}

		switch msg.Type {
		case DATA:
			// ok
		case STATUS:
			t.mu.Lock()
			if t.statusWaiting != nil {
				go func() {
					t.statusWaiting <- msg
				}()
			}
			t.mu.Unlock()

			goto retry
		default:
			t.Close()
			return 0, io.EOF
		}

		data = msg.Payload
	}

	if len(data) > len(buf) {
		n := copy(buf, data)
		t.readRest = data[n:]
		return n, nil
	}

	t.readRest = nil

	return copy(buf, data), nil
}

func (t *Tunnel) Write(buf []byte) (int, error) {
	var msg Message

	msg.Type = DATA

	sent := len(buf)

	for len(buf) > 0 {
		if len(buf) > maxPayloadSize {
			msg.Payload = buf[:maxPayloadSize]
			buf = buf[maxPayloadSize:]
		} else {
			msg.Payload = buf
			buf = nil
		}

		err := t.sendMessage(&msg)
		if err != nil {
			return 0, err
		}
	}

	return sent, nil
}
