package workspacecapability

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

var ErrNotFound = errors.New("workspace capability not found")

type Scope struct {
	SpaceID     string
	WorkspaceID string
}

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

const selectColumns = "id, kind, name, description, version, source, source_id, content, files_json, definition_json, auth_json, snapshot, created_by, created_at, updated_at"

func (s *Store) List(ctx context.Context, scope Scope, kind Kind, query string) ([]Resource, error) {
	args := []any{scope.SpaceID, scope.WorkspaceID}
	sqlText := "SELECT " + selectColumns + " FROM workspace_capability WHERE space_id=? AND workspace_id=?"
	if kind != "" {
		sqlText += " AND kind=?"
		args = append(args, string(kind))
	}
	if strings.TrimSpace(query) != "" {
		sqlText += " AND (name LIKE ? OR description LIKE ?)"
		like := "%" + strings.TrimSpace(query) + "%"
		args = append(args, like, like)
	}
	sqlText += " ORDER BY updated_at DESC, id DESC"
	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("list capabilities: %w", err)
	}
	defer rows.Close()
	resources := []Resource{}
	for rows.Next() {
		resource, err := scanResource(rows)
		if err != nil {
			return nil, err
		}
		resources = append(resources, resource)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.attachBindings(ctx, scope, resources); err != nil {
		return nil, err
	}
	return resources, nil
}

func (s *Store) Get(ctx context.Context, scope Scope, id string) (Resource, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+selectColumns+" FROM workspace_capability WHERE space_id=? AND workspace_id=? AND id=?", scope.SpaceID, scope.WorkspaceID, id)
	resource, err := scanResource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Resource{}, ErrNotFound
	}
	if err != nil {
		return Resource{}, fmt.Errorf("get capability: %w", err)
	}
	list := []Resource{resource}
	if err := s.attachBindings(ctx, scope, list); err != nil {
		return Resource{}, err
	}
	return list[0], nil
}

func (s *Store) Import(ctx context.Context, scope Scope, userID string, items []ImportItem) (ImportResult, error) {
	for attempt := 0; attempt < 3; attempt++ {
		result, err := s.importOnce(ctx, scope, userID, items)
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			continue
		}
		return result, err
	}
	return ImportResult{}, errors.New("import conflicted with a concurrent request; retry")
}

func (s *Store) importOnce(ctx context.Context, scope Scope, userID string, items []ImportItem) (ImportResult, error) {
	if len(items) == 0 {
		return ImportResult{Items: []Resource{}}, invalidf("items are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ImportResult{}, fmt.Errorf("begin import: %w", err)
	}
	defer tx.Rollback()

	result := ImportResult{Items: []Resource{}}
	ids := make([]string, 0, len(items))
	now := time.Now().UTC()
	for _, item := range items {
		resource, err := item.normalized(now, userID)
		if err != nil {
			return ImportResult{}, err
		}
		var existingID, existingSnapshot string
		if resource.Kind == KindSkill {
			err = tx.QueryRowContext(ctx, "SELECT id, snapshot FROM workspace_capability WHERE space_id=? AND workspace_id=? AND kind='skill' AND name=? AND version=? LIMIT 1",
				scope.SpaceID, scope.WorkspaceID, resource.Name, resource.Version).Scan(&existingID, &existingSnapshot)
			if errors.Is(err, sql.ErrNoRows) {
				err = tx.QueryRowContext(ctx, "SELECT id, snapshot FROM workspace_capability WHERE space_id=? AND workspace_id=? AND kind=? AND source=? AND source_id=? LIMIT 1",
					scope.SpaceID, scope.WorkspaceID, string(resource.Kind), string(resource.Source), resource.SourceID).Scan(&existingID, &existingSnapshot)
			}
		} else {
			err = tx.QueryRowContext(ctx, "SELECT id, snapshot FROM workspace_capability WHERE space_id=? AND workspace_id=? AND kind=? AND source=? AND source_id=? LIMIT 1",
				scope.SpaceID, scope.WorkspaceID, string(resource.Kind), string(resource.Source), resource.SourceID).Scan(&existingID, &existingSnapshot)
		}
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if err := insertTx(ctx, tx, scope, resource); err != nil {
				return ImportResult{}, err
			}
			result.Added++
			ids = append(ids, resource.ID)
		case err != nil:
			return ImportResult{}, fmt.Errorf("lookup capability: %w", err)
		case existingSnapshot == resource.Snapshot:
			result.Reused++
			ids = append(ids, existingID)
		default:
			resource.ID = existingID
			if err := updateTx(ctx, tx, scope, resource); err != nil {
				return ImportResult{}, err
			}
			result.Updated++
			ids = append(ids, existingID)
		}
	}
	if err := tx.Commit(); err != nil {
		return ImportResult{}, fmt.Errorf("commit import: %w", err)
	}
	for _, id := range ids {
		resource, err := s.Get(ctx, scope, id)
		if err != nil {
			return ImportResult{}, err
		}
		result.Items = append(result.Items, resource)
	}
	return result, nil
}

