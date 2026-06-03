package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnsureV(t *testing.T) {
	cases := map[string]string{
		"0.2.3":   "v0.2.3",
		"v0.2.3":  "v0.2.3",
		"":        "",
		"  1.0.0": "v1.0.0",
	}
	for in, want := range cases {
		if got := ensureV(in); got != want {
			t.Errorf("ensureV(%q) = %q, want %q", in, got, want)
		}
	}
}

// withRelease points githubAPIBase at a server that returns rel for the
// releases/latest endpoint, restoring the base on cleanup.
func withRelease(t *testing.T, rel Release) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(rel)
	}))
	t.Cleanup(srv.Close)
	old := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = old })
}

func TestCheck(t *testing.T) {
	withRelease(t, Release{TagName: "v0.3.0", HTMLURL: "https://example/r"})

	t.Run("update available", func(t *testing.T) {
		res, err := Check(context.Background(), "0.2.3", "txco-cli/test")
		if err != nil {
			t.Fatal(err)
		}
		if !res.Comparable || !res.Available {
			t.Errorf("got %+v, want comparable+available", res)
		}
		if res.Latest != "0.3.0" {
			t.Errorf("Latest = %q, want 0.3.0", res.Latest)
		}
	})
	t.Run("equal version is up to date", func(t *testing.T) {
		res, _ := Check(context.Background(), "0.3.0", "txco-cli/test")
		if res.Available {
			t.Errorf("equal version should not report available")
		}
	})
	t.Run("current newer than latest", func(t *testing.T) {
		res, _ := Check(context.Background(), "0.4.0", "txco-cli/test")
		if res.Available {
			t.Errorf("current > latest should not report available")
		}
	})
	t.Run("non-semver current is not comparable", func(t *testing.T) {
		res, _ := Check(context.Background(), "dev", "txco-cli/test")
		if res.Comparable {
			t.Errorf("dev should not be comparable")
		}
	})
}

func TestLatestReleaseAssetURL(t *testing.T) {
	withRelease(t, Release{
		TagName: "v0.3.0",
		Assets: []Asset{
			{Name: "checksums.txt", DownloadURL: "https://x/checksums.txt"},
			{Name: "txco_0.3.0_linux_amd64.tar.gz", DownloadURL: "https://x/a.tar.gz"},
		},
	})
	rel, err := LatestRelease(context.Background(), "txco-cli/test")
	if err != nil {
		t.Fatal(err)
	}
	if got := rel.AssetURL("txco_0.3.0_linux_amd64.tar.gz"); got != "https://x/a.tar.gz" {
		t.Errorf("AssetURL = %q, want https://x/a.tar.gz", got)
	}
	if got := rel.AssetURL("nope"); got != "" {
		t.Errorf("AssetURL(missing) = %q, want empty", got)
	}
}
