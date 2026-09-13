// Copyright 2026 Hack The Box
// SPDX-License-Identifier: Apache-2.0

package phpfpm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apiWatch "k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// recordedRequest is what the API server saw, so a test can assert that the
// namespace and label selector were applied server-side rather than in memory.
type recordedRequest struct {
	path     string
	selector string
	watch    string
}

// apiServer is a stand-in for the Kubernetes API. Using a real *kubernetes.Clientset
// over httptest rather than a fake clientset is deliberate: only a real HTTP round
// trip proves that the context reaches the request and cancels it.
type apiServer struct {
	mu       sync.Mutex
	requests []recordedRequest
	clients  *kubernetes.Clientset
}

func (s *apiServer) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, recordedRequest{
		path:     r.URL.Path,
		selector: r.URL.Query().Get("labelSelector"),
		watch:    r.URL.Query().Get("watch"),
	})
}

func (s *apiServer) seen() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedRequest(nil), s.requests...)
}

func newAPIServer(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *apiServer {
	t.Helper()

	srv := &apiServer{}
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.record(r)
		handler(w, r)
	}))
	t.Cleanup(httpSrv.Close)

	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: httpSrv.URL})
	require.NoError(t, err)
	srv.clients = clientset

	return srv
}

func pod(name, ip string, phase v1.PodPhase) *v1.Pod {
	return &v1.Pod{
		TypeMeta:   metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "php", ResourceVersion: "1"},
		Status:     v1.PodStatus{Phase: phase, PodIP: ip},
	}
}

func writePodList(t *testing.T, w http.ResponseWriter, pods ...v1.Pod) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(&v1.PodList{
		TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"},
		ListMeta: metav1.ListMeta{ResourceVersion: "1"},
		Items:    pods,
	}))
}

// watchStream serves a watch response and keeps it open, so the RetryWatcher
// behaves as it would against a live API server.
type watchStream struct {
	events chan apiWatch.Event
}

func newWatchStream() *watchStream {
	return &watchStream{events: make(chan apiWatch.Event)}
}

func (s *watchStream) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()

	encoder := json.NewEncoder(w)

	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-s.events:
			raw, err := json.Marshal(event.Object)
			if err != nil {
				return
			}
			if err := encoder.Encode(&metav1.WatchEvent{
				Type:   string(event.Type),
				Object: runtime.RawExtension{Raw: raw},
			}); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}
}

func newPoolManager() PoolManager {
	return PoolManager{PodPhases: map[string]v1.PodPhase{}}
}

func TestListPodsScopesToNamespaceAndSelector(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writePodList(t, w, *pod("php-fpm-0", "10.0.0.1", v1.PodRunning))
	})

	podList, err := listPods(t.Context(), srv.clients, "php", "collect=true")

	require.NoError(t, err)
	require.Len(t, podList.Items, 1)
	assert.Equal(t, "php-fpm-0", podList.Items[0].Name)

	require.Len(t, srv.seen(), 1)
	assert.Equal(t, "/api/v1/namespaces/php/pods", srv.seen()[0].path)
	assert.Equal(t, "collect=true", srv.seen()[0].selector, "the selector must filter server-side")
}

func TestListPodsDefaultsToAllNamespaces(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writePodList(t, w)
	})

	_, err := listPods(t.Context(), srv.clients, "", "")

	require.NoError(t, err)
	require.Len(t, srv.seen(), 1)
	assert.Equal(t, "/api/v1/pods", srv.seen()[0].path, "an empty namespace lists across all namespaces")
}

// The point of threading a context: a cancelled caller must abort the request
// instead of the call running to completion against the API server.
func TestListPodsHonoursCancelledContext(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writePodList(t, w)
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	podList, err := listPods(ctx, srv.clients, "php", "")

	require.Error(t, err)
	assert.Nil(t, podList)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, srv.seen(), "a cancelled context must not reach the API server")
}

func TestListPodsWrapsAPIError(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	podList, err := listPods(t.Context(), srv.clients, "php", "")

	require.Error(t, err)
	assert.Nil(t, podList)
	assert.Contains(t, err.Error(), "failed to list pods")
}

