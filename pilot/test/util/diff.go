// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package util

import (
	"errors"
	"os"
	"regexp"
	"strings"

	"github.com/onsi/gomega"
	"github.com/pmezard/go-difflib/difflib"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/yaml"

	"istio.io/istio/pkg/env"
	"istio.io/istio/pkg/file"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/test"
)

const (
	statusReplacement = "sidecar.istio.io/status: '{\"version\":\"\","
)

var statusPattern = regexp.MustCompile("sidecar.istio.io/status: '{\"version\":\"([0-9a-f]+)\",")

// Refresh controls whether to update the golden artifacts instead.
// It is set using the environment variable REFRESH_GOLDEN.
func Refresh() bool {
	return env.Register("REFRESH_GOLDEN", false, "").Get()
}

// Compare compares two byte slices. It returns an error with a
// contextual diff if they are not equal.
func Compare(content, golden []byte) error {
	data := strings.TrimSpace(string(content))
	expected := strings.TrimSpace(string(golden))

	if data != expected {
		diff := difflib.UnifiedDiff{
			A:       difflib.SplitLines(expected),
			B:       difflib.SplitLines(data),
			Context: 2,
		}
		text, err := difflib.GetUnifiedDiffString(diff)
		if err != nil {
			return err
		}
		return errors.New(text)
	}

	return nil
}

// CompareYAML compares a file "x" against a golden file "x.golden"
func CompareYAML(t test.Failer, filename string) {
	t.Helper()
	content, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf(err.Error())
	}
	goldenFile := filename + ".golden"
	if Refresh() {
		t.Logf("Refreshing golden file for %s", filename)
		if err = os.WriteFile(goldenFile, content, 0o644); err != nil {
			t.Fatal(err.Error())
		}
	}

	golden, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatalf(err.Error())
	}
	if err = Compare(content, golden); err != nil {
		t.Fatalf("Failed validating artifact %s:\n%v", filename, err)
	}
}

// CompareContent compares the content value against the golden file and fails the test if they differ
func CompareContent(t test.Failer, content []byte, goldenFile string) {
	t.Helper()
	golden := ReadGoldenFile(t, content, goldenFile)
	CompareBytes(t, content, golden, goldenFile)
}

func CompareObjects(t test.Failer, client kube.Client, goldenFile string) {
	t.Helper()
	golden := ReadFile(t, goldenFile)

	store := client.Dynamic().(*dynamicfake.FakeDynamicClient).Tracker()

	for _, doc := range strings.Split(string(golden), "---\n") {
		if len(strings.TrimSpace(doc)) == 0 {
			continue
		}
		data := map[string]any{}
		err := yaml.Unmarshal([]byte(doc), &data)
		if err != nil {
			t.Fatalf("Unable to parse golden file as yaml: %s", err)
		}
		us := unstructured.Unstructured{Object: data}
		gvr, err := controllers.UnstructuredToGVR(us)
		if err != nil {
			t.Fatalf("Unable to parse golden file as unstructured: %s", err)
		}

		stored, err := store.Get(gvr, us.GetName(), us.GetNamespace())
		if err != nil {
			t.Fatalf("Unable to retrieve object [%s/%s/%s] from tracker: %s", gvr.Resource, us.GetNamespace(), us.GetName(), err)
		}

		g := gomega.NewGomegaWithT(t)
		g.Expect(stored).To(gomega.BeEquivalentTo(&us))
	}
}

// ReadGoldenFile reads the content of the golden file and fails the test if an error is encountered
func ReadGoldenFile(t test.Failer, content []byte, goldenFile string) []byte {
	t.Helper()
	RefreshGoldenFile(t, content, goldenFile)

	return ReadFile(t, goldenFile)
}

// StripVersion strips the version fields of a YAML content.
func StripVersion(yaml []byte) []byte {
	return statusPattern.ReplaceAllLiteral(yaml, []byte(statusReplacement))
}

// RefreshGoldenFile updates the golden file with the given content
func RefreshGoldenFile(t test.Failer, content []byte, goldenFile string) {
	if Refresh() {
		t.Logf("Refreshing golden file %s", goldenFile)
		if err := file.AtomicWrite(goldenFile, content, os.FileMode(0o644)); err != nil {
			t.Fatal(err.Error())
		}
	}
}

// ReadFile reads the content of the given file or fails the test if an error is encountered.
func ReadFile(t test.Failer, file string) []byte {
	t.Helper()
	golden, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err.Error())
	}
	return golden
}

// CompareBytes compares the content value against the golden bytes and fails the test if they differ
func CompareBytes(t test.Failer, content []byte, golden []byte, name string) {
	t.Helper()
	if err := Compare(content, golden); err != nil {
		t.Fatalf("Failed validating golden file %s:\n%v", name, err)
	}
}
