package hub

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// TCPServer serves one hub subscription to many TCP clients. This replaces the
// str2str/STRSVR TCP server outputs on :5003 and :5001.
type TCPServer struct {
	Name   string
	Addr   string
	Filter *Filter
	Queue  int
	Log    *slog.Logger

	hub      *Hub
	mu       sync.Mutex
	clients  map[*tcpClient]struct{}
	ln       net.Listener
	Accepted atomic.Int64
	Active   atomic.Int64
}

type tcpClient struct {
	conn net.Conn
	ch   chan []byte
	once sync.Once
}

func (c *tcpClient) close() { c.once.Do(func() { close(c.ch); c.conn.Close() }) }

func NewTCPServer(h *Hub, name, addr string, f *Filter, queue int, log *slog.Logger) *TCPServer {
	if log == nil {
		log = slog.Default()
	}
	if queue <= 0 {
		queue = 512
	}
	return &TCPServer{Name: name, Addr: addr, Filter: f, Queue: queue, Log: log,
		hub: h, clients: make(map[*tcpClient]struct{})}
}

// Run listens and serves until ctx is cancelled.
func (s *TCPServer) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.Addr)
	if err != nil {
		return err
	}
	s.ln = ln
	s.Log.Info("listener up", "name", s.Name, "addr", s.Addr)

	sub := s.hub.Subscribe(s.Name, s.Filter, s.Queue)
	defer sub.Close()

	go s.broadcast(ctx, sub)

	go func() { <-ctx.Done(); ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.closeAll()
				return ctx.Err()
			}
			s.Log.Warn("accept failed", "name", s.Name, "err", err)
			continue
		}
		s.add(c)
	}
}

func (s *TCPServer) add(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true) // corrections are latency-sensitive; never Nagle them
	}
	cl := &tcpClient{conn: c, ch: make(chan []byte, s.Queue)}
	s.mu.Lock()
	s.clients[cl] = struct{}{}
	s.mu.Unlock()
	s.Accepted.Add(1)
	s.Active.Add(1)
	s.Log.Info("client connected", "name", s.Name, "peer", c.RemoteAddr().String())

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.clients, cl)
			s.mu.Unlock()
			cl.close()
			s.Active.Add(-1)
			s.Log.Info("client disconnected", "name", s.Name, "peer", c.RemoteAddr().String())
		}()
		for b := range cl.ch {
			c.SetWriteDeadline(time.Now().Add(20 * time.Second))
			if _, err := c.Write(b); err != nil {
				return
			}
		}
	}()

	// Drain anything the client sends. A raw output is one-way; reading keeps
	// the socket's state accurate so a half-closed peer is noticed.
	go func() {
		buf := make([]byte, 512)
		for {
			if _, err := c.Read(buf); err != nil {
				cl.close()
				return
			}
		}
	}()
}

func (s *TCPServer) broadcast(ctx context.Context, sub *Sub) {
	for {
		select {
		case <-ctx.Done():
			return
		case b, ok := <-sub.C():
			if !ok {
				return
			}
			s.mu.Lock()
			for cl := range s.clients {
				select {
				case cl.ch <- b:
				default:
					// This client cannot keep up. Drop it rather than let it
					// slow the others or the hub.
					go cl.close()
				}
			}
			s.mu.Unlock()
		}
	}
}

func (s *TCPServer) closeAll() {
	s.mu.Lock()
	for cl := range s.clients {
		cl.close()
	}
	s.mu.Unlock()
}
