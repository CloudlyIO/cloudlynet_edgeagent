package cwmp

import (
	"crypto/md5"
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

// TriggerConnectionRequest digest-auths a GET to the given Connection Request
// URL so the device opens a CWMP session now instead of waiting for its next
// periodic/ATC inform. It is an optimisation, not a dependency: if it fails,
// the queued task still applies on the next device-initiated (~60s) session, so
// callers treat the error as non-fatal. The caller resolves the URL per device
// (override > advertised ConnectionRequestURL > <deviceIP>:30005 — see
// Server.crURL), so a multi-device fleet pokes each device's own listener.
func TriggerConnectionRequest(url, user, pass string) error {
	c := &http.Client{Timeout: 8 * time.Second}
	r1, err := c.Get(url)
	if err != nil {
		return err
	}
	r1.Body.Close()
	if r1.StatusCode == http.StatusOK {
		return nil
	}
	if r1.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("CR unexpected status %d", r1.StatusCode)
	}
	uri := "/"
	if u, uerr := neturl.Parse(url); uerr == nil && u.RequestURI() != "" {
		uri = u.RequestURI()
	}
	ch := parseDigest(r1.Header.Get("WWW-Authenticate"))
	ha1 := md5hex(user + ":" + ch["realm"] + ":" + pass)
	ha2 := md5hex("GET:" + uri)
	nc, cnonce := "00000001", "cloudlynet"
	resp := md5hex(strings.Join([]string{ha1, ch["nonce"], nc, cnonce, "auth", ha2}, ":"))
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf(
		`Digest username=%q, realm=%q, nonce=%q, uri=%q, qop=auth, nc=%s, cnonce=%q, response=%q`,
		user, ch["realm"], ch["nonce"], uri, nc, cnonce, resp))
	r2, err := c.Do(req)
	if err != nil {
		return err
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		return fmt.Errorf("CR auth failed %d", r2.StatusCode)
	}
	return nil
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return fmt.Sprintf("%x", h)
}

// parseDigest extracts the key/value pairs from a WWW-Authenticate: Digest header.
func parseDigest(h string) map[string]string {
	m := map[string]string{}
	h = strings.TrimPrefix(h, "Digest ")
	for _, kv := range strings.Split(h, ",") {
		parts := strings.SplitN(strings.TrimSpace(kv), "=", 2)
		if len(parts) == 2 {
			m[parts[0]] = strings.Trim(parts[1], `"`)
		}
	}
	return m
}
