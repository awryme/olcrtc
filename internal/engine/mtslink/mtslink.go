// Package mtslink is the engine for MTS Link rooms: SDP exchanged over plain
// HTTP with the service's SFU, payload riding the video track (the
// vp8channel transport on top), the guest always let in.
//
// The signaling is two calls, both measured live: POST the offer to the room
// join URL for the answer, then POST the peer pin for the session id the
// answer's `s=` line names. No WebSocket, no trickle — the SDPs carry their
// candidates inline.
package mtslink

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"

	"github.com/openlibrecommunity/olcrtc/internal/engine"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
)

// Session errors.
var (
	ErrSessionClosed   = errors.New("mtslink: session closed")
	ErrNoEndpoint      = errors.New("mtslink: room join URL missing")
	ErrNoPeerID        = errors.New("mtslink: the answer named no peer to pin")
	ErrConnectTimeout  = errors.New("mtslink: peer connection did not connect")
	ErrSendUnsupported = errors.New("mtslink: send requires the video transport")
)

const (
	connectWait   = 30 * time.Second
	maxReconnects = 10
)

// Session is an engine.Session over one MTS Link endless room. The room never
// closes (an endless meeting), so a reconnect is always a fresh guest join
// with the same session id.
type Session struct {
	cfg engine.Config

	mu      sync.Mutex
	joinURL string
	cookies string
	gen     *generation

	video engine.VideoTrackState

	engine.Reconnector

	closed       atomic.Bool
	reconnecting atomic.Bool
	closeCh      chan struct{}
	closeOnce    sync.Once
}

// generation bundles everything one join owns, so closing it tears the whole
// attempt down and nothing crosses attempts.
type generation struct {
	pc        *webrtc.PeerConnection
	peerID    string
	connected chan struct{}
	done      chan struct{}
	doneOnce  sync.Once
}

func (gen *generation) isDone() bool {
	select {
	case <-gen.done:
		return true
	default:
		return false
	}
}

func (gen *generation) stop() {
	gen.doneOnce.Do(func() { close(gen.done) })
}

func (gen *generation) teardown() {
	gen.stop()
	if gen.pc != nil {
		_ = gen.pc.Close()
	}
}

// New builds an MTS Link session from provider credentials: URL is the room
// join URL, Token the guest cookie pair, Extra carrying userId and sessionId.
func New(_ context.Context, cfg engine.Config) (engine.Session, error) {
	if cfg.URL == "" {
		return nil, ErrNoEndpoint
	}
	s := &Session{cfg: cfg, joinURL: cfg.URL, cookies: cfg.Token, closeCh: make(chan struct{})}
	s.Configure(engine.ReconnectorConfig{
		MaxAttempts: maxReconnects,
		Reconnect:   s.reconnect,
		OnError:     func(err error) { logger.Debugf("mtslink: reconnect failed: %v", err) },
		OnLimit:     func(string) { s.signalEnded("reconnect limit reached") },
		LimitReason: "reconnect limit reached",
	})
	return s, nil
}

// AddVideoTrack records a local track for the next join: the vp8channel
// transport's publish slot.
func (s *Session) AddVideoTrack(track webrtc.TrackLocal) error { //nolint:unparam // the VideoTrackCapable contract
	s.video.StoreVideoTrack(track)
	return nil
}

// SetVideoTrackHandler registers the remote-track callback the tunnel's
// downlink arrives on.
func (s *Session) SetVideoTrackHandler(cb func(*webrtc.TrackRemote, *webrtc.RTPReceiver)) {
	s.video.SetVideoTrackHandler(cb)
}

