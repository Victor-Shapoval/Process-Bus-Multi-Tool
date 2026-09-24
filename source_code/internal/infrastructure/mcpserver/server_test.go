package mcpserver

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"pbmt/internal/application/automation"
	"pbmt/internal/application/control"
	"pbmt/internal/application/goosepub"
	"pbmt/internal/config"
	"pbmt/internal/domain/goose"
	"pbmt/profiles"
)

const testToken = "test-only-user-editable-token-123456"

func TestExplicitStopReturnsControlWithoutStoppingPublishers(t *testing.T) {
	s, p, _ := liveServer(t)
	client, _ := connectClient(t, s)
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if p.active.Load() || p.releases.Load() != 1 || p.closes.Load() != 0 {
		t.Fatalf("wrong handover: active=%v releases=%d closes=%d", p.active.Load(), p.releases.Load(), p.closes.Load())
	}
	if err := s.Stop(); err != nil || p.releases.Load() != 1 {
		t.Fatal("stop is not idempotent", err)
	}
}

func TestTokenMinimumLength(t *testing.T) {
	root, cfg := testProject(t)
	for _, token := range []string{"12345678901", "123456789012", "1234qwerASDF!", "contains space", "12345678901\n"} {
		t.Run(token, func(t *testing.T) {
			listened := false
			_, err := start(root, cfg, &testProvider{}, automation.ServerOptions{Address: "127.0.0.1:8765", Token: token}, func(string, string) (net.Listener, error) {
				listened = true
				return nil, errors.New("test listener")
			})
			valid := token == "123456789012" || token == "1234qwerASDF!"
			if err == nil || listened != valid {
				t.Fatal("incorrect token validation", listened, err)
			}
		})
	}
}

type testProvider struct {
	active   atomic.Bool
	closes   atomic.Int32
	releases atomic.Int32
	gooseMu  sync.Mutex
	goose    []goose.DataValue
	writes   atomic.Int32
}
type testAgent struct {
	control.Agent
	p *testProvider
}

func (p *testProvider) AgentActive() bool { return p.active.Load() }
func (p *testProvider) AcquireAgent() (control.Agent, error) {
	if !p.active.CompareAndSwap(false, true) {
		return nil, errors.New("owned")
	}
	return &testAgent{p: p}, nil
}
func (a *testAgent) Close() error {
	if a.p.active.Swap(false) {
		a.p.closes.Add(1)
	}
	return nil
}

func (a *testAgent) Release() error {
	if a.p.active.Swap(false) {
		a.p.releases.Add(1)
	}
	return nil
}
func (a *testAgent) Statuses() ([]control.ModuleStatus, error) {
	return []control.ModuleStatus{{ID: control.ModuleGoosePub, State: control.StatusStopped}}, nil
}

func (a *testAgent) GooseOutput(string) (goosepub.Snapshot, control.ModuleStatus, error) {
	a.p.gooseMu.Lock()
	defer a.p.gooseMu.Unlock()
	return goosepub.Snapshot{Data: goose.CloneDataValues(a.p.goose)}, control.ModuleStatus{ID: control.ModuleGoosePub, State: control.StatusRunning}, nil
}

func (a *testAgent) ApplyGoose(_ string, data []goose.DataValue, _, _ bool) (bool, error) {
	a.p.gooseMu.Lock()
	defer a.p.gooseMu.Unlock()
	a.p.goose = goose.CloneDataValues(data)
	a.p.writes.Add(1)
	return true, nil
}

func testProject(t *testing.T) (string, *config.Config) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "project")
	if err := profiles.CreateProject(root); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, cfg
}

func TestHTTPAuthorizationAndOrigin(t *testing.T) {
	s := &Server{stopping: make(chan struct{})}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, tc := range []struct {
		token, origin string
		want          int
	}{
		{"", "", 401}, {"wrong", "", 401}, {testToken, "http://evil.example", 403},
		{testToken, "http://127.0.0.1:8765", 204}, {testToken, "", 204},
	} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8765/mcp", nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		s.protect(next, testToken, "http").ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("status=%d want=%d", w.Code, tc.want)
		}
	}
}

