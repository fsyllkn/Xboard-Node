package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const defaultUDPIdleTimeout = 90 * time.Second

type Config struct {
	ListenIP       string
	ListenPort     int
	TargetHost     string
	TargetPort     int
	Networks       []string
	UDPIdleTimeout time.Duration
}

func (c Config) normalized() (Config, error) {
	if c.ListenIP == "" {
		c.ListenIP = "0.0.0.0"
	}
	if c.ListenPort < 1 || c.ListenPort > 65535 {
		return Config{}, fmt.Errorf("invalid listen port %d", c.ListenPort)
	}
	if strings.TrimSpace(c.TargetHost) == "" {
		return Config{}, errors.New("target host is required")
	}
	if c.TargetPort < 1 || c.TargetPort > 65535 {
		return Config{}, fmt.Errorf("invalid target port %d", c.TargetPort)
	}
	seen := map[string]bool{}
	var networks []string
	for _, network := range c.Networks {
		n := strings.ToLower(strings.TrimSpace(network))
		if n == "" || seen[n] {
			continue
		}
		if n != "tcp" && n != "udp" {
			return Config{}, fmt.Errorf("unsupported relay network %q", network)
		}
		seen[n] = true
		networks = append(networks, n)
	}
	if len(networks) == 0 {
		return Config{}, errors.New("at least one relay network is required")
	}
	sort.Strings(networks)
	c.Networks = networks
	if c.UDPIdleTimeout <= 0 {
		c.UDPIdleTimeout = defaultUDPIdleTimeout
	}
	return c, nil
}

func (c Config) listenKey() string {
	return fmt.Sprintf("%s:%d|%s", c.ListenIP, c.ListenPort, strings.Join(c.Networks, ","))
}

func (c Config) targetKey() string {
	return net.JoinHostPort(c.TargetHost, fmt.Sprintf("%d", c.TargetPort))
}

type Metrics struct {
	ActiveTCPConnections int64
	TotalTCPConnections  uint64
	TCPDialErrors        uint64
	TCPBytesUp           uint64
	TCPBytesDown         uint64
	ActiveUDPSessions    int64
	TotalUDPSessions     uint64
	UDPBytesUp           uint64
	UDPBytesDown         uint64
}

type Runner struct {
	initial Config
	updates chan Config

	activeTCP atomic.Int64
	totalTCP  atomic.Uint64
	tcpErrors atomic.Uint64
	tcpUp     atomic.Uint64
	tcpDown   atomic.Uint64
	activeUDP atomic.Int64
	totalUDP  atomic.Uint64
	udpUp     atomic.Uint64
	udpDown   atomic.Uint64
}

func New(cfg Config) (*Runner, error) {
	normalized, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	return &Runner{initial: normalized, updates: make(chan Config, 1)}, nil
}

func (r *Runner) Update(cfg Config) error {
	normalized, err := cfg.normalized()
	if err != nil {
		return err
	}
	select {
	case r.updates <- normalized:
	default:
		select {
		case <-r.updates:
		default:
		}
		r.updates <- normalized
	}
	return nil
}

func (r *Runner) Metrics() Metrics {
	return Metrics{
		ActiveTCPConnections: r.activeTCP.Load(),
		TotalTCPConnections:  r.totalTCP.Load(),
		TCPDialErrors:         r.tcpErrors.Load(),
		TCPBytesUp:            r.tcpUp.Load(),
		TCPBytesDown:          r.tcpDown.Load(),
		ActiveUDPSessions:     r.activeUDP.Load(),
		TotalUDPSessions:      r.totalUDP.Load(),
		UDPBytesUp:            r.udpUp.Load(),
		UDPBytesDown:          r.udpDown.Load(),
	}
}

func (r *Runner) Run(ctx context.Context) error {
	current := r.initial
	inst, err := r.startInstance(ctx, current)
	if err != nil {
		return err
	}
	defer inst.close()

	for {
		select {
		case <-ctx.Done():
			return nil
		case next := <-r.updates:
			if next.listenKey() != current.listenKey() {
				sameEndpoint := next.ListenIP == current.ListenIP && next.ListenPort == current.ListenPort
				if sameEndpoint {
					// Changing TCP/UDP listeners on the same port requires releasing the old
					// socket first. Restore the old config if the replacement cannot start.
					inst.close()
					replacement, err := r.startInstance(ctx, next)
					if err != nil {
						restored, restoreErr := r.startInstance(ctx, current)
						if restoreErr != nil {
							return fmt.Errorf("relay listener update failed: %v; restore failed: %w", err, restoreErr)
						}
						inst = restored
						continue
					}
					inst = replacement
				} else {
					replacement, err := r.startInstance(ctx, next)
					if err != nil {
						// Keep the old, working listener alive if the replacement fails.
						continue
					}
					inst.close()
					inst = replacement
				}
			} else if next.targetKey() != current.targetKey() || next.UDPIdleTimeout != current.UDPIdleTimeout {
				inst.update(next)
			}
			current = next
		}
	}
}

