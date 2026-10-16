// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc.

/*
 * Copyright 2026 Tenstorrent USA, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Golden-file tests for the Helm chart.
//
// The chart is the only thing that configures the driver in a real
// deployment: every flag the kubelet plugin reads arrives as an environment
// variable set here, and the DeviceClass it renders is what claims select
// against. None of that is exercised by the Go tests, and a template bug is
// invisible until an install fails -- as one did in commit 1100eb9, where the
// ServiceAccount rendered as an empty document.
//
// Each case renders the chart with one set of values and compares the result
// byte for byte against a checked-in manifest, so a template change shows up
// in review as a diff of the YAML it produces rather than of the templating
// that produces it.
//
// Regenerate the golden files after an intended change with:
//
//	go test ./test/chart -update
package chart_test

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// chartPath is the chart under test, relative to this package.
const chartPath = "../../helm/tt-dra-driver"

// Release name and namespace, matching the install documented in README.md so
// that the golden files read like a real deployment.
const (
	releaseName = "tt-dra-driver"
	namespace   = "tt-dra-driver"
)

// goldenHelmMajor is the Helm major version the golden files were generated
// with. Output differs across major versions in ways that have nothing to do
// with this chart, so a mismatch skips rather than failing: CI pins this
// version, and a developer with another one still gets a clear reason why the
// case did not run. Bump it together with the CI pin and the golden files.
const goldenHelmMajor = "v4"

var update = flag.Bool("update", false, "rewrite the golden files from the current chart")

// apiVersions is passed to every render so that the DeviceClass template
// resolves against the GA resource API regardless of which Kubernetes release
// the local helm was built against.
//
// It cannot exercise the template's fallback to the beta API: --api-versions
// only adds to helm's built-in capability set and there is no way to take
// resource.k8s.io/v1 back out of it, so that branch is not reachable from
// here.
var apiVersions = []string{"resource.k8s.io/v1"}

type renderCase struct {
	// name is both the subtest name and the golden file's basename.
	name string

	// why records what the case is here to catch, so that a failure is
	// readable without digging through the chart.
	why string

	// set are the --set arguments this case renders with.
	set []string
}

var cases = []renderCase{
	{
		name: "defaults",
		why: "The stock install. Pins the DaemonSet's environment -- which is " +
			"how every driver flag is configured in production -- along with " +
			"the RBAC rules, the node affinity and the DeviceClass.",
	},
	{
		name: "healthcheck-disabled",
		why: "A negative healthcheckPort disables the liveness service. The " +
			"probe has to disappear with it, or the kubelet probes a port " +
			"nothing is listening on and restarts a healthy driver.",
		set: []string{"kubeletPlugin.containers.plugin.healthcheckPort=-1"},
	},
	{
		name: "existing-service-account",
		why: "Installing against a ServiceAccount the cluster already has. " +
			"The chart must not render one of its own, and both the " +
			"ClusterRoleBinding subject and the DaemonSet have to name the " +
			"account given instead of the one they would have created.",
		set: []string{
			"serviceAccount.create=false",
			"serviceAccount.name=preexisting-sa",
		},
	},
	{
		name: "custom-profile",
		why: "A profile other than tenstorrent derives both the driver name " +
			"and the DeviceClass name from it. The driver and the DeviceClass " +
			"have to agree, or claims select a class no driver publishes to.",
		set: []string{"deviceProfile=example"},
	},
	{
		name: "explicit-driver-name",
		why: "An explicit driverName overrides the derivation entirely, " +
			"including for the DeviceClass and the CEL selector inside it.",
		set: []string{"driverName=dra.example.com"},
	},
}

func TestChartRenders(t *testing.T) {
	helm := requireHelm(t)

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Log(test.why)

			got := render(t, helm, test)
			golden := filepath.Join("testdata", test.name+".golden.yaml")

			if *update {
				if err := os.WriteFile(golden, []byte(got), 0600); err != nil {
					t.Fatalf("write %s: %v", golden, err)
				}
				return
			}

			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read %s (run `go test ./test/chart -update` to create it): %v", golden, err)
			}
			if got != string(want) {
				t.Errorf("the rendered chart does not match %s.\n%s\n"+
					"Run `go test ./test/chart -update` and review the diff if the change was intended.",
					golden, firstDifference(string(want), got))
			}
		})
	}
}

// render runs `helm template` for one case and returns its output.
func render(t *testing.T, helm string, test renderCase) string {
	t.Helper()

	args := []string{
		"template", releaseName, chartPath,
		"--namespace", namespace,
	}
	for _, apiVersion := range apiVersions {
		args = append(args, "--api-versions", apiVersion)
	}
	for _, set := range test.set {
		args = append(args, "--set", set)
	}

	cmd := exec.Command(helm, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// requireHelm locates a usable helm binary, skipping the test when there is
// none or when its major version is not the one the golden files were
// generated with.
func requireHelm(t *testing.T) string {
	t.Helper()

	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not on PATH; skipping the chart render tests")
	}

	out, err := exec.Command(helm, "version", "--short").Output()
	if err != nil {
		t.Fatalf("helm version: %v", err)
	}
	version := strings.TrimSpace(string(out))
	if !strings.HasPrefix(version, goldenHelmMajor+".") {
		t.Skipf("the golden files were generated with helm %s.x; this is %q, "+
			"whose output differs for reasons unrelated to this chart", goldenHelmMajor, version)
	}
	return helm
}

// firstDifference renders the first differing line with a little context, so
// that a failure names the change instead of printing two whole manifests.
func firstDifference(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")

	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		wantLine, gotLine := lineAt(wantLines, i), lineAt(gotLines, i)
		if wantLine == gotLine {
			continue
		}
		var b strings.Builder
		for j := max(0, i-3); j < i; j++ {
			b.WriteString("  " + lineAt(wantLines, j) + "\n")
		}
		b.WriteString("- " + wantLine + "\n")
		b.WriteString("+ " + gotLine)
		return "first difference at line " + strconv.Itoa(i+1) + ":\n" + b.String()
	}
	return "the files differ only in trailing content"
}

func lineAt(lines []string, i int) string {
	if i < 0 || i >= len(lines) {
		return "<end of file>"
	}
	return lines[i]
}
