// Package mtslink implements the MTS Link auth provider: the guest half of
// the carrier protocol, measured 2026-10-10 against the live service
// (proofkit-dvpn docs/superpowers/specs/2026-10-10-olcrtc-mtslink-carrier-design.md).
//
// A guest needs no MTS account: guestlogin with a nickname is accepted with
// no captcha and no moderation, and the cookie pair it sets (access, refresh)
// authenticates every later call. The rooms the platform makes are endless
// meetings («постоянные встречи») — a guest enters one with no organizer
// inside, which is why the engine can be a guest and nothing else.
package mtslink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	defaultAPIBase = "https://my.mts-link.ru"
	defaultGWBase  = "https://gw.mts-link.ru"
	// defaultEdge is where a meeting's SFU answered in measurement (a meeting's
	// conncheck went to sfu.mts-link.ru; a webinar's room join to
	// live-webrtc-ixcN-*.webinar.ru). Overridable until the first live engine
	// run pins what the service itself names for meetings.
	defaultEdge = "https://sfu.mts-link.ru"
)

var (
	// ErrBadStatus wraps every unexpected HTTP status a gateway call answered;
	// the number rides along.
	ErrBadStatus = errors.New("mtslink: unexpected status")
	// ErrUnreadableReply is a reply the service sent that is not the JSON it
	// promises by its own content type.
	ErrUnreadableReply = errors.New("mtslink: unreadable reply")
	// ErrIncompleteReply is a reply that names nothing usable.
	ErrIncompleteReply = errors.New("mtslink: incomplete reply")
	// ErrRoomRef names a room reference that is not the event join link.
	ErrRoomRef = errors.New("mtslink: room must be the event join link (…/j/{ownerId}/{eventId})")
	// ErrNotAccepted is the service turning a guest away.
	ErrNotAccepted = errors.New("mtslink: the service did not accept the guest")
	// ErrNoSession is an event without an endless session to join.
	ErrNoSession = errors.New("mtslink: the event has no endless session")
)

// guestSession is what a guest login yields: the cookie pair every gateway
// call presents, and the user id the room join names. The pair is a live
// session and never reaches a log.
type guestSession struct {
	access, refresh string
	userID          string
}

func (g guestSession) cookieHeader() string {
	return "access=" + g.access + "; refresh=" + g.refresh
}

func cookieValue(setCookies []string, name string) string {
	for _, line := range setCookies {
		pair := strings.SplitN(strings.TrimSpace(line), ";", 2)[0]
		if k, v, ok := strings.Cut(pair, "="); ok && k == name {
			return v
		}
	}
	return ""
}

// parseEventLink takes a room reference — the event join link the platform
// stores, `https://my.mts-link.ru/j/{ownerId}/{eventId}` — and returns the
// owner and event ids. A session link (…/session/{sid}) also carries its
// session, which saves the resolution call.
func parseEventLink(room string) (string, string, string, error) {
	var owner, event, session string
	u, err := url.Parse(strings.TrimSpace(room))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", "", ErrRoomRef
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	idx := -1
	for i, p := range parts {
		if p == "j" {
			idx = i
			break
		}
	}
	if idx < 0 || len(parts) < idx+3 {
		return "", "", "", ErrRoomRef
	}
	owner, event = parts[idx+1], parts[idx+2]
	if len(parts) >= idx+5 && parts[idx+3] == "session" {
		session = parts[idx+4]
	}
	if !isID(owner) || !isID(event) {
		return "", "", "", ErrRoomRef
	}
	if session != "" && !isID(session) {
		session = ""
	}
	return owner, event, session, nil
}

func isID(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// guestLogin registers a guest on the session and returns what it yields.
func guestLogin(ctx context.Context, c *http.Client, apiBase, session, nickname string) (guestSession, error) {
	payload, err := json.Marshal(map[string]string{"nickname": nickname})
	if err != nil {
		return guestSession{}, fmt.Errorf("mtslink: guestlogin payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiBase+"/api/eventsessions/"+session+"/guestlogin", strings.NewReader(string(payload)))
	if err != nil {
		return guestSession{}, fmt.Errorf("mtslink: guestlogin request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return guestSession{}, fmt.Errorf("mtslink: guestlogin send: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return guestSession{}, fmt.Errorf("guestlogin: %w %d", ErrBadStatus, resp.StatusCode)
	}
	var reply struct {
		IsAccepted bool `json:"isAccepted"` //nolint:tagliatelle // the service wire is camelCase
		User       struct {
			ID int64 `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return guestSession{}, fmt.Errorf("guestlogin: %w", ErrUnreadableReply)
	}
	if !reply.IsAccepted {
		return guestSession{}, ErrNotAccepted
	}
	setCookies := append([]string(nil), resp.Header.Values("Set-Cookie")...)
	g := guestSession{
		access:  cookieValue(setCookies, "access"),
		refresh: cookieValue(setCookies, "refresh"),
		userID:  strconv.FormatInt(reply.User.ID, 10),
	}
	if g.access == "" || g.refresh == "" || g.userID == "0" {
		return guestSession{}, fmt.Errorf("guestlogin: %w: no cookie pair", ErrIncompleteReply)
	}
	return g, nil
}

// resolveSession finds the event's endless session id: from the link when it
// carries one, else from the event's public record.
func resolveSession(ctx context.Context, c *http.Client, gwBase, event, fromLink string) (string, error) {
	if fromLink != "" {
		return fromLink, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gwBase+"/api/event/"+event, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("mtslink: event lookup request: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("mtslink: event lookup send: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("event lookup: %w %d", ErrBadStatus, resp.StatusCode)
	}
	var ev struct {
		EventSessions []struct {
			ID int64 `json:"id"`
		} `json:"eventSessions"` //nolint:tagliatelle // the service wire is camelCase
	}
	if err := json.Unmarshal(raw, &ev); err != nil || len(ev.EventSessions) == 0 {
		return "", ErrNoSession
	}
	return strconv.FormatInt(ev.EventSessions[0].ID, 10), nil
}

// joinToken is the ticket the room join presents.
func joinToken(ctx context.Context, c *http.Client, gwBase, session string, g guestSession) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		gwBase+"/api/eventsessions/"+session+"/join-token", http.NoBody)
	if err != nil {
		return "", fmt.Errorf("mtslink: join-token request: %w", err)
	}
	req.Header.Set("Cookie", g.cookieHeader())
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("mtslink: event lookup send: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("join-token: %w %d", ErrBadStatus, resp.StatusCode)
	}
	var reply struct {
		Data struct {
			JoinToken string `json:"joinToken"` //nolint:tagliatelle // the service wire is camelCase
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil || reply.Data.JoinToken == "" {
		return "", fmt.Errorf("join-token: %w: no token", ErrIncompleteReply)
	}
	return reply.Data.JoinToken, nil
}

// RoomJoinURL is the SFU join the web client issues, the measured shape:
// POST {edge}/rtc/room/{roomId}/join?userId=&joinToken=&disableRecording=false,
// where the room id is the session's and the user id is the guest's.
func RoomJoinURL(edge, room, userID, token string) string {
	if edge == "" {
		edge = defaultEdge
	}
	return fmt.Sprintf("%s/rtc/room/%s/join?userId=%s&joinToken=%s&disableRecording=false",
		strings.TrimSuffix(edge, "/"), room, userID, token)
}
