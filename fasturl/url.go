// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package fasturl parses URLs and implements query escaping.
package fasturl

// Modifications by terorie

// See RFC 3986. This package generally follows RFC 3986, except where
// it deviates for compatibility reasons. When sending changes, first
// search old issues for history on decisions. Unit tests should also
// contain references to issue numbers with details.

import (
	"errors"
	"fmt"
	"github.com/terorie/oddb-go/runes"
	"strconv"
	"strings"
)

// Error reports an error and the operation and URL that caused it.
type Error struct {
	Op  string
	URL []rune
	Err error
}

func (e *Error) Error() string { return e.Op + " " + string(e.URL) + ": " + e.Err.Error() }

type timeout interface {
	Timeout() bool
}

func (e *Error) Timeout() bool {
	t, ok := e.Err.(timeout)
	return ok && t.Timeout()
}

type temporary interface {
	Temporary() bool
}

func (e *Error) Temporary() bool {
	t, ok := e.Err.(temporary)
	return ok && t.Temporary()
}

func ishex(c byte) bool {
	switch {
	case '0' <= c && c <= '9':
		return true
	case 'a' <= c && c <= 'f':
		return true
	case 'A' <= c && c <= 'F':
		return true
	}
	return false
}

func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10
	}
	return 0
}

type encoding int

const (
	encodePath encoding = 1 + iota
	encodePathSegment
	encodeHost
	encodeZone
	encodeUserPassword
	encodeQueryComponent
	encodeFragment
)

type EscapeError string

func (e EscapeError) Error() string {
	return "invalid URL escape " + strconv.Quote(string(e))
}

type InvalidHostError string

func (e InvalidHostError) Error() string {
	return "invalid character " + strconv.Quote(string(e)) + " in host name"
}

// Return true if the specified character should be escaped when
// appearing in a URL string, according to RFC 3986.
//
// Please be informed that for now shouldEscape does not check all
// reserved characters correctly. See golang.org/issue/5684.
func shouldEscape(c rune, mode encoding) bool {
	// §2.3 Unreserved characters (alphanum)
	if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' {
		return false
	}

	if mode == encodeHost || mode == encodeZone {
		// §3.2.2 Host allows
		//	sub-delims = "!" / "$" / "&" / "'" / "(" / ")" / "*" / "+" / "," / ";" / "="
		// as part of reg-name.
		// We add : because we include :port as part of host.
		// We add [ ] because we include [ipv6]:port as part of host.
		// We add < > because they're the only characters left that
		// we could possibly allow, and Parse will reject them if we
		// escape them (because hosts can't use %-encoding for
		// ASCII bytes).
		switch c {
		case '!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=', ':', '[', ']', '<', '>', '"':
			return false
		}
	}

	switch c {
	case '-', '_', '.', '~': // §2.3 Unreserved characters (mark)
		return false

	case '$', '&', '+', ',', '/', ':', ';', '=', '?', '@': // §2.2 Reserved characters (reserved)
		// Different sections of the URL allow a few of
		// the reserved characters to appear unescaped.
		switch mode {
		case encodePath: // §3.3
			// The RFC allows : @ & = + $ but saves / ; , for assigning
			// meaning to individual path segments. This package
			// only manipulates the path as a whole, so we allow those
			// last three as well. That leaves only ? to escape.
			return c == '?'

		case encodePathSegment: // §3.3
			// The RFC allows : @ & = + $ but saves / ; , for assigning
			// meaning to individual path segments.
			return c == '/' || c == ';' || c == ',' || c == '?'

		case encodeUserPassword: // §3.2.1
			// The RFC allows ';', ':', '&', '=', '+', '$', and ',' in
			// userinfo, so we must escape only '@', '/', and '?'.
			// The parsing of userinfo treats ':' as special so we must escape
			// that too.
			return c == '@' || c == '/' || c == '?' || c == ':'

		case encodeQueryComponent: // §3.4
			// The RFC reserves (so we must escape) everything.
			return true

		case encodeFragment: // §4.1
			// The RFC text is silent but the grammar allows
			// everything, so escape nothing.
			return false
		}
	}

	if mode == encodeFragment {
		// RFC 3986 §2.2 allows not escaping sub-delims. A subset of sub-delims are
		// included in reserved from RFC 2396 §2.2. The remaining sub-delims do not
		// need to be escaped. To minimize potential breakage, we apply two restrictions:
		// (1) we always escape sub-delims outside of the fragment, and (2) we always
		// escape single quote to avoid breaking callers that had previously assumed that
		// single quotes would be escaped. See issue #19917.
		switch c {
		case '!', '(', ')', '*':
			return false
		}
	}

	// Everything else must be escaped.
	return true
}