func insertTx(ctx context.Context, tx *sql.Tx, scope Scope, resource Resource) error {
	files, definition, auth, err := jsonColumns(resource)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO workspace_capability (id, space_id, workspace_id, kind, name, description, version, source, source_id, content, files_json, definition_json, auth_json, snapshot, created_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		resource.ID, scope.SpaceID, scope.WorkspaceID, string(resource.Kind), resource.Name, resource.Description, resource.Version, string(resource.Source), resource.SourceID,
		resource.Content, files, definition, auth, resource.Snapshot, resource.CreatedBy, resource.CreatedAt, resource.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert capability: %w", err)
	}
	return nil
}

func updateTx(ctx context.Context, tx *sql.Tx, scope Scope, resource Resource) error {
	files, definition, auth, err := jsonColumns(resource)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE workspace_capability SET name=?, description=?, version=?, source=?, source_id=?, content=?, files_json=?, definition_json=?, auth_json=?, snapshot=?, updated_at=? WHERE space_id=? AND workspace_id=? AND id=?",
		resource.Name, resource.Description, resource.Version, string(resource.Source), resource.SourceID, resource.Content, files, definition, auth, resource.Snapshot, resource.UpdatedAt,
		scope.SpaceID, scope.WorkspaceID, resource.ID)
	if err != nil {
		return fmt.Errorf("update capability: %w", err)
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, scope Scope, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM workspace_capability_binding WHERE space_id=? AND workspace_id=? AND capability_id=?", scope.SpaceID, scope.WorkspaceID, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM workspace_capability WHERE space_id=? AND workspace_id=? AND id=?", scope.SpaceID, scope.WorkspaceID, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) SetAuthorization(ctx context.Context, scope Scope, id string, status AuthStatus) (Resource, error) {
	resource, err := s.Get(ctx, scope, id)
	if err != nil {
		return Resource{}, err
	}
	if resource.Kind != KindConnector {
		return Resource{}, invalidf("authorization applies only to connectors")
	}
	auth := normalizeAuth(resource.Auth)
	if status == AuthStatusAuthorized && auth.Mode == AuthModeNone {
		status = AuthStatusNotRequired
	}
	if status != AuthStatusAuthorized && status != AuthStatusPending && status != AuthStatusNotRequired {
		return Resource{}, invalidf("invalid authorization status")
	}
	auth.Status = status
	raw, err := json.Marshal(auth)
	if err != nil {
		return Resource{}, err
	}
	_, err = s.db.ExecContext(ctx, "UPDATE workspace_capability SET auth_json=?, updated_at=? WHERE space_id=? AND workspace_id=? AND id=?",
		string(raw), time.Now().UTC(), scope.SpaceID, scope.WorkspaceID, id)
	if err != nil {
		return Resource{}, err
	}
	return s.Get(ctx, scope, id)
}

