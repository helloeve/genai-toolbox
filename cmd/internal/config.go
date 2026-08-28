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

package internal

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/parser"
	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/auth/generic"
	"github.com/googleapis/mcp-toolbox/internal/server"
	"github.com/googleapis/mcp-toolbox/internal/util"
)

type Config struct {
	Sources         server.SourceConfigs         `yaml:"sources"`
	AuthServices    server.AuthServiceConfigs    `yaml:"authServices"`
	EmbeddingModels server.EmbeddingModelConfigs `yaml:"embeddingModels"`
	Tools           server.ToolConfigs           `yaml:"tools"`
	Prompts         server.PromptConfigs         `yaml:"prompts"`
	Groups          server.GroupConfigs          `yaml:"groups"`

	// RawSourceBlocks holds each source's env-unresolved, flat-format YAML block
	// keyed by source name. It is populated only when the parser runs with
	// LazySources enabled; in that mode Sources is left empty and sources are
	// materialized from these blocks on first use.
	RawSourceBlocks map[string][]byte `yaml:"-"`
}

type ConfigParser struct {
	EnvVars         map[string]string
	OptionalEnvVars []string
	requiredEnvVars []string

	// AllowMissingEnvVars, when true, substitutes the variable name for an unset
	// required ${VAR} placeholder instead of erroring. Used by offline flows like
	// skills-generate, where source env vars are needed only to satisfy config
	// parsing/validation, never to connect. A non-empty placeholder is used (not
	// "") so required string fields still pass validation. The served path leaves
	// this false so missing config still fails fast.
	AllowMissingEnvVars bool

	// LazySources, when true, defers source config env resolution and decoding:
	// each source's flat-format YAML block is retained (env-unresolved) in
	// Config.RawSourceBlocks instead of being decoded into Config.Sources. Used
	// by --lazy-loading so a missing source env var never fails startup.
	LazySources bool
}

// parseEnv replaces environment variables ${ENV_NAME} with their values.
// also support ${ENV_NAME:default_value}.
func (p *ConfigParser) parseEnv(input string) (string, error) {
	if p.EnvVars == nil {
		p.EnvVars = make(map[string]string)
	}

	var missing []string
	seenMissing := make(map[string]bool)
	// The ${VAR}/${VAR:default} scanning lives in util.ExpandEnvVars; this
	// closure supplies the config-parse policy (env tracking, AllowMissingEnvVars
	// placeholder, and line/column error reporting).
	output := util.ExpandEnvVars(input, func(r util.EnvRef) string {
		if r.DefaultDefined {
			p.OptionalEnvVars = append(p.OptionalEnvVars, r.Name)
		} else {
			p.requiredEnvVars = append(p.requiredEnvVars, r.Name)
		}

		if value, found := os.LookupEnv(r.Name); found {
			p.EnvVars[r.Name] = value
			return value
		}
		if r.DefaultDefined {
			p.EnvVars[r.Name] = r.DefaultValue
			return r.DefaultValue
		}
		if p.AllowMissingEnvVars {
			p.EnvVars[r.Name] = r.Name
			return r.Name
		}
		if !seenMissing[r.Name] {
			seenMissing[r.Name] = true
			line, column := lineColumnAt(input, r.Start)
			missing = append(missing, fmt.Sprintf("%q (line %d, column %d)", r.Name, line, column))
		}
		return ""
	})

	// Filter out OptionalEnvVars that were also found as required
	var finalOptional []string
	for _, v := range p.OptionalEnvVars {
		if !slices.Contains(p.requiredEnvVars, v) && !slices.Contains(finalOptional, v) {
			finalOptional = append(finalOptional, v)
		}
	}
	p.OptionalEnvVars = finalOptional

	var err error
	if len(missing) > 0 {
		if len(missing) == 1 {
			err = fmt.Errorf("environment variable not found: %s", missing[0])
		} else {
			err = fmt.Errorf("environment variables not found:\n  - %s", strings.Join(missing, "\n  - "))
		}
	}

	return output, err
}

// ParseConfig parses the provided yaml into appropriate configs.
func lineColumnAt(input string, index int) (int, int) {
	line := 1
	column := 1
	for i, r := range input {
		if i >= index {
			break
		}
		if r == '\n' {
			line++
			column = 1
		} else {
			column++
		}
	}
	return line, column
}