func TestWatchWithContextScopesToNamespaceAndSelector(t *testing.T) {
	stream := newWatchStream()
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		stream.serve(w, r)
	})

	result, err := newWatcher(srv.clients, "php", "collect=true").
		WatchWithContext(t.Context(), metav1.ListOptions{})

	require.NoError(t, err)
	t.Cleanup(result.Stop)

	require.Len(t, srv.seen(), 1)
	assert.Equal(t, "/api/v1/namespaces/php/pods", srv.seen()[0].path)
	assert.Equal(t, "collect=true", srv.seen()[0].selector, "newWatcher's selector overrides the caller's options")
	assert.Equal(t, "true", srv.seen()[0].watch)
}

func TestWatchWithContextHonoursCancelledContext(t *testing.T) {
	stream := newWatchStream()
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		stream.serve(w, r)
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := newWatcher(srv.clients, "php", "").WatchWithContext(ctx, metav1.ListOptions{})

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestInitialPodEnlistingAddsOnlyRunningPodsWithAnIP(t *testing.T) {
	pm := newPoolManager()
	exporter := NewExporter(pm)

	resourceVersion := pm.initialPodEnlisting(exporter, &v1.PodList{
		ListMeta: metav1.ListMeta{ResourceVersion: "42"},
		Items: []v1.Pod{
			*pod("running-with-ip", "10.0.0.1", v1.PodRunning),
			*pod("running-no-ip", "", v1.PodRunning),
			*pod("pending", "", v1.PodPending),
		},
	}, "9000")

	assert.Equal(t, "42", resourceVersion, "the list's ResourceVersion seeds the retry watcher")

	require.Len(t, pm.Pools, 1)
	assert.Equal(t, "tcp://10.0.0.1:9000/status", pm.Pools[0].Address)
	assert.Equal(t, "running-with-ip", pm.Pools[0].Pod)

	assert.Equal(t, map[string]v1.PodPhase{
		"running-with-ip": v1.PodRunning,
		"running-no-ip":   v1.PodRunning,
		"pending":         v1.PodPending,
	}, pm.PodPhases, "every listed pod's phase is tracked, enlisted or not")
}

func TestProcessPodModifiedEnlistsOnlyOnPendingToRunning(t *testing.T) {
	tests := []struct {
		name      string
		lastPhase *v1.PodPhase
		newPhase  v1.PodPhase
		wantPools int
	}{
		{name: "pending to running enlists", lastPhase: new(v1.PodPending), newPhase: v1.PodRunning, wantPools: 1},
		{name: "running to running is a no-op", lastPhase: new(v1.PodRunning), newPhase: v1.PodRunning, wantPools: 0},
		{name: "unknown pod is not enlisted", lastPhase: nil, newPhase: v1.PodRunning, wantPools: 0},
		{name: "pending to failed is a no-op", lastPhase: new(v1.PodPending), newPhase: v1.PodFailed, wantPools: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pm := newPoolManager()
			if tt.lastPhase != nil {
				pm.PodPhases["php-fpm-0"] = *tt.lastPhase
			}
			exporter := NewExporter(pm)

			pm.processPodModified(exporter, pod("php-fpm-0", "10.0.0.1", tt.newPhase), "tcp://10.0.0.1:9000/status")

			assert.Len(t, pm.Pools, tt.wantPools)
			assert.Equal(t, tt.newPhase, pm.PodPhases["php-fpm-0"], "the last seen phase is always updated")
		})
	}
}

func TestProcessPodDeletedRemovesPoolAndForgetsPhase(t *testing.T) {
	pm := newPoolManager()
	pm.processPodAdded(NewExporter(pm), pod("php-fpm-0", "10.0.0.1", v1.PodRunning), "tcp://10.0.0.1:9000/status")
	require.Len(t, pm.Pools, 1)

	exporter := NewExporter(pm)
	pm.processPodDeleted(exporter, pod("php-fpm-0", "10.0.0.1", v1.PodRunning), "tcp://10.0.0.1:9000/status")

	assert.Empty(t, pm.Pools)
	assert.NotContains(t, pm.PodPhases, "php-fpm-0")
	assert.Empty(t, exporter.PoolManager.Pools, "the exporter must see the removal")
}

