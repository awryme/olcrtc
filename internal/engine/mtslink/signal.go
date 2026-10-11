package mtslink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// The MTS Link SFU speaks SDP over plain HTTP (measured 2026-10-10, live):
// one POST exchanges an offer for an answer, and the peer the answer names is
// pinned by its session id. No WebSocket, no trickle — the SDPs carry their
// candidates inline, as the engine's other carriers work.
//
// docs: proofkit-dvpn docs/superpowers/specs/2026-10-11-olcrtc-mtslink-engine-design.md

var (
	// ErrNoAnswer is a join that answered anything but an SDP answer.
	ErrNoAnswer = errors.New("mtslink: the room join did not answer an SDP")
	// ErrPeerUnpinned is a pin the SFU refused; the join is worthless then.
	ErrPeerUnpinned = errors.New("mtslink: the SFU refused the peer pin")
	// ErrBadStatus wraps every unexpected HTTP status the SFU answered.
	ErrBadStatus = errors.New("mtslink: unexpected status")
)

// maxSignalBody bounds what a signaling reply may be; an SDP answer is a few
// kilobytes, and anything larger is not one.
const maxSignalBody = 1 << 20

// joinOffer POSTs the offer to the room join URL with the guest's cookies and
// returns the answer's SDP. The web client sends the offer as the bare body;
// the content type it used was not captured, so `application/sdp` rides along
// and the SFU takes the body either way (measured: it did).
func joinOffer(ctx context.Context, client *http.Client, joinURL, cookies, offer string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, joinURL, strings.NewReader(offer))
	if err != nil {
		return "", fmt.Errorf("mtslink: join request: %w", err)
	}
	req.Header.Set("Content-Type", "application/sdp")
	if cookies != "" {
		req.Header.Set("Cookie", cookies)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("mtslink: join send: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxSignalBody))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("join: %w %d", ErrBadStatus, resp.StatusCode)
	}
	answer := string(body)
	if !strings.Contains(answer, "v=0") {
		return "", ErrNoAnswer
	}
	return answer, nil
}

// pinPeer pins the peer the answer named (its SDP session id, `s=`): the call
// the web client always makes after a join, and without which the SFU drops
// the media. The URL the answer's pin used was the join URL's host with the
// peer path, so the pin is built from the join URL's own origin.
func pinPeer(ctx context.Context, client *http.Client, joinURL, peerID, cookies string) error {
	origin, err := url.Parse(joinURL)
	if err != nil || origin.Scheme == "" || origin.Host == "" {
		return fmt.Errorf("pin url: %w: %w", ErrBadStatus, err)
	}
	pinURL := origin.Scheme + "://" + origin.Host + "/rtc/peer/" + peerID + "/pin"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pinURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("mtslink: pin request: %w", err)
	}
	if cookies != "" {
		req.Header.Set("Cookie", cookies)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("mtslink: pin send: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxSignalBody))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: status %d", ErrPeerUnpinned, resp.StatusCode)
	}
	return nil
}

// sdpSessionID reads the `s=` line out of an SDP: the SFU's peer id, which the
// web client's own pin call used verbatim (measured: the answer's `s=` value
// and the peer path's id were one and the same).
func sdpSessionID(sdp string) string {
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "s="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
