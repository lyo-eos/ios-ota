package linkcore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/danielpaulus/go-ios/ios/instruments"
)

const locationAttemptTimeout = 60 * time.Second
const locationRefreshInterval = 5 * time.Second

type locationDriver interface {
	StartSimulateLocationContext(context.Context, float64, float64) error
	StopSimulateLocationContext(context.Context) error
	Close()
}

type LocationState struct {
	Phase        string   `json:"phase"`
	Latitude     *float64 `json:"latitude,omitempty"`
	Longitude    *float64 `json:"longitude,omitempty"`
	CommandTime  float64  `json:"commandTime,omitempty"`
	TunnelActive bool     `json:"tunnelActive"`
	Error        string   `json:"error,omitempty"`
}

// HTTP owns only the latest intent. The single worker owns all developer I/O.
type locationSession struct {
	mu              sync.Mutex
	state           LocationState
	revision, epoch uint64
	cancel          context.CancelFunc
	wake            chan struct{}
	done            chan struct{}
	open            func(context.Context) (locationDriver, error)
}

func newLocationSession(open func(context.Context) (locationDriver, error)) *locationSession {
	return &locationSession{state: LocationState{Phase: "idle"}, open: open, wake: make(chan struct{}, 1), done: make(chan struct{})}
}

func validLocation(lat, lon float64) bool {
	return !math.IsNaN(lat) && !math.IsNaN(lon) && !math.IsInf(lat, 0) && !math.IsInf(lon, 0) && lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180
}

func (s *Session) OpenLocation(ctx context.Context) (locationDriver, error) {
	device, err := s.captureDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("Location service discovery failed: %w", err)
	}
	driver, err := instruments.NewLocationSimulationServiceContext(ctx, device)
	if err != nil {
		return nil, fmt.Errorf("Location channel connection failed: %w", err)
	}
	return driver, nil
}

func (s *locationSession) snapshot(tunnelActive bool) LocationState {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state
	state.TunnelActive = tunnelActive
	return state
}
func (s *locationSession) signalLocked() {
	if s.cancel != nil {
		s.cancel()
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *locationSession) set(lat, lon float64) error {
	if !validLocation(lat, lon) {
		return errors.New("Invalid coordinates.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revision++
	s.state = LocationState{Phase: "applying", Latitude: &lat, Longitude: &lon}
	s.signalLocked()
	return nil
}
func (s *locationSession) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revision++
	// Removing the target happens before cancelling old I/O. No late Set can
	// restore its authority, even if iOS processed it just before cancellation.
	s.state = LocationState{Phase: "restoring"}
	s.signalLocked()
}
func (s *locationSession) lost() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.epoch++
	s.revision++
	if s.state.Phase != "idle" {
		if s.state.Latitude != nil {
			s.state.Phase = "recovering"
		}
		s.state.CommandTime = 0
		s.state.Error = "Waiting for the iPhone connection."
	}
	s.signalLocked()
}

func closeLocation(driver *locationDriver) {
	if *driver != nil {
		(*driver).Close()
		*driver = nil
	}
}

func (s *locationSession) reconcile(owner context.Context, driver *locationDriver, epoch *uint64) {
	s.mu.Lock()
	if s.state.Phase == "idle" || owner.Err() != nil {
		s.mu.Unlock()
		return
	}
	request, revision, currentEpoch := s.state, s.revision, s.epoch
	ctx, cancel := context.WithTimeout(owner, locationAttemptTimeout)
	s.cancel = cancel
	s.mu.Unlock()
	defer cancel()
	if *epoch != currentEpoch {
		closeLocation(driver)
		*epoch = currentEpoch
	}
	var err error
	if *driver == nil {
		*driver, err = s.open(ctx)
	}
	commandTime := float64(time.Now().UnixMilli()) / 1000
	if err == nil {
		if request.Latitude != nil {
			err = (*driver).StartSimulateLocationContext(ctx, *request.Latitude, *request.Longitude)
			if err != nil {
				err = fmt.Errorf("Location set acknowledgement failed: %w", err)
			}
		} else {
			err = (*driver).StopSimulateLocationContext(ctx)
			if err != nil {
				err = fmt.Errorf("Location restore acknowledgement failed: %w", err)
			}
		}
	}
	// Cancellation wins over a response that arrived concurrently.
	if ctx.Err() != nil && err == nil {
		err = ctx.Err()
	}
	s.mu.Lock()
	s.cancel = nil
	if revision != s.revision || owner.Err() != nil {
		s.mu.Unlock()
		closeLocation(driver)
		return
	}
	if err != nil {
		message := err.Error()
		if s.state.Error != message {
			slog.Warn(ServiceName+" location recovery pending", "error", message)
		}
		s.state.Error = message
		s.state.CommandTime = 0
		if request.Latitude != nil {
			s.state.Phase = "recovering"
		}
		s.mu.Unlock()
		closeLocation(driver)
		return
	}
	s.state.Error = ""
	if request.Latitude != nil {
		s.state.Phase = "active"
		if s.state.CommandTime == 0 {
			s.state.CommandTime = commandTime
		}
	} else {
		s.state = LocationState{Phase: "idle"}
	}
	s.mu.Unlock()
	if request.Latitude == nil {
		closeLocation(driver)
	}
}

func (s *locationSession) run(ctx context.Context, ticks <-chan time.Time) {
	var driver locationDriver
	var epoch uint64
	defer close(s.done)
	defer func() {
		// The outer session stays owned by the daemon until this worker has exited.
		if s.snapshot(false).Phase != "idle" {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var err error
			if driver == nil {
				driver, err = s.open(cleanup)
			}
			if err == nil {
				err = driver.StopSimulateLocationContext(cleanup)
			}
			if err != nil {
				slog.Warn(ServiceName + " shutdown location clear was not acknowledged")
			}
		}
		closeLocation(&driver)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-ticks:
		}
		s.reconcile(ctx, &driver, &epoch)
	}
}
func (s *locationSession) pulse(ctx context.Context) {
	ticker := time.NewTicker(locationRefreshInterval)
	defer ticker.Stop()
	s.run(ctx, ticker.C)
}
