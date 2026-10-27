// Package workspacecapability stores workspace-scoped Skill and connector resources.
//
// The P1 server owns the space library; experts and bindings arrive later.
// Every import is idempotent per (workspace, kind, source_id): a changed
// payload updates the stored row and bumps its snapshot, while byte-identical
// payloads are reported as reused.
package workspacecapability

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Kind string

const (
	KindSkill     Kind = "skill"
	KindConnector Kind = "connector"
)

type Source string

const (
	SourceMarket Source = "market"
	SourceLocal  Source = "local"
)

type AuthMode string

const (
	AuthModeRequired AuthMode = "required"
	AuthModeNone     AuthMode = "none"
	AuthModeUnknown  AuthMode = "unknown"
)

type AuthStatus string

const (
	AuthStatusPending     AuthStatus = "pending"
	AuthStatusAuthorized  AuthStatus = "authorized"
	AuthStatusNotRequired AuthStatus = "not_required"
)

// Auth metadata is portable; credentials never enter this service.
type Auth struct {
	Mode           AuthMode   `json:"mode"`
	Status         AuthStatus `json:"status"`
	CredentialKeys []string   `json:"credential_keys"`
}

// File is one text attachment inside a Skill package.
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Resource is one row in a workspace capability library.
type Resource struct {
	ID           string         `json:"id"`
	Kind         Kind           `json:"kind"`
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	Version      string         `json:"version"`
	Source       Source         `json:"source"`
	SourceID     string         `json:"source_id"`
	Content      string         `json:"content,omitempty"`
	Files        []File         `json:"files,omitempty"`
	Definition   map[string]any `json:"definition,omitempty"`
	Auth         *Auth          `json:"auth,omitempty"`
	BoundExperts []string       `json:"bound_experts"`
	Snapshot     string         `json:"-"`
	CreatedBy    string         `json:"-"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// ImportItem is the client-normalized payload for one resource. Definition and
// content are stored as provided; connector credentials are stripped again on
// the server so a compromised client cannot persist secrets.
type ImportItem struct {
	Kind        Kind           `json:"kind"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Version     string         `json:"version"`
	Source      Source         `json:"source"`
	SourceID    string         `json:"source_id"`
	Content     string         `json:"content"`
	Files       []File         `json:"files"`
	Definition  map[string]any `json:"definition"`
	Auth        *Auth          `json:"auth"`
}

type ImportResult struct {
	Items   []Resource `json:"items"`
	Added   int        `json:"added"`
	Reused  int        `json:"reused"`
	Updated int        `json:"updated"`
}

// ValidationError is a client input error that is safe to return to the caller.
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

func invalidf(format string, args ...any) error {
	return &ValidationError{Message: fmt.Sprintf(format, args...)}
}

const (
	maxNameRunes        = 160
	maxDescriptionRunes = 2000
	maxVersionRunes     = 64
	maxSourceIDRunes    = 200
	maxContentBytes     = 2 << 20
	maxFiles            = 200
	maxFileBytes        = 1 << 20
	maxFilesBytes       = 10 << 20
	maxCredentialKeys   = 50
	maxCredentialRunes  = 128
)