func (p *ConfigParser) ParseConfig(ctx context.Context, raw []byte) (Config, error) {
	if p.LazySources {
		return p.parseConfigLazy(ctx, raw)
	}

	var config Config
	// Replace environment variables if found
	output, err := p.parseEnv(string(raw))
	if err != nil {
		return config, fmt.Errorf("error parsing environment variables: %s", err)
	}
	raw = []byte(output)

	raw, err = ConvertConfig(ctx, raw)
	if err != nil {
		return config, fmt.Errorf("error converting config file: %s", err)
	}

	// Parse contents
	config.Sources, config.AuthServices, config.EmbeddingModels, config.Tools, config.Prompts, config.Groups, err = server.UnmarshalPrimitiveConfig(ctx, raw)
	if err != nil {
		return config, err
	}
	return config, nil
}

// parseConfigLazy parses a config while deferring source env resolution and
// decoding. Sources are converted to flat format but not env-resolved; their raw
// blocks are retained in Config.RawSourceBlocks for materialization on first use.
// Everything else (tools, authServices, embeddingModels, prompts, groups) is
// env-resolved and decoded eagerly, exactly as in the non-lazy path.
func (p *ConfigParser) parseConfigLazy(ctx context.Context, raw []byte) (Config, error) {
	var config Config

	// Convert to flat format first, without resolving env vars, so that source
	// blocks keep their unresolved ${VAR} references.
	flat, err := ConvertConfig(ctx, raw)
	if err != nil {
		return config, fmt.Errorf("error converting config file: %s", err)
	}

	sourceBlocks, otherDocs, err := splitSourceBlocks(flat)
	if err != nil {
		return config, err
	}

	// Resolve env + decode everything except sources.
	resolved, err := p.parseEnv(string(otherDocs))
	if err != nil {
		return config, fmt.Errorf("error parsing environment variables: %s", err)
	}

	config.Sources, config.AuthServices, config.EmbeddingModels, config.Tools, config.Prompts, config.Groups, err = server.UnmarshalPrimitiveConfig(ctx, []byte(resolved))
	if err != nil {
		return config, err
	}
	config.RawSourceBlocks = sourceBlocks
	return config, nil
}

// splitSourceBlocks partitions a flat-format (multi-document) config into the
// per-source raw YAML blocks (keyed by source name, env-unresolved) and the
// remaining documents concatenated back together. It performs no env
// substitution, so a source block referencing an unset variable does not fail.
func splitSourceBlocks(flatRaw []byte) (map[string][]byte, []byte, error) {
	file, err := parser.ParseBytes(flatRaw, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to parse config: %s", yaml.FormatError(err, false, false))
	}

	sourceBlocks := make(map[string][]byte)
	var otherDocs bytes.Buffer
	// keepDoc appends a single normalized document (exactly one "---" marker) to
	// otherDocs. DocumentNode.String() already includes a leading "---", so we
	// strip it first to avoid emitting empty documents from doubled separators.
	keepDoc := func(docStr string) {
		body := strings.TrimPrefix(strings.TrimSpace(docStr), "---")
		body = strings.TrimSpace(body)
		otherDocs.WriteString("---\n")
		otherDocs.WriteString(body)
		otherDocs.WriteString("\n")
	}
	for _, doc := range file.Docs {
		if doc == nil || doc.Body == nil {
			continue
		}
		docStr := doc.String()

		var resource map[string]any
		// Classifying only needs 'kind' and 'name', which are literals; decoding
		// tolerates unresolved ${VAR} values (they stay strings).
		if derr := yaml.NewDecoder(strings.NewReader(docStr)).Decode(&resource); derr != nil {
			// Could not classify; leave it in the eager path.
			keepDoc(docStr)
			continue
		}
		kind, _ := resource["kind"].(string)
		name, _ := resource["name"].(string)
		if kind == "source" && name != "" {
			sourceBlocks[name] = []byte(docStr)
			continue
		}
		keepDoc(docStr)
	}
	return sourceBlocks, otherDocs.Bytes(), nil
}

