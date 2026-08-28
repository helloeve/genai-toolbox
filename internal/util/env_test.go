// Copyright 2026 Google LLC
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
	"strings"
	"testing"
)

func TestExpandEnvVars_SubstitutesAndPreservesText(t *testing.T) {
	// replace echoes the ref so we can assert both parsing and assembly.
	out := ExpandEnvVars("a=${FOO} b=${BAR:def} c", func(r EnvRef) string {
		if r.DefaultDefined {
			return r.Name + "|" + r.DefaultValue
		}
		return r.Name
	})
	want := "a=FOO b=BAR|def c"
	if out != want {
		t.Fatalf("ExpandEnvVars = %q, want %q", out, want)
	}
}

func TestExpandEnvVars_ParsesRefFields(t *testing.T) {
	input := "x ${FOO} y ${BAR:d1} z ${BAZ:}"
	var got []EnvRef
	ExpandEnvVars(input, func(r EnvRef) string {
		got = append(got, r)
		return ""
	})

	want := []EnvRef{
		{Name: "FOO", DefaultDefined: false, DefaultValue: "", Start: 2},
		{Name: "BAR", DefaultDefined: true, DefaultValue: "d1", Start: 11},
		{Name: "BAZ", DefaultDefined: true, DefaultValue: "", Start: 23}, // ${BAZ:} => empty default, still "defined"
	}
	if len(got) != len(want) {
		t.Fatalf("got %d refs, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ref[%d] = %+v, want %+v", i, got[i], want[i])
		}
		// Start must point at the '$' of the reference.
		if input[got[i].Start] != '$' {
			t.Errorf("ref[%d].Start=%d does not point at '$' (got %q)", i, got[i].Start, input[got[i].Start])
		}
	}
}

func TestExpandEnvVars_NoReferences(t *testing.T) {
	called := false
	out := ExpandEnvVars("plain text, no refs", func(EnvRef) string {
		called = true
		return "X"
	})
	if out != "plain text, no refs" {
		t.Fatalf("out = %q, want unchanged", out)
	}
	if called {
		t.Fatal("replace should not be called when there are no references")
	}
}

func TestResolveEnvVars_UsesEnvThenDefault(t *testing.T) {
	t.Setenv("RESOLVE_PRESENT", "hello")
	// RESOLVE_ABSENT intentionally unset → falls back to default.

	out, err := ResolveEnvVars("p=${RESOLVE_PRESENT} d=${RESOLVE_ABSENT:fallback} e=${RESOLVE_PRESENT:ignored}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// A present var wins over its default.
	want := "p=hello d=fallback e=hello"
	if out != want {
		t.Fatalf("ResolveEnvVars = %q, want %q", out, want)
	}
}

func TestResolveEnvVars_EmptyDefault(t *testing.T) {
	out, err := ResolveEnvVars("v=[${RESOLVE_UNSET_EMPTY:}]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "v=[]" {
		t.Fatalf("ResolveEnvVars = %q, want %q", out, "v=[]")
	}
}

func TestResolveEnvVars_MissingWithoutDefaultErrors(t *testing.T) {
	_, err := ResolveEnvVars("${RESOLVE_MISSING_ONE}")
	if err == nil {
		t.Fatal("expected error for missing var without default, got nil")
	}
	if !strings.Contains(err.Error(), "RESOLVE_MISSING_ONE") {
		t.Fatalf("error should name the missing var, got: %v", err)
	}
}

func TestResolveEnvVars_MultipleMissingDeduped(t *testing.T) {
	// Two distinct missing vars, one repeated — error should list each once.
	_, err := ResolveEnvVars("${MISS_A} ${MISS_B} ${MISS_A}")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "MISS_A") || !strings.Contains(msg, "MISS_B") {
		t.Fatalf("error should name both missing vars, got: %v", err)
	}
	if strings.Count(msg, "MISS_A") != 1 {
		t.Fatalf("MISS_A should appear once (deduped), got: %v", err)
	}
}

func TestResolveEnvVars_NoRefsPassthrough(t *testing.T) {
	out, err := ResolveEnvVars("nothing to expand")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "nothing to expand" {
		t.Fatalf("out = %q, want unchanged", out)
	}
}
