package mota

import (
	"errors"
	"net/http"
	"strings"
)

// CredentialedRedirectPolicy allows relative and same-origin redirects while
// preventing clients carrying Bearer tokens or Provider credentials from
// forwarding a request to another scheme, host, or port.
func CredentialedRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if req == nil || req.URL == nil || via[0] == nil || via[0].URL == nil {
		return errors.New("refusing redirect with an invalid origin")
	}
	original := via[0].URL
	if !strings.EqualFold(req.URL.Scheme, original.Scheme) ||
		!strings.EqualFold(req.URL.Host, original.Host) {
		return errors.New("refusing cross-origin redirect for credentialed request")
	}
	return nil
}
