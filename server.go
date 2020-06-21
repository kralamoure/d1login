package d1login

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"

	"github.com/kralamoure/d1"
	"go.uber.org/zap"
)

type ServerConfig struct {
	Addr   string
	Repo   d1.Repository
	Logger *zap.Logger
}

type Server struct {
	logger *zap.Logger
	addr   *net.TCPAddr
	repo   d1.Repository

	ln       *net.TCPListener
	sessions map[*session]struct{}
	mu       sync.Mutex
}

func NewServer(c ServerConfig) (*Server, error) {
	if c.Repo == nil {
		return nil, errors.New("repository should not be nil")
	}
	if c.Logger == nil {
		c.Logger = zap.NewNop()
	}
	addr, err := net.ResolveTCPAddr("tcp4", c.Addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		addr: addr,
	}
	return s, nil
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()

	ln, err := net.ListenTCP("tcp4", s.addr)
	if err != nil {
		return err
	}
	defer func() {
		ln.Close()
		s.logger.Info("stopped listening",
			zap.String("address", ln.Addr().String()),
		)
	}()
	s.logger.Info("listening",
		zap.String("address", ln.Addr().String()),
	)
	s.ln = ln

	errCh := make(chan error)
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := s.acceptLoop(ctx)
		if err != nil {
			select {
			case errCh <- err:
			case <-ctx.Done():
			}
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (s *Server) acceptLoop(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := s.ln.AcceptTCP()
		if err != nil {
			return err
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.handleClientConn(ctx, conn)
			if err != nil && !(errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || errors.Is(err, errEndOfService)) {
				s.logger.Debug("error while handling client connection",
					zap.Error(err),
					zap.String("client_address", conn.RemoteAddr().String()),
				)
			}
		}()
	}
}

func (s *Server) handleClientConn(ctx context.Context, conn *net.TCPConn) error {
	var wg sync.WaitGroup
	defer wg.Wait()

	defer func() {
		conn.Close()
		s.logger.Info("client disconnected",
			zap.String("client_address", conn.RemoteAddr().String()),
		)
	}()
	s.logger.Info("client connected",
		zap.String("client_address", conn.RemoteAddr().String()),
	)

	sess := &session{
		svr:  s,
		conn: conn,
	}

	s.trackSession(sess, true)
	defer s.trackSession(sess, false)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error)

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := sess.receivePkts(ctx)
		if err != nil {
			select {
			case errCh <- err:
			case <-ctx.Done():
			}
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) trackSession(sess *session, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		if s.sessions == nil {
			s.sessions = make(map[*session]struct{})
		}
		s.sessions[sess] = struct{}{}
	} else {
		delete(s.sessions, sess)
	}
}