// QueryUnescape does the inverse transformation of QueryEscape,
// converting each 3-byte encoded substring of the form "%AB" into the
// hex-decoded byte 0xAB.
// It returns an error if any % is not followed by two hexadecimal
// digits.
func QueryUnescape(s []rune) ([]rune, error) {
	return unescape(s, encodeQueryComponent)
}

// PathUnescape does the inverse transformation of PathEscape,
// converting each 3-byte encoded substring of the form "%AB" into the
// hex-decoded byte 0xAB. It returns an error if any % is not followed
// by two hexadecimal digits.
//
// PathUnescape is identical to QueryUnescape except that it does not
// unescape '+' to ' ' (space).
func PathUnescape(s []rune) ([]rune, error) {
	return unescape(s, encodePathSegment)
}

// unescape unescapes a string; the mode specifies
// which section of the URL string is being unescaped.
func unescape(s []rune, mode encoding) ([]rune, error) {
	// Count %, check that they're well-formed.
	n := 0
	hasPlus := false
	for i := 0; i < len(s); {
		switch s[i] {
		case '%':
			n++
			if i+2 >= len(s) || !ishex(byte(s[i+1])) || !ishex(byte(s[i+2])) {
				s = s[i:]
				if len(s) > 3 {
					s = s[:3]
				}
				return nil, EscapeError(s)
			}
			// Per https://tools.ietf.org/html/rfc3986#page-21
			// in the host component %-encoding can only be used
			// for non-ASCII bytes.
			// But https://tools.ietf.org/html/rfc6874#section-2
			// introduces %25 being allowed to escape a percent sign
			// in IPv6 scoped-address literals. Yay.
			if mode == encodeHost && unhex(byte(s[i+1])) < 8 && !runes.Equals(s[i:i+3], []rune("%25")) {
				return nil, EscapeError(s[i : i+3])
			}
			if mode == encodeZone {
				// RFC 6874 says basically "anything goes" for zone identifiers
				// and that even non-ASCII can be redundantly escaped,
				// but it seems prudent to restrict %-escaped bytes here to those
				// that are valid host name bytes in their unescaped form.
				// That is, you can use escaping in the zone identifier but not
				// to introduce bytes you couldn't just write directly.
				// But Windows puts spaces here! Yay.
				v := unhex(byte(s[i+1]))<<4 | unhex(byte(s[i+2]))
				if !runes.Equals(s[i:i+3], []rune("%25")) && v != ' ' && shouldEscape(rune(v), encodeHost) {
					return nil, EscapeError(s[i : i+3])
				}
			}
			i += 3
		case '+':
			hasPlus = mode == encodeQueryComponent
			i++
		default:
			if (mode == encodeHost || mode == encodeZone) && s[i] < 0x80 && shouldEscape(s[i], mode) {
				return nil, InvalidHostError(s[i : i+1])
			}
			i++
		}
	}

	if n == 0 && !hasPlus {
		return s, nil
	}

	t := make([]byte, len(s)-2*n)
	j := 0
	for i := 0; i < len(s); {
		switch s[i] {
		case '%':
			t[j] = unhex(byte(s[i+1]))<<4 | unhex(byte(s[i+2]))
			j++
			i += 3
		case '+':
			if mode == encodeQueryComponent {
				t[j] = ' '
			} else {
				t[j] = '+'
			}
			j++
			i++
		default:
			t[j] = byte(s[i])
			j++
			i++
		}
	}
	return []rune(string(t)), nil
}

