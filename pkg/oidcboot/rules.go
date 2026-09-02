// Package oidcboot holds the refusal rules that decide whether an OIDC provider
// environment can boot.
//
// Why this is a leaf package rather than a helper inside modules/oidc:
// two independent places need the same answer, and they cannot import each other.
//
//   - modules/oidc.LoadConfig needs it to refuse startup with a specific error.
//   - modules/common.isOIDCFullyConfigured needs it to avoid reporting a
//     configuration as usable when the module it configures cannot start.
//
// modules/oidc transitively imports modules/common, so the dependency can only
// run one way — which is how the second place ended up as a hand-maintained
// mirror of the first. That mirror then drifted, and the drift has a
// disproportionate blast radius:
//
//	a typo in the provider kind
//	  -> LoadConfig errors, the module registers 404 handlers for every endpoint
//	  -> but the mirror still answers "fully configured"
//	  -> so anyThirdPartyLoginConfigured stays true and login.local_off=1 is honoured
//	  -> password login also stays off
//	  -> an SSO-only deployment has no working login path at all, and the only
//	     recovery is a redeploy.
//
// Keeping the rules here means there is one copy. RefusedScenarios additionally
// pins the tests on both sides to the same table, so a future provider kind
// cannot re-open the gap by being added in only one of them.
//
// The package deliberately depends on nothing outside the standard library.
package oidcboot

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Supported provider kinds.
const (
	KindOIDC   = "oidc"
	KindOAuth2 = "oauth2"
)

// LogoutPathPrefix is the fixed path prefix of the plain-OAuth2 upstream's
// single-logout endpoint; the app id is appended as the final path segment.
const LogoutPathPrefix = "public/sp/slo"

// AppIDPattern limits the app id to URL-safe characters, must start
// alphanumeric, and caps the length at 64.
//
// The value is interpolated into a URL path segment, so something like "../x"
// would silently point the logout request at a different endpoint. Rejecting it
// at boot beats discovering it when a user logs out.
//
// **This is the only definition.** The provider used to carry a second, stricter
// pattern of its own, so an app id like "_tenant" — or one over 64 characters —
// passed boot validation and was then refused at runtime, where LogoutURL
// swallows the error and returns ("", false). Logout degraded silently to
// clearing the local session, which is precisely the failure the boot-time
// refusal exists to prevent, reached by another route.
var AppIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ErrUnknownKind is returned for a provider kind this build does not implement.
var ErrUnknownKind = errors.New("oidcboot: unknown provider kind")

// KindInput carries the already-resolved provider settings a refusal decision
// depends on.
//
// Resolved, not raw env: BaseURL in particular falls back to the issuer when
// unset, and a rule about "the base URL" has to see the value that will actually
// be used. Callers are responsible for applying that fallback first.
type KindInput struct {
	Kind    string
	BaseURL string
	AppID   string

	EndSessionURL         string
	PostLogoutRedirectURI string

	AutoLinkByEmail      bool
	RequireEmailVerified bool

	// AllowInsecureUpstream permits a plaintext base URL. Separate from the
	// logout-specific escape hatch on purpose: sharing one flag would let
	// "relax the pre-production logout URL" also open the production token
	// endpoint, which carries the client secret.
	AllowInsecureUpstream bool
}

