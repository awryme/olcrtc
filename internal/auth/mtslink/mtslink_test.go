package mtslink

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openlibrecommunity/olcrtc/internal/auth"
)

const (
	testEvent   = "26440844595"
	testSession = "25948393178"
	testOwner   = "184238075"
)

func TestParseEventLink(t *testing.T) {
	for _, tc := range []struct {
		link, owner, event, session string
		ok                          bool
	}{
		{"https://my.mts-link.ru/j/" + testOwner + "/" + testEvent, testOwner, testEvent, "", true},
		{"https://my.mts-link.ru/j/" + testOwner + "/" + testEvent + "/session/" + testSession, testOwner, testEvent, testSession, true},
		{"https://my.mts-link.ru/j/" + testOwner + "/" + testEvent + "/stream-new/" + testSession, testOwner, testEvent, "", true},
		{"https://evil.example/j/1/2", "1", "2", "", true},
		{"not a url", "", "", "", false},
		{"https://my.mts-link.ru/events", "", "", "", false},
	} {
		owner, event, session, err := parseEventLink(tc.link)
		if tc.ok && (err != nil || owner != tc.owner || event != tc.event || session != tc.session) {
			t.Fatalf("parseEventLink(%q) = %q,%q,%q,%v", tc.link, owner, event, session, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("parseEventLink(%q) accepted", tc.link)
		}
	}
}

func TestRoomJoinURLIsTheMeasuredShape(t *testing.T) {
	got := RoomJoinURL("", testSession, "184239223", "vcTOKEN")
	want := "https://sfu.mts-link.ru/rtc/room/" + testSession + "/join?userId=184239223&joinToken=vcTOKEN&disableRecording=false"
	if got != want {
		t.Fatalf("RoomJoinURL = %q, want %q", got, want)
	}
}

// The gateway and the guest endpoints behind one test server, answering the
// measured shapes: a guest accepted with the cookie pair, and a join-token.
func TestIssueMintsAGuestAndTheRoomJoin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/guestlogin") && r.Method == http.MethodPost:
			w.Header().Add("Set-Cookie", "access=eyJabc.def; Path=/; Secure; HttpOnly")
			w.Header().Add("Set-Cookie", "refresh=6074refresh00000000000000000000009; Path=/; Secure")
			w.Write([]byte(`{"isAccepted":true,"user":{"id":184239223}}`))
		case strings.HasSuffix(r.URL.Path, "/join-token") && r.Method == http.MethodGet:
			if r.Header.Get("Cookie") == "" {
				t.Error("join-token without the guest cookie")
			}
			w.Write([]byte(`{"data":{"joinToken":"vcTOKEN"}}`))
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p := Provider{apiBase: srv.URL, gwBase: srv.URL, edge: srv.URL}
	creds, err := p.Issue(context.Background(), auth.Config{
		RoomURL: "https://my.mts-link.ru/j/" + testOwner + "/" + testEvent + "/session/" + testSession,
		Name:    "pk-de",
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	wantURL := srv.URL + "/rtc/room/" + testSession + "/join?userId=184239223&joinToken=vcTOKEN&disableRecording=false"
	if creds.URL != wantURL {
		t.Fatalf("URL = %q, want %q", creds.URL, wantURL)
	}
	if creds.Token != "access=eyJabc.def; refresh=6074refresh00000000000000000000009" {
		t.Error("Token is not the cookie pair")
	}
	if creds.Extra["userId"] != "184239223" || creds.Extra["sessionId"] != testSession {
		t.Errorf("Extra = %v", creds.Extra)
	}
}
