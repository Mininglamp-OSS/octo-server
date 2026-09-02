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
	"os"
	"regexp"
	"strconv"
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
// It is a function rather than an exported `var` on purpose: a mutable
// package-level regexp that both boot validation and the runtime URL builder
// depend on can be reassigned by any importer, which would silently widen the
// validator both consumers were unified onto.
func ValidAppID(appID string) bool { return appIDPattern.MatchString(appID) }

// AppIDDescription is the human-readable form of the rule, for error messages.
// Callers must not need the regexp object itself.
const AppIDDescription = "alphanumeric first character, then letters/digits/underscore/hyphen, max 64 characters"

var appIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// EnvBool reads a boolean env var with an optional legacy alias.
//
// **This is the only definition.** Two copies existed — one in the module's own
// config loader, one in the settings helper that decides whether a login method
// counts as configured — with a comment on the second claiming it matched the
// first. It did not: on a value that is present but unparseable, one fell through
// to the legacy alias and the other returned the default.
//
// That single disagreement is enough to remove every login path. With
// `kind=oauth2`, `AUTO_LINK_BY_EMAIL=true`, a malformed
// `REQUIRE_EMAIL_VERIFIED` and a legacy alias saying `false`, the module refuses
// to boot (all endpoints 404) while the helper reports "configured" — so
// `login.local_off` stays honoured and an SSO-only deployment has nothing left to
// log in with, recoverable only by redeploy.
//
// Fall-through on an unparseable primary is the deliberate semantic: during a key
// migration a typo in the new key must not swallow a still-valid old key.
func EnvBool(primary, alias string, def bool) bool {
	if v, ok := os.LookupEnv(primary); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	if alias != "" {
		if v, ok := os.LookupEnv(alias); ok && v != "" {
			if b, err := strconv.ParseBool(v); err == nil {
				return b
			}
		}
	}
	return def
}

// UpstreamBaseURL resolves the base URL that will actually be used.
//
// **Single definition of the fallback.** Under KindOAuth2 the issuer doubles as the
// site root, so an unset base URL falls back to it. Both config readers must make this
// decision identically, and they did not: the module tested the **untrimmed** value
// (`p.BaseURL == ""`) while the settings helper trimmed first. A whitespace-only base URL
// therefore made the module refuse to boot -- every OIDC route a 404 -- while the helper
// took the fallback, reported "configured", and left `login.local_off` honoured: an
// SSO-only deployment with no way to log in and no recovery short of a redeploy.
//
// Normalising inside ValidateKind was not enough, because this decision runs **before**
// it. That is why the fallback itself has to live here.
func UpstreamBaseURL(kind, baseURL, issuer string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL != "" {
		return baseURL
	}
	if strings.TrimSpace(kind) != KindOAuth2 {
		// Under the standard kind endpoints come from Discovery; substituting the issuer
		// would make an unconfigured base URL look configured.
		return ""
	}
	return strings.TrimSpace(issuer)
}

// ValidateLogoutURL checks one of the two RP-Initiated Logout URLs.
//
// **Single definition.** Both are boot-fatal in the module, and the settings helper had
// no counterpart -- while its own doc comment claimed to mirror every fatal check and to
// omit only non-fatal ones. A relative `OCTO_OIDC_POST_LOGOUT_REDIRECT_URI` therefore
// produced the same total lockout as the cases above.
//
// Empty means "feature off" and is allowed. Non-empty must be absolute and https: both
// values end up in a browser top-level navigation and the end-session URL carries an
// id_token, so a misconfiguration can send a token to an arbitrary origin or execute
// script on navigation. allowInsecure relaxes to http for development only.
func ValidateLogoutURL(envName, raw string, allowInsecure bool) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("oidcboot: invalid %s %q: %w", envName, raw, err)
	}
	if u.Host == "" {
		return fmt.Errorf("oidcboot: %s %q must be absolute (scheme://host/path)", envName, raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && allowInsecure {
		return nil
	}
	return fmt.Errorf("oidcboot: %s %q must use https scheme "+
		"(set OCTO_OIDC_LOGOUT_ALLOW_INSECURE=1 to allow http for dev)", envName, raw)
}

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

	// AllowInsecureLogout permits plain http for the two logout URLs (dev only).
	// Deliberately a different flag from AllowInsecureUpstream -- see that field.
	AllowInsecureLogout bool

	// Issuer is the identity namespace. Under KindOAuth2 it doubles as the default
	// base URL; that fallback is UpstreamBaseURL, which both callers must share.
	Issuer string
}

