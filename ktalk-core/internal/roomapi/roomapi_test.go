package roomapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/cwash797-cmd/Project-XK/ktalk-core/internal/roomapi"
)

// buildTestServer creates an httptest.Server that responds to /api/rooms/<name>
// with the provided JSON body and status code.
// It also sets the ngtoken cookie on 200 responses, matching the real server behaviour.
func buildTestServer(t *testing.T, roomName string, status int, body interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/rooms/" + roomName
		if r.URL.Path != wantPath {
			http.NotFound(w, r)
			return
		}
		if status == http.StatusOK {
			http.SetCookie(w, &http.Cookie{
				Name:  "ngtoken",
				Value: "test-ng-token",
				Path:  "/",
			})
			http.SetCookie(w, &http.Cookie{
				Name:  "kontur_ngtoken",
				Value: "test-kontur-token",
				Path:  "/",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
}

func TestFetchRoom_OK(t *testing.T) {
	room := roomapi.RoomInfo{
		RoomName:       "cb140blkff7i",
		ConferenceID:   "cb140blkff7i_3074b65d29905f8e4418e2113a329f487fcadc8e4ed58df7b108624d199a4110",
		AllowAnonymous: true,
		AudioPolicy:    "none",
		VideoPolicy:    "none",
		UsersOnline:    2,
	}
	srv := buildTestServer(t, "cb140blkff7i", http.StatusOK, room)
	defer srv.Close()

	client, err := roomapi.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	info, err := client.FetchRoom(context.Background(), "cb140blkff7i")
	if err != nil {
		t.Fatalf("FetchRoom: %v", err)
	}

	if info.RoomName != "cb140blkff7i" {
		t.Errorf("RoomName = %q, want cb140blkff7i", info.RoomName)
	}
	if info.ConferenceID == "" {
		t.Error("ConferenceID is empty")
	}
	if !info.AllowAnonymous {
		t.Error("AllowAnonymous should be true")
	}
}

func TestFetchRoom_CookiesSet(t *testing.T) {
	room := roomapi.RoomInfo{
		RoomName:       "testroom",
		ConferenceID:   "testroom_abc123",
		AllowAnonymous: true,
	}
	srv := buildTestServer(t, "testroom", http.StatusOK, room)
	defer srv.Close()

	client, err := roomapi.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = client.FetchRoom(context.Background(), "testroom")
	if err != nil {
		t.Fatalf("FetchRoom: %v", err)
	}

	// Verify cookies are in the jar
	jar := client.CookieJar()
	u, _ := url.Parse(srv.URL)
	cookies := jar.Cookies(u)
	if len(cookies) == 0 {
		t.Fatal("no cookies in jar after FetchRoom")
	}

	var hasNGToken bool
	for _, c := range cookies {
		if c.Name == "ngtoken" {
			hasNGToken = true
		}
	}
	if !hasNGToken {
		t.Error("ngtoken cookie not found in jar")
	}
}

func TestFetchRoom_NotFound(t *testing.T) {
	srv := buildTestServer(t, "realroom", http.StatusNotFound, map[string]string{"error": "not found"})
	defer srv.Close()

	client, _ := roomapi.NewClient(srv.URL)
	_, err := client.FetchRoom(context.Background(), "realroom")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !isError(err, roomapi.ErrRoomNotFound) {
		t.Errorf("expected ErrRoomNotFound, got %v", err)
	}
}

func TestFetchRoom_Unauthorized(t *testing.T) {
	srv := buildTestServer(t, "privroom", http.StatusUnauthorized, map[string]string{})
	defer srv.Close()

	client, _ := roomapi.NewClient(srv.URL)
	_, err := client.FetchRoom(context.Background(), "privroom")
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !isError(err, roomapi.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized, got %v", err)
	}
}

func TestFetchRoom_AnonymousNotAllowed(t *testing.T) {
	room := roomapi.RoomInfo{
		RoomName:       "prvroom",
		ConferenceID:   "prvroom_abc",
		AllowAnonymous: false,
	}
	srv := buildTestServer(t, "prvroom", http.StatusOK, room)
	defer srv.Close()

	client, _ := roomapi.NewClient(srv.URL)
	_, err := client.FetchRoom(context.Background(), "prvroom")
	if err == nil {
		t.Fatal("expected error for allowAnonymous=false")
	}
	if !isError(err, roomapi.ErrAnonymousNotAllowed) {
		t.Errorf("expected ErrAnonymousNotAllowed, got %v", err)
	}
}

func TestFetchRoom_MissingConferenceID(t *testing.T) {
	// Server returns valid JSON but missing conferenceId
	body := map[string]interface{}{
		"roomName":       "badroom",
		"allowAnonymous": true,
		// conferenceId is missing
	}
	srv := buildTestServer(t, "badroom", http.StatusOK, body)
	defer srv.Close()

	client, _ := roomapi.NewClient(srv.URL)
	_, err := client.FetchRoom(context.Background(), "badroom")
	if err == nil {
		t.Fatal("expected error for missing conferenceId")
	}
}

func TestFetchRoom_ContextCancel(t *testing.T) {
	room := roomapi.RoomInfo{
		RoomName: "slowroom", ConferenceID: "slowroom_abc", AllowAnonymous: true,
	}
	srv := buildTestServer(t, "slowroom", http.StatusOK, room)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	client, _ := roomapi.NewClient(srv.URL)
	_, err := client.FetchRoom(ctx, "slowroom")
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
}

// isError checks if err wraps target (for sentinel errors we use fmt.Errorf with %w).
func isError(err, target error) bool {
	if err == nil {
		return false
	}
	return contains(err.Error(), target.Error())
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