func (item ImportItem) normalized(now time.Time, createdBy string) (Resource, error) {
	item.Kind = Kind(strings.TrimSpace(string(item.Kind)))
	item.Source = Source(strings.TrimSpace(string(item.Source)))
	item.Name = strings.TrimSpace(item.Name)
	item.Description = strings.TrimSpace(item.Description)
	item.Version = strings.TrimSpace(item.Version)
	item.SourceID = strings.TrimSpace(item.SourceID)

	if item.Kind != KindSkill && item.Kind != KindConnector {
		return Resource{}, invalidf("kind must be skill or connector")
	}
	if item.Source != SourceMarket && item.Source != SourceLocal {
		return Resource{}, invalidf("source must be market or local")
	}
	if item.Name == "" || len([]rune(item.Name)) > maxNameRunes {
		return Resource{}, invalidf("name is required and must be at most 160 characters")
	}
	if len([]rune(item.Description)) > maxDescriptionRunes {
		return Resource{}, invalidf("description is too long")
	}
	if len([]rune(item.Version)) > maxVersionRunes {
		return Resource{}, invalidf("version is too long")
	}
	if item.SourceID == "" || len([]rune(item.SourceID)) > maxSourceIDRunes {
		return Resource{}, invalidf("source_id is required and must be at most 200 characters")
	}
	if len(item.Content) > maxContentBytes {
		return Resource{}, invalidf("content is too large")
	}

	definition := map[string]any(nil)
	var auth *Auth
	if item.Kind == KindConnector {
		if err := validateDefinitionURLs(item.Definition, 0); err != nil {
			return Resource{}, err
		}
		definition = sanitizeDefinition(item.Definition)
		auth = normalizeAuth(item.Auth)
	} else if item.Source == SourceLocal && strings.TrimSpace(item.Content) == "" {
		// A market skill can be referenced by id before its package is fetched;
		// a locally uploaded skill without content is not usable.
		return Resource{}, invalidf("local skill content is required")
	}

	files, err := normalizeFiles(item.Files)
	if err != nil {
		return Resource{}, err
	}

	resource := Resource{
		ID:           newID(),
		Kind:         item.Kind,
		Name:         item.Name,
		Description:  item.Description,
		Version:      item.Version,
		Source:       item.Source,
		SourceID:     item.SourceID,
		Content:      item.Content,
		Files:        files,
		Definition:   definition,
		Auth:         auth,
		BoundExperts: []string{},
		CreatedBy:    createdBy,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	snapshot, err := snapshotOf(resource)
	if err != nil {
		return Resource{}, err
	}
	resource.Snapshot = snapshot
	return resource, nil
}

func snapshotOf(resource Resource) (string, error) {
	payload := struct {
		Kind        Kind           `json:"kind"`
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Version     string         `json:"version"`
		Content     string         `json:"content"`
		Files       []File         `json:"files"`
		Definition  map[string]any `json:"definition"`
		Auth        *Auth          `json:"auth"`
	}{
		Kind:        resource.Kind,
		Name:        resource.Name,
		Description: resource.Description,
		Version:     resource.Version,
		Content:     resource.Content,
		Files:       resource.Files,
		Definition:  resource.Definition,
		Auth:        resource.Auth,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("snapshot: %w", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func validateDefinitionURLs(value any, depth int) error {
	if depth > 20 {
		return invalidf("connector definition is too deeply nested")
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if strings.EqualFold(key, "url") {
				text, ok := child.(string)
				if !ok {
					return invalidf("connector definition contains an invalid URL")
				}
				parsed, err := url.Parse(text)
				if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
					return invalidf("connector definition contains an invalid URL")
				}
			}
			if err := validateDefinitionURLs(child, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := validateDefinitionURLs(child, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func sanitizeDefinition(input map[string]any) map[string]any {
	if input == nil {
		return map[string]any{}
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return map[string]any{}
	}
	var decoded any
	if json.Unmarshal(raw, &decoded) != nil {
		return map[string]any{}
	}
	clean, ok := sanitizeValue(decoded, "", 0).(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return clean
}

var envReferencePattern = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)

var sensitiveKeyPattern = regexp.MustCompile(`(?i)(^|[_-])(auth|authorization|token|access[_-]?token|refresh[_-]?token|id[_-]?token|api[_-]?key|apikey|secret|client[_-]?secret|password|passwd|pwd|credential|credentials|private[_-]?key|bearer|signature|sig|cookie|session)([_-]|$)`)

func sanitizeValue(value any, key string, depth int) any {
	if depth > 20 {
		return nil
	}
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for childKey, childValue := range typed {
			lower := strings.ToLower(childKey)
			if lower == "env" || lower == "headers" || lower == "http_headers" {
				out[childKey] = sanitizeStringMap(childValue)
				continue
			}
			if lower == "auth" {
				out[childKey] = sanitizeAuthValue(childValue, depth+1)
				continue
			}
			if sensitiveKeyPattern.MatchString(childKey) && !isEnvReferenceKey(lower) {
				out[childKey] = sensitivePlaceholder(childValue)
				continue
			}
			out[childKey] = sanitizeValue(childValue, childKey, depth+1)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, sanitizeValue(item, key, depth+1))
		}
		return out
	case string:
		if key == "url" {
			return stripURLCredentials(typed)
		}
		return typed
	default:
		return value
	}
}

func sanitizeStringMap(value any) map[string]any {
	entries, ok := value.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	out := make(map[string]any, len(entries))
	for key, raw := range entries {
		if text, ok := raw.(string); ok && envReferencePattern.MatchString(text) {
			out[key] = text
			continue
		}
		out[key] = ""
	}
	return out
}

func sanitizeAuthValue(value any, depth int) any {
	switch typed := value.(type) {
	case string:
		return ""
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, raw := range typed {
			if nested, ok := raw.(map[string]any); ok {
				out[key] = sanitizeAuthValue(nested, depth+1)
				continue
			}
			if list, ok := raw.([]any); ok {
				out[key] = sanitizeAuthValue(list, depth+1)
				continue
			}
			out[key] = ""
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			switch item.(type) {
			case map[string]any, []any:
				out = append(out, sanitizeAuthValue(item, depth+1))
			default:
				out = append(out, "")
			}
		}
		return out
	default:
		return nil
	}
}

func sensitivePlaceholder(value any) any {
	switch value.(type) {
	case map[string]any:
		return sanitizeAuthValue(value, 0)
	case []any:
		return []any{}
	default:
		return ""
	}
}

func isEnvReferenceKey(key string) bool {
	return key == "env_vars" || key == "env_var" || strings.HasSuffix(key, "_env_var") || strings.HasSuffix(key, "_env")
}

var sensitiveQueryKeys = map[string]struct{}{
	"token": {}, "access_token": {}, "refresh_token": {}, "id_token": {},
	"api_key": {}, "apikey": {}, "key": {}, "secret": {}, "client_secret": {},
	"password": {}, "passwd": {}, "signature": {}, "sig": {}, "auth": {},
	"authorization": {}, "code": {}, "cookie": {}, "session": {},
}

func stripURLCredentials(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parsed.User = nil
	parsed.Fragment = ""
	query := parsed.Query()
	for key := range query {
		if _, sensitive := sensitiveQueryKeys[strings.ToLower(key)]; sensitive {
			query.Del(key)
			continue
		}
		if sensitiveKeyPattern.MatchString(key) {
			query.Del(key)
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func normalizeFiles(files []File) ([]File, error) {
	if len(files) > maxFiles {
		return nil, invalidf("files must contain at most %d entries", maxFiles)
	}
	out := make([]File, 0, len(files))
	total := 0
	seen := map[string]struct{}{}
	for _, file := range files {
		path := strings.TrimSpace(file.Path)
		if path == "" {
			return nil, invalidf("file path is required")
		}
		if strings.ContainsAny(path, "\\:") || strings.Contains(path, "..") || strings.HasPrefix(path, "/") || len([]rune(path)) > 300 {
			return nil, invalidf("invalid file path: %s", path)
		}
		if _, exists := seen[path]; exists {
			return nil, invalidf("duplicate file path: %s", path)
		}
		if len(file.Content) > maxFileBytes {
			return nil, invalidf("file %s exceeds %d bytes", path, maxFileBytes)
		}
		total += len(file.Content)
		if total > maxFilesBytes {
			return nil, invalidf("files exceed %d bytes in total", maxFilesBytes)
		}
		seen[path] = struct{}{}
		out = append(out, File{Path: path, Content: file.Content})
	}
	return out, nil
}

func normalizeAuth(input *Auth) *Auth {
	if input == nil {
		return &Auth{Mode: AuthModeUnknown, Status: AuthStatusPending, CredentialKeys: []string{}}
	}
	mode := AuthMode(strings.TrimSpace(string(input.Mode)))
	if mode != AuthModeRequired && mode != AuthModeNone && mode != AuthModeUnknown {
		mode = AuthModeUnknown
	}
	status := AuthStatus(strings.TrimSpace(string(input.Status)))
	if status != AuthStatusPending && status != AuthStatusAuthorized && status != AuthStatusNotRequired {
		status = AuthStatusPending
	}
	if mode == AuthModeNone {
		status = AuthStatusNotRequired
	}
	keys := make([]string, 0, len(input.CredentialKeys))
	seen := map[string]struct{}{}
	for _, key := range input.CredentialKeys {
		key = strings.TrimSpace(key)
		if key == "" || len([]rune(key)) > maxCredentialRunes {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
		if len(keys) == maxCredentialKeys {
			break
		}
	}
	return &Auth{Mode: mode, Status: status, CredentialKeys: keys}
}

func newID() string {
	var random [10]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Sprintf("cap_%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("cap_%d_%s", time.Now().UnixMilli(), hex.EncodeToString(random[:]))
}
