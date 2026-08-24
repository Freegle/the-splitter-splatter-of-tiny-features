// Adapted from seifghazi/claude-code-proxy (MIT), commit 02c9c766.
package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// hopByHopHeaders are stripped from both the outbound request and the
// returned response, per RFC 7230 6.1. Proxy-* headers are handled
// separately below since they are a prefix match, not a fixed set.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Transfer-Encoding",
	"Upgrade",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailers",
}

// cleanForwardHeaders removes hop-by-hop headers, any header named in the
// request's own Connection header, and any Proxy-* header, from h in
// place. When stripAcceptEncoding is true, Accept-Encoding is also removed
// so upstream is never asked to compress its response; see DECISIONS.md
// for why the proxy always requests uncompressed responses.
func cleanForwardHeaders(h http.Header, stripAcceptEncoding bool) {
	if conn := h.Get("Connection"); conn != "" {
		for _, name := range strings.Split(conn, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
	for name := range h {
		if strings.HasPrefix(strings.ToLower(name), "proxy-") {
			h.Del(name)
		}
	}
	if stripAcceptEncoding {
		h.Del("Accept-Encoding")
	}
}

// buildUpstreamRequest constructs the request to send to s.upstream,
// preserving r's method, path, query and all non-hop-by-hop headers
// (including auth) verbatim, with body replaced by the already-read body
// bytes.
func (s *Server) buildUpstreamRequest(ctx context.Context, r *http.Request, body []byte) (*http.Request, error) {
	dest := *s.upstream
	dest.Path = joinURLPath(s.upstream.Path, r.URL.Path)
	dest.RawQuery = r.URL.RawQuery

	req, err := http.NewRequestWithContext(ctx, r.Method, dest.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building upstream request to %s: %w", dest.String(), err)
	}
	req.Header = r.Header.Clone()
	cleanForwardHeaders(req.Header, true)
	req.Host = dest.Host
	req.ContentLength = int64(len(body))
	return req, nil
}

// joinURLPath joins a base path and a request path with exactly one slash
// between them.
func joinURLPath(base, reqPath string) string {
	baseSlash := strings.HasSuffix(base, "/")
	reqSlash := strings.HasPrefix(reqPath, "/")
	switch {
	case baseSlash && reqSlash:
		return base + reqPath[1:]
	case !baseSlash && !reqSlash:
		return base + "/" + reqPath
	default:
		return base + reqPath
	}
}

// copyResponseHeaders copies every header from src into dst, then strips
// hop-by-hop headers from dst so the client never sees, for example, a
// stale Transfer-Encoding that would conflict with net/http's own framing.
func copyResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		for _, v := range values {
			dst.Add(name, v)
		}
	}
	cleanForwardHeaders(dst, false)
}

// writeUpstreamError writes a 502 response carrying err's text, used when
// the upstream round trip itself fails (as opposed to upstream returning an
// HTTP error status, which is forwarded verbatim like any other response).
func writeUpstreamError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	io.WriteString(w, err.Error())
}

// plainForward forwards r to upstream verbatim with no capture and no
// session id derivation. It is used for every uncaptured path and as the
// panic-recovery fallback for a captured one.
func (s *Server) plainForward(w http.ResponseWriter, r *http.Request, body []byte) {
	upReq, err := s.buildUpstreamRequest(r.Context(), r, body)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}

	resp, err := s.client.Do(upReq)
	if err != nil {
		writeUpstreamError(w, fmt.Errorf("forwarding to upstream: %w", err))
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	relayBody(w, resp.Body, nil)
}

// relayBody copies src to w in chunkSize reads, flushing after each write
// so a streaming response never waits on internal buffering. When capBuf is
// non-nil, every chunk is also teed into it for capture.
func relayBody(w http.ResponseWriter, src io.Reader, capBuf *capBuffer) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, chunkSize)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
			if capBuf != nil {
				capBuf.Write(buf[:n])
			}
		}
		if rerr != nil {
			return
		}
	}
}
