package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// latestURL redirects to the page of the latest release, whose address ends in
// its tag. Unlike the GitHub API it needs no token and has no hourly quota.
var latestURL = "https://github.com/paurosello/agwmap/releases/latest"

// updateNotice returns a line that tells the user about a release newer than
// current, or "" when there is none or it cannot tell: a build without a
// release version, no network, a slow or unexpected answer.
func updateNotice(current string) string {
	have, ok := parseVersion(current)
	if !ok {
		return ""
	}
	client := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest(http.MethodHead, latestURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "agwmap")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	resp.Body.Close()
	_, tag, found := strings.Cut(resp.Header.Get("Location"), "/releases/tag/")
	if !found {
		return ""
	}
	latest, ok := parseVersion(tag)
	if !ok || !less(have, latest) {
		return ""
	}
	return fmt.Sprintf("agwmap %s is available (this is %s). To update: kubectl krew upgrade agwmap", tag, current)
}

// parseVersion reads a release tag like v1.2.3. Anything else, such as "dev"
// or a pseudo-version, is not a release.
func parseVersion(s string) (v [3]int, ok bool) {
	s, ok = strings.CutPrefix(s, "v")
	parts := strings.Split(s, ".")
	if !ok || len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

func less(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