// buildPeerConnection assembles the browser-class endpoint the SFU expects:
// the measured codecs, the protected resolver, the default interceptors that
// keep the SFU's RTCP fed.
func (s *Session) buildPeerConnection() (*webrtc.PeerConnection, error) {
	media := &webrtc.MediaEngine{}
	if err := registerCodecs(media); err != nil {
		return nil, fmt.Errorf("mtslink: codecs: %w", err)
	}
	settings := webrtc.SettingEngine{}
	apply, err := engine.NewPionSettings(engine.PionSettingsOptions{
		Resolver:         s.cfg.Resolver,
		DTLSProfile:      s.cfg.DTLSProfile,
		LoggerFactory:    logger.NewPionLoggerFactory(),
		IPv4Only:         true,
		DisableMulticast: true,
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // the shared builder already adds context
	}
	if apply != nil {
		apply(&settings)
	}
	// The engine offers as a browser-class endpoint: default interceptors
	// (RTCP reports, NACK, TWCC) keep the SFU's forward fed.
	registry := &interceptor.Registry{}
	if regErr := webrtc.RegisterDefaultInterceptorsWithOptions(
		media, registry, engine.DefaultInterceptorOptions()...,
	); regErr != nil {
		return nil, fmt.Errorf("mtslink: interceptors: %w", regErr)
	}
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(media),
		webrtc.WithSettingEngine(settings),
		webrtc.WithInterceptorRegistry(registry),
	)
	pc, pcErr := api.NewPeerConnection(webrtc.Configuration{})
	if pcErr != nil {
		return nil, fmt.Errorf("mtslink: new peer connection: %w", pcErr)
	}
	return pc, nil
}

// Connect joins the room: one HTTP SDP exchange, one pin, then the peer
// connection settles with the publish track inside.
func (s *Session) Connect(ctx context.Context) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}

	resolver := s.cfg.Resolver
	if resolver == nil {
		resolver = protect.NewResolver(s.cfg.DNSServer)
	}
	client := protect.NewHTTPClient(resolver)

	pc, err := s.buildPeerConnection()
	if err != nil {
		return err
	}
	gen := &generation{pc: pc, connected: make(chan struct{}), done: make(chan struct{})}

	s.video.RangeVideoTracks(func(track webrtc.TrackLocal, _ bool) {
		sender, addErr := pc.AddTrack(track)
		if addErr != nil {
			logger.Debugf("mtslink: add track: %v", addErr)
			return
		}
		go drainRTCP(sender)
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		logger.Debugf("mtslink: remote track kind=%s codec=%s ssrc=%d", track.Kind(), track.Codec().MimeType, track.SSRC())
		if handler := s.video.VideoTrackHandler(); handler != nil {
			handler(track, receiver)
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		logger.Debugf("mtslink: peer connection %s", state)
		switch state {
		case webrtc.PeerConnectionStateConnected:
			select {
			case <-gen.connected:
			default:
				close(gen.connected)
			}
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			gen.stop()
			s.reconnectAttempt(gen)
		case webrtc.PeerConnectionStateNew, webrtc.PeerConnectionStateConnecting,
			webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateUnknown:
		}
	})

	s.mu.Lock()
	joinURL, cookies := s.joinURL, s.cookies
	s.mu.Unlock()

	if err := s.negotiateJoin(ctx, gen, client, joinURL, cookies); err != nil {
		return err
	}
	return s.awaitConnected(ctx, gen)
}

// negotiateJoin runs the measured flow: the offer POSTed to the room join URL
// for the answer, the peer the answer names pinned. Nothing here waits on ICE.
func (s *Session) negotiateJoin(
	ctx context.Context, gen *generation, client *http.Client, joinURL, cookies string,
) error {
	pc := gen.pc
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		gen.teardown()
		return fmt.Errorf("mtslink: offer: %w", err)
	}
	if setErr := pc.SetLocalDescription(offer); setErr != nil {
		gen.teardown()
		return fmt.Errorf("mtslink: local description: %w", setErr)
	}
	answer, err := joinOffer(ctx, client, joinURL, cookies, offer.SDP)
	if err != nil {
		gen.teardown()
		return err
	}
	peerID := sdpSessionID(answer)
	if peerID == "" {
		gen.teardown()
		return ErrNoPeerID
	}
	gen.peerID = peerID
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: answer,
	}); err != nil {
		gen.teardown()
		return fmt.Errorf("mtslink: remote description: %w", err)
	}
	if err := pinPeer(ctx, client, joinURL, peerID, cookies); err != nil {
		gen.teardown()
		return err
	}

	return nil
}

