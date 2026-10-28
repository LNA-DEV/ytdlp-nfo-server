// Package browser is the HTTP client of the Reddit providers. It presents
// one consistent desktop Chrome: TLS and HTTP/2 fingerprint (azuretls),
// headers in Chrome's order, navigation headers, a persistent cookie jar
// and an optional proxy (Reddit concept §5.1). It only talks to allowlisted
// hosts over HTTPS and never to private addresses (§10).
package browser

import (
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	nethttp "net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	azuretls "github.com/Noooste/azuretls-client"
	http "github.com/Noooste/fhttp"
	utls "github.com/Noooste/utls"
	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// Chrome is the browser identity: User-Agent and client hints of one
// Chrome version, matching the TLS profile of the library (§5.1).
var Chrome = Profile{
	UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36",
	SecChUa:   `"Google Chrome";v="135", "Not-A.Brand";v="8", "Chromium";v="135"`,
	Platform:  `"Windows"`,
	Language:  "en-US,en;q=0.9",
}

// Profile is a browser identity.
type Profile struct {
	UserAgent string
	SecChUa   string
	Platform  string
	Language  string
}

// DefaultHosts are the hosts the Reddit code may contact (§10).
var DefaultHosts = []string{
	"www.reddit.com", "reddit.com",
	"i.redd.it", "preview.redd.it", "external-preview.redd.it", "v.redd.it",
	"i.imgur.com", // still images of link posts (§7.6)
	"arctic-shift.photon-reddit.com",
}

// Options configure a client.
type Options struct {
	Proxy   string        // http://, https:// or socks5:// URL; "" = direct
	JarPath string        // persistent cookie jar; "" = in memory
	Timeout time.Duration // per request; default 30 s
	Hosts   []string      // allowed hosts; nil = DefaultHosts
	// Tests: trust these roots instead of the system's, and dial here
	// instead of the checked direct dial.
	RootCAs *x509.CertPool
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	// Plain sends requests with Go's own HTTP client and a descriptive
	// User-Agent instead of the browser identity (reddit-probe compares
	// both).
	Plain bool
	// OnResponse sees every response with a body (reddit-probe).
	OnResponse func(url string, status int, body []byte)
}

// Client sends requests as the browser identity. It is safe for concurrent
// use.
type Client struct {
	opts    Options
	profile Profile
	jar     *Jar
	hosts   map[string]bool

	mu   sync.Mutex
	sess *azuretls.Session
}

// Response is a received response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
	URL    string // after redirects
}

// ErrNotAllowed is returned for URLs outside the allowlist.
var ErrNotAllowed = errors.New("host not allowed")

// ErrTooLarge is returned by Download for a file over its limit.
var ErrTooLarge = errors.New("file too large")

// New creates a client.
func New(opts Options) (*Client, error) {
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}
	hosts := opts.Hosts
	if hosts == nil {
		hosts = DefaultHosts
	}
	c := &Client{opts: opts, profile: Chrome, jar: NewJar(opts.JarPath, map[string]string{"over18": "1"}),
		hosts: map[string]bool{}}
	for _, h := range hosts {
		c.hosts[strings.ToLower(h)] = true
	}
	if opts.Proxy != "" {
		if _, err := url.Parse(opts.Proxy); err != nil {
			return nil, fmt.Errorf("proxy: %w", err)
		}
	}
	return c, nil
}

// Jar returns the cookie jar.
func (c *Client) Jar() *Jar { return c.jar }

// Allowed reports whether a URL may be requested: HTTPS to an allowlisted
// host.
func (c *Client) Allowed(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && c.hosts[strings.ToLower(u.Hostname())] &&
		(u.Port() == "" || u.Port() == "443")
}

// session returns the shared session, creating it on first use.
func (c *Client) session() (*azuretls.Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess != nil {
		return c.sess, nil
	}
	s := azuretls.NewSession()
	s.Browser = azuretls.Chrome
	s.UserAgent = c.profile.UserAgent
	s.CookieJar = c.jar
	s.TimeOut = c.opts.Timeout
	s.MaxRedirects = 10
	// Certificates are verified below with VerifyConnection. The library's
	// own pinning would open an extra direct connection per host, past the
	// proxy and the address check.
	s.InsecureSkipVerify = true
	roots := c.opts.RootCAs
	s.ModifyConfig = func(cfg *utls.Config) error {
		host := cfg.ServerName
		cfg.VerifyConnection = func(cs utls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no certificate")
			}
			vo := x509.VerifyOptions{DNSName: host, Roots: roots, Intermediates: x509.NewCertPool()}
			for _, ic := range cs.PeerCertificates[1:] {
				vo.Intermediates.AddCert(ic)
			}
			_, err := cs.PeerCertificates[0].Verify(vo)
			return err
		}
		return nil
	}
	s.CheckRedirect = func(req *azuretls.Request, via []*azuretls.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if !c.Allowed(req.Url) {
			return fmt.Errorf("redirect to %s: %w", hostOf(req.Url), ErrNotAllowed)
		}
		return nil
	}
	switch {
	case c.opts.Dial != nil:
		s.Dial = c.opts.Dial
	case c.opts.Proxy != "":
		if err := s.SetProxy(c.opts.Proxy); err != nil {
			s.Close()
			return nil, fmt.Errorf("proxy: %w", err)
		}
	default:
		s.Dial = checkedDial(c.opts.Timeout)
	}
	c.sess = s
	return s, nil
}

