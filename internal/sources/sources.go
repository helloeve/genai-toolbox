// Copyright 2024 Google LLC
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

package sources

import (
	"context"
	"sync"

	"fmt"

	"github.com/goccy/go-yaml"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// CloudPlatformScope is the OAuth2 scope for Google Cloud Platform services.
const CloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// SourceConfigFactory defines the function signature for creating a SourceConfig.
type SourceConfigFactory func(ctx context.Context, name string, decoder *yaml.Decoder) (SourceConfig, error)

var sourceRegistry = make(map[string]SourceConfigFactory)

// Register registers a new source type with its factory.
// It returns false if the type is already registered.
func Register(sourceType string, factory SourceConfigFactory) bool {
	if _, exists := sourceRegistry[sourceType]; exists {
		// Source with this type already exists, do not overwrite.
		return false
	}
	sourceRegistry[sourceType] = factory
	return true
}

// DecodeConfig decodes a source configuration using the registered factory for the given type.
func DecodeConfig(ctx context.Context, sourceType string, name string, decoder *yaml.Decoder) (SourceConfig, error) {
	factory, found := sourceRegistry[sourceType]
	if !found {
		return nil, fmt.Errorf("unknown source type: %q", sourceType)
	}
	sourceConfig, err := factory(ctx, name, decoder)
	if err != nil {
		return nil, fmt.Errorf("unable to parse source %q as %q: %w", name, sourceType, err)
	}
	return sourceConfig, err
}

// SourceConfig is the interface for configuring a source.
type SourceConfig interface {
	SourceConfigType() string
	Initialize(ctx context.Context, tracer trace.Tracer) (Source, error)
}

// Source is the interface for the source itself.
type Source interface {
	SourceType() string
	ToConfig() SourceConfig
}

// LazySource is a Source whose real, connected implementation is created on
// first use rather than at startup. It backs the --lazy-loading mode: the
// PrimitiveManager holds a LazySource in place of a live source, and callers on
// the invocation path unwrap it via Materialize before using the source (e.g.
// before a tool type-asserts it to a capability interface). Read-only callers
// (tools/list manifest rendering) instead detect the LazySource and fall back to
// static parameters, so listing never forces a connection.
//
// Materialize is safe for concurrent use: it materializes at most once, caches
// the result, and does not cache errors (a failed attempt is retryable once the
// environment or datastore recovers).
type LazySource interface {
	Source
	Materialize(ctx context.Context) (Source, error)
}

// lazySource is a deferred Source: it holds a source's env-unresolved config
// block and materializes the real, connected source on first use. It backs
// --lazy-loading. Construct it with NewLazySource.
type lazySource struct {
	name        string
	raw         []byte
	materialize func(context.Context, []byte) (Source, error)

	mu       sync.Mutex
	resolved Source
}

// NewLazySource returns a deferred source. The materialize callback (env
// resolution + decode + connect) runs on the first Materialize call and receives
// the retained raw config block; the layer that owns primitive-config decoding
// supplies it (avoiding an import cycle into this package).
func NewLazySource(name string, raw []byte, materialize func(context.Context, []byte) (Source, error)) LazySource {
	return &lazySource{name: name, raw: raw, materialize: materialize}
}

// SourceType reports a placeholder type for an unmaterialized source. Callers on
// the invocation path unwrap via Materialize before inspecting the source, so
// this is only seen by generic tooling that lists sources without using them.
func (l *lazySource) SourceType() string { return "lazy" }

// ToConfig is unused for deferred sources; the real config is decoded lazily from
// the retained raw block during Materialize.
func (l *lazySource) ToConfig() SourceConfig { return nil }

// Materialize returns the real, connected source, creating it on first call.
// Concurrent callers for the same source serialize on the mutex so it is
// materialized once; different sources hold different locks and proceed in
// parallel. Errors are not cached, so a later call can retry.
func (l *lazySource) Materialize(ctx context.Context) (Source, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.resolved != nil {
		return l.resolved, nil
	}
	s, err := l.materialize(ctx, l.raw)
	if err != nil {
		return nil, fmt.Errorf("unable to initialize source %q: %w", l.name, err)
	}
	l.resolved = s
	return s, nil
}

// InitConnectionSpan adds a span for database pool connection initialization
func InitConnectionSpan(ctx context.Context, tracer trace.Tracer, sourceType, sourceName string) (context.Context, trace.Span) {
	ctx, span := tracer.Start(
		ctx,
		"toolbox/server/source/connect",
		trace.WithAttributes(attribute.String("source_type", sourceType)),
		trace.WithAttributes(attribute.String("source_name", sourceName)),
	)
	return ctx, span
}
