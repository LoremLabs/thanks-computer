package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/outlet"
	"github.com/loremlabs/thanks-computer/chassis/outlet/outlettest"
)

func registerFakeOutletDriver(t *testing.T) {
	t.Helper()
	outlet.Register(&outlettest.Driver{DriverName: "fake"})
}

const outletOkRule = `WHEN @web.req.url.query.email.0 != ""
  EXEC "outlet://crm/query"
    WITH sql = "SELECT id, name FROM customers WHERE email = $1",
         args = &array(@web.req.url.query.email.0),
         into = "_crm"
`

func TestCollectOutletFiles(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "OPS/site")
	if files, err := collectOutletFiles(stackDir); err != nil || files != nil {
		t.Fatalf("absent OUTLETS/: %v %v", files, err)
	}
	writeFile(t, filepath.Join(stackDir, "OUTLETS/crm.yaml"), "driver: fake\nsecret: CRM_DSN\n")
	writeFile(t, filepath.Join(stackDir, "OUTLETS/.hidden.yaml"), "x")
	files, err := collectOutletFiles(stackDir)
	if err != nil || len(files) != 1 || files[0].Path != "OUTLETS/crm.yaml" || files[0].Content == "" || files[0].ContentHash == "" {
		t.Fatalf("collect: %+v %v", files, err)
	}
	writeFile(t, filepath.Join(stackDir, "OUTLETS/Bad.yaml"), "driver: fake\n")
	if _, err := collectOutletFiles(stackDir); err == nil || !strings.Contains(err.Error(), "Bad.yaml") {
		t.Fatalf("bad name must fail: %v", err)
	}
}

func TestCheckOutletOps(t *testing.T) {
	registerFakeOutletDriver(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "OPS/site/OUTLETS/crm.yaml"), "driver: fake\nsecret: CRM_DSN\naccess: read\n")
	writeFile(t, filepath.Join(root, "OPS/site/100/lookup.txcl"), outletOkRule)
	writeFile(t, filepath.Join(root, "OPS/site/200/write.txcl"), `EXEC "outlet://crm/exec" WITH sql = "UPDATE customers SET plan = $1", args = &array("team")`+"\n")
	writeFile(t, filepath.Join(root, "OPS/site/300/nope.txcl"), `EXEC "outlet://nope/query" WITH sql = "SELECT 1"`+"\n")
	writeFile(t, filepath.Join(root, "OPS/site/400/dyn.txcl"), `EXEC "outlet://crm/query" WITH sql = @web.req.body`+"\n")
	writeFile(t, filepath.Join(root, "OPS/other/100/plain.txcl"), `EXEC "outlet://crm/query" WITH sql = "SELECT 1"`+"\n")
	ops, _, err := bundle.WalkDiag(root)
	if err != nil {
		t.Fatal(err)
	}
	msgs := checkOutletOps(ops, root)
	want := map[string]string{
		"200/write.txcl": "access: read",
		"300/nope.txcl":  "not declared",
		"400/dyn.txcl":   "path reference",
		"other/100":      "not declared",
	}
	if len(msgs) != len(want) {
		t.Fatalf("want %d messages, got %d:\n%s", len(want), len(msgs), strings.Join(msgs, "\n"))
	}
	for frag, sub := range want {
		found := false
		for _, m := range msgs {
			if strings.Contains(m, frag) && strings.Contains(m, sub) {
				found = true
			}
		}
		if !found {
			t.Errorf("no message for %s containing %q:\n%s", frag, sub, strings.Join(msgs, "\n"))
		}
	}

	// A declaration that doesn't parse is reported once, against its file.
	writeFile(t, filepath.Join(root, "OPS/site/OUTLETS/broken.yaml"), "driver: mysql\nsecret: X\n")
	msgs = checkOutletOps(ops, root)
	n := 0
	for _, m := range msgs {
		if strings.Contains(m, "broken.yaml") && strings.Contains(m, "unknown driver") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("broken declaration should be reported once, got %d:\n%s", n, strings.Join(msgs, "\n"))
	}
}

func TestLintOutletErrorFlipsExit(t *testing.T) {
	registerFakeOutletDriver(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "OPS/site/OUTLETS/crm.yaml"), "driver: fake\nsecret: CRM_DSN\n")
	writeFile(t, filepath.Join(root, "OPS/site/100/lookup.txcl"), outletOkRule)
	var stdout, stderr bytes.Buffer
	if code := runLint([]string{root}, &stdout, &stderr); code != 0 {
		t.Fatalf("clean workspace: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	writeFile(t, filepath.Join(root, "OPS/site/200/write.txcl"), `EXEC "outlet://crm/exec" WITH sql = "DELETE FROM customers"`+"\n")
	stdout.Reset()
	stderr.Reset()
	if code := runLint([]string{root}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "access: read") {
		t.Fatalf("exec on a read outlet must fail lint: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
}
