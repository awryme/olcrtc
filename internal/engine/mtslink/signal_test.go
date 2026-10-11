package mtslink

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The measured answer, abridged to the lines the engine reads.
const answerSDP = "v=0\r\no=odin 0 0 IN IP4 127.0.0.1\r\ns=31djU7RlhsDoZq\r\nc=IN IP4 127.0.0.1\r\n" +
	"a=ice-ufrag:zaBE92Hg\r\na=ice-pwd:xuUfDndbAcYzhJcDu0ZGfr\r\n" +
	"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=mid:0\r\na=sendonly\r\n" +
	"m=video 9 UDP/TLS/RTP/SAVPF 98\r\na=mid:1\r\na=sendonly\r\n"

func TestSDPSessionIDIsThePeer(t *testing.T) {
	if got := sdpSessionID(answerSDP); got != "31djU7RlhsDoZq" {
		t.Fatalf("sdpSessionID = %q", got)
	}
	if got := sdpSessionID("v=0\no=odin 0 0\ns= peer with spaces \n"); got != "peer with spaces" {
		t.Fatalf("sdpSessionID spaces = %q", got)
	}
	if got := sdpSessionID("v=0\no=odin 0 0\n"); got != "" {
		t.Fatalf("sdpSessionID without s = %q", got)
	}
}

func TestJoinOfferExchangesSDPForAnswer(t *testing.T) {
	var gotOffer, gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		gotOffer, gotCookie = string(body[:n]), r.Header.Get("Cookie")
		w.Write([]byte(answerSDP))
	}))
	defer srv.Close()

	answer, err := joinOffer(context.Background(), srv.Client(), srv.URL+"/rtc/room/1/join", "access=a; refresh=r", "v=0\nmy offer")
	if err != nil {
		t.Fatalf("joinOffer: %v", err)
	}
	if answer != answerSDP || gotOffer != "v=0\nmy offer" {
		t.Fatalf("exchange mismatch: offer %q answer %q", gotOffer, answer)
	}
	if gotCookie != "access=a; refresh=r" {
		t.Fatalf("cookie = %q", gotCookie)
	}
}

func TestJoinOfferRefusesANonSDPAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"error":"no room"}`))
	}))
	defer srv.Close()
	if _, err := joinOffer(context.Background(), srv.Client(), srv.URL, "", "v=0"); err == nil {
		t.Fatal("a JSON error body must not pass as an answer")
	}
}

func TestPinPeerUsesTheJoinOriginAndThePeerID(t *testing.T) {
	var pinPath, gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pinPath, gotCookie = r.URL.Path, r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := pinPeer(context.Background(), srv.Client(),
		srv.URL+"/rtc/room/25948393178/join?userId=1", "31djU7RlhsDoZq", "access=a; refresh=r"); err != nil {
		t.Fatalf("pinPeer: %v", err)
	}
	if want := "/rtc/peer/31djU7RlhsDoZq/pin"; pinPath != want {
		t.Fatalf("pin path = %q, want %q", pinPath, want)
	}
	if gotCookie != "access=a; refresh=r" {
		t.Fatalf("cookie = %q", gotCookie)
	}
}

func TestPinPeerRefusedIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	err := pinPeer(context.Background(), srv.Client(), srv.URL+"/x", "p", "")
	if err == nil || !strings.Contains(err.Error(), "pin") {
		t.Fatalf("a refused pin must say so, got %v", err)
	}
}
