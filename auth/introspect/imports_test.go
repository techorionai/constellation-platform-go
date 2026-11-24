package introspect_test

import (
	"go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Issue 15 U28: the adapter does not import another adapter, the JOSE library,
// or the generated client. The depguard rule adapter-auth-introspect in
// .golangci.yml checks the same rule in the lint job.
func TestU28Imports(t *testing.T) {
	const mod = "github.com/techorionai/constellation-platform-go/"
	denied := []string{
		"github.com/go-jose/go-jose", mod + "clients/identity", mod + "auth/clientcred", mod + "auth/jwt",
		mod + "migrate/postgres", mod + "health/postgres", mod + "health/valkey", mod + "cache/valkey",
		mod + "transaction/postgres", mod + "secrets/postgres", mod + "retention/postgres",
	}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatal(err)
	}
	for _, f := range files {
		src, err := os.ReadFile(f) //nolint:gosec // f is from Glob in the test folder
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(gotoken.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, d := range denied {
				if path == d || strings.HasPrefix(path, d+"/") {
					t.Errorf("%s imports %s", f, path)
				}
			}
		}
	}
}