// QueryEscape escapes the string so it can be safely placed
// inside a URL query.
func QueryEscape(s []rune) []rune {
	return escape(s, encodeQueryComponent)
}

// PathEscape escapes the string so it can be safely placed
// inside a URL path segment.
func PathEscape(s []rune) []rune {
	return escape(s, encodePathSegment)
}

func escape(s []rune, mode encoding) []rune {
	spaceCount, hexCount := 0, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if shouldEscape(c, mode) {
			if c == ' ' && mode == encodeQueryComponent {
				spaceCount++
			} else {
				hexCount++
			}
		}
	}

	if spaceCount == 0 && hexCount == 0 {
		return s
	}

	t := make([]byte, len(s)+2*hexCount)
	j := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == ' ' && mode == encodeQueryComponent:
			t[j] = '+'
			j++
		case shouldEscape(c, mode):
			t[j] = '%'
			t[j+1] = "0123456789ABCDEF"[c>>4]
			t[j+2] = "0123456789ABCDEF"[c&15]
			j += 3
		default:
			t[j] = byte(s[i])
			j++
		}
	}
	return []rune(string(t))
}

// A URL represents a parsed URL (technically, a URI reference).
//
// The general form represented is:
//
//	[scheme:][//[userinfo@]host][/]path[?query][#fragment]
//
// URLs that do not start with a slash after the scheme are interpreted as:
//
//	scheme:opaque[?query][#fragment]
//
// Note that the Path field is stored in decoded form: /%47%6f%2f becomes /Go/.
// A consequence is that it is impossible to tell which slashes in the Path were
// slashes in the raw URL and which were %2f. This distinction is rarely important,
// but when it is, code must not use Path directly.
// The Parse function sets both Path and RawPath in the URL it returns,
// and URL's String method uses RawPath if it is a valid encoding of Path,
// by calling the EscapedPath method.
type URL struct {
	Scheme     []rune
	Opaque     []rune    // encoded opaque data
	Host       []rune    // host or host:port
	Path       []rune    // path (relative paths may omit leading slash)
	RawPath    []rune    // encoded path hint (see EscapedPath method)
	ForceQuery bool      // append a query ('?') even if RawQuery is empty
	RawQuery   []rune    // encoded query values, without '?'
}

// Maybe rawurl is of the form scheme:path.
// (Scheme must be [a-zA-Z][a-zA-Z0-9+-.]*)
// If so, return scheme, path; else return "", rawurl.
func getscheme(rawurl []rune) (scheme []rune, path []rune, err error) {
	for i := 0; i < len(rawurl); i++ {
		c := rawurl[i]
		switch {
		case 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z':
			// do nothing
		case '0' <= c && c <= '9' || c == '+' || c == '-' || c == '.':
			if i == 0 {
				return nil, rawurl, nil
			}
		case c == ':':
			if i == 0 {
				return nil, nil, errors.New("missing protocol scheme")
			}
			scheme = rawurl[:i]
			path = rawurl[i+1:]
			return
		default:
			// we have encountered an invalid character,
			// so there is no valid scheme
			return nil, rawurl, nil
		}
	}
	return nil, rawurl, nil
}

// Maybe s is of the form t c u.
// If so, return t, c u (or t, u if cutc == true).
// If not, return s, "".
func split(s []rune, c rune, cutc bool) ([]rune, []rune) {
	i := strings.Index(string(s), string(c)) // TODO Optimize
	if i < 0 {
		return s, nil
	}
	if cutc {
		return s[:i], s[i+1:]
	}
	return s[:i], s[i:]
}

// Parse parses rawurl into a URL structure.
//
// The rawurl may be relative (a path, without a host) or absolute
// (starting with a scheme). Trying to parse a hostname and path
// without a scheme is invalid but may not necessarily return an
// error, due to parsing ambiguities.
func (u *URL) Parse(rawurl []rune) error {
	// Cut off #frag
	s, frag := split(rawurl, '#', true)
	err := u.parse(s, false)
	if err != nil {
		return &Error{"parse", s, err}
	}
	if len(frag) == 0 {
		return nil
	}
	return nil
}

