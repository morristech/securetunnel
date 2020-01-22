package securetunnel

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	fmt "fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/go-hclog"
	"github.com/pkg/errors"
	uuid "github.com/satori/go.uuid"
	"golang.org/x/crypto/blake2s"
)

const (
	_ = iota
	ToSource
	ToDest
)

type SessionCommand struct {
	Command int
	Data    []byte
	Conn    *websocket.Conn
}

type Session struct {
	startTime time.Time

	ctx    context.Context
	cancel context.CancelFunc

	key    []byte
	id     string
	mu     sync.Mutex
	source *websocket.Conn
	dest   *websocket.Conn

	dataTransfered uint64

	commands chan SessionCommand
}

type Server struct {
	L    hclog.Logger
	Host string
	mux  *http.ServeMux

	mu       sync.Mutex
	sessions map[string]*Session

	upgrader websocket.Upgrader
}

func NewServer(l hclog.Logger) (*Server, error) {
	serv := &Server{
		L:        l,
		mux:      http.NewServeMux(),
		sessions: map[string]*Session{},
	}

	serv.mux.HandleFunc("/create-tunnel", serv.createTunnel)
	serv.mux.HandleFunc("/tunnel", serv.connectTunnel)

	return serv, nil
}

func (s *Server) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	s.mux.ServeHTTP(rw, req)
}

func (s *Server) randKey() []byte {
	buf := make([]byte, 32)

	_, err := rand.Read(buf)
	if err != nil {
		panic(err)
	}

	return buf
}

func (s *Server) host(req *http.Request) string {
	if s.Host != "" {
		return s.Host
	}

	return req.Host
}

func (s *Server) createTunnel(rw http.ResponseWriter, req *http.Request) {
	u := uuid.NewV4()

	sid := u.String()

	key := s.randKey()

	var srcOut, destOut bytes.Buffer

	h, err := blake2s.New256(key)
	if err != nil {
		panic(err)
	}

	var srcToken Token
	srcToken.Id = u.Bytes()
	srcToken.Mode = SOURCE
	srcToken.Host = s.host(req)

	srcBytes, err := srcToken.Marshal()
	if err != nil {
		panic(err)
	}

	binary.Write(&srcOut, binary.BigEndian, uint16(len(srcBytes)))
	binary.Write(&srcOut, binary.BigEndian, srcBytes)

	_, err = h.Write(srcBytes)
	if err != nil {
		panic(err)
	}

	binary.Write(&srcOut, binary.BigEndian, h.Sum(nil))

	h.Reset()

	var destToken Token
	destToken.Id = u.Bytes()
	destToken.Mode = DESTINATION
	destToken.Host = s.host(req)

	destBytes, err := destToken.Marshal()
	if err != nil {
		panic(err)
	}

	binary.Write(&destOut, binary.BigEndian, uint16(len(destBytes)))
	binary.Write(&destOut, binary.BigEndian, destBytes)

	_, err = h.Write(destBytes)
	if err != nil {
		panic(err)
	}

	binary.Write(&destOut, binary.BigEndian, h.Sum(nil))

	s.mu.Lock()

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)

	session := &Session{
		startTime: time.Now(),
		key:       key,
		id:        sid,
		ctx:       ctx,
		cancel:    cancel,
		commands:  make(chan SessionCommand),
	}

	s.sessions[sid] = session

	go s.sessionMonitor(session)

	s.mu.Unlock()

	var response = struct {
		TunnelID    string `json:"tunnel-id"`
		SourceToken string `json:"source-token"`
		DestToken   string `json:"destination-token"`
	}{
		TunnelID:    sid,
		SourceToken: base64.RawURLEncoding.EncodeToString(srcOut.Bytes()),
		DestToken:   base64.RawURLEncoding.EncodeToString(destOut.Bytes()),
	}

	json.NewEncoder(rw).Encode(&response)

	fmt.Printf("Created tunnel: %s\n", sid)
}

func (s *Server) sessionMonitor(sess *Session) {
	sess.mu.Lock()

	var (
		ctx  = sess.ctx
		cmds = sess.commands
	)

	sess.mu.Unlock()

	fmt.Printf("monitoring session: %s\n", sess.id)

	for {
		select {
		case <-ctx.Done():
			sess.mu.Lock()
			if sess.source != nil {
				sess.source.Close()
			}

			if sess.dest != nil {
				sess.dest.Close()
			}
			sess.mu.Unlock()
			return
		case cmd := <-cmds:
			switch cmd.Command {
			case ToSource:
				sess.mu.Lock()

				sess.dataTransfered += uint64(len(cmd.Data))

				if sess.source != nil {
					sess.source.WriteMessage(websocket.BinaryMessage, cmd.Data)
				}

				sess.mu.Unlock()
			case ToDest:
				sess.mu.Lock()

				sess.dataTransfered += uint64(len(cmd.Data))

				if sess.dest != nil {
					sess.dest.WriteMessage(websocket.BinaryMessage, cmd.Data)
				}

				sess.mu.Unlock()
			}
		}
	}
}

func (s *Server) httpError(rw http.ResponseWriter, reason string, code int) {
	rw.Header().Set("X-Status-Reason", reason)

	http.Error(rw, reason, code)
}