// Close releases the connections.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess != nil {
		c.sess.Close()
		c.sess = nil
	}
}

// Request kinds, each with Chrome's headers and order.
type kind int

const (
	kindDocument kind = iota // a navigation (listing or post page)
	kindPartial              // a fetch() of the page (more posts)
	kindJSON                 // an API request (Arctic Shift)
	kindImage                // an <img> load
)

func (c *Client) headers(k kind, target *url.URL, referer string) azuretls.OrderedHeaders {
	p := c.profile
	site := "none"
	if referer != "" {
		if r, err := url.Parse(referer); err == nil && r.Hostname() == target.Hostname() {
			site = "same-origin"
		} else {
			site = "cross-site"
		}
	}
	var h azuretls.OrderedHeaders
	add := func(k, v string) {
		if v != "" {
			h = append(h, []string{k, v})
		}
	}
	switch k {
	case kindDocument:
		add("sec-ch-ua", p.SecChUa)
		add("sec-ch-ua-mobile", "?0")
		add("sec-ch-ua-platform", p.Platform)
		add("upgrade-insecure-requests", "1")
		add("user-agent", p.UserAgent)
		add("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
		add("sec-fetch-site", site)
		add("sec-fetch-mode", "navigate")
		add("sec-fetch-user", "?1")
		add("sec-fetch-dest", "document")
		add("referer", referer)
		add("accept-encoding", "gzip, deflate, br, zstd")
		add("accept-language", p.Language)
		add("priority", "u=0, i")
	case kindPartial, kindJSON:
		add("sec-ch-ua-platform", p.Platform)
		add("user-agent", p.UserAgent)
		add("sec-ch-ua", p.SecChUa)
		add("sec-ch-ua-mobile", "?0")
		if k == kindJSON {
			add("accept", "application/json")
		} else {
			add("accept", "*/*")
		}
		if site == "none" {
			site = "cross-site"
		}
		add("sec-fetch-site", site)
		add("sec-fetch-mode", "cors")
		add("sec-fetch-dest", "empty")
		add("referer", referer)
		add("accept-encoding", "gzip, deflate, br, zstd")
		add("accept-language", p.Language)
		add("priority", "u=1, i")
	case kindImage:
		add("sec-ch-ua-platform", p.Platform)
		add("user-agent", p.UserAgent)
		add("sec-ch-ua", p.SecChUa)
		add("sec-ch-ua-mobile", "?0")
		add("accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
		add("sec-fetch-site", "cross-site")
		add("sec-fetch-mode", "no-cors")
		add("sec-fetch-dest", "image")
		add("referer", referer)
		add("accept-encoding", "gzip, deflate, br, zstd")
		add("accept-language", p.Language)
		add("priority", "i")
	}
	return h
}

func (c *Client) do(ctx context.Context, k kind, raw, referer string, stream bool) (*azuretls.Response, error) {
	if !c.Allowed(raw) {
		return nil, fmt.Errorf("%s: %w", hostOf(raw), ErrNotAllowed)
	}
	u, _ := url.Parse(raw)
	s, err := c.session()
	if err != nil {
		return nil, err
	}
	req := &azuretls.Request{Method: http.MethodGet, Url: raw, OrderedHeaders: c.headers(k, u, referer),
		TimeOut: c.opts.Timeout, IgnoreBody: stream, NoCookie: k == kindImage || k == kindJSON}
	req.SetContext(ctx)
	resp, err := s.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return resp, nil
}

func (c *Client) get(ctx context.Context, k kind, raw, referer string, max int64) (*Response, error) {
	if c.opts.Plain {
		return c.plainGet(ctx, k, raw, max)
	}
	resp, err := c.do(ctx, k, raw, referer, false)
	if err != nil {
		return nil, err
	}
	if int64(len(resp.Body)) > max {
		return nil, fmt.Errorf("larger than %d MB: %w", max>>20, ErrTooLarge)
	}
	if c.opts.OnResponse != nil {
		c.opts.OnResponse(resp.Url, resp.StatusCode, resp.Body)
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body, URL: resp.Url}, nil
}

// plainGet is the request of a client without the browser identity.
func (c *Client) plainGet(ctx context.Context, k kind, raw string, max int64) (*Response, error) {
	if !c.Allowed(raw) {
		return nil, fmt.Errorf("%s: %w", hostOf(raw), ErrNotAllowed)
	}
	tr := &nethttp.Transport{Proxy: nil, ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{RootCAs: c.opts.RootCAs}}
	switch {
	case c.opts.Dial != nil:
		tr.DialContext = c.opts.Dial
	case c.opts.Proxy != "":
		u, err := url.Parse(c.opts.Proxy)
		if err != nil {
			return nil, err
		}
		tr.Proxy = nethttp.ProxyURL(u)
	default:
		tr.DialContext = checkedDial(c.opts.Timeout)
	}
	hc := &nethttp.Client{Transport: tr, Timeout: c.opts.Timeout,
		CheckRedirect: func(req *nethttp.Request, via []*nethttp.Request) error {
			if len(via) >= 10 || !c.Allowed(req.URL.String()) {
				return ErrNotAllowed
			}
			return nil
		}}
	defer tr.CloseIdleConnections()
	req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ytdlp-nfo-server (reddit-probe)")
	if k == kindJSON {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("larger than %d MB: %w", max>>20, ErrTooLarge)
	}
	if c.opts.OnResponse != nil {
		c.opts.OnResponse(resp.Request.URL.String(), resp.StatusCode, body)
	}
	h := http.Header{}
	for k, v := range resp.Header {
		h[k] = v
	}
	return &Response{Status: resp.StatusCode, Header: h, Body: body, URL: resp.Request.URL.String()}, nil
}

// Document loads a page as a navigation; referer "" is a typed URL.
func (c *Client) Document(ctx context.Context, raw, referer string, max int64) (*Response, error) {
	return c.get(ctx, kindDocument, raw, referer, max)
}

// Partial loads what a page's script fetches (more posts).
func (c *Client) Partial(ctx context.Context, raw, referer string, max int64) (*Response, error) {
	return c.get(ctx, kindPartial, raw, referer, max)
}

// JSON calls an API.
func (c *Client) JSON(ctx context.Context, raw string, max int64) (*Response, error) {
	return c.get(ctx, kindJSON, raw, "https://arctic-shift.photon-reddit.com/", max)
}

// Download streams an image to w, at most max bytes. The response has no
// Body; a status other than 200 writes nothing.
func (c *Client) Download(ctx context.Context, raw, referer string, w io.Writer, max int64) (*Response, error) {
	if c.opts.Plain {
		r, err := c.plainGet(ctx, kindImage, raw, max)
		if err == nil && r.Status == 200 {
			_, err = w.Write(r.Body)
		}
		if r != nil {
			r.Body = nil
		}
		return r, err
	}
	resp, err := c.do(ctx, kindImage, raw, referer, true)
	if err != nil {
		return nil, err
	}
	defer resp.CloseBody()
	out := &Response{Status: resp.StatusCode, Header: resp.Header, URL: resp.Url}
	if resp.StatusCode != 200 {
		_, _ = io.CopyN(io.Discard, resp.RawBody, 64<<10)
		return out, nil
	}
	body, err := decode(resp.RawBody, resp.Header.Get("Content-Encoding"))
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(w, io.LimitReader(body, max+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if n > max {
		return nil, fmt.Errorf("larger than %d MB: %w", max>>20, ErrTooLarge)
	}
	return out, nil
}

func decode(r io.Reader, enc string) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "", "identity":
		return r, nil
	case "gzip":
		return gzip.NewReader(r)
	case "deflate":
		return flate.NewReader(r), nil
	case "br":
		return brotli.NewReader(r), nil
	case "zstd":
		d, err := zstd.NewReader(r)
		if err != nil {
			return nil, err
		}
		return d.IOReadCloser(), nil
	}
	return nil, fmt.Errorf("unsupported content encoding %q", enc)
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return "invalid URL"
}

// PublicAddr reports whether an address may be contacted: not loopback,
// private, link-local, unique-local, multicast or unspecified (§10).
func PublicAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsValid() && !a.IsLoopback() && !a.IsPrivate() && !a.IsLinkLocalUnicast() &&
		!a.IsLinkLocalMulticast() && !a.IsMulticast() && !a.IsUnspecified() && !a.IsInterfaceLocalMulticast() &&
		!netip.MustParsePrefix("100.64.0.0/10").Contains(a) // carrier-grade NAT
}

// PublicHost reports whether every address of a host is public (the
// check for link targets on hosts the user added, Reddit concept §7.6).
func PublicHost(ctx context.Context, host string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !PublicAddr(ip) {
			return false
		}
	}
	return true
}

// checkedDial resolves the host and dials the first public address. It
// checks after resolution, which also covers DNS rebinding.
func checkedDial(timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if !PublicAddr(ip) {
				return nil, fmt.Errorf("%s resolves to %s: %w", host, ip, ErrNotAllowed)
			}
		}
		var last error
		for _, ip := range ips {
			conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			last = err
		}
		if last == nil {
			last = fmt.Errorf("no address for %s", host)
		}
		return nil, last
	}
}