// ValidateKind reports whether the provider configuration can boot.
//
// A nil return means modules/oidc will not refuse startup on account of these
// settings, and therefore that modules/common may treat the deployment as
// configured. The two statements must stay equivalent — that is the whole point
// of this function existing.
func ValidateKind(in KindInput) error {
	switch in.Kind {
	case "", KindOIDC:
		// The standard route keeps its existing semantics. The two settings that
		// belong to the other kind are still rejected rather than ignored:
		// copying configuration between kinds is routine, and a silently ignored
		// key reads as "logout is configured" when nothing is.
		if in.BaseURL != "" {
			return fmt.Errorf("oidcboot: a base URL is not applicable to provider kind %q "+
				"(endpoints come from Discovery); unset OCTO_OIDC_PROVIDER_BASE_URL "+
				"to avoid a silently ineffective setting", KindOIDC)
		}
		if in.AppID != "" {
			return fmt.Errorf("oidcboot: an app id is not applicable to provider kind %q "+
				"(logout uses RP-Initiated Logout with id_token_hint); unset "+
				"OCTO_OIDC_PROVIDER_APP_ID to avoid a silently ineffective setting", KindOIDC)
		}
		return nil

	case KindOAuth2:
		if err := validateUpstreamBaseURL(in.BaseURL, in.AllowInsecureUpstream); err != nil {
			return err
		}
		// This kind derives its logout endpoint from the app id, so an explicit
		// override cannot take effect.
		if in.EndSessionURL != "" {
			return fmt.Errorf("oidcboot: OCTO_OIDC_PROVIDER_END_SESSION_URL is not applicable "+
				"to provider kind %q (its logout endpoint is derived from "+
				"OCTO_OIDC_PROVIDER_APP_ID); unset it to avoid a silently ineffective override",
				in.Kind)
		}
		if in.AppID != "" && !AppIDPattern.MatchString(in.AppID) {
			return fmt.Errorf("oidcboot: OCTO_OIDC_PROVIDER_APP_ID %q invalid: must match %s",
				in.AppID, AppIDPattern)
		}
		// A redirect target without an app id cannot produce a logout URL, so
		// logout would degrade to clearing the local session while the operator
		// believes the upstream session is being ended too.
		if in.PostLogoutRedirectURI != "" && in.AppID == "" {
			return fmt.Errorf("oidcboot: OCTO_OIDC_POST_LOGOUT_REDIRECT_URI is set but "+
				"OCTO_OIDC_PROVIDER_APP_ID is empty: provider kind %q builds its logout URL "+
				"as {base}/%s/{appId}, so without an app id logout silently degrades to "+
				"clearing the local session only", in.Kind, LogoutPathPrefix)
		}
		// This kind's upstream has no verified-claim semantics at all, so the
		// verified flags are always false by construction. Combining
		// autolink-by-email with "verification not required" therefore promotes an
		// unverified address to an account-linking key: whoever controls an
		// address the upstream reports gets bound to the existing account that
		// holds it, permanently, on every later login.
		//
		// The safe default (verification required) is what prevents this today.
		// Refusing the combination turns a value someone can flip in a configmap
		// into a decision that has to pass review.
		if in.AutoLinkByEmail && !in.RequireEmailVerified {
			return fmt.Errorf("oidcboot: provider kind %q cannot assert email verification, "+
				"so OCTO_OIDC_PROVIDER_AUTO_LINK_BY_EMAIL=true together with "+
				"REQUIRE_EMAIL_VERIFIED=false would let an unverified address claim an "+
				"existing account; set REQUIRE_EMAIL_VERIFIED=true or turn autolink off",
				in.Kind)
		}
		return nil

	default:
		// A typo must not fall back to the standard kind: that would send a
		// deployment meant for plain OAuth2 into Discovery and then fail at boot
		// with an unrelated error.
		return fmt.Errorf("%w %q (supported: %q, %q)", ErrUnknownKind, in.Kind, KindOIDC, KindOAuth2)
	}
}

// validateUpstreamBaseURL requires an absolute http(s) site root.
//
// https is mandatory by default because this value carries both the client
// secret (token endpoint) and the access token (userinfo endpoint), and it is
// also what a browser is redirected to, where plaintext exposes state and code.
func validateUpstreamBaseURL(raw string, allowInsecure bool) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("oidcboot: provider kind " + KindOAuth2 +
			" requires a base URL (OCTO_OIDC_PROVIDER_BASE_URL, or a usable issuer to fall back to)")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("oidcboot: invalid upstream base URL %q: %w", raw, err)
	}
	if !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("oidcboot: upstream base URL %q must be absolute (scheme://host/path)", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if allowInsecure {
			return nil
		}
		return fmt.Errorf("oidcboot: upstream base URL %q must use https "+
			"(it carries client_secret and access_token; set "+
			"OCTO_OIDC_ALLOW_INSECURE_UPSTREAM=1 only for an isolated test environment)", raw)
	default:
		return fmt.Errorf("oidcboot: upstream base URL %q has scheme %q, want http(s)", raw, u.Scheme)
	}
}

// RefusedScenario is one configuration that must not be allowed to boot.
type RefusedScenario struct {
	Name  string
	Input KindInput

	// Env is the same scenario expressed as environment variables, for callers
	// that validate straight from the environment.
	Env map[string]string

	// ExpectKeyInError, when set, is a substring the refusal must mention so an
	// operator can act on the log line.
	ExpectKeyInError string
}