// ParseRequestURI parses rawurl into a URL structure. It assumes that
// rawurl was received in an HTTP request, so the rawurl is interpreted
// only as an absolute URI or an absolute path.
// The string rawurl is assumed not to have a #fragment suffix.
// (Web browsers strip #fragment before sending the URL to a web server.)
func (u *URL) ParseRequestURI(rawurl []rune) error {
	err := u.parse(rawurl, true)
	if err != nil {
		return &Error{"parse", rawurl, err}
	}
	return nil
}

// parse parses a URL from a string in one of two contexts. If
// viaRequest is true, the URL is assumed to have arrived via an HTTP request,
// in which case only absolute URLs or path-absolute relative URLs are allowed.
// If viaRequest is false, all forms of relative URLs are allowed.
func (u *URL) parse(rawurl []rune, viaRequest bool) error {
	var rest []rune
	var err error

	if len(rawurl) == 0 && viaRequest {
		return errors.New("empty url")
	}

	if runes.Equals(rawurl, []rune("*")) {
		u.Path = []rune("*")
		return nil
	}

	// Split off possible leading "http:", "mailto:", etc.
	// Cannot contain escaped characters.
	if u.Scheme, rest, err = getscheme(rawurl); err != nil {
		return err
	}

	if runes.HasSuffix(rest, []rune("?")) && runes.Count(rest, '?') == 1 {
		u.ForceQuery = true
		rest = rest[:len(rest)-1]
	} else {
		rest, u.RawQuery = split(rest, '?', true)
	}

	if !runes.HasPrefix(rest, []rune("/")) {
		if len(u.Scheme) != 0 {
			// We consider rootless paths per RFC 3986 as opaque.
			u.Opaque = rest
			return nil
		}
		if viaRequest {
			return errors.New("invalid URI for request")
		}

		// Avoid confusion with malformed schemes, like cache_object:foo/bar.
		// See golang.org/issue/16822.
		//
		// RFC 3986, §3.3:
		// In addition, a URI reference (Section 4.1) may be a relative-path reference,
		// in which case the first path segment cannot contain a colon (":") character.
		colon := runes.IndexRune(rest, ':')
		slash := runes.IndexRune(rest, '/')
		if colon >= 0 && (slash < 0 || colon < slash) {
			// First path segment has colon. Not allowed in relative URL.
			return errors.New("first path segment in URL cannot contain colon")
		}
	}

	if (len(u.Scheme) != 0 || !viaRequest && !runes.HasPrefix(rest, []rune("///"))) && runes.HasPrefix(rest, []rune("//")) {
		var authority []rune
		authority, rest = split(rest[2:], '/', false)
		u.Host, err = parseAuthority(authority)
		if err != nil {
			return err
		}
	}
	// Set Path and, optionally, RawPath.
	// RawPath is a hint of the encoding of Path. We don't want to set it if
	// the default escaping of Path is equivalent, to help make sure that people
	// don't rely on it in general.
	if err := u.setPath(rest); err != nil {
		return err
	}
	return nil
}

func parseAuthority(authority []rune) (host []rune, err error) {
	i := runes.LastIndexRune(authority, '@')
	if i < 0 {
		host, err = parseHost(authority)
	} else {
		host, err = parseHost(authority[i+1:])
	}
	if err != nil {
		return nil, err
	}
	if i < 0 {
		return host, nil
	}
	userinfo := authority[:i]
	if !validUserinfo(userinfo) {
		return nil, errors.New("fasturl: invalid userinfo")
	}
	return host, nil
}