// normalised trims every string field. A value that is only whitespace is not a
// configured value, and both callers must agree on that without having to remember to
// trim — which is exactly what they failed to do.
func (in KindInput) normalised() KindInput {
	in.Kind = strings.TrimSpace(in.Kind)
	in.BaseURL = strings.TrimSpace(in.BaseURL)
	in.AppID = strings.TrimSpace(in.AppID)
	in.EndSessionURL = strings.TrimSpace(in.EndSessionURL)
	in.PostLogoutRedirectURI = strings.TrimSpace(in.PostLogoutRedirectURI)
	in.Issuer = strings.TrimSpace(in.Issuer)
	return in
}

// ValidateKind reports whether the provider configuration can boot.
//
// A nil return means modules/oidc will not refuse startup on account of these
// settings, and therefore that modules/common may treat the deployment as
// configured. The two statements must stay equivalent — that is the whole point
// of this function existing.
func ValidateKind(in KindInput) error {
	if err := validateKindRules(in); err != nil {
		return err
	}
	// Logout URL shape is kind-independent -- it is a property of the URL, not of the
	// protocol. Checked **after** the kind rules so that a value which is not applicable
	// to the kind at all reports that first, which is the more actionable message.
	in = in.normalised()
	if err := ValidateLogoutURL("OCTO_OIDC_POST_LOGOUT_REDIRECT_URI",
		in.PostLogoutRedirectURI, in.AllowInsecureLogout); err != nil {
		return err
	}
	return ValidateLogoutURL("OCTO_OIDC_PROVIDER_END_SESSION_URL",
		in.EndSessionURL, in.AllowInsecureLogout)
}