// RefusedScenarios is the shared table both sides' tests iterate.
//
// Adding a provider kind means adding its refusal cases here once; the tests in
// modules/oidc and modules/common then both pick them up, which is what stops
// the two from drifting again.
var RefusedScenarios = []RefusedScenario{
	{
		Name:  "unknown kind (typo)",
		Input: KindInput{Kind: "oauth22", RequireEmailVerified: true},
		Env: map[string]string{
			"OCTO_OIDC_PROVIDER_KIND": "oauth22",
		},
		ExpectKeyInError: "oauth22",
	},
	{
		Name: "oauth2 with a plaintext base url and no escape hatch",
		Input: KindInput{Kind: KindOAuth2, BaseURL: "http://idp.example.com",
			RequireEmailVerified: true},
		Env: map[string]string{
			"OCTO_OIDC_PROVIDER_KIND":     "oauth2",
			"OCTO_OIDC_PROVIDER_BASE_URL": "http://idp.example.com",
		},
		ExpectKeyInError: "https",
	},
	{
		Name: "oauth2 with a non-absolute base url",
		Input: KindInput{Kind: KindOAuth2, BaseURL: "idp.example.com",
			RequireEmailVerified: true},
		Env: map[string]string{
			"OCTO_OIDC_PROVIDER_KIND":     "oauth2",
			"OCTO_OIDC_PROVIDER_BASE_URL": "idp.example.com",
		},
		ExpectKeyInError: "absolute",
	},
	{
		Name: "oauth2 with an end-session override that cannot take effect",
		Input: KindInput{Kind: KindOAuth2, BaseURL: "https://idp.example.com",
			EndSessionURL: "https://idp.example.com/logout", RequireEmailVerified: true},
		Env: map[string]string{
			"OCTO_OIDC_PROVIDER_KIND":            "oauth2",
			"OCTO_OIDC_PROVIDER_BASE_URL":        "https://idp.example.com",
			"OCTO_OIDC_PROVIDER_END_SESSION_URL": "https://idp.example.com/logout",
		},
		ExpectKeyInError: "END_SESSION_URL",
	},
	{
		Name: "oauth2 with a path-traversing app id",
		Input: KindInput{Kind: KindOAuth2, BaseURL: "https://idp.example.com",
			AppID: "../../evil", RequireEmailVerified: true},
		Env: map[string]string{
			"OCTO_OIDC_PROVIDER_KIND":     "oauth2",
			"OCTO_OIDC_PROVIDER_BASE_URL": "https://idp.example.com",
			"OCTO_OIDC_PROVIDER_APP_ID":   "../../evil",
		},
		ExpectKeyInError: "APP_ID",
	},
	{
		Name: "oauth2 with a post-logout redirect but no app id",
		Input: KindInput{Kind: KindOAuth2, BaseURL: "https://idp.example.com",
			PostLogoutRedirectURI: "https://app.example.com/login", RequireEmailVerified: true},
		Env: map[string]string{
			"OCTO_OIDC_PROVIDER_KIND":            "oauth2",
			"OCTO_OIDC_PROVIDER_BASE_URL":        "https://idp.example.com",
			"OCTO_OIDC_POST_LOGOUT_REDIRECT_URI": "https://app.example.com/login",
		},
		ExpectKeyInError: "APP_ID",
	},
	{
		Name: "oauth2 with autolink on and verification not required",
		Input: KindInput{Kind: KindOAuth2, BaseURL: "https://idp.example.com",
			AutoLinkByEmail: true, RequireEmailVerified: false},
		Env: map[string]string{
			"OCTO_OIDC_PROVIDER_KIND":                 "oauth2",
			"OCTO_OIDC_PROVIDER_BASE_URL":             "https://idp.example.com",
			"DM_OIDC_PROVIDER_AUTO_LINK_BY_EMAIL":     "true",
			"DM_OIDC_PROVIDER_REQUIRE_EMAIL_VERIFIED": "false",
		},
		ExpectKeyInError: "AUTO_LINK_BY_EMAIL",
	},
	{
		Name: "oidc kind with a base url that would be ignored",
		Input: KindInput{Kind: KindOIDC, BaseURL: "https://idp.example.com",
			RequireEmailVerified: true},
		Env: map[string]string{
			"OCTO_OIDC_PROVIDER_KIND":     "oidc",
			"OCTO_OIDC_PROVIDER_BASE_URL": "https://idp.example.com",
		},
		ExpectKeyInError: "BASE_URL",
	},
	{
		Name:  "oidc kind with an app id that would be ignored",
		Input: KindInput{Kind: KindOIDC, AppID: "app1", RequireEmailVerified: true},
		Env: map[string]string{
			"OCTO_OIDC_PROVIDER_KIND":   "oidc",
			"OCTO_OIDC_PROVIDER_APP_ID": "app1",
		},
		ExpectKeyInError: "APP_ID",
	},
}