func TestStartFailureDoesNotAcquireOwnership(t *testing.T) {
	root, cfg := testProject(t)
	p := &testProvider{}
	listen := func(string, string) (net.Listener, error) { return nil, errors.New("occupied") }
	if _, err := start(root, cfg, p, automation.ServerOptions{Address: "127.0.0.1:8765", Token: testToken}, listen); err == nil {
		t.Fatal("listen failure ignored")
	}
	if p.active.Load() || p.closes.Load() != 0 {
		t.Fatal("failed start took control or stopped publishers")
	}
	for _, opts := range []automation.ServerOptions{{Address: "hostname:8765", Token: testToken}, {Address: "127.0.0.1:0", Token: testToken}, {Address: "127.0.0.1:8765", Token: "short"}, {Address: "127.0.0.1:8765", Token: testToken + "\n"}} {
		if _, err := start(root, cfg, p, opts, listen); err == nil {
			t.Fatal("invalid options accepted")
		}
	}
	for _, name := range []string{"mcp_server.crt", "mcp_server.key"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatal("HTTP created TLS artifacts")
		}
	}
}

type authenticatedTransport struct {
	token   string
	blocked atomic.Bool
	base    *http.Transport
}

func (t *authenticatedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.blocked.Load() {
		return nil, errors.New("simulated network loss")
	}
	copy := r.Clone(r.Context())
	copy.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(copy)
}

func liveServer(t *testing.T) (*Server, *testProvider, string) {
	t.Helper()
	root, cfg := testProject(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	p := &testProvider{}
	s, err := start(root, cfg, p, automation.ServerOptions{Address: listener.Addr().String(), Token: testToken}, func(string, string) (net.Listener, error) { return listener, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Stop(); err != nil {
			t.Error(err)
		}
	})
	return s, p, root
}
func connectClient(t *testing.T, s *Server) (*mcp.ClientSession, *authenticatedTransport) {
	t.Helper()
	tr := &authenticatedTransport{token: testToken, base: http.DefaultTransport.(*http.Transport).Clone()}
	t.Cleanup(tr.base.CloseIdleConnections)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "PBMT test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: s.Status().Address, HTTPClient: &http.Client{Transport: tr}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session, tr
}
func waitStopped(t *testing.T, s *Server, p *testProvider) {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(8 * time.Second):
		t.Fatal("server did not stop after disconnection/change")
	}
	if s.Status().Running || p.active.Load() || p.closes.Load() != 1 {
		t.Fatal("shutdown did not revoke exactly once")
	}
}

func TestStreamableHTTPToolsAndDisconnect(t *testing.T) {
	s, p, _ := liveServer(t)
	client, _ := connectClient(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	list, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "cancel_sequence,cancel_test,get_catalog,get_sequence,get_status,get_test,goose_get,goose_set,run_test,sv_get,sv_set,t_set_goose,t_set_sv" {
		t.Fatal("wrong exposed tools", names)
	}
	for _, name := range []string{"get_catalog", "get_status"} {
		r, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		if err != nil || r.IsError || r.StructuredContent == nil {
			t.Fatalf("%s: %+v %v", name, r, err)
		}
	}
	r, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "get_status", Arguments: map[string]any{"typo": true}})
	if err != nil || !r.IsError {
		t.Fatal("unknown fields ignored", r, err)
	}
	// A second session cannot replace the controller, even with the correct token.
	req, _ := http.NewRequestWithContext(ctx, "POST", s.Status().Address, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"other","version":"1"}}}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatal("second owner accepted", resp.StatusCode)
	}
	// Leave the first client idle through a ping: idleness is not connection loss.
	timer := time.NewTimer(pingInterval + 500*time.Millisecond)
	defer timer.Stop()
	select {
	case <-s.done:
		t.Fatal("healthy idle client disconnected", s.Status())
	case <-timer.C:
	}
	if !s.Status().Connected {
		t.Fatal("initialized client not shown")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	waitStopped(t, s, p)
}

func TestLostHeartbeatAndExternalEditStopControl(t *testing.T) {
	for _, kind := range []string{"heartbeat", "source_edit"} {
		t.Run(kind, func(t *testing.T) {
			s, p, root := liveServer(t)
			client, tr := connectClient(t, s)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "get_status", Arguments: map[string]any{}}); err != nil {
				t.Fatal(err)
			}
			if kind == "heartbeat" {
				tr.blocked.Store(true)
			} else {
				path := filepath.Join(root, "cfg", "goose_sub.yaml")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(raw, []byte("\n# changed\n")...), 0600); err != nil {
					t.Fatal(err)
				}
			}
			waitStopped(t, s, p)
			if s.Status().Error == "" {
				t.Fatal("automatic stop reason missing")
			}
		})
	}
}
