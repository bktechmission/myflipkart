// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Keep the architecture docs current: every top-level package in backend/internal
// and backend/cmd, and every HTTP route, must be mentioned on a page in
// docs/architecture/. Add the page (or the row) in the same PR as the feature.
package api

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestArchitectureDocsCoverPackagesAndRoutes(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	backend := filepath.Join(filepath.Dir(file), "../..")
	pages, _ := filepath.Glob(filepath.Join(backend, "../docs/architecture/*.md"))
	if len(pages) == 0 {
		t.Fatal("docs/architecture/*.md not found")
	}
	var docs strings.Builder
	for _, p := range pages {
		b, _ := os.ReadFile(p)
		docs.Write(b)
	}
	all := docs.String()
	for _, root := range []string{"internal", "cmd"} {
		entries, _ := os.ReadDir(filepath.Join(backend, root))
		for _, e := range entries {
			if e.IsDir() && !strings.Contains(all, root+"/"+e.Name()) {
				t.Errorf("package %s/%s has no architecture page: add it to docs/architecture/", root, e.Name())
			}
		}
	}
	// routes: every mux pattern registered in this package
	re := regexp.MustCompile(`m\.Handle(?:Func)?\("([^"]+)"`)
	srcs, _ := filepath.Glob(filepath.Join(filepath.Dir(file), "*.go"))
	n := 0
	for _, src := range srcs {
		if strings.HasSuffix(src, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(src)
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			n++
			if !strings.Contains(all, m[1]) {
				t.Errorf("route %q has no architecture page: add it to docs/architecture/", m[1])
			}
		}
	}
	if n < 40 {
		t.Fatalf("found only %d routes; the route pattern in this test needs updating", n)
	}
}