func TestDiscoverPodsEnlistsInitialPodsAndStartsWatching(t *testing.T) {
	watching := make(chan struct{})
	closeOnce := sync.Once{}
	stream := newWatchStream()

	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") != "true" {
			writePodList(t, w, *pod("php-fpm-0", "10.0.0.1", v1.PodRunning))
			return
		}
		closeOnce.Do(func() { close(watching) })
		stream.serve(w, r)
	})

	// The watch goroutine is fire-and-forget, so cancel it before the test ends
	// rather than letting it outlive the case and race the next one.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	pm := newPoolManager()
	exporter := NewExporter(pm)

	require.NoError(t, pm.discoverPods(ctx, exporter, srv.clients, "php", "", "9000"))

	require.Len(t, pm.Pools, 1, "the initial list is enlisted before the watch starts")
	assert.Equal(t, "tcp://10.0.0.1:9000/status", pm.Pools[0].Address)

	select {
	case <-watching:
	case <-time.After(10 * time.Second):
		t.Fatal("DiscoverPods must start watching in the background")
	}
}

func TestDiscoverPodsReturnsListError(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	pm := newPoolManager()

	err := pm.discoverPods(t.Context(), NewExporter(pm), srv.clients, "php", "", "9000")

	require.Error(t, err, "a failed initial list must not be swallowed")
	assert.Contains(t, err.Error(), "failed to list pods")
	assert.Empty(t, pm.Pools)
}

func TestWatchPodEventsStopsWhenContextIsCancelled(t *testing.T) {
	stream := newWatchStream()
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		stream.serve(w, r)
	})

	ctx, cancel := context.WithCancel(t.Context())

	pm := newPoolManager()
	returned := make(chan struct{})

	go func() {
		defer close(returned)
		pm.watchPodEvents(ctx, NewExporter(pm), newWatcher(srv.clients, "php", ""), "1", "9000")
	}()

	cancel()

	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling the context must stop the watcher; it ran on")
	}
}

func TestWatchPodEventsDispatchesEventsToTheHandlers(t *testing.T) {
	logs := captureLogs(t)

	stream := newWatchStream()
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		stream.serve(w, r)
	})

	ctx, cancel := context.WithCancel(t.Context())

	pm := newPoolManager()
	returned := make(chan struct{})

	go func() {
		defer close(returned)
		pm.watchPodEvents(ctx, NewExporter(pm), newWatcher(srv.clients, "php", ""), "1", "9000")
	}()

	// Added while Pending, then Modified to Running: only the transition enlists.
	stream.events <- apiWatch.Event{Type: apiWatch.Added, Object: pod("php-fpm-0", "", v1.PodPending)}
	stream.events <- apiWatch.Event{Type: apiWatch.Modified, Object: pod("php-fpm-0", "10.0.0.1", v1.PodRunning)}
	requireLogged(t, logs, "transitioned from Pending to Running")

	stream.events <- apiWatch.Event{Type: apiWatch.Deleted, Object: pod("php-fpm-0", "10.0.0.1", v1.PodRunning)}
	requireLogged(t, logs, "Removing pod")

	// Join before reading pm: the watcher goroutine owns it until then.
	cancel()
	<-returned

	assert.Empty(t, pm.Pools, "the pool added on Running must be gone after Deleted")
	assert.NotContains(t, pm.PodPhases, "php-fpm-0")
}

func TestWatchPodEventsReturnsOnUnusableResourceVersion(t *testing.T) {
	stream := newWatchStream()
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		stream.serve(w, r)
	})

	pm := newPoolManager()
	returned := make(chan struct{})

	go func() {
		defer close(returned)
		pm.watchPodEvents(t.Context(), NewExporter(pm), newWatcher(srv.clients, "php", ""), "", "9000")
	}()

	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("an unconstructable retry watcher must return, not spin")
	}
}
