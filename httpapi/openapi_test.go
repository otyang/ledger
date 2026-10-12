package httpapi_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The OpenAPI spec documents exactly the routes the server registers.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	src, err := os.ReadFile("httpapi.go")
	if err != nil {
		t.Fatal(err)
	}
	var routes []string
	for _, m := range regexp.MustCompile(`(?:route|mux\.HandleFunc)\("([A-Z]+) (/[^"]*)"`).FindAllStringSubmatch(string(src), -1) {
		routes = append(routes, m[1]+" "+m[2])
	}

	spec, err := os.ReadFile("../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var documented []string
	path := ""
	inPaths := false
	for _, line := range strings.Split(string(spec), "\n") {
		switch {
		case line == "paths:":
			inPaths = true
		case inPaths && len(line) > 0 && line[0] != ' ':
			inPaths = false
		case inPaths && strings.HasPrefix(line, "  /") && strings.HasSuffix(line, ":"):
			path = strings.TrimSuffix(strings.TrimSpace(line), ":")
		case inPaths && regexp.MustCompile(`^    (get|post|put|patch|delete):$`).MatchString(line):
			documented = append(documented, strings.ToUpper(strings.TrimSuffix(strings.TrimSpace(line), ":"))+" "+path)
		}
	}

	slices.Sort(routes)
	slices.Sort(documented)
	if len(routes) < 30 {
		t.Fatalf("found only %d routes in httpapi.go; has the registration changed?", len(routes))
	}
	for _, r := range routes {
		if !slices.Contains(documented, r) {
			t.Errorf("route %q is missing from docs/openapi.yaml", r)
		}
	}
	for _, d := range documented {
		if !slices.Contains(routes, d) {
			t.Errorf("docs/openapi.yaml documents %q, which the server doesn't serve", d)
		}
	}
}
