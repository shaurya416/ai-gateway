package redisstore

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// defaultPort is the port a URL that names none connects to.
const defaultPort = "6379"

// Endpoint is a parsed Redis URL: where the store connects and how it
// authenticates.
type Endpoint struct {
	// Addr is host:port. The port defaults to 6379.
	Addr string
	// Host is the host alone, the name a rediss:// certificate is verified
	// against.
	Host string
	// TLS is true for rediss://.
	TLS bool
	// Username and Password authenticate each connection: AUTH username
	// password when both are set, AUTH password for a password alone.
	Username string
	Password string
	// DB is the logical database the URL's path selects; 0 when it has none.
	DB int
}

// String is the endpoint with its credentials left out, for a log line.
func (e Endpoint) String() string {
	scheme := "redis"
	if e.TLS {
		scheme = "rediss"
	}
	return scheme + "://" + e.Addr + "/" + strconv.Itoa(e.DB)
}

// ParseURL reads a Redis URL of the form
//
//	redis://[[username]:password@]host[:port][/db]
//
// with rediss:// for TLS. A password with a reserved character — @ : / ? # %
// — is written percent-encoded, as in any URL.
//
// A query string or a fragment is refused rather than ignored: clients read
// options such as TLS settings and timeouts there, and an option this store
// does not honour must not look as though it were in force. A username with no
// password is refused too, because AUTH takes a password; a password alone is
// written redis://:password@host.
//
// No error quotes the URL. It can carry a password, and these errors reach
// `ferrogw validate` output and the startup log.
func ParseURL(raw string) (Endpoint, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// The parser's error is not passed on. It quotes the input, and even
		// its cause quotes the part that failed — which, when a password
		// holding an unescaped / or # cut the authority short, is the start
		// of the password.
		return Endpoint{}, errors.New("not a valid URL: percent-encode any @ : / ? # or % in the password")
	}

	var ep Endpoint
	switch u.Scheme {
	case "redis":
	case "rediss":
		ep.TLS = true
	default:
		// The scheme is not quoted either: in a value written without one, the
		// text before the first colon can be a username or a password.
		return Endpoint{}, errors.New("must be a redis:// or rediss:// URL")
	}
	if u.Opaque != "" {
		return Endpoint{}, errors.New("must be written redis://host:port, with the two slashes")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return Endpoint{}, errors.New("must not carry a query string: no URL option is read, so none would take effect")
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return Endpoint{}, errors.New("must not carry a fragment")
	}

	ep.Host = u.Hostname()
	if ep.Host == "" {
		return Endpoint{}, errors.New("names no host")
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	} else if n, perr := strconv.Atoi(port); perr != nil || n < 1 || n > 65535 {
		return Endpoint{}, errors.New("port must be between 1 and 65535")
	}
	ep.Addr = net.JoinHostPort(ep.Host, port)

	if ep.DB, err = parseDB(u.Path); err != nil {
		return Endpoint{}, err
	}

	if u.User != nil {
		ep.Username = u.User.Username()
		ep.Password, _ = u.User.Password()
		if ep.Password == "" && ep.Username != "" {
			return Endpoint{}, errors.New("carries a username but no password: AUTH needs a password; write redis://:password@host for a password alone")
		}
	}
	return ep, nil
}

// parseDB reads the database number from a URL path: "" and "/" select 0,
// "/3" selects 3, and anything else is an error.
func parseDB(path string) (int, error) {
	db := strings.TrimPrefix(path, "/")
	if db == "" {
		return 0, nil
	}
	if len(db) > 9 || strings.Trim(db, "0123456789") != "" {
		return 0, errors.New("path must be a database number, such as /0")
	}
	return strconv.Atoi(db)
}
