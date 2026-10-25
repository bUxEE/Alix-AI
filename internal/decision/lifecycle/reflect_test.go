package lifecycle

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// reflectMethodCount counts the methods of an interface given as a nil pointer.
func reflectMethodCount(ifacePtr any) int {
	return reflect.TypeOf(ifacePtr).Elem().NumMethod()
}

func reflectMethodNames(v any) []string {
	t := reflect.TypeOf(v)
	var out []string
	for i := 0; i < t.NumMethod(); i++ {
		out = append(out, t.Method(i).Name)
	}
	if t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Interface {
		out = nil
		e := t.Elem()
		for i := 0; i < e.NumMethod(); i++ {
			out = append(out, e.Method(i).Name)
		}
	}
	sort.Strings(out)
	return out
}

func reflectFieldNames(v any) []string {
	t := reflect.TypeOf(v)
	var out []string
	for i := 0; i < t.NumField(); i++ {
		out = append(out, t.Field(i).Name)
	}
	return out
}

// assertNoOutboundCapability parses every non-test source file of the package and
// fails when one imports something that can leave the process or contains a go statement.
func assertNoOutboundCapability(t *testing.T) {
	t.Helper()
	banned := map[string]bool{
		"net": true, "net/http": true, "net/rpc": true, "net/smtp": true, "net/url": true, "os/exec": true,
		"plugin": true, "syscall": true, "unsafe": true, "log/syslog": true, "database/sql": true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("cannot list package sources: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if banned[path] || strings.HasPrefix(path, "net/") {
				t.Errorf("%s imports %q", name, path)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if g, ok := n.(*ast.GoStmt); ok {
				t.Errorf("%s starts a goroutine at %s", name, fset.Position(g.Pos()))
			}
			return true
		})
	}
	if checked < 3 {
		t.Fatalf("only %d source files checked", checked)
	}
}