type instance struct {
	runner *Runner
	ctx    context.Context
	cancel context.CancelFunc

	cfgMu sync.RWMutex
	cfg   Config

	tcpLn net.Listener
	udp   *udpRelay
	wg    sync.WaitGroup

	connMu sync.Mutex
	conns  map[net.Conn]struct{}
}

func (r *Runner) startInstance(parent context.Context, cfg Config) (*instance, error) {
	ctx, cancel := context.WithCancel(parent)
	inst := &instance{runner: r, ctx: ctx, cancel: cancel, cfg: cfg, conns: make(map[net.Conn]struct{})}

	var tcpLn net.Listener
	var udpConn *net.UDPConn
	var err error
	for _, network := range cfg.Networks {
		switch network {
		case "tcp":
			tcpLn, err = net.Listen("tcp", net.JoinHostPort(cfg.ListenIP, fmt.Sprintf("%d", cfg.ListenPort)))
			if err != nil {
				cancel()
				return nil, fmt.Errorf("listen tcp: %w", err)
			}
			inst.tcpLn = tcpLn
		case "udp":
			addr, resolveErr := net.ResolveUDPAddr("udp", net.JoinHostPort(cfg.ListenIP, fmt.Sprintf("%d", cfg.ListenPort)))
			if resolveErr != nil {
				if tcpLn != nil {
					_ = tcpLn.Close()
				}
				cancel()
				return nil, fmt.Errorf("resolve udp listen address: %w", resolveErr)
			}
			udpConn, err = net.ListenUDP("udp", addr)
			if err != nil {
				if tcpLn != nil {
					_ = tcpLn.Close()
				}
				cancel()
				return nil, fmt.Errorf("listen udp: %w", err)
			}
			inst.udp = newUDPRelay(ctx, udpConn, inst.target, cfg.UDPIdleTimeout, r)
		}
	}

	if inst.tcpLn != nil {
		inst.wg.Add(1)
		go func() {
			defer inst.wg.Done()
			inst.serveTCP()
		}()
	}
	if inst.udp != nil {
		inst.wg.Add(1)
		go func() {
			defer inst.wg.Done()
			inst.udp.run()
		}()
	}
	return inst, nil
}

func (i *instance) target() string {
	i.cfgMu.RLock()
	defer i.cfgMu.RUnlock()
	return i.cfg.targetKey()
}

func (i *instance) update(cfg Config) {
	i.cfgMu.Lock()
	oldTarget := i.cfg.targetKey()
	i.cfg = cfg
	i.cfgMu.Unlock()
	if i.udp != nil {
		i.udp.setIdleTimeout(cfg.UDPIdleTimeout)
		if oldTarget != cfg.targetKey() {
			i.udp.resetSessions()
		}
	}
}

func (i *instance) close() {
	i.cancel()
	if i.tcpLn != nil {
		_ = i.tcpLn.Close()
	}
	if i.udp != nil {
		i.udp.close()
	}
	i.closeConnections()
	i.wg.Wait()
}

func (i *instance) trackConn(conn net.Conn) {
	i.connMu.Lock()
	i.conns[conn] = struct{}{}
	i.connMu.Unlock()
}

func (i *instance) untrackConn(conn net.Conn) {
	i.connMu.Lock()
	delete(i.conns, conn)
	i.connMu.Unlock()
}

func (i *instance) closeConnections() {
	i.connMu.Lock()
	conns := make([]net.Conn, 0, len(i.conns))
	for conn := range i.conns {
		conns = append(conns, conn)
	}
	i.connMu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (i *instance) serveTCP() {
	for {
		conn, err := i.tcpLn.Accept()
		if err != nil {
			select {
			case <-i.ctx.Done():
				return
			default:
			}
			continue
		}
		i.runner.totalTCP.Add(1)
		i.runner.activeTCP.Add(1)
		i.trackConn(conn)
		i.wg.Add(1)
		go func(client net.Conn) {
			defer i.wg.Done()
			defer i.runner.activeTCP.Add(-1)
			defer i.untrackConn(client)
			defer client.Close()
			i.handleTCP(client)
		}(conn)
	}
}

func (i *instance) handleTCP(client net.Conn) {
	dialer := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	upstream, err := dialer.DialContext(i.ctx, "tcp", i.target())
	if err != nil {
		i.runner.tcpErrors.Add(1)
		return
	}
	i.trackConn(upstream)
	defer i.untrackConn(upstream)
	defer upstream.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, _ := io.Copy(upstream, client)
		if n > 0 {
			i.runner.tcpUp.Add(uint64(n))
		}
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		n, _ := io.Copy(client, upstream)
		if n > 0 {
			i.runner.tcpDown.Add(uint64(n))
		}
		closeWrite(client)
	}()
	wg.Wait()
}

