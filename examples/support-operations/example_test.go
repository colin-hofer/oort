package supportoperations

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"

	"oort/internal/connector"
	"oort/internal/manifest"
	"oort/internal/queryexec"
)

func TestExampleBuildsAndQueriesFixtureData(t *testing.T) {
	m, err := manifest.Load(manifest.FileName)
	if err != nil {
		t.Fatal(err)
	}
	var bundle bytes.Buffer
	if err := manifest.BuildBundle(".", m, &bundle); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manifest.ReadBundle(bundle.Bytes()); err != nil {
		t.Fatal(err)
	}
	mainJS, _, err := manifest.Asset(bundle.Bytes(), m.App.Dir, "/main.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mainJS), "'./oort-sdk.js'") {
		t.Fatal("main.js must import the bundled oort-sdk.js")
	}
	if _, _, err := manifest.Asset(bundle.Bytes(), m.App.Dir, "/oort-sdk.js"); err != nil {
		t.Fatalf("load imported SDK: %v", err)
	}

	var lines bytes.Buffer
	if os.Getenv("OORT_EXAMPLE_LIVE") == "1" {
		result, err := connector.Fetch(context.Background(), connector.Config{
			URL:            "https://api.github.com/search/issues?q=repo%3Aduckdb%2Fduckdb%20is%3Aissue&sort=updated&order=desc&per_page=100",
			RecordsPointer: "/items",
		}, &lines)
		if err != nil || result.Rows == 0 {
			t.Fatalf("fetch GitHub issues: rows=%d err=%v", result.Rows, err)
		}
	} else {
		contents, err := os.ReadFile("testdata/github-issues.json")
		if err != nil {
			t.Fatal(err)
		}
		var source []json.RawMessage
		if err := json.Unmarshal(contents, &source); err != nil {
			t.Fatal(err)
		}
		for _, issue := range source {
			lines.Write(issue)
			lines.WriteByte('\n')
		}
	}
	jsonFile := t.TempDir() + "/issues.ndjson"
	if err := os.WriteFile(jsonFile, lines.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	database, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`CREATE TABLE "github-issues" AS SELECT * FROM read_ndjson_auto(?)`, jsonFile); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE "triage-policies" AS SELECT * FROM read_csv_auto(?)`, "data/triage-policies.csv"); err != nil {
		t.Fatal(err)
	}

	tests := map[string]struct {
		parameters map[string]any
		arguments  []any
	}{
		"ops-summary":   {map[string]any{"days": json.Number("30")}, []any{sql.Named("days", 30)}},
		"team-health":   {map[string]any{"days": json.Number("30")}, []any{sql.Named("days", 30)}},
		"ticket-volume": {map[string]any{"days": json.Number("30")}, []any{sql.Named("days", 30)}},
		"priority-backlog": {map[string]any{"days": json.Number("30"), "queue": "all", "limit": json.Number("50")}, []any{
			sql.Named("days", 30), sql.Named("queue", "all"), sql.Named("limit", 50),
		}},
	}
	for _, query := range m.Queries {
		t.Run(query.Name, func(t *testing.T) {
			test := tests[query.Name]
			contents, err := os.ReadFile(query.File)
			if err != nil {
				t.Fatal(err)
			}
			cleaned, types, err := queryexec.Validate(string(contents), test.parameters)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(types, query.Parameters) {
				t.Fatalf("manifest parameters do not match query: got %v, want %v", types, query.Parameters)
			}
			rows, err := database.Query(cleaned, test.arguments...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			if !rows.Next() {
				t.Fatalf("query returned no fixture rows: %v", rows.Err())
			}
		})
	}
}
