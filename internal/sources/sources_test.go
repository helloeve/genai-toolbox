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

package sources_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/sources"
)

// mockSource is a minimal sources.Source for the lazy wrapper tests.
type mockSource struct{}

func (m *mockSource) SourceType() string             { return "mock" }
func (m *mockSource) ToConfig() sources.SourceConfig { return nil }

func TestLazySourceImplementsInterface(t *testing.T) {
	var s sources.Source = sources.NewLazySource("s", []byte("block"), nil)
	if _, ok := s.(sources.LazySource); !ok {
		t.Fatal("NewLazySource result does not implement sources.LazySource")
	}
	if got := s.SourceType(); got != "lazy" {
		t.Fatalf("SourceType() = %q, want %q", got, "lazy")
	}
}

func TestLazySourceMaterializesOnceAndCaches(t *testing.T) {
	var calls atomic.Int32
	ls := sources.NewLazySource("s", []byte("block"), func(context.Context, []byte) (sources.Source, error) {
		calls.Add(1)
		return &mockSource{}, nil
	})

	for i := 0; i < 3; i++ {
		got, err := ls.Materialize(context.Background())
		if err != nil {
			t.Fatalf("Materialize: unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("Materialize returned nil source")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("materialize called %d times, want 1 (should cache)", got)
	}
}

func TestLazySourceFailureNotCached(t *testing.T) {
	var calls atomic.Int32
	ls := sources.NewLazySource("s", []byte("block"), func(context.Context, []byte) (sources.Source, error) {
		if calls.Add(1) == 1 {
			return nil, fmt.Errorf("boom")
		}
		return &mockSource{}, nil
	})

	if _, err := ls.Materialize(context.Background()); err == nil {
		t.Fatal("first Materialize: expected error, got nil")
	}
	// A failed materialization must be retryable.
	if _, err := ls.Materialize(context.Background()); err != nil {
		t.Fatalf("second Materialize: expected success on retry, got %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("materialize called %d times, want 2 (failure not cached)", got)
	}
}

func TestLazySourceConcurrentMaterializeDedupes(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	ls := sources.NewLazySource("s", []byte("block"), func(context.Context, []byte) (sources.Source, error) {
		calls.Add(1)
		<-release // hold until all goroutines are in-flight
		return &mockSource{}, nil
	})

	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := ls.Materialize(context.Background()); err != nil {
				t.Errorf("Materialize: %v", err)
			}
		}()
	}
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("materialize called %d times under concurrency, want 1", got)
	}
}
