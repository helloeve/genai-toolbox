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
	"fmt"
	"os"
	"regexp"
	"strings"
)

// envVarPattern matches ${VAR} and ${VAR:default} references.
var envVarPattern = regexp.MustCompile(`\$\{(\w+)(:([^}]*))?\}`)

// EnvRef describes a single ${VAR} or ${VAR:default} reference found in an input.
type EnvRef struct {
	// Name is the variable name (the VAR in ${VAR}).
	Name string
	// DefaultValue is the text after ':' in ${VAR:default} (empty if none).
	DefaultValue string
	// DefaultDefined reports whether a ':default' clause was present (it may be
	// the empty string, e.g. ${VAR:}).
	DefaultDefined bool
	// Start is the byte offset of the reference in the original input, useful for
	// line/column error reporting.
	Start int
}

// ExpandEnvVars replaces every ${VAR}/${VAR:default} reference in input with the
// string returned by replace for that reference. It performs no environment
// lookups itself; the caller's replace closure decides each substitution (and
// any missing-variable policy). This is the shared scanner behind both the
// config-parse env substitution and lazy source materialization.
func ExpandEnvVars(input string, replace func(EnvRef) string) string {
	matches := envVarPattern.FindAllStringSubmatchIndex(input, -1)
	var out strings.Builder
	last := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		out.WriteString(input[last:start])

		ref := EnvRef{Name: input[m[2]:m[3]], Start: start}
		if m[4] != -1 && m[5] != -1 {
			ref.DefaultDefined = true
			ref.DefaultValue = input[m[6]:m[7]]
		}
		out.WriteString(replace(ref))
		last = end
	}
	out.WriteString(input[last:])
	return out.String()
}

// ResolveEnvVars substitutes ${VAR}/${VAR:default} references using the process
// environment. A reference with no matching variable and no default is an error
// (all such names are collected). It is used by lazy source materialization,
// where a missing variable should fail — retryably — at first use.
func ResolveEnvVars(input string) (string, error) {
	var missing []string
	seen := make(map[string]bool)
	out := ExpandEnvVars(input, func(r EnvRef) string {
		if v, ok := os.LookupEnv(r.Name); ok {
			return v
		}
		if r.DefaultDefined {
			return r.DefaultValue
		}
		if !seen[r.Name] {
			seen[r.Name] = true
			missing = append(missing, r.Name)
		}
		return ""
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("environment variable(s) not found: %s", strings.Join(missing, ", "))
	}
	return out, nil
}
