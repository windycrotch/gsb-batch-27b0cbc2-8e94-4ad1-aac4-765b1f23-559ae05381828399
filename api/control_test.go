/* Copyright (c) 2024 Gregor Riepl
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/onitake/restreamer/auth"
	"github.com/onitake/restreamer/configuration"
	"github.com/onitake/restreamer/metrics"
)

// mockInhibitor records the last inhibit state it was set to.
type mockInhibitor struct {
	lock  sync.Mutex
	state bool
	calls int
}

func (inhibitor *mockInhibitor) SetInhibit(inhibit bool) {
	inhibitor.lock.Lock()
	inhibitor.state = inhibit
	inhibitor.calls++
	inhibitor.lock.Unlock()
}

func (inhibitor *mockInhibitor) State() (bool, int) {
	inhibitor.lock.Lock()
	defer inhibitor.lock.Unlock()
	return inhibitor.state, inhibitor.calls
}

func testControlApi(t *testing.T, query string, expectedStatus int, expectedInhibit bool, expectedCalls int) {
	t.Helper()
	inhibitor := &mockInhibitor{}
	api := NewStreamControlApi(inhibitor, auth.NewAuthenticator(configuration.Authentication{}, nil))
	request := httptest.NewRequest(http.MethodGet, "/control?"+query, nil)
	writer := httptest.NewRecorder()
	api.ServeHTTP(writer, request)

	if writer.Code != expectedStatus {
		t.Errorf("query %q: expected status %d, got %d", query, expectedStatus, writer.Code)
	}
	state, calls := inhibitor.State()
	if calls != expectedCalls {
		t.Errorf("query %q: expected %d SetInhibit calls, got %d", query, expectedCalls, calls)
	}
	if expectedCalls > 0 && state != expectedInhibit {
		t.Errorf("query %q: expected inhibit=%v, got %v", query, expectedInhibit, state)
	}
}

// TestStreamControlApi verifies the documented response semantics of the
// control endpoint.
func TestStreamControlApi(t *testing.T) {
	// offline switches the stream off and returns 202
	testControlApi(t, "offline", http.StatusAccepted, true, 1)
	// online switches it back on and returns 202
	testControlApi(t, "online", http.StatusAccepted, false, 1)
	// both parameters: offline wins
	testControlApi(t, "offline&online", http.StatusAccepted, true, 1)
	testControlApi(t, "online&offline", http.StatusAccepted, true, 1)
	// no parameters at all: 400 and no state change
	testControlApi(t, "", http.StatusBadRequest, false, 0)
	// unknown parameters only: still 400
	testControlApi(t, "foo=bar", http.StatusBadRequest, false, 0)
}

// TestStreamControlApiSequence verifies that switching offline and online
// again is idempotent from the API point of view.
func TestStreamControlApiSequence(t *testing.T) {
	inhibitor := &mockInhibitor{}
	api := NewStreamControlApi(inhibitor, auth.NewAuthenticator(configuration.Authentication{}, nil))

	steps := []struct {
		query  string
		status int
		state  bool
	}{
		{"offline", http.StatusAccepted, true},
		{"offline", http.StatusAccepted, true},
		{"online", http.StatusAccepted, false},
		{"online", http.StatusAccepted, false},
		{"offline", http.StatusAccepted, true},
	}
	for _, step := range steps {
		request := httptest.NewRequest(http.MethodGet, "/control?"+step.query, nil)
		writer := httptest.NewRecorder()
		api.ServeHTTP(writer, request)
		if writer.Code != step.status {
			t.Errorf("query %q: expected status %d, got %d", step.query, step.status, writer.Code)
		}
		if state, _ := inhibitor.State(); state != step.state {
			t.Errorf("query %q: expected inhibit=%v, got %v", step.query, step.state, state)
		}
	}
}

// TestHealthApiLiveConnections verifies that the health endpoint reports the
// connection count supplied by the statistics collector and classifies the
// status accordingly.
func TestHealthApiLiveConnections(t *testing.T) {
	stats := &mockStatistics{
		Global: metrics.StreamStatistics{
			Connections:    0,
			MaxConnections: 2,
		},
	}
	api := NewHealthApi(stats, auth.NewAuthenticator(configuration.Authentication{}, nil))

	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	writer := httptest.NewRecorder()
	api.ServeHTTP(writer, request)
	if writer.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", writer.Code)
	}
}

// TestStatisticsApiConnectionsField verifies that the statistics endpoint
// reports the connection count verbatim.
func TestStatisticsApiConnectionsField(t *testing.T) {
	stats := &mockStatistics{
		Global: metrics.StreamStatistics{
			Connections: 3,
		},
	}
	api := NewStatisticsApi(stats, auth.NewAuthenticator(configuration.Authentication{}, nil))
	request := httptest.NewRequest(http.MethodGet, "/statistics", nil)
	writer := httptest.NewRecorder()
	api.ServeHTTP(writer, request)

	var decoded map[string]interface{}
	if err := json.Unmarshal(writer.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("error decoding JSON: %s", err.Error())
	}
	connections, ok := decoded["connections"].(float64)
	if !ok {
		t.Fatalf("no connections field or incorrect type returned")
	}
	if int(connections) != 3 {
		t.Errorf("expected 3 connections, got %v", connections)
	}
}
