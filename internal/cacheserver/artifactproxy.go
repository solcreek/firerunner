package cacheserver

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const (
	// artifactTwirpBase is the Twirp route prefix of the GitHub Actions
	// artifact service (actions/upload-artifact and download-artifact v4+).
	artifactTwirpBase = "/twirp/github.actions.results.api.v1.ArtifactService/"

	// DefaultArtifactUpstream is the github.com Results service that owns
	// artifacts. The runner receives it per job as ResultsServiceUrl and would
	// have exported it as ACTIONS_RESULTS_URL; a cache-redirect golden diverts
	// that variable here, so the server has to know where to send artifact
	// traffic back to. GHES deployments override it with SetArtifactUpstream.
	DefaultArtifactUpstream = "https://results-receiver.actions.githubusercontent.com/"

	// maxArtifactRPCBody bounds a forwarded Twirp request body. Artifact RPCs
	// carry only small JSON metadata (names, ids, sizes); the archive itself is
	// streamed by the client straight to the signed blob URL the upstream
	// returns and never passes through this server.
	maxArtifactRPCBody = 1 << 20
)

// SetArtifactUpstream configures where ArtifactService RPCs are forwarded.
// An empty value disables forwarding: artifact RPCs then fail fast with a
// Twirp "unimplemented" error that names this setting, instead of the opaque
// 404 the toolkit would otherwise surface. Call before serving.
func (s *Server) SetArtifactUpstream(raw string) error {
	if raw == "" {
		s.mu.Lock()
		s.artifactProxy = nil
		s.mu.Unlock()
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("artifact upstream %q: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("artifact upstream %q: must be an absolute http(s) URL", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("artifact upstream %q: must not carry a query or fragment", raw)
	}
	p := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// SetURL joins the upstream base path with the inbound Twirp path and
			// rewrites Host to the upstream. It deliberately does not add
			// X-Forwarded-* headers: the upstream is GitHub, and the guest's
			// per-slot address is meaningless (and none of its business) there.
			pr.SetURL(u)
		},
		Transport:    artifactTransport(),
		ErrorHandler: s.artifactProxyError,
	}
	s.mu.Lock()
	s.artifactProxy = p
	s.mu.Unlock()
	return nil
}

// artifactTransport is a bounded HTTP transport for the upstream hop. The
// server sits between a guest and the internet, so every phase gets a deadline
// rather than inheriting the client's patience.
func artifactTransport() http.RoundTripper {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          32,
		ForceAttemptHTTP2:     true,
	}
}

// handleArtifactProxy forwards one ArtifactService RPC to the configured
// upstream and relays the response verbatim.
//
// Only this one Twirp service is forwarded, and only to the operator-configured
// upstream, so the server never acts as a general proxy for guests. The
// client's Authorization header (its ACTIONS_RUNTIME_TOKEN) is passed through
// untouched: it is GitHub's credential for GitHub's service, and this server
// neither needs nor is able to validate it. The Twirp reply — including any
// Twirp error envelope such as 409 already_exists — is returned as-is so the
// toolkit sees exactly what GitHub said.
func (s *Server) handleArtifactProxy(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	p := s.artifactProxy
	s.mu.Unlock()
	method := strings.TrimPrefix(r.URL.Path, artifactTwirpBase)
	if p == nil {
		s.artifactErrors.Add(1)
		s.log.Warn("artifact rpc refused: no upstream configured", "method", method)
		twirpError(w, http.StatusNotImplemented, "unimplemented",
			"artifact service forwarding is disabled on this cache-server; configure --artifact-upstream")
		return
	}
	if r.ContentLength > maxArtifactRPCBody {
		s.artifactErrors.Add(1)
		twirpError(w, http.StatusRequestEntityTooLarge, "invalid_argument", "artifact rpc body too large")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxArtifactRPCBody)
	s.artifactRPCs.Add(1)
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	p.ServeHTTP(rec, r)
	if rec.proxyErr || rec.status >= 500 {
		s.artifactErrors.Add(1)
	}
	s.log.Info("artifact rpc forwarded", "method", method, "status", rec.status)
}

// artifactProxyError turns an upstream transport failure into a Twirp error
// envelope so the toolkit reports a readable reason rather than a bare 502.
func (s *Server) artifactProxyError(w http.ResponseWriter, r *http.Request, err error) {
	if rec, ok := w.(*statusRecorder); ok {
		rec.proxyErr = true
	}
	code, twirpCode, msg := http.StatusBadGateway, "unavailable", "artifact upstream unreachable: "+err.Error()
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		code, twirpCode, msg = http.StatusRequestEntityTooLarge, "invalid_argument", "artifact rpc body too large"
	}
	s.log.Warn("artifact rpc failed", "method", strings.TrimPrefix(r.URL.Path, artifactTwirpBase), "err", err)
	twirpError(w, code, twirpCode, msg)
}

// statusRecorder captures the status the proxy wrote, and whether the reply
// came from the proxy's own error path rather than the upstream, so the handler
// can count failures without buffering the body.
type statusRecorder struct {
	http.ResponseWriter
	status   int
	proxyErr bool
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer (the reverse
// proxy uses it to flush streamed responses).
func (sr *statusRecorder) Unwrap() http.ResponseWriter { return sr.ResponseWriter }
