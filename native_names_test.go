package typewirego_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestNativeTestsManifestNamesExist fails when a test named in
// native_tests.json is not defined in this package, so a
// rename or deletion cannot leave a native rule (P11-P20) covered only on paper.
func TestNativeTestsManifestNamesExist(t *testing.T) {
	// Next to cases.json, so TYPEWIRE_CONFORMANCE_CASES relocates both files.
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(casesPath()), "native_tests.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string][]string
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	defined := map[string]bool{}
	testFunc := regexp.MustCompile(`(?m)^func (Test\w+)\(t \*testing\.T\)`)
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range testFunc.FindAllStringSubmatch(string(src), -1) {
			defined[m[1]] = true
		}
	}
	for rule, names := range manifest {
		for _, name := range names {
			if !defined[name] {
				t.Errorf("%s: %s is listed in native_tests.json but not defined", rule, name)
			}
		}
	}
}
