package linkcore

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeLocation struct {
	sets, stops, closes atomic.Int32
	failSet, failStop   atomic.Bool
	onSet               func(context.Context, float64, float64) error
}

func (f *fakeLocation) StartSimulateLocationContext(ctx context.Context, lat, lon float64) error {
	f.sets.Add(1)
	if f.onSet != nil {
		return f.onSet(ctx, lat, lon)
	}
	if f.failSet.Load() {
		return errors.New("set transport lost")
	}
	return nil
}
func (f *fakeLocation) StopSimulateLocationContext(context.Context) error {
	f.stops.Add(1)
	if f.failStop.Load() {
		return errors.New("stop transport lost")
	}
	return nil
}
func (f *fakeLocation) Close() { f.closes.Add(1) }
func awaitLocation(t *testing.T, s *locationSession, phase string) LocationState {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state := s.snapshot(true)
		if state.Phase == phase {
			return state
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("wanted %s, got %+v", phase, s.snapshot(true))
	return LocationState{}
}
func runLocation(t *testing.T, s *locationSession) chan time.Time {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	go s.run(ctx, ticks)
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			t.Error("worker did not stop")
		}
	})
	return ticks
}
func TestLocationMaintainsTargetAndRecoversOnSameTunnel(t *testing.T) {
	first, second := &fakeLocation{}, &fakeLocation{}
	var opens atomic.Int32
	s := newLocationSession(func(context.Context) (locationDriver, error) {
		if opens.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	})
	ticks := runLocation(t, s)
	if err := s.set(29.56, 106.58); err != nil {
		t.Fatal(err)
	}
	initial := awaitLocation(t, s, "active")
	first.failSet.Store(true)
	ticks <- time.Now()
	failed := awaitLocation(t, s, "recovering")
	if failed.Latitude == nil || *failed.Latitude != 29.56 {
		t.Fatal("refresh lost desired target")
	}
	ticks <- time.Now()
	recovered := awaitLocation(t, s, "active")
	if opens.Load() != 2 || second.sets.Load() != 1 || recovered.CommandTime < initial.CommandTime {
		t.Fatal("target did not recover on a fresh inner service")
	}
	ticks <- time.Now()
	deadline := time.Now().Add(time.Second)
	for second.sets.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if second.sets.Load() != 2 || opens.Load() != 2 {
		t.Fatal("healthy target was not maintained on its existing connection")
	}
	s.clear()
	awaitLocation(t, s, "idle")
	before := second.sets.Load()
	ticks <- time.Now()
	time.Sleep(10 * time.Millisecond)
	if second.stops.Load() != 1 || second.sets.Load() != before {
		t.Fatal("restore did not stop maintenance")
	}
}
func TestLocationRestoresAcrossTunnelReplacement(t *testing.T) {
	var opens atomic.Int32
	s := newLocationSession(func(context.Context) (locationDriver, error) { opens.Add(1); return &fakeLocation{}, nil })
	ticks := runLocation(t, s)
	s.set(1, 2)
	awaitLocation(t, s, "active")
	s.lost()
	ticks <- time.Now()
	awaitLocation(t, s, "active")
	if opens.Load() != 2 {
		t.Fatal("replacement tunnel retained the obsolete inner connection")
	}
	if *s.snapshot(true).Latitude != 1 {
		t.Fatal("outer recovery discarded target")
	}
}
func TestLocationRestoreCancelsInflightSetAndRejectsLateReply(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	first := &fakeLocation{onSet: func(ctx context.Context, lat, lon float64) error { close(started); <-ctx.Done(); <-release; return nil }}
	second := &fakeLocation{}
	var opens atomic.Int32
	s := newLocationSession(func(context.Context) (locationDriver, error) {
		if opens.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	})
	runLocation(t, s)
	s.set(1, 2)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("set never started")
	}
	s.clear()
	if state := s.snapshot(true); state.Phase != "restoring" || state.Latitude != nil {
		t.Fatal("restore retained set intent")
	}
	close(release)
	awaitLocation(t, s, "idle")
	if first.closes.Load() != 1 || second.stops.Load() != 1 || second.sets.Load() != 0 {
		t.Fatal("stale set resurrected after restore")
	}
}
func TestLocationUpdateSupersedesSlowDiscovery(t *testing.T) {
	started := make(chan struct{})
	var opens atomic.Int32
	points := make(chan [2]float64, 2)
	driver := &fakeLocation{onSet: func(_ context.Context, a, b float64) error { points <- [2]float64{a, b}; return nil }}
	s := newLocationSession(func(ctx context.Context) (locationDriver, error) {
		if opens.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return driver, nil
	})
	runLocation(t, s)
	s.set(1, 2)
	<-started
	s.set(3, 4)
	awaitLocation(t, s, "active")
	if got := <-points; got != [2]float64{3, 4} {
		t.Fatalf("obsolete target applied: %v", got)
	}
}
func TestLocationClearFailureRemainsPendingWithoutReapplying(t *testing.T) {
	first, second := &fakeLocation{}, &fakeLocation{}
	first.failStop.Store(true)
	var opens atomic.Int32
	s := newLocationSession(func(context.Context) (locationDriver, error) {
		if opens.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	})
	ticks := runLocation(t, s)
	s.set(1, 2)
	awaitLocation(t, s, "active")
	s.clear()
	deadline := time.Now().Add(time.Second)
	for s.snapshot(true).Error == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	state := s.snapshot(true)
	if state.Phase != "restoring" || state.Latitude != nil || state.Error == "" {
		t.Fatal(state)
	}
	ticks <- time.Now()
	awaitLocation(t, s, "idle")
	if second.stops.Load() != 1 || second.sets.Load() != 0 {
		t.Fatal("failed clear recovered as a set")
	}
}
func TestLocationRejectsInvalidCoordinates(t *testing.T) {
	for _, point := range [][2]float64{{math.NaN(), 0}, {0, math.Inf(1)}, {90.01, 0}, {0, -180.01}} {
		if validLocation(point[0], point[1]) {
			t.Fatal(point)
		}
	}
	if !validLocation(-90, 180) {
		t.Fatal("valid boundary rejected")
	}
}