// ConvertConfig converts configuration file to flat format and rewrites toolsets
// to the group kind.
func ConvertConfig(ctx context.Context, raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	// Manually copy top-level comments and empty lines from the source
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// If the line is a comment or whitespace, preserve it
		if strings.HasPrefix(trimmed, "#") || trimmed == "" {
			buf.WriteString(line + "\n")
		} else {
			// Stop at the first line of actual data
			break
		}
	}

	// convert configuration file to flat format
	var input yaml.MapSlice
	decoder := yaml.NewDecoder(bytes.NewReader(raw), yaml.UseOrderedMap())
	encoder := yaml.NewEncoder(&buf, yaml.UseLiteralStyleIfMultiline(true))

	nestedFormatKey := []string{"sources", "authServices", "embeddingModels", "tools", "toolsets", "prompts", "groups"}
	docIndex := 0
	for {
		if err := decoder.Decode(&input); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		docIndex++
		for _, item := range input {
			key, ok := item.Key.(string)
			if !ok {
				return nil, fmt.Errorf("doc %d: unexpected non-string key in input: %v", docIndex, item.Key)
			}
			if hasKindField(input) {
				// this doc is already in flat format, encode to buf
				if err := encoder.Encode(migrateToolsetKind(ctx, input)); err != nil {
					return nil, err
				}
				break
			}
			// check if value conversion to yaml.MapSlice successfully
			if slice, ok := item.Value.(yaml.MapSlice); slices.Contains(nestedFormatKey, key) && ok {
				// srcKey is kept for error messages, which should name the key the
				// user actually wrote rather than the flat kind it maps to.
				srcKey := key
				switch key {
				case "authServices":
					key = "authService"
				case "sources":
					key = "source"
				case "embeddingModels":
					key = "embeddingModel"
				case "tools":
					key = "tool"
				case "toolsets":
					key = "toolset"
				case "prompts":
					key = "prompt"
				case "groups":
					key = "group"
				}
				transformed, err := transformDocs(key, slice)
				if err != nil {
					return nil, fmt.Errorf("doc %d: invalid config format at key %q: %w", docIndex, srcKey, err)
				}
				// encode per-doc
				for _, doc := range transformed {
					if err := encoder.Encode(migrateToolsetKind(ctx, doc)); err != nil {
						return nil, err
					}
				}
			} else {
				return nil, fmt.Errorf("doc %d: invalid config format at key %q: expected nested format keys and type map", docIndex, key)
			}
		}
	}
	return buf.Bytes(), nil
}

// hasKindField is a helper function to check if an input is in flat format
func hasKindField(input yaml.MapSlice) bool {
	for _, item := range input {
		if key, ok := item.Key.(string); ok && key == "kind" {
			return true
		}
	}
	return false
}

// migrateToolsetKind rewrites `kind: toolset` to `kind: group`, preserving field
// order, and returns other kinds unchanged. Every flat doc passes through here,
// so nested and already-flat toolsets cannot diverge.
//
// A description on a toolset is dropped with a warning rather than promoted: a
// toolset has none of its own, so publishing it would give the collection a
// description it never declared.
func migrateToolsetKind(ctx context.Context, input yaml.MapSlice) yaml.MapSlice {
	kindIndex, descIndex, nameIndex := -1, -1, -1
	for i, item := range input {
		switch item.Key {
		case "kind":
			kindIndex = i
		case "description":
			descIndex = i
		case "name":
			nameIndex = i
		}
	}
	if kindIndex < 0 {
		return input
	}
	if val, ok := input[kindIndex].Value.(string); !ok || val != "toolset" {
		return input
	}

	// Copy before rewriting: ConvertConfig hands us the slice it is still
	// ranging over, so editing input in place would shift elements out from
	// under that loop.
	migrated := slices.Clone(input)
	migrated[kindIndex].Value = "group"
	if descIndex >= 0 {
		migrated = slices.Delete(migrated, descIndex, descIndex+1)

		var name string
		if nameIndex >= 0 {
			name, _ = input[nameIndex].Value.(string)
		}
		// Warning is best effort: a caller without a logger in context still
		// gets the conversion.
		if logger, err := util.LoggerFromContext(ctx); err == nil {
			logger.WarnContext(ctx, fmt.Sprintf("toolset %q: dropping description, which a toolset does not support; declare the collection as `kind: group` to keep it", name))
		}
	}
	return migrated
}