var ErrUnknownSession = errors.New("unknown session")

func (s *Server) decodeToken(token string) (*Session, bool, error) {
	t, body, sig, err := DecodeToken(token)
	if err != nil {
		return nil, false, err
	}

	s.mu.Lock()

	session, ok := s.sessions[t.TunnelID()]

	s.mu.Unlock()

	if !ok {
		return nil, false, ErrUnknownSession
	}

	h, err := blake2s.New256(session.key)
	if err != nil {
		return nil, false, err
	}

	_, err = h.Write(body)
	if err != nil {
		return nil, false, err
	}

	sum := h.Sum(nil)

	if subtle.ConstantTimeCompare(sum, sig) == 0 {
		return nil, false, ErrUnknownSession
	}

	return session, t.Mode == SOURCE, nil
}

func (s *Server) sendStatus(sess *Session, conn *websocket.Conn) error {
	sess.mu.Lock()
	defer sess.mu.Unlock()

	var stat Status

	stat.DataTransfered = int64(sess.dataTransfered)
	stat.Lifetime = int64(time.Since(sess.startTime).Seconds())

	data, err := stat.Marshal()
	if err != nil {
		return err
	}

	var msg Message

	msg.Type = STATUS
	msg.Payload = data

	msgSz := msg.Size()

	buf := make([]byte, 2+msgSz)

	_, err = msg.MarshalTo(buf[2:])
	if err != nil {
		return err
	}

	binary.BigEndian.PutUint16(buf, uint16(msgSz))

	return conn.WriteMessage(websocket.BinaryMessage, buf)
}

func (s *Server) connectTunnel(rw http.ResponseWriter, req *http.Request) {
	fmt.Printf("Tunnel connection: %s\n", req.RemoteAddr)

	role := req.URL.Query().Get("role")

	if role == "" {
		s.httpError(rw, "missing role", http.StatusBadRequest)
		return
	}

	tid := req.Header.Get("access-token")

	session, isSource, err := s.decodeToken(tid)

	if err != nil {
		if err == ErrUnknownSession {
			s.httpError(rw, "unknown tunnel", http.StatusBadRequest)
			return
		} else {
			s.httpError(rw, "internal error", http.StatusInternalServerError)
			return
		}
	}

	var (
		conn   *websocket.Conn
		target int
	)

	repHeaders := http.Header{}
	repHeaders.Add("channel-id", tid)

	func() {
		session.mu.Lock()
		defer session.mu.Unlock()

		switch role {
		case "source":
			if !isSource {
				s.httpError(rw, "mismatch token for role", http.StatusBadRequest)
				return
			}

			if session.source != nil {
				s.httpError(rw, "source already established", http.StatusBadRequest)
				return
			}

			conn, err = s.upgrader.Upgrade(rw, req, repHeaders)
			if err != nil {
				s.httpError(rw, "unable to upgrade to websocket: "+err.Error(), http.StatusBadRequest)
				return
			}

			session.source = conn
			target = ToDest
		case "destination":
			if isSource {
				s.httpError(rw, "mismatch token for role", http.StatusBadRequest)
				return
			}

			if session.dest != nil {
				s.httpError(rw, "destination already established", http.StatusBadRequest)
				return
			}

			conn, err = s.upgrader.Upgrade(rw, req, repHeaders)
			if err != nil {
				s.httpError(rw, "unable to upgrade to websocket: "+err.Error(), http.StatusBadRequest)
				return
			}

			session.dest = conn
			target = ToSource
		default:
			s.httpError(rw, "invalid role", http.StatusBadRequest)
			return
		}

		if session.source != nil && session.dest != nil {
			var m Message
			m.Type = CONNECTED

			data, err := m.Marshal()
			if err != nil {
				s.L.Error("error creating connected message", "eror", err)
				session.cancel()
				conn.Close()
				conn = nil
			}

			var szBuf [2]byte

			binary.BigEndian.PutUint16(szBuf[:], uint16(len(data)))

			data = append(szBuf[:], data...)

			err = session.source.WriteMessage(websocket.BinaryMessage, data)
			if err != nil {
				s.L.Error("error sending connected message", "error", err)
				session.cancel()
				conn.Close()
				conn = nil
			}
		}
	}()

	if conn == nil {
		return
	}

	fmt.Printf("forwarding data for %s (%s)\n", req.RemoteAddr, role)

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			session.mu.Lock()
			if session.source == conn {
				session.source = nil
			} else {
				session.dest = nil
			}

			session.mu.Unlock()
			return
		}

		for len(data) > 0 {
			sz := binary.BigEndian.Uint16(data)

			var msg Message

			err = msg.Unmarshal(data[2 : 2+sz])
			if err != nil {
				s.L.Error("error decoding message", "error", err, "tunnel-id", session.id)
				session.cancel()
				return
			}

			switch msg.Type {
			case STATUS_REQUEST:
				s.sendStatus(session, conn)
			default:
				fmt.Printf("<< %s (%d) %+v\n", req.RemoteAddr, target, data)

				select {
				case <-session.ctx.Done():
					return
				case session.commands <- SessionCommand{Command: target, Data: data[:2+sz]}:
					// ok
				}
			}

			data = data[2+sz:]
		}
	}
}