func validateKindRules(in KindInput) error {
	// Normalise **here**, so the two callers cannot normalise differently.
	//
	// Unifying the rules was not enough: one reader trimmed its env values and the
	// other did not, while the rules below compare against "". A whitespace-only
	// BASE_URL (a trailing space, or a multi-line YAML scalar) therefore arrived as
	// two different inputs — the module refused to boot, every OIDC route became a
	// 404, and the settings helper reported "configured" so `login.local_off` stayed
	// honoured. That is the same total lockout this package exists to prevent,
	// reached through input handling rather than through duplicated rules.
	in = in.normalised()

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
		if in.AppID != "" && !ValidAppID(in.AppID) {
			return fmt.Errorf("oidcboot: OCTO_OIDC_PROVIDER_APP_ID %q invalid: must match %s",
				in.AppID, AppIDDescription)
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
		// The provider used to carry a stricter pattern of its own, so this shape
		// passed boot and was refused only at runtime — where LogoutURL swallows
		// the error and logout degrades to clearing the local session.
		Name: "oauth2 with an app id starting with an underscore",
		Input: KindInput{Kind: KindOAuth2, BaseURL: "https://idp.example.com",
			AppID: "_tenant", RequireEmailVerified: true},
		Env: map[string]string{
			"OCTO_OIDC_PROVIDER_KIND":     "oauth2",
			"OCTO_OIDC_PROVIDER_BASE_URL": "https://idp.example.com",
			"OCTO_OIDC_PROVIDER_APP_ID":   "_tenant",
		},
		ExpectKeyInError: "APP_ID",
	},
	{
		Name: "oauth2 with an app id over the length cap",
		Input: KindInput{Kind: KindOAuth2, BaseURL: "https://idp.example.com",
			AppID: strings.Repeat("a", 65), RequireEmailVerified: true},
		Env: map[string]string{
			"OCTO_OIDC_PROVIDER_KIND":     "oauth2",
			"OCTO_OIDC_PROVIDER_BASE_URL": "https://idp.example.com",
			"OCTO_OIDC_PROVIDER_APP_ID":   strings.Repeat("a", 65),
		},
		ExpectKeyInError: "APP_ID",
	},
	{
		// validateLogoutURL is boot-fatal in the module and had no counterpart in the
		// settings helper, whose own doc comment says it mirrors every fatal check and
		// "intentionally does NOT replicate non-fatal checks" -- these two are fatal.
		// A relative post-logout redirect therefore 404s every OIDC route while the
		// helper still reports configured, so `login.local_off` stays honoured.
		Name: "relative post-logout redirect URI",
		Input: KindInput{Kind: KindOIDC, PostLogoutRedirectURI: "/login",
			AutoLinkByEmail: true, RequireEmailVerified: true},
		Env: map[string]string{
			"OCTO_OIDC_POST_LOGOUT_REDIRECT_URI": "/login",
		},
		ExpectKeyInError: "POST_LOGOUT_REDIRECT_URI",
	},
	{
		Name: "plain-http end-session URL without the insecure opt-in",
		Input: KindInput{Kind: KindOIDC, EndSessionURL: "http://idp.example.com/logout",
			AutoLinkByEmail: true, RequireEmailVerified: true},
		Env: map[string]string{
			"OCTO_OIDC_PROVIDER_END_SESSION_URL": "http://idp.example.com/logout",
		},
		ExpectKeyInError: "END_SESSION_URL",
	},
	{
		// The two config readers parsed booleans differently: one fell through to
		// the legacy alias when the primary was present but unparseable, the other
		// returned the default. With this exact env the module refuses to boot
		// (all endpoints 404) while the settings helper reports "configured", so
		// `login.local_off` stays honoured — an SSO-only deployment with no working
		// login path, recoverable only by redeploy.
		Name: "oauth2 with a malformed primary flag contradicted by its legacy alias",
		Input: KindInput{Kind: KindOAuth2, BaseURL: "https://idp.example.com",
			AutoLinkByEmail: true, RequireEmailVerified: false},
		Env: map[string]string{
			"OCTO_OIDC_PROVIDER_KIND":                 "oauth2",
			"OCTO_OIDC_PROVIDER_BASE_URL":             "https://idp.example.com",
			"DM_OIDC_PROVIDER_AUTO_LINK_BY_EMAIL":     "true",
			"DM_OIDC_PROVIDER_REQUIRE_EMAIL_VERIFIED": "not-a-bool",
			"DM_OIDC_AEGIS_REQUIRE_EMAIL_VERIFIED":    "false",
		},
		ExpectKeyInError: "REQUIRE_EMAIL_VERIFIED",
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

// AcceptedScenario is a configuration both readers must agree is usable.
type AcceptedScenario struct {
	Name string
	Env  map[string]string
}

// AcceptedScenarios pins the **accepting** direction of the agreement.
//
// RefusedScenarios only pins one half: "if the module refuses to boot, the settings
// helper must not report configured". The other half matters just as much, and it was
// the one that broke: the rules were unified but the input normalisation was not, so a
// whitespace-only BASE_URL made the module refuse to boot while the helper reported
// configured — every OIDC route a 404, `login.local_off` still honoured, no login path
// left. RefusedScenarios could not express that, because after the fix the correct
// verdict for that input is *accept* on both sides, not refuse on both.
//
// Both consumers' pin tests walk this table. It replaces a local copy that each side
// maintained separately — the accepting direction was already tested, just not against
// a shared list, which is why the divergence had somewhere to hide.
var AcceptedScenarios = []AcceptedScenario{
	{Name: "default kind", Env: map[string]string{}},
	{Name: "explicit oidc", Env: map[string]string{"OCTO_OIDC_PROVIDER_KIND": "oidc"}},
	{Name: "oauth2 with base url", Env: map[string]string{
		"OCTO_OIDC_PROVIDER_KIND":     "oauth2",
		"OCTO_OIDC_PROVIDER_BASE_URL": "https://idp.example.com",
	}},
	{Name: "oauth2 falling back to the issuer as base url", Env: map[string]string{
		"OCTO_OIDC_PROVIDER_KIND": "oauth2",
	}},
	// Whitespace is not a configured value. Both sides must reach that conclusion
	// without either having to remember to trim — normalisation lives in ValidateKind.
	{Name: "standard kind with a whitespace-only base URL", Env: map[string]string{
		"OCTO_OIDC_PROVIDER_BASE_URL": "   ",
	}},
	{Name: "oauth2 with app id and post-logout redirect", Env: map[string]string{
		"OCTO_OIDC_PROVIDER_KIND":            "oauth2",
		"OCTO_OIDC_PROVIDER_APP_ID":          "app1",
		"OCTO_OIDC_POST_LOGOUT_REDIRECT_URI": "https://app.example.com/login",
	}},
	// Recipe B of the whitespace lockout: whitespace is not a configured base URL, so
	// under oauth2 it falls back to the issuer and the deployment boots. Both readers
	// must reach that same conclusion -- they did not, because the fallback decision ran
	// before normalisation and one side compared the untrimmed value. The decision now
	// lives in UpstreamBaseURL.
	{Name: "oauth2 with a whitespace-only base URL and a usable issuer", Env: map[string]string{
		"OCTO_OIDC_PROVIDER_KIND":     "oauth2",
		"OCTO_OIDC_PROVIDER_BASE_URL": "   ",
	}},
	{Name: "standard kind with a whitespace-only app id", Env: map[string]string{
		"OCTO_OIDC_PROVIDER_APP_ID": " \t ",
	}},
}