// awaitConnected waits for the peer connection to settle within connectWait
// and records the generation as the session's once it has.
func (s *Session) awaitConnected(ctx context.Context, gen *generation) error {
	timer := time.NewTimer(connectWait)
	defer timer.Stop()
	select {
	case <-gen.connected:
	case <-gen.done:
		return ErrConnectTimeout
	case <-ctx.Done():
		gen.teardown()
		return fmt.Errorf("mtslink: connect wait: %w", ctx.Err())
	case <-timer.C:
		gen.teardown()
		return ErrConnectTimeout
	}
	s.mu.Lock()
	s.gen = gen
	s.mu.Unlock()
	return nil
}

// reconnectAttempt queues a rejoin for a failed attempt of the session's.
func (s *Session) reconnectAttempt(gen *generation) {
	if s.closed.Load() || gen.isDone() {
		return
	}
	s.Request(s.closed.Load(), s.reconnecting.Load())
}

// Reconnect asks the session to rejoin; the supervisor serializes it.
func (s *Session) Reconnect(reason string) {
	if s.closed.Load() {
		return
	}
	logger.Debugf("mtslink: reconnect requested: %s", reason)
	s.Request(s.closed.Load(), s.reconnecting.Load())
}

// reconnect tears the current attempt down and joins again, with fresh guest
// credentials when the provider can mint them.
func (s *Session) reconnect(ctx context.Context) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	s.reconnecting.Store(true)
	defer s.reconnecting.Store(false)

	s.mu.Lock()
	gen := s.gen
	s.gen = nil
	s.mu.Unlock()
	if gen != nil {
		gen.teardown()
	}
	if s.cfg.Refresh != nil {
		creds, err := s.cfg.Refresh(ctx)
		if err != nil {
			return fmt.Errorf("refresh credentials: %w", err)
		}
		s.mu.Lock()
		s.joinURL = creds.URL
		s.cookies = creds.Token
		s.mu.Unlock()
	}
	if err := s.Connect(ctx); err != nil {
		return err
	}
	s.NotifyReconnect()
	return nil
}

// Send is unsupported: payload rides the video track (the vp8channel
// transport), as on WB Stream and VK Calls.
func (s *Session) Send([]byte) error { return ErrSendUnsupported }

// CanSend says whether the video transport can carry right now.
func (s *Session) CanSend() bool {
	s.mu.Lock()
	gen := s.gen
	s.mu.Unlock()
	return gen != nil && !gen.isDone() && s.video.HasVideoTracks()
}

// SubscriberCanSend mirrors CanSend: one peer connection carries both ways.
func (s *Session) SubscriberCanSend() bool { return s.CanSend() }

// GetBufferedAmount is the video transport's, not a data channel's.
func (s *Session) GetBufferedAmount() uint64 { return 0 }

// WatchConnection hands the session's lifetime to the shared supervisor.
func (s *Session) WatchConnection(ctx context.Context) {
	s.Watch(ctx, s.closeCh)
}

// signalEnded ends the session for good: no rejoin is asked for.
func (s *Session) signalEnded(_ string) {
	s.closed.Store(true)
	s.closeOnce.Do(func() { close(s.closeCh) })
}

// Close leaves the room and frees the peer connection.
func (s *Session) Close() error {
	s.signalEnded("closed by caller")
	s.mu.Lock()
	gen := s.gen
	s.gen = nil
	s.mu.Unlock()
	if gen != nil {
		gen.teardown()
	}
	return nil
}

// registerCodecs registers what the SFU's answers carried (measured): opus
// with red, VP8 and VP9 video. The offer stays within what the room takes.
func registerCodecs(media *webrtc.MediaEngine) error {
	for _, c := range []webrtc.RTPCodecParameters{
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
				SDPFmtpLine: "minptime=10;useinbandfec=1",
			},
			PayloadType: 111,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
			PayloadType:        96,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000},
			PayloadType:        98,
		},
	} {
		kind := webrtc.RTPCodecTypeVideo
		if strings.HasPrefix(c.MimeType, "audio/") {
			kind = webrtc.RTPCodecTypeAudio
		}
		if err := media.RegisterCodec(c, kind); err != nil {
			return fmt.Errorf("mtslink: register codec: %w", err)
		}
	}
	return nil
}

// drainRTCP reads a sender's RTCP until the sender closes, so the RTCP
// reaches the interceptors.
func drainRTCP(sender *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buf); err != nil {
			return
		}
	}
}
