package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpdateNotice(t *testing.T) {
	location := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if location == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		http.Redirect(w, r, location, http.StatusFound)
	}))
	defer srv.Close()
	old := latestURL
	defer func() { latestURL = old }()
	latestURL = srv.URL

	for _, tc := range []struct {
		name, current, location string
		want                    bool
	}{
		{"newer release", "v0.1.0", "/paurosello/agwmap/releases/tag/v0.2.0", true},
		{"compared as numbers", "v0.9.0", "/paurosello/agwmap/releases/tag/v0.10.0", true},
		{"same release", "v0.2.0", "/paurosello/agwmap/releases/tag/v0.2.0", false},
		{"older release", "v0.3.0", "/paurosello/agwmap/releases/tag/v0.2.0", false},
		{"no releases yet", "v0.1.0", "/paurosello/agwmap/releases", false},
		{"not a redirect", "v0.1.0", "", false},
		{"tag that is not a version", "v0.1.0", "/paurosello/agwmap/releases/tag/nightly", false},
		{"development build", "dev", "/paurosello/agwmap/releases/tag/v0.2.0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			location = tc.location
			got := updateNotice(tc.current)
			if (got != "") != tc.want {
				t.Fatalf("updateNotice(%q) = %q, want a notice: %v", tc.current, got, tc.want)
			}
			if tc.want && !strings.Contains(got, "kubectl krew upgrade agwmap") {
				t.Errorf("notice %q does not say how to update", got)
			}
		})
	}

	t.Run("no network", func(t *testing.T) {
		srv.Close()
		if got := updateNotice("v0.1.0"); got != "" {
			t.Errorf("got %q, want nothing", got)
		}
	})
}
