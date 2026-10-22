package opcuatest

import (
	"net"
	"strconv"
	"sync"
)

// proxy forwards TCP connections to a target. Drop closes every live
// connection and keeps listening, as a network loss does; a target that
// refuses connections makes the server unreachable, as a stopped one is.
// The library's server does not drop live connections on Close, so a loss
// can only be simulated this way.
type proxy struct {
	ln net.Listener
	wg sync.WaitGroup

	mu     sync.Mutex
	target string
	conns  map[net.Conn]struct{}
	closed bool
}

func newProxy() (*proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	p := &proxy{ln: ln, conns: map[net.Conn]struct{}{}}

	p.wg.Add(1)
	go p.accept()

	return p, nil
}

func (p *proxy) port() int {
	return p.ln.Addr().(*net.TCPAddr).Port
}

func (p *proxy) setTarget(addr string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.target = addr
}

// drop closes every live connection; new ones are still accepted.
func (p *proxy) drop() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for c := range p.conns {
		_ = c.Close()
	}

	clear(p.conns)
}

// close stops listening, drops every connection and waits for the
// forwarding goroutines.
func (p *proxy) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()

		return
	}

	p.closed = true
	p.mu.Unlock()

	_ = p.ln.Close()
	p.drop()
	p.wg.Wait()
}

func (p *proxy) accept() {
	defer p.wg.Done()

	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}

		p.mu.Lock()
		target := p.target
		p.mu.Unlock()

		upstream, err := net.Dial("tcp", target)
		if err != nil {
			_ = client.Close()

			continue
		}

		if !p.track(client, upstream) {
			return
		}

		p.wg.Add(2)

		go p.pipe(upstream, client)
		go p.pipe(client, upstream)
	}
}

// track registers a connection pair, or closes it when the proxy is closed.
func (p *proxy) track(conns ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		for _, c := range conns {
			_ = c.Close()
		}

		return false
	}

	for _, c := range conns {
		p.conns[c] = struct{}{}
	}

	return true
}

// pipe copies src to dst; closing either side ends both.
func (p *proxy) pipe(dst, src net.Conn) {
	defer p.wg.Done()
	defer func() {
		_ = dst.Close()
		_ = src.Close()
		p.forget(dst, src)
	}()

	buf := make([]byte, 32<<10)

	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}

		if err != nil {
			return
		}
	}
}

// forget removes ended connections from the map, so a long run does not
// keep every connection it ever made.
func (p *proxy) forget(conns ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, c := range conns {
		delete(p.conns, c)
	}
}

// freePort returns a port that was free on 127.0.0.1 a moment ago.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()

	return l.Addr().(*net.TCPAddr).Port, nil
}

func addr(port int) string {
	return "127.0.0.1:" + strconv.Itoa(port)
}