func TestLocationHTTPAuthorizationAndSlowDiscovery(t *testing.T) {
	started := make(chan struct{})
	ready := make(chan struct{})
	driver := &fakeLocation{}
	s := newLocationSession(func(ctx context.Context) (locationDriver, error) {
		close(started)
		select {
		case <-ready:
			return driver, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	runLocation(t, s)
	handler := locationHandler(context.Background(), "owner-secret", s, func() bool { return true })
	cases := []struct {
		method, path, token, body string
		want                      int
	}{
		{"PUT", "/v1/location", "", `{"latitude":1,"longitude":2}`, 401},
		{"POST", "/install", "Bearer owner-secret", "", 404},
		{"PUT", "/v1/location", "Bearer owner-secret", `{"latitude":1}`, 400},
		{"PUT", "/v1/location", "Bearer owner-secret", `{"latitude":1,"longitude":181}`, 400},
		{"PUT", "/v1/location", "Bearer owner-secret", `{"latitude":1,"longitude":2,"command":"install"}`, 400},
		{"PUT", "/v1/location", "Bearer owner-secret", `{"latitude":1,"longitude":2} {}`, 400},
		{"GET", "/v1/location?token=owner-secret", "Bearer owner-secret", "", 404},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		r.Header.Set("Authorization", c.token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Fatalf("%s %s: %d", c.method, c.path, w.Code)
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("PUT", "/v1/location", strings.NewReader(`{"latitude":1,"longitude":2}`)).WithContext(cancelled)
	r.Header.Set("Authorization", "Bearer owner-secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted || s.snapshot(true).Phase != "applying" {
		t.Fatal("HTTP awaited device I/O or claimed completion")
	}
	<-started
	r = httptest.NewRequest("GET", "/v1/location", nil)
	r.Header.Set("Authorization", "Bearer owner-secret")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("GET blocked by slow discovery")
	}
	close(ready)
	awaitLocation(t, s, "active")
	if driver.stops.Load() != 0 {
		t.Fatal("HTTP disconnect stopped simulation")
	}
}
