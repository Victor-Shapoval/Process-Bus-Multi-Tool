// Package mcpserver adapts project operations to authenticated Streamable HTTP.
package mcpserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"pbmt/internal/application/automation"
	"pbmt/internal/application/control"
	"pbmt/internal/config"
)

const pingInterval = 3 * time.Second
const pingTimeout = 2 * time.Second

type Server struct {
	mu       sync.Mutex
	status   automation.ServerStatus
	api      *automation.API
	rpc      *mcp.Server
	http     *http.Server
	owner    *mcp.ServerSession
	stopOnce sync.Once
	stopping chan struct{}
	done     chan struct{}
	stopErr  error
	log      *slog.Logger
}

func Start(root string, cfg *config.Config, provider control.AgentProvider, options automation.ServerOptions) (automation.Server, error) {
	return start(root, cfg, provider, options, net.Listen)
}

func start(root string, cfg *config.Config, provider control.AgentProvider, options automation.ServerOptions, listen func(string, string) (net.Listener, error)) (*Server, error) {
	host, port, err := net.SplitHostPort(options.Address)
	if err != nil || net.ParseIP(host) == nil {
		return nil, errors.New("Server IP must be a numeric local interface IP or 0.0.0.0")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return nil, errors.New("Server Port must be between 1 and 65535")
	}
	if len(options.Token) < 12 {
		return nil, errors.New("MCP token must contain at least 12 characters")
	}
	if strings.IndexFunc(options.Token, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		return nil, errors.New("MCP token must contain only printable ASCII without spaces")
	}
	if provider == nil {
		return nil, errors.New("project runtime does not support MCP control")
	}
	api, err := automation.Prepare(root, cfg)
	if err != nil {
		return nil, err
	}
	listener, err := listen("tcp", options.Address)
	if err != nil {
		return nil, fmt.Errorf("listen MCP: %w", err)
	}
	agent, err := provider.AcquireAgent()
	if err != nil {
		listener.Close()
		return nil, err
	}
	api.Attach(agent)
	scheme := "http"
	s := &Server{api: api, stopping: make(chan struct{}), done: make(chan struct{}), log: slog.Default().With("component", "mcp"),
		status: automation.ServerStatus{Running: true, Address: scheme + "://" + options.Address + "/mcp"}}
	s.rpc = mcp.NewServer(&mcp.Implementation{Name: "PBMT", Version: "1.0.0"}, &mcp.ServerOptions{
		// Stateful sessions and server ping are required to detect connection
		// loss. Newer sessionless protocol revisions are deliberately not offered.
		SupportedProtocolVersions: []string{"2025-11-25", "2025-06-18", "2025-03-26"}, Capabilities: &mcp.ServerCapabilities{},
		Instructions: "PBMT controls one prepared test terminal through GOOSE/SV only. Call get_catalog and get_status first. Stream directions are relative to PBMT. sv_set uses RMS (A/V), never peak amplitude. Do not infer terminal reception from a successful set. t_set_goose/t_set_sv return a sequence_id immediately; use get_sequence/cancel_sequence. run_test measures SV-step-to-GOOSE-edge reaction internally; use get_test/cancel_test. Tests zero all affected channel RMS on EVERY exit, including Stop MCP, and confirm a reset frame; reset failure stops both publishers. Do not blindly retry a start after an uncertain response: inspect get_status sequences/tests first. Timing is best-effort on the server monotonic clock, not hard real-time or NIC timestamps. The MCP client must maintain the GET/SSE stream and answer ping, including during sequences/tests. Agent disconnect cancels work and stops both publishers. The application's Stop MCP button cancels work and returns control to Manual without stopping publishers (active tests reset first; ordinary sequences retain outputs). PTP is never exposed.",
		InitializedHandler: func(_ context.Context, req *mcp.InitializedRequest) {
			if !s.claim(req.Session) {
				go req.Session.Close()
			}
		},
	})
	s.addTools()
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.rpc }, &mcp.StreamableHTTPOptions{SessionTimeout: 30 * time.Second, MaxRequestBodyBytes: 256 << 10})
	s.http = &http.Server{Handler: s.protect(handler, options.Token, scheme), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 45 * time.Second, MaxHeaderBytes: 16 << 10,
		ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelWarn)}
	go func() {
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.requestStop("MCP listener failed: " + err.Error())
		}
	}()
	go s.watchSources()
	s.log.Info("MCP server started", "address", s.Status().Address)
	return s, nil
}

func (s *Server) protect(next http.Handler, token, scheme string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "MCP bearer token required", http.StatusUnauthorized)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Scheme != scheme || u.Host != r.Host || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
				http.Error(w, "invalid Origin", http.StatusForbidden)
				return
			}
		}
		select {
		case <-s.stopping:
			http.Error(w, "MCP stopping", http.StatusServiceUnavailable)
			return
		default:
		}
		s.mu.Lock()
		owner := s.owner
		s.mu.Unlock()
		if owner != nil && r.Header.Get("Mcp-Session-Id") != owner.ID() {
			http.Error(w, "another MCP session owns control", http.StatusConflict)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) claim(session *mcp.ServerSession) bool {
	s.mu.Lock()
	select {
	case <-s.stopping:
		s.mu.Unlock()
		return false
	default:
	}
	if s.owner != nil {
		same := s.owner == session
		s.mu.Unlock()
		return same
	}
	s.owner = session
	s.status.Connected = true
	s.mu.Unlock()
	s.log.Info("MCP agent connected")
	go func() {
		err := session.Wait()
		select {
		case <-s.stopping:
			return
		default:
		}
		reason := "MCP agent disconnected"
		if err != nil {
			reason += ": " + err.Error()
		}
		s.requestStop(reason)
	}()
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stopping:
				return
			case <-ticker.C:
			}
			ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
			err := session.Ping(ctx, nil)
			cancel()
			if err != nil {
				s.requestStop("MCP agent heartbeat lost; publishers stopped")
				return
			}
		}
	}()
	return true
}

func (s *Server) watchSources() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopping:
			return
		case <-ticker.C:
			if err := s.api.CheckSources(); err != nil {
				s.requestStop(err.Error())
				return
			}
		}
	}
}

func (s *Server) Status() automation.ServerStatus { s.mu.Lock(); defer s.mu.Unlock(); return s.status }

func (s *Server) requestStop(reason string) {
	s.stopOnce.Do(func() {
		close(s.stopping)
		s.api.Revoke()
		go func() {
			// Explicit Stop returns unchanged outputs to Manual. All fault paths
			// retain the fail-stop policy. stopOnce selects exactly one outcome.
			var err error
			if reason == "" {
				err = s.api.Release()
			} else {
				err = s.api.Close()
			}
			for session := range s.rpc.Sessions() {
				_ = session.Close()
			}
			// Drain the final DELETE response instead of cutting it off with EOF.
			// Sessions are closed first so their long-lived SSE streams can finish.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			if s.http.Shutdown(ctx) != nil {
				_ = s.http.Close()
			}
			cancel()
			s.mu.Lock()
			s.stopErr = err
			s.status.Running = false
			s.status.Connected = false
			s.status.Error = reason
			if err != nil {
				s.status.Error += "; " + err.Error()
			}
			s.mu.Unlock()
			if reason != "" || err != nil {
				s.log.Warn("MCP server stopped", "reason", reason, "error", err)
			} else {
				s.log.Info("MCP server stopped")
			}
			close(s.done)
		}()
	})
}

func (s *Server) Stop() error {
	s.requestStop("")
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopErr
}