func closeWrite(conn net.Conn) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
}

type udpRelay struct {
	ctx    context.Context
	conn   *net.UDPConn
	target func() string
	runner *Runner

	mu        sync.Mutex
	sessions  map[string]*udpSession
	idleNanos atomic.Int64
	closed    atomic.Bool
}

type udpSession struct {
	key        string
	client     *net.UDPAddr
	upstream   *net.UDPConn
	lastActive atomic.Int64
	parent     *udpRelay
	closeOnce  sync.Once
}

func newUDPRelay(ctx context.Context, conn *net.UDPConn, target func() string, idle time.Duration, runner *Runner) *udpRelay {
	u := &udpRelay{ctx: ctx, conn: conn, target: target, runner: runner, sessions: make(map[string]*udpSession)}
	u.idleNanos.Store(int64(idle))
	return u
}

func (u *udpRelay) setIdleTimeout(d time.Duration) { u.idleNanos.Store(int64(d)) }

func (u *udpRelay) run() {
	gcDone := make(chan struct{})
	go func() {
		defer close(gcDone)
		u.gcLoop()
	}()

	buf := make([]byte, 64*1024)
	for {
		n, clientAddr, err := u.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-u.ctx.Done():
				<-gcDone
				return
			default:
			}
			if u.closed.Load() {
				<-gcDone
				return
			}
			continue
		}
		session, err := u.getOrCreate(clientAddr)
		if err != nil {
			continue
		}
		session.touch()
		if _, err := session.upstream.Write(buf[:n]); err != nil {
			session.close()
			continue
		}
		u.runner.udpUp.Add(uint64(n))
	}
}

func (u *udpRelay) getOrCreate(client *net.UDPAddr) (*udpSession, error) {
	key := client.String()
	u.mu.Lock()
	if s := u.sessions[key]; s != nil {
		u.mu.Unlock()
		return s, nil
	}
	targetAddr, err := net.ResolveUDPAddr("udp", u.target())
	if err != nil {
		u.mu.Unlock()
		return nil, err
	}
	upstream, err := net.DialUDP("udp", nil, targetAddr)
	if err != nil {
		u.mu.Unlock()
		return nil, err
	}
	s := &udpSession{key: key, client: cloneUDPAddr(client), upstream: upstream, parent: u}
	s.touch()
	u.sessions[key] = s
	u.runner.totalUDP.Add(1)
	u.runner.activeUDP.Add(1)
	u.mu.Unlock()

	go s.replyLoop()
	return s, nil
}

func (s *udpSession) touch() { s.lastActive.Store(time.Now().UnixNano()) }

func (s *udpSession) replyLoop() {
	buf := make([]byte, 64*1024)
	for {
		n, err := s.upstream.Read(buf)
		if err != nil {
			s.close()
			return
		}
		s.touch()
		if _, err := s.parent.conn.WriteToUDP(buf[:n], s.client); err != nil {
			s.close()
			return
		}
		s.parent.runner.udpDown.Add(uint64(n))
	}
}

func (s *udpSession) close() {
	s.closeOnce.Do(func() {
		_ = s.upstream.Close()
		s.parent.mu.Lock()
		if s.parent.sessions[s.key] == s {
			delete(s.parent.sessions, s.key)
			s.parent.runner.activeUDP.Add(-1)
		}
		s.parent.mu.Unlock()
	})
}

func (u *udpRelay) gcLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-u.ctx.Done():
			return
		case now := <-ticker.C:
			idle := time.Duration(u.idleNanos.Load())
			var stale []*udpSession
			u.mu.Lock()
			for _, s := range u.sessions {
				if now.Sub(time.Unix(0, s.lastActive.Load())) >= idle {
					stale = append(stale, s)
				}
			}
			u.mu.Unlock()
			for _, s := range stale {
				s.close()
			}
		}
	}
}

func (u *udpRelay) resetSessions() {
	u.mu.Lock()
	sessions := make([]*udpSession, 0, len(u.sessions))
	for _, s := range u.sessions {
		sessions = append(sessions, s)
	}
	u.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
}

func (u *udpRelay) close() {
	if !u.closed.CompareAndSwap(false, true) {
		return
	}
	_ = u.conn.Close()
	u.resetSessions()
}

func cloneUDPAddr(a *net.UDPAddr) *net.UDPAddr {
	out := *a
	if a.IP != nil {
		out.IP = append(net.IP(nil), a.IP...)
	}
	return &out
}