// transformDocs transforms the configuration file from nested to flat format
// yaml.MapSlice will preserve the order in a map
func transformDocs(kind string, input yaml.MapSlice) ([]yaml.MapSlice, error) {
	var transformed []yaml.MapSlice
	for _, entry := range input {
		entryName, ok := entry.Key.(string)
		if !ok {
			return nil, fmt.Errorf("unexpected non-string key for entry in '%s': %v", kind, entry.Key)
		}
		entryBody := processValue(entry.Value, kind == "toolset")

		currentTransformed := yaml.MapSlice{
			{Key: "kind", Value: kind},
			{Key: "name", Value: entryName},
		}

		// Merge the transformed body into our result
		if bodySlice, ok := entryBody.(yaml.MapSlice); ok {
			currentTransformed = append(currentTransformed, bodySlice...)
		} else {
			return nil, fmt.Errorf("unable to convert entryBody to MapSlice")
		}
		transformed = append(transformed, currentTransformed)
	}
	return transformed, nil
}

// processValue recursively looks for MapSlices to rename 'kind' -> 'type'
func processValue(v any, isToolset bool) any {
	switch val := v.(type) {
	case yaml.MapSlice:
		// creating a new MapSlice is safer for recursive transformation
		newVal := make(yaml.MapSlice, len(val))
		for i, item := range val {
			// Perform renaming
			if item.Key == "kind" {
				item.Key = "type"
			}
			// Recursive call for nested values (e.g., nested objects or lists)
			item.Value = processValue(item.Value, false)
			newVal[i] = item
		}
		return newVal
	case []any:
		// Process lists: If it's a toolset top-level list, wrap it.
		if isToolset {
			return yaml.MapSlice{{Key: "tools", Value: val}}
		}
		// Otherwise, recurse into list items (to catch nested objects)
		newVal := make([]any, len(val))
		for i := range val {
			newVal[i] = processValue(val[i], false)
		}
		return newVal
	default:
		return val
	}
}

