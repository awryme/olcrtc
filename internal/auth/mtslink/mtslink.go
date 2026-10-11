package mtslink

import (
	"context"
	"net/http"
	"strings"

	"github.com/openlibrecommunity/olcrtc/internal/auth"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
)

// Provider produces mtslink engine credentials for the MTS Link service.
//
// apiBase/gwBase/edge override the endpoints the provider talks to; zero
// values mean the measured production ones. Only tests set them.
type Provider struct {
	apiBase string
	gwBase  string
	edge    string
}

// New returns an MTS Link auth provider against the production endpoints.
func New() Provider { return Provider{} }

// Engine reports which engine consumes credentials from this auth provider.
// The engine itself (REST+SDP room join, peer pin, VP9 publish) is the
// carrier's engine workstream; this provider hands it the join URL and the
// guest session.
func (Provider) Engine() string { return "mtslink" }

// DefaultServiceURL returns the MTS Link service URL.
func (Provider) DefaultServiceURL() string { return defaultAPIBase }

func (p Provider) newClient(cfg auth.Config) *http.Client {
	resolver := cfg.Resolver
	if resolver == nil {
		resolver = protect.NewResolver(cfg.DNSServer)
	}
	return protect.NewHTTPClient(resolver)
}

// Issue runs the guest join flow for an endless meeting and returns the
// credentials the mtslink engine consumes:
//
//	URL   — the SFU room join URL (SDP offer in, answer out)
//	Token — the guest cookie pair (access;refresh), for peer calls
//	Extra — userId and the session id the room join names
//
// cfg.RoomURL is the event join link the platform stores. cfg.Token, when
// set, is an existing cookie pair and skips the guestlogin step — the reuse
// pattern wbstream's Issue offers for its guest identity.
func (p Provider) Issue(ctx context.Context, cfg auth.Config) (auth.Credentials, error) {
	_, event, session, err := parseEventLink(cfg.RoomURL)
	if err != nil {
		return auth.Credentials{}, err
	}
	apiBase, gwBase := p.apiBase, p.gwBase
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	if gwBase == "" {
		gwBase = defaultGWBase
	}
	client := p.newClient(cfg)

	if session == "" {
		if session, err = resolveSession(ctx, client, gwBase, event, ""); err != nil {
			return auth.Credentials{}, err
		}
	}

	var g guestSession
	if pair := cfg.Token; pair != "" {
		access, refresh, _ := cutPair(pair)
		g = guestSession{access: access, refresh: refresh, userID: cfg.Name}
	} else {
		nickname := cfg.Name
		if nickname == "" {
			nickname = "olcrtc"
		}
		if g, err = guestLogin(ctx, client, apiBase, session, nickname); err != nil {
			return auth.Credentials{}, err
		}
	}

	token, err := joinToken(ctx, client, gwBase, session, g)
	if err != nil {
		return auth.Credentials{}, err
	}

	return auth.Credentials{
		URL:   RoomJoinURL(p.edge, session, g.userID, token),
		Token: g.cookieHeader(),
		Extra: map[string]string{"userId": g.userID, "sessionId": session},
	}, nil
}

func cutPair(pair string) (string, string, bool) {
	var access, refresh string
	for _, part := range strings.Split(pair, ";") {
		if k, v, f := strings.Cut(strings.TrimSpace(part), "="); f {
			switch k {
			case "access":
				access = v
			case "refresh":
				refresh = v
			}
		}
	}
	return access, refresh, access != "" && refresh != ""
}
