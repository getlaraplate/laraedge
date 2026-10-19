// Package docrules holds the rules shared by the hand-written documents the
// agent reads (the bootstrap file and the configuration document): the URL
// rule for backend addresses and the reporting of decode errors without any
// text taken from the document, which could carry a secret written in the
// wrong place.
package docrules

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// safeKey matches the shape of the keys the agent documents define
// (snake_case). A key that does not match is never echoed: a secret written as
// a key would otherwise leak.
var safeKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// IsSafeKey reports whether key has the shape of a document key and may be
// named in a message.
func IsSafeKey(key string) bool {
	return safeKey.MatchString(key)
}

// CheckURL validates a backend base URL: absolute, without credentials, https,
// or http for a loopback host only. The error never echoes raw.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return errors.New("must be an absolute URL such as https://mes.example.com")
	}
	if u.User != nil {
		return errors.New("must not contain credentials")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if IsLoopbackHost(u.Hostname()) {
			return nil
		}
		return errors.New("must use https (http is accepted only for a loopback host)")
	default:
		return errors.New("must use https")
	}
}

// IsLoopbackHost reports whether host is localhost or a loopback IP address.
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