// mergeConfigs merges multiple Config structs into one.
// Detects and raises errors for resource conflicts in sources, authServices, tools, and groups.
// All resource names (sources, authServices, tools, groups) must be unique across all files.
func mergeConfigs(files ...Config) (Config, error) {
	merged := Config{
		Sources:         make(server.SourceConfigs),
		AuthServices:    make(server.AuthServiceConfigs),
		EmbeddingModels: make(server.EmbeddingModelConfigs),
		Tools:           make(server.ToolConfigs),
		Prompts:         make(server.PromptConfigs),
		Groups:          make(server.GroupConfigs),
	}

	var conflicts []string

	for fileIndex, file := range files {
		// Check for conflicts and merge sources
		for name, source := range file.Sources {
			if mergedSource, exists := merged.Sources[name]; exists {
				if !cmp.Equal(mergedSource, source) {
					conflicts = append(conflicts, fmt.Sprintf("source '%s' (file #%d)", name, fileIndex+1))
				}
			} else {
				merged.Sources[name] = source
			}
		}

		// Check for conflicts and merge authServices
		for name, authService := range file.AuthServices {
			if _, exists := merged.AuthServices[name]; exists {
				conflicts = append(conflicts, fmt.Sprintf("authService '%s' (file #%d)", name, fileIndex+1))
			} else {
				merged.AuthServices[name] = authService
			}
		}

		// Check for conflicts and merge embeddingModels
		for name, em := range file.EmbeddingModels {
			if _, exists := merged.EmbeddingModels[name]; exists {
				conflicts = append(conflicts, fmt.Sprintf("embedding model '%s' (file #%d)", name, fileIndex+1))
			} else {
				merged.EmbeddingModels[name] = em
			}
		}

		// Check for conflicts and merge tools
		for name, tool := range file.Tools {
			if _, exists := merged.Tools[name]; exists {
				conflicts = append(conflicts, fmt.Sprintf("tool '%s' (file #%d)", name, fileIndex+1))
			} else {
				merged.Tools[name] = tool
			}
		}

		// Check for conflicts and merge prompts
		for name, prompt := range file.Prompts {
			if _, exists := merged.Prompts[name]; exists {
				conflicts = append(conflicts, fmt.Sprintf("prompt '%s' (file #%d)", name, fileIndex+1))
			} else {
				merged.Prompts[name] = prompt
			}
		}

		// Check for conflicts and merge groups
		for name, grp := range file.Groups {
			if _, exists := merged.Groups[name]; exists {
				conflicts = append(conflicts, fmt.Sprintf("group '%s' (file #%d)", name, fileIndex+1))
			} else {
				merged.Groups[name] = grp
			}
		}

		// Check for conflicts and merge deferred (lazy) source blocks. These
		// share the source namespace, so a name colliding with an eager source
		// or another lazy block is a conflict.
		for name, block := range file.RawSourceBlocks {
			_, inEager := merged.Sources[name]
			_, inLazy := merged.RawSourceBlocks[name]
			if inEager || inLazy {
				conflicts = append(conflicts, fmt.Sprintf("source '%s' (file #%d)", name, fileIndex+1))
			} else {
				if merged.RawSourceBlocks == nil {
					merged.RawSourceBlocks = make(map[string][]byte)
				}
				merged.RawSourceBlocks[name] = block
			}
		}
	}

	// If conflicts were detected, return an error
	if len(conflicts) > 0 {
		return Config{}, fmt.Errorf("resource conflicts detected:\n  - %s\n\nPlease ensure each source, authService, tool, prompt and group has a unique name across all files", strings.Join(conflicts, "\n  - "))
	}

	// Ensure only one authService has mcpEnabled = true
	var mcpEnabledAuthServers []string
	for name, authService := range merged.AuthServices {
		// Only generic type has McpEnabled right now
		if genericService, ok := authService.(generic.Config); ok && genericService.McpEnabled {
			mcpEnabledAuthServers = append(mcpEnabledAuthServers, name)
		}
	}
	if len(mcpEnabledAuthServers) > 1 {
		return Config{}, fmt.Errorf("multiple authServices with mcpEnabled=true detected: %s. Only one MCP authorization server is currently supported", strings.Join(mcpEnabledAuthServers, ", "))
	}

	return merged, nil
}

// LoadAndMergeConfigs loads multiple YAML files and merges them
func (p *ConfigParser) LoadAndMergeConfigs(ctx context.Context, filePaths []string) (Config, error) {
	var configs []Config

	for _, filePath := range filePaths {
		buf, err := os.ReadFile(filePath)
		if err != nil {
			return Config{}, fmt.Errorf("unable to read config file at %q: %w", filePath, err)
		}

		config, err := p.ParseConfig(ctx, buf)
		if err != nil {
			return Config{}, fmt.Errorf("unable to parse config file at %q: %w", filePath, err)
		}

		configs = append(configs, config)
	}
	if len(configs) == 0 {
		return Config{}, fmt.Errorf("no YAML files found")
	}
	if len(configs) > 1 {
		mergedFile, err := mergeConfigs(configs...)
		if err != nil {
			return Config{}, fmt.Errorf("unable to merge config files: %w", err)
		}
		return mergedFile, nil
	}
	return configs[0], nil
}

// GetPathsFromConfigFolder loads all YAML files from a directory and merges them
func GetPathsFromConfigFolder(ctx context.Context, folderPath string) ([]string, error) {
	// Check if directory exists
	info, err := os.Stat(folderPath)
	if err != nil {
		return nil, fmt.Errorf("unable to access config folder at %q: %w", folderPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("path %q is not a directory", folderPath)
	}

	// Find all YAML files in the directory
	pattern := filepath.Join(folderPath, "*.yaml")
	yamlFiles, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("error finding YAML files in %q: %w", folderPath, err)
	}

	// Also find .yml files
	ymlPattern := filepath.Join(folderPath, "*.yml")
	ymlFiles, err := filepath.Glob(ymlPattern)
	if err != nil {
		return nil, fmt.Errorf("error finding YML files in %q: %w", folderPath, err)
	}

	// Combine both file lists
	allFiles := append(yamlFiles, ymlFiles...)
	return allFiles, nil
}
