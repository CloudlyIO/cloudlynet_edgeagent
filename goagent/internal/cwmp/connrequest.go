package cwmp

import (
	"crypto/md5"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// TriggerConnectionRequest digest-auths a GET to the device's Connection
// Request URL so it opens a CWMP session now instead of waiting for its next
// periodic/ATC inform. It is an optimisation, not a dependency: if it fails,
// the queued task still applies on the next device-initiated (~60s) session, so
// callers treat the error as non-fatal. urlOverride lets us use the device's
// LAN CR address when the advertised WAN one is not routable from the edge.
func TriggerConnectionRequest(deviceIP, user, pass, urlOverride string) error {
	url := urlOverride
	if url == "" {
		url = fmt.Sprintf("http://%s:30005/", deviceIP)
	}
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
	ch := parseDigest(r1.Header.Get("WWW-Authenticate"))
	ha1 := md5hex(user + ":" + ch["realm"] + ":" + pass)
	ha2 := md5hex("GET:/")
	nc, cnonce := "00000001", "cloudlynet"
	resp := md5hex(strings.Join([]string{ha1, ch["nonce"], nc, cnonce, "auth", ha2}, ":"))
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf(
		`Digest username=%q, realm=%q, nonce=%q, uri="/", qop=auth, nc=%s, cnonce=%q, response=%q`,
		user, ch["realm"], ch["nonce"], nc, cnonce, resp))
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