// parseHost parses host as an authority without user
// information. That is, as host[:port].
func parseHost(host []rune) ([]rune, error) {
	if runes.HasPrefix(host, []rune("[")) {
		// Parse an IP-Literal in RFC 3986 and RFC 6874.
		// E.g., "[fe80::1]", "[fe80::1%25en0]", "[fe80::1]:80".
		i := runes.LastIndexRune(host, ']')
		if i < 0 {
			return nil, errors.New("missing ']' in host")
		}
		colonPort := host[i+1:]
		if !validOptionalPort(colonPort) {
			return nil, fmt.Errorf("invalid port %q after host", colonPort)
		}

		// RFC 6874 defines that %25 (%-encoded percent) introduces
		// the zone identifier, and the zone identifier can use basically
		// any %-encoding it likes. That's different from the host, which
		// can only %-encode non-ASCII bytes.
		// We do impose some restrictions on the zone, to avoid stupidity
		// like newlines.
		zone := strings.Index(string(host[:i]), "%25")
		if zone >= 0 {
			host1, err := unescape(host[:zone], encodeHost)
			if err != nil {
				return nil, err
			}
			host2, err := unescape(host[zone:i], encodeZone)
			if err != nil {
				return nil, err
			}
			host3, err := unescape(host[i:], encodeHost)
			if err != nil {
				return nil, err
			}
			// TODO Optimize
			return runes.Create(host1, host2, host3), nil
		}
	}

	var err error
	if host, err = unescape(host, encodeHost); err != nil {
		return nil, err
	}
	return host, nil
}

// setPath sets the Path and RawPath fields of the URL based on the provided
// escaped path p. It maintains the invariant that RawPath is only specified
// when it differs from the default encoding of the path.
// For example:
// - setPath("/foo/bar")   will set Path="/foo/bar" and RawPath=""
// - setPath("/foo%2fbar") will set Path="/foo/bar" and RawPath="/foo%2fbar"
// setPath will return an error only if the provided path contains an invalid
// escaping.
func (u *URL) setPath(p []rune) error {
	path, err := unescape(p, encodePath)
	if err != nil {
		return err
	}
	u.Path = path
	if escp := escape(path, encodePath); runes.Equals(p, escp) {
		// Default encoding is fine.
		u.RawPath = nil
	} else {
		u.RawPath = p
	}
	return nil
}

// EscapedPath returns the escaped form of u.Path.
// In general there are multiple possible escaped forms of any path.
// EscapedPath returns u.RawPath when it is a valid escaping of u.Path.
// Otherwise EscapedPath ignores u.RawPath and computes an escaped
// form on its own.
// The String and RequestURI methods use EscapedPath to construct
// their results.
// In general, code should call EscapedPath instead of
// reading u.RawPath directly.
func (u *URL) EscapedPath() []rune {
	if len(u.RawPath) != 0 && validEncodedPath(u.RawPath) {
		p, err := unescape(u.RawPath, encodePath)
		if err == nil && runes.Equals(p, u.Path) {
			return u.RawPath
		}
	}
	if runes.Equals(u.Path, []rune("*")) {
		return []rune("*") // don't escape (Issue 11202)
	}
	return escape(u.Path, encodePath)
}

// validEncodedPath reports whether s is a valid encoded path.
// It must not contain any bytes that require escaping during path encoding.
func validEncodedPath(s []rune) bool {
	for i := 0; i < len(s); i++ {
		// RFC 3986, Appendix A.
		// pchar = unreserved / pct-encoded / sub-delims / ":" / "@".
		// shouldEscape is not quite compliant with the RFC,
		// so we check the sub-delims ourselves and let
		// shouldEscape handle the others.
		switch s[i] {
		case '!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=', ':', '@':
			// ok
		case '[', ']':
			// ok - not specified in RFC 3986 but left alone by modern browsers
		case '%':
			// ok - percent encoded, will decode
		default:
			if shouldEscape(s[i], encodePath) {
				return false
			}
		}
	}
	return true
}

