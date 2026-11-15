package ghapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCurrentUserScopes(t *testing.T) {
	cases := []struct {
		name    string
		header  *string
		classic bool
		scopes  int
	}{
		{"classic token", ptr("repo, workflow"), true, 2},
		{"fine-grained token sends no header", nil, false, 0},
		{"app token sends an empty header", ptr(""), false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer secret" {
					t.Errorf("Authorization = %q", got)
				}
				if tc.header != nil {
					w.Header().Set("X-OAuth-Scopes", *tc.header)
				}
				w.Write([]byte(`{"login":"me"}`))
			}))
			defer srv.Close()
			u, err := (&Client{Token: "secret", BaseURL: srv.URL}).CurrentUser(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if u.Login != "me" || u.Classic != tc.classic || len(u.Scopes) != tc.scopes {
				t.Errorf("user = %+v", u)
			}
			if tc.classic && (!u.HasScope("workflow") || u.HasScope("admin")) {
				t.Errorf("HasScope is wrong for %v", u.Scopes)
			}
		})
	}
}

func ptr(s string) *string { return &s }

func TestAPIErrorCarriesGitHubMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"message":"Validation Failed","errors":[{"message":"name already exists on this account"}]}`))
	}))
	defer srv.Close()
	_, err := (&Client{Token: "t", BaseURL: srv.URL}).GetRepo(context.Background(), "me", "app")
	if err == nil || !strings.Contains(err.Error(), "name already exists") || IsNotFound(err) {
		t.Errorf("err = %v", err)
	}
}

// A download that is redirected to storage must not send the token there,
// and a failure to reach storage must not quote the signed link.
func TestDownloadFollowsRedirectWithoutToken(t *testing.T) {
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("the token was sent to the storage host")
		}
		w.Write([]byte("zip-bytes"))
	}))
	defer storage.Close()
	target := storage.URL + "/blob?sig=SIGNATURE"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	}))
	defer api.Close()

	c := &Client{Token: "secret", BaseURL: api.URL}
	var buf bytes.Buffer
	if err := c.DownloadArtifact(context.Background(), "me", "app", 1, &buf, 1<<20); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "zip-bytes" {
		t.Errorf("body = %q", buf.String())
	}

	buf.Reset()
	if err := c.DownloadArtifact(context.Background(), "me", "app", 1, &buf, 4); err == nil {
		t.Error("a download over the limit must fail")
	}

	storage.Close()
	err := c.DownloadArtifact(context.Background(), "me", "app", 1, &buf, 1<<20)
	if err == nil {
		t.Fatal("expected a network error")
	}
	if strings.Contains(err.Error(), "SIGNATURE") || !strings.Contains(err.Error(), "нет связи с") {
		t.Errorf("err = %v", err)
	}
}