func (s *Store) ListBindings(ctx context.Context, scope Scope, expertID string) ([]Resource, error) {
	aliased := "c.id, c.kind, c.name, c.description, c.version, c.source, c.source_id, c.content, c.files_json, c.definition_json, c.auth_json, c.snapshot, c.created_by, c.created_at, c.updated_at"
	rows, err := s.db.QueryContext(ctx, "SELECT "+aliased+" FROM workspace_capability c JOIN workspace_capability_binding b ON b.capability_id=c.id AND b.space_id=c.space_id AND b.workspace_id=c.workspace_id WHERE c.space_id=? AND c.workspace_id=? AND b.expert_id=? ORDER BY c.updated_at DESC, c.id DESC",
		scope.SpaceID, scope.WorkspaceID, expertID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	resources := []Resource{}
	for rows.Next() {
		resource, err := scanResource(rows)
		if err != nil {
			return nil, err
		}
		resources = append(resources, resource)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.attachBindings(ctx, scope, resources); err != nil {
		return nil, err
	}
	return resources, nil
}

func (s *Store) Bind(ctx context.Context, scope Scope, expertID string, ids []string) ([]Resource, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, id := range ids {
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM workspace_capability WHERE space_id=? AND workspace_id=? AND id=?", scope.SpaceID, scope.WorkspaceID, id).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("capability %s not found: %w", id, ErrNotFound)
			}
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT IGNORE INTO workspace_capability_binding (space_id, workspace_id, expert_id, capability_id, created_at) VALUES (?,?,?,?,?)",
			scope.SpaceID, scope.WorkspaceID, expertID, id, time.Now().UTC()); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.ListBindings(ctx, scope, expertID)
}

func (s *Store) Unbind(ctx context.Context, scope Scope, expertID, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM workspace_capability_binding WHERE space_id=? AND workspace_id=? AND expert_id=? AND capability_id=?",
		scope.SpaceID, scope.WorkspaceID, expertID, id)
	return err
}

func (s *Store) attachBindings(ctx context.Context, scope Scope, resources []Resource) error {
	if len(resources) == 0 {
		return nil
	}
	ids := make([]any, 0, len(resources)+2)
	ids = append(ids, scope.SpaceID, scope.WorkspaceID)
	placeholders := make([]string, 0, len(resources))
	byID := make(map[string]*Resource, len(resources))
	for i := range resources {
		byID[resources[i].ID] = &resources[i]
		ids = append(ids, resources[i].ID)
		placeholders = append(placeholders, "?")
	}
	query := "SELECT capability_id, expert_id FROM workspace_capability_binding WHERE space_id=? AND workspace_id=? AND capability_id IN (" + strings.Join(placeholders, ",") + ") ORDER BY created_at"
	rows, err := s.db.QueryContext(ctx, query, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var capabilityID, expertID string
		if err := rows.Scan(&capabilityID, &expertID); err != nil {
			return err
		}
		if resource, ok := byID[capabilityID]; ok {
			resource.BoundExperts = append(resource.BoundExperts, expertID)
		}
	}
	return rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanResource(row rowScanner) (Resource, error) {
	var resource Resource
	var kind, source, filesJSON, definitionJSON, authJSON string
	var createdAt, updatedAt time.Time
	if err := row.Scan(&resource.ID, &kind, &resource.Name, &resource.Description, &resource.Version, &source, &resource.SourceID, &resource.Content,
		&filesJSON, &definitionJSON, &authJSON, &resource.Snapshot, &resource.CreatedBy, &createdAt, &updatedAt); err != nil {
		return Resource{}, err
	}
	resource.Kind = Kind(kind)
	resource.Source = Source(source)
	resource.CreatedAt = createdAt
	resource.UpdatedAt = updatedAt
	resource.Files = []File{}
	resource.BoundExperts = []string{}
	_ = json.Unmarshal([]byte(filesJSON), &resource.Files)
	_ = json.Unmarshal([]byte(definitionJSON), &resource.Definition)
	if authJSON != "" && authJSON != "null" {
		var auth Auth
		if json.Unmarshal([]byte(authJSON), &auth) == nil {
			resource.Auth = &auth
		}
	}
	return resource, nil
}

func jsonColumns(resource Resource) (files, definition, auth string, err error) {
	files = "[]"
	if resource.Files != nil {
		raw, marshalErr := json.Marshal(resource.Files)
		if marshalErr != nil {
			return "", "", "", marshalErr
		}
		files = string(raw)
	}
	definition = "{}"
	if resource.Definition != nil {
		raw, marshalErr := json.Marshal(resource.Definition)
		if marshalErr != nil {
			return "", "", "", marshalErr
		}
		definition = string(raw)
	}
	auth = "null"
	if resource.Auth != nil {
		raw, marshalErr := json.Marshal(resource.Auth)
		if marshalErr != nil {
			return "", "", "", marshalErr
		}
		auth = string(raw)
	}
	return files, definition, auth, nil
}