// validOptionalPort reports whether port is either an empty string
// or matches /^:\d*$/
func validOptionalPort(port []rune) bool {
	if len(port) == 0 {
		return true
	}
	if port[0] != ':' {
		return false
	}
	for _, b := range port[1:] {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}

func (u *URL) Runes() (buf []rune) {
	if len(u.Scheme) != 0 {
		buf = append(buf, u.Scheme...)
		buf = append(buf, ':')
	}
	if len(u.Opaque) != 0 {
		buf = append(buf, u.Opaque...)
	} else {
		if len(u.Scheme) != 0 || len(u.Host) != 0 {
			if len(u.Host) != 0 || len(u.Path) != 0 {
				buf = append(buf, '/', '/')
			}
			if h := u.Host; len(h) != 0 {
				buf = append(buf, escape(h, encodeHost)...)
			}
		}
		path := u.EscapedPath()
		if len(path) != 0 && path[0] != '/' && len(u.Host) != 0 {
			buf = append(buf, '/')
		}
		if len(buf) == 0 {
			// RFC 3986 §4.2
			// A path segment that contains a colon character (e.g., "this:that")
			// cannot be used as the first segment of a relative-path reference, as
			// it would be mistaken for a scheme name. Such a segment must be
			// preceded by a dot-segment (e.g., "./this:that") to make a relative-
			// path reference.
			if i := runes.IndexRune(path, ':'); i > -1 && runes.IndexRune(path[:i], '/') == -1 {
				buf = append(buf, '.', '/')
			}
		}
		buf = append(buf, path...)
	}
	if u.ForceQuery || len(u.RawQuery) != 0 {
		buf = append(buf, '?')
		buf = append(buf, u.RawQuery...)
	}
	return
}

// String reassembles the URL into a valid URL string.
// The general form of the result is one of:
//
//	scheme:opaque?query#fragment
//	scheme://userinfo@host/path?query#fragment
//
// If u.Opaque is non-empty, String uses the first form;
// otherwise it uses the second form.
// To obtain the path, String uses u.EscapedPath().
//
// In the second form, the following rules apply:
//	- if u.Scheme is empty, scheme: is omitted.
//	- if u.User is nil, userinfo@ is omitted.
//	- if u.Host is empty, host/ is omitted.
//	- if u.Scheme and u.Host are empty and u.User is nil,
//	   the entire scheme://userinfo@host/ is omitted.
//	- if u.Host is non-empty and u.Path begins with a /,
//	   the form host/path does not add its own /.
//	- if u.RawQuery is empty, ?query is omitted.
//	- if u.Fragment is empty, #fragment is omitted.
func (u *URL) String() string {
	return string(u.Runes())
}

// resolvePath applies special path segments from refs and applies
// them to base, per RFC 3986.
func resolvePath(base, ref []rune) []rune {
	var full []rune
	if len(ref) == 0 {
		full = base
	} else if ref[0] != '/' {
		// TODO Optimize
		i := strings.LastIndex(string(base), "/")
		full = runes.Create(base[:i+1], ref)
	} else {
		full = ref
	}
	if len(full) == 0 {
		return nil
	}
	var dst []string
	// TODO Optimize
	src := strings.Split(string(full), "/")
	for _, elem := range src {
		switch elem {
		case ".":
			// drop
		case "..":
			if len(dst) > 0 {
				dst = dst[:len(dst)-1]
			}
		default:
			dst = append(dst, elem)
		}
	}
	if last := src[len(src)-1]; last == "." || last == ".." {
		// Add final slash to the joined path.
		dst = append(dst, "") // TODO Wtf?
	}
	// TODO Optimize
	return []rune("/" + strings.TrimPrefix(strings.Join(dst, "/"), "/"))
}

// IsAbs reports whether the URL is absolute.
// Absolute means that it has a non-empty scheme.
func (u *URL) IsAbs() bool {
	return len(u.Scheme) != 0
}

// ParseRel parses a URL in the context of the receiver. The provided URL
// may be relative or absolute. Parse returns nil, err on parse
// failure, otherwise its return value is the same as ResolveReference.
func (u *URL) ParseRel(out *URL, ref []rune) error {
	var refurl URL

	err := refurl.Parse(ref)
	if err != nil {
		return err
	}

	u.ResolveReference(out, &refurl)
	return nil
}

// ResolveReference resolves a URI reference to an absolute URI from
// an absolute base URI u, per RFC 3986 Section 5.2. The URI reference
// may be relative or absolute. ResolveReference always returns a new
// URL instance, even if the returned URL is identical to either the
// base or reference. If ref is an absolute URL, then ResolveReference
// ignores base and returns a copy of ref.
func (u *URL) ResolveReference(url *URL, ref *URL) {
	*url = *ref
	if len(ref.Scheme) == 0 {
		url.Scheme = u.Scheme
	}
	if len(ref.Scheme) != 0 || len(ref.Host) != 0 {
		// The "absoluteURI" or "net_path" cases.
		// We can ignore the error from setPath since we know we provided a
		// validly-escaped path.
		url.setPath(resolvePath(ref.EscapedPath(), nil))
		return
	}
	if len(ref.Opaque) != 0 {
		url.Host = nil
		url.Path = nil
		return
	}
	if len(ref.Path) == 0 && len(ref.RawQuery) == 0 {
		url.RawQuery = u.RawQuery
	}
	// The "abs_path" or "rel_path" cases.
	url.Host = u.Host
	url.setPath(resolvePath(u.EscapedPath(), ref.EscapedPath()))
	return
}

// RequestURI returns the encoded path?query or opaque?query
// string that would be used in an HTTP request for u.
func (u *URL) RequestURI() []rune {
	result := u.Opaque
	if len(result) == 0 {
		result = u.EscapedPath()
		if len(result) == 0 {
			result = []rune("/")
		}
	} else {
		if runes.HasPrefix(result, []rune("//")) {
			result = runes.Create(u.Scheme, []rune(":"), result)
		}
	}
	if u.ForceQuery || len(u.RawQuery) != 0 {
		result = append(result, '?')
		result = append(result, u.RawQuery...)
	}
	return result
}

// Hostname returns u.Host, without any port number.
//
// If Host is an IPv6 literal with a port number, Hostname returns the
// IPv6 literal without the square brackets. IPv6 literals may include
// a zone identifier.
func (u *URL) Hostname() []rune {
	return stripPort(u.Host)
}

// Port returns the port part of u.Host, without the leading colon.
// If u.Host doesn't contain a port, Port returns an empty string.
func (u *URL) Port() []rune {
	return portOnly(u.Host)
}

func stripPort(hostport []rune) []rune {
	colon := runes.IndexRune(hostport, ':')
	if colon == -1 {
		return hostport
	}
	if i := runes.IndexRune(hostport, ']'); i != -1 {
		return runes.TrimPrefix(hostport[:i], []rune("["))
	}
	return hostport[:colon]
}

func portOnly(hostport []rune) []rune {
	colon := runes.IndexRune(hostport, ':')
	if colon == -1 {
		return nil
	}
	// TODO Optimize
	if i := strings.Index(string(hostport), "]:"); i != -1 {
		return hostport[i+len("]:"):]
	}
	if strings.Contains(string(hostport), "]") {
		return nil
	}
	return hostport[colon+len(":"):]
}

// Marshaling interface implementations.
// Would like to implement MarshalText/UnmarshalText but that will change the JSON representation of URLs.

func (u *URL) MarshalBinary() (text []byte, err error) {
	return []byte(u.String()), nil
}

func (u *URL) UnmarshalBinary(text []byte) error {
	var u1 URL
	err := u1.Parse([]rune(string(text)))
	if err != nil {
		return err
	}
	*u = u1
	return nil
}

// validUserinfo reports whether s is a valid userinfo string per RFC 3986
// Section 3.2.1:
//     userinfo    = *( unreserved / pct-encoded / sub-delims / ":" )
//     unreserved  = ALPHA / DIGIT / "-" / "." / "_" / "~"
//     sub-delims  = "!" / "$" / "&" / "'" / "(" / ")"
//                   / "*" / "+" / "," / ";" / "="
//
// It doesn't validate pct-encoded. The caller does that via func unescape.
func validUserinfo(s []rune) bool {
	for _, r := range s {
		if 'A' <= r && r <= 'Z' {
			continue
		}
		if 'a' <= r && r <= 'z' {
			continue
		}
		if '0' <= r && r <= '9' {
			continue
		}
		switch r {
		case '-', '.', '_', ':', '~', '!', '$', '&', '\'',
			'(', ')', '*', '+', ',', ';', '=', '%', '@':
			continue
		default:
			return false
		}
	}
	return true
}