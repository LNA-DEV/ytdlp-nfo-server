// Package fakereddit is an HTTPS server that answers for Reddit's hosts,
// Arctic Shift and the media hosts in tests. Its certificate is signed by
// a test CA; clients dial it through Dial and trust Roots, so requests run
// the real TLS path without touching the network.
package fakereddit

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Hosts the server answers for.
var Hosts = []string{
	"www.reddit.com", "reddit.com", "i.redd.it", "preview.redd.it", "external-preview.redd.it", "v.redd.it",
	"arctic-shift.photon-reddit.com", "i.imgur.com", "example.com",
}

// Request is a recorded request.
type Request struct {
	Host   string
	Path   string
	Query  string
	Header http.Header
}

// Server is the fake.
type Server struct {
	srv   *httptest.Server
	Roots *x509.CertPool
	CAPEM []byte // the test CA, for a client in another process

	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	reqs     []Request
}

// New starts a server; it stops when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fakereddit CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "fakereddit"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), DNSNames: Hosts,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Roots: x509.NewCertPool(), handlers: map[string]http.HandlerFunc{},
		CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})}
	s.Roots.AddCert(ca)
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	s.srv.EnableHTTP2 = true
	s.srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		NextProtos: []string{"h2", "http/1.1"}}
	s.srv.StartTLS()
	t.Cleanup(s.srv.Close)
	return s
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, Request{Host: host, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone()})
	h := s.handlers[host]
	s.mu.Unlock()
	if h == nil {
		http.NotFound(w, r)
		return
	}
	h(w, r)
}

// Handle sets the handler of a host.
func (s *Server) Handle(host string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[host] = h
}

// Requests returns the recorded requests.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

// Count returns how many requests went to host with a path prefix.
func (s *Server) Count(host, pathPrefix string) int {
	n := 0
	for _, r := range s.Requests() {
		if r.Host == host && strings.HasPrefix(r.Path, pathPrefix) {
			n++
		}
	}
	return n
}

// Addr returns the address every host is served on.
func (s *Server) Addr() string { return s.srv.Listener.Addr().String() }

// Dial connects to the server, whatever address is asked for.
func (s *Server) Dial(ctx context.Context, network, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, s.srv.Listener.Addr().String())
}
