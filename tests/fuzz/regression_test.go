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

package fuzz

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"istio.io/istio/pilot/pkg/util/runtime"
	"istio.io/istio/pilot/pkg/util/sets"
	"istio.io/istio/pkg/test/env"
)

// baseCases contains a few trivial test cases to do a very brief sanity check of a test
var baseCases = [][]byte{
	{},
	[]byte("."),
	[]byte(".............."),
}

// brokenCases contains test cases that are currently failing. These should only be added if the
// failure is publicly disclosed!
var brokenCases = map[string]string{
	"6169070276837376": "https://github.com/go-yaml/yaml/issues/666",
	"6087702507290624": "https://github.com/go-yaml/yaml/issues/768",
}

func runRegressionTest(t *testing.T, name string, fuzz func(data []byte) int, expectBadCasesToFail bool) {
	dir := filepath.Join("testdata", name)
	cases, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	runfuzz := func(t *testing.T, name string, by []byte) {
		defer func() {
			if r := recover(); r != nil {
				if _, broken := brokenCases[name]; broken && expectBadCasesToFail {
					t.Logf("expected broken case failed: %v", broken)
				} else {
					runtime.LogPanic(r)
					t.Fatalf("panic encountered: %v", r)
				}
			} else {
				// Ensure we update brokenCases when they are fixed
				if _, broken := brokenCases[name]; broken && expectBadCasesToFail {
					t.Fatalf("expected broken case passed")
				}
			}
		}()
		fuzz(by)
	}
	for i, c := range baseCases {
		t.Run(fmt.Sprintf("base case %d", i), func(t *testing.T) {
			runfuzz(t, "", c)
		})
	}
	for _, c := range cases {
		t.Run(c.Name(), func(t *testing.T) {
			by, err := os.ReadFile(filepath.Join(dir, c.Name()))
			if err != nil {
				t.Fatal(err)
			}
			runfuzz(t, c.Name(), by)
		})
	}
}

func walkMatch(root string, pattern *regexp.Regexp) ([]string, error) {
	var matches []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if filepath.Base(path) == "regression_test.go" {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		bytes, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		matched := pattern.FindAllString(string(bytes), -1)
		for _, m := range matched {
			// Add the match, with trailing ( and previous `func ` stripped
			matches = append(matches, m[5:len(m)-1])
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return matches, nil
}

func TestFuzzers(t *testing.T) {
	testedFuzzers := sets.NewSet()
	cases := []struct {
		name                 string
		fuzzer               func([]byte) int
		expectBadCasesToFail bool
	}{
		{"FuzzConfigValidation", FuzzConfigValidation, true},
		{"FuzzParseInputs", FuzzParseInputs, true},
		{"FuzzParseAndBuildSchema", FuzzParseAndBuildSchema, true},
		{"FuzzParseMeshNetworks", FuzzParseMeshNetworks, true},
		{"FuzzValidateMeshConfig", FuzzValidateMeshConfig, true},
		{"FuzzInitContext", FuzzInitContext, true},
		{"FuzzXds", FuzzXds, true},
		{"FuzzAnalyzer", FuzzAnalyzer, false},
		{"FuzzCompareDiff", FuzzCompareDiff, true},
		{"FuzzHelmReconciler", FuzzHelmReconciler, true},
		{"FuzzIntoResourceFile", FuzzIntoResourceFile, true},
		{"FuzzTranslateFromValueToSpec", FuzzTranslateFromValueToSpec, true},
		{"FuzzConfigValidation2", FuzzConfigValidation2, true},
		{"FuzzBNMUnmarshalJSON", FuzzBNMUnmarshalJSON, true},
		{"FuzzValidateClusters", FuzzValidateClusters, true},
		{"FuzzCheckIstioOperatorSpec", FuzzCheckIstioOperatorSpec, true},
		{"FuzzV1Alpha1ValidateConfig", FuzzV1Alpha1ValidateConfig, true},
		{"FuzzGetEnabledComponents", FuzzGetEnabledComponents, true},
		{"FuzzUnmarshalAndValidateIOPS", FuzzUnmarshalAndValidateIOPS, true},
		{"FuzzVerify", FuzzVerify, true},
		{"FuzzRenderManifests", FuzzRenderManifests, true},
		{"FuzzOverlayIOP", FuzzOverlayIOP, true},
		{"FuzzNewControlplane", FuzzNewControlplane, true},
		{"FuzzResolveK8sConflict", FuzzResolveK8sConflict, true},
		{"FuzzYAMLManifestPatch", FuzzYAMLManifestPatch, true},
		{"FuzzGalleyMeshFs", FuzzGalleyMeshFs, true},
		{"FuzzGalleyDiag", FuzzGalleyDiag, true},
		{"FuzzNewBootstrapServer", FuzzNewBootstrapServer, true},
		{"FuzzGenCSR", FuzzGenCSR, true},
		{"FuzzCreateCertE2EUsingClientCertAuthenticator", FuzzCreateCertE2EUsingClientCertAuthenticator, true},
	}
	for _, tt := range cases {
		if testedFuzzers.Contains(tt.name) {
			t.Fatalf("dupliate fuzzer test %v", tt.name)
		}
		testedFuzzers.Insert(tt.name)
		t.Run(tt.name, func(t *testing.T) {
			runRegressionTest(t, tt.name, tt.fuzzer, tt.expectBadCasesToFail)
		})
	}
	t.Run("completeness", func(t *testing.T) {
		match := regexp.MustCompile(`func Fuzz.+\(`)
		fuzzers, err := walkMatch(filepath.Join(env.IstioSrc, "tests/fuzz"), match)
		if err != nil {
			t.Fatal(err)
		}
		allFuzzers := sets.NewSet(fuzzers...)
		if !allFuzzers.Equals(testedFuzzers) {
			t.Fatalf("Not all fuzzers are tested! Missing %v", allFuzzers.Difference(testedFuzzers))
		}
	})
}
