package matrix

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// ErrForbidden is returned (wrapped) for any request that is not on the
// read-only allowlist. It is raised before any network I/O.
var ErrForbidden = errors.New("request refused by read-only allowlist")

type rule struct {
	method string
	re     *regexp.Regexp
}

// seg matches one escaped path segment. Empty segments are not allowed except
// where noted (state keys may be empty).
const seg = `[^/]+`

// allowlist is the complete set of Matrix endpoints mcp-matrix-ro may call (SPEC §3).
// Do not add entries without the owner's approval.
var allowlist = []rule{
	{http.MethodGet, regexp.MustCompile(`^/_matrix/client/versions$`)},
	{http.MethodGet, regexp.MustCompile(`^/_matrix/client/v3/account/whoami$`)},
	{http.MethodGet, regexp.MustCompile(`^/_matrix/client/v3/joined_rooms$`)},
	{http.MethodGet, regexp.MustCompile(`^/_matrix/client/v3/sync$`)},
	{http.MethodGet, regexp.MustCompile(`^/_matrix/client/v3/rooms/` + seg + `/state$`)},
	{http.MethodGet, regexp.MustCompile(`^/_matrix/client/v3/rooms/` + seg + `/state/` + seg + `/[^/]*$`)},
	{http.MethodGet, regexp.MustCompile(`^/_matrix/client/v3/rooms/` + seg + `/messages$`)},
	{http.MethodGet, regexp.MustCompile(`^/_matrix/client/v3/rooms/` + seg + `/members$`)},
	{http.MethodGet, regexp.MustCompile(`^/_matrix/client/v3/rooms/` + seg + `/joined_members$`)},
	{http.MethodGet, regexp.MustCompile(`^/_matrix/client/v3/rooms/` + seg + `/event/` + seg + `$`)},
	{http.MethodGet, regexp.MustCompile(`^/_matrix/client/v3/rooms/` + seg + `/context/` + seg + `$`)},
	{http.MethodGet, regexp.MustCompile(`^/_matrix/client/v3/profile/` + seg + `$`)},
	{http.MethodPost, regexp.MustCompile(`^/_matrix/client/v3/search$`)},
	// Password login (MATRIX_USER/MATRIX_PASSWORD). Added at the owner's
	// request on 2026-10-31; it only obtains a token for this device.
	{http.MethodPost, regexp.MustCompile(`^/_matrix/client/v3/login$`)},
}

// CheckAllowed reports whether method+URL is on the read-only allowlist.
// It is exported so the fake homeserver in tests can independently verify
// every request it receives.
func CheckAllowed(method string, u *url.URL) error {
	if u == nil {
		return fmt.Errorf("%w: nil URL", ErrForbidden)
	}
	p := u.EscapedPath()
	// Refuse dot segments, including encoded ones, that a proxy or the server
	// could normalise into another endpoint. Encoded slashes alone are fine:
	// room v3 event IDs are standard base64 and may contain '/'.
	for _, s := range strings.Split(p, "/") {
		dec, err := url.PathUnescape(s)
		if err != nil {
			return fmt.Errorf("%w: bad escaping in %s", ErrForbidden, p)
		}
		for _, part := range strings.FieldsFunc(dec, func(r rune) bool { return r == '/' || r == '\\' }) {
			if part == "." || part == ".." {
				return fmt.Errorf("%w: dot segment in %s", ErrForbidden, p)
			}
		}
		if dec == "." || dec == ".." {
			return fmt.Errorf("%w: dot segment in %s", ErrForbidden, p)
		}
	}
	for _, r := range allowlist {
		if r.method == method && r.re.MatchString(p) {
			if strings.HasSuffix(p, "/_matrix/client/v3/sync") {
				vals := u.Query()["set_presence"]
				if len(vals) != 1 || vals[0] != "offline" {
					return fmt.Errorf("%w: /sync without set_presence=offline", ErrForbidden)
				}
			}
			return nil
		}
	}
	return fmt.Errorf("%w: %s %s", ErrForbidden, method, p)
}

// guardTransport enforces the allowlist and pins the homeserver origin.
type guardTransport struct {
	scheme, host string
	next         http.RoundTripper
	log          func(method, path string)
}

func (g *guardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != g.scheme || req.URL.Host != g.host {
		closeBody(req)
		return nil, fmt.Errorf("%w: host %s://%s is not the configured homeserver", ErrForbidden, req.URL.Scheme, req.URL.Host)
	}
	if err := CheckAllowed(req.Method, req.URL); err != nil {
		closeBody(req)
		return nil, err
	}
	if g.log != nil {
		g.log(req.Method, req.URL.EscapedPath())
	}
	return g.next.RoundTrip(req)
}

func closeBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}
