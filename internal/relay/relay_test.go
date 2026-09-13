package relay

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

func freePort(t *testing.T, network string) int {
	t.Helper()
	if network == "udp" {
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).Port
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startTCPServer(t *testing.T, prefix string) (int, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				n, err := c.Read(buf)
				if err != nil {
					return
				}
				_, _ = io.WriteString(c, prefix+string(buf[:n]))
			}(c)
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()
	return l.Addr().(*net.TCPAddr).Port, func() { cancel(); _ = l.Close() }
}

func TestTCPRelayAndTargetUpdate(t *testing.T) {
	b1, stop1 := startTCPServer(t, "one:")
	defer stop1()
	b2, stop2 := startTCPServer(t, "two:")
	defer stop2()
	a := freePort(t, "tcp")
	r, err := New(Config{ListenIP: "127.0.0.1", ListenPort: a, TargetHost: "127.0.0.1", TargetPort: b1, Networks: []string{"tcp"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := r.Run(ctx); err != nil {
			t.Errorf("run: %v", err)
		}
	}()
	waitTCP(t, a)
	if got := tcpRoundTrip(t, a, "hello"); got != "one:hello" {
		t.Fatalf("got %q", got)
	}
	if err := r.Update(Config{ListenIP: "127.0.0.1", ListenPort: a, TargetHost: "127.0.0.1", TargetPort: b2, Networks: []string{"tcp"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := tcpRoundTrip(t, a, "hello"); got != "two:hello" {
		t.Fatalf("got %q", got)
	}
}

func tcpRoundTrip(t *testing.T, port int, payload string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, payload); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 1024)
	n, err := c.Read(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(b[:n])
}

func waitTCP(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 50*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("relay did not start")
}

func startUDPServer(t *testing.T, prefix string) (int, func()) {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		b := make([]byte, 65535)
		for {
			n, addr, err := c.ReadFromUDP(b)
			if err != nil {
				return
			}
			_, _ = c.WriteToUDP([]byte(prefix+string(b[:n])), addr)
		}
	}()
	return c.LocalAddr().(*net.UDPAddr).Port, func() { _ = c.Close() }
}

func TestUDPRelayAndTargetUpdate(t *testing.T) {
	b1, stop1 := startUDPServer(t, "one:")
	defer stop1()
	b2, stop2 := startUDPServer(t, "two:")
	defer stop2()
	a := freePort(t, "udp")
	r, err := New(Config{ListenIP: "127.0.0.1", ListenPort: a, TargetHost: "127.0.0.1", TargetPort: b1, Networks: []string{"udp"}, UDPIdleTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := r.Run(ctx); err != nil {
			t.Errorf("run: %v", err)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	if got := udpRoundTrip(t, a, "hello"); got != "one:hello" {
		t.Fatalf("got %q", got)
	}
	if err := r.Update(Config{ListenIP: "127.0.0.1", ListenPort: a, TargetHost: "127.0.0.1", TargetPort: b2, Networks: []string{"udp"}, UDPIdleTimeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := udpRoundTrip(t, a, "hello"); got != "two:hello" {
		t.Fatalf("got %q", got)
	}
}

func udpRoundTrip(t *testing.T, port int, payload string) string {
	t.Helper()
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}
	c, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 1024)
	n, err := c.Read(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(b[:n])
}

func TestTLSServiceBehindTCPRelay(t *testing.T) {
	cert := testCertificate(t)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				b := make([]byte, 1024)
				n, err := c.Read(b)
				if err != nil {
					return
				}
				_, _ = io.WriteString(c, "tls:"+string(b[:n]))
			}(c)
		}
	}()

	bPort := listener.Addr().(*net.TCPAddr).Port
	aPort := freePort(t, "tcp")
	r, err := New(Config{ListenIP: "127.0.0.1", ListenPort: aPort, TargetHost: "127.0.0.1", TargetPort: bPort, Networks: []string{"tcp"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := r.Run(ctx); err != nil {
			t.Errorf("run: %v", err)
		}
	}()
	waitTCP(t, aPort)

	client, err := tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", aPort), &tls.Config{InsecureSkipVerify: true, ServerName: "localhost", MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("TLS handshake through relay failed: %v", err)
	}
	defer client.Close()
	if _, err := io.WriteString(client, "encrypted-payload"); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 1024)
	n, err := client.Read(b)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b[:n]); got != "tls:encrypted-payload" {
		t.Fatalf("got %q", got)
	}
}

func testCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestValidation(t *testing.T) {
	_, err := New(Config{ListenPort: 1, TargetHost: "x", TargetPort: 2, Networks: []string{"quic"}})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unexpected error: %v", err)
	}
}
