package manifest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestBundleRoundTrip(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dist"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "queries"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dist", "index.html"), []byte("<h1>Sales</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "queries", "recent.sql"), []byte("SELECT $limit"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := Manifest{App: App{Slug: "sales", Dir: "dist"}, Queries: []Query{{
		Name: "recent-orders", File: "queries/recent.sql", Parameters: map[string]string{"limit": "integer"},
	}}}
	var bundle bytes.Buffer
	if err := BuildBundle(root, m, &bundle); err != nil {
		t.Fatal(err)
	}
	parsed, queries, err := ReadBundle(bundle.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.App.Slug != "sales" || queries["recent-orders"] != "SELECT $limit" {
		t.Fatalf("unexpected bundle: %+v %+v", parsed, queries)
	}
	asset, _, err := Asset(bundle.Bytes(), parsed.App.Dir, "/")
	if err != nil || string(asset) != "<h1>Sales</h1>" {
		t.Fatalf("unexpected asset %q: %v", asset, err)
	}
}

func TestRejectsUnsafeManifest(t *testing.T) {
	for _, input := range []string{
		`{"app":{"slug":"sales","dir":"../dist"},"queries":[]}`,
		`{"app":{"slug":"sales","dir":"dist"},"queries":[{"name":"query-one","file":"/tmp/a.sql","parameters":{}}]}`,
		`{"app":{"slug":"sales","dir":"dist"},"queries":[{"name":"query-one","file":"q.sql","parameters":{"x":"object"}}]}`,
	} {
		if _, err := Parse([]byte(input)); err == nil {
			t.Fatalf("unsafe manifest accepted: %s", input)
		}
	}
}
