package presence

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/event"
)

// The presence request for a ghost carries the presence and nothing else: a status_msg ("last seen
// ...") must never reach the homeserver, whichever presence is set.
func TestGhostPresenceRequestHasNoStatusMsg(t *testing.T) {
	var lock sync.Mutex
	var bodies []string
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/status") && strings.Contains(r.URL.Path, "/presence/") {
			lock.Lock()
			bodies = append(bodies, string(body))
			lock.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	defer hs.Close()

	reg := appservice.CreateRegistration()
	reg.SenderLocalpart = "metabot"
	as, err := appservice.CreateFull(appservice.CreateOpts{
		Registration:     reg,
		HomeserverDomain: "example.com",
		HomeserverURL:    hs.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	ghost := &bridgev2.Ghost{Intent: &matrix.ASIntent{Matrix: as.NewIntentAPI("meta_123")}}

	for _, p := range []event.Presence{event.PresenceOnline, event.PresenceOffline} {
		if err = SetGhostPresence(context.Background(), ghost, p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("expected two presence requests, got %q", bodies)
	}
	for i, want := range []event.Presence{event.PresenceOnline, event.PresenceOffline} {
		var fields map[string]any
		if err = json.Unmarshal([]byte(bodies[i]), &fields); err != nil {
			t.Fatalf("request %d is not JSON: %q", i, bodies[i])
		}
		if len(fields) != 1 || fields["presence"] != string(want) {
			t.Errorf("request %d: got %s, want only presence=%s", i, bodies[i], want)
		}
	}
}
