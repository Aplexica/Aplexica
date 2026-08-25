package kilo

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

type kiloSessionBundle struct {
	Session  kiloSession
	Messages []kiloMessageBundle
}

type kiloMessageBundle struct {
	Message kiloMessage
	Parts   []kiloPart
}

type kiloSession struct {
	ID             string
	ProjectID      string
	ParentID       string
	Slug           string
	Directory      string
	Path           string
	Title          string
	Version        string
	ShareURL       string
	Revert         json.RawMessage
	Permission     json.RawMessage
	WorkspaceID    string
	Agent          string
	Model          json.RawMessage
	TimeCreated    int64
	TimeUpdated    int64
	TimeCompacting int64
	TimeArchived   int64
}

type kiloMessage struct {
	ID          string
	SessionID   string
	Role        string
	TimeCreated int64
	TimeUpdated int64
	Data        json.RawMessage
}

type kiloPart struct {
	ID          string
	MessageID   string
	SessionID   string
	Type        string
	TimeCreated int64
	TimeUpdated int64
	Data        json.RawMessage
}

func listKiloSessionBundles(dbPath string, sinceMillis int64) ([]kiloSessionBundle, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	sessions, err := listKiloSessions(db, sinceMillis)
	if err != nil {
		return nil, err
	}
	out := make([]kiloSessionBundle, 0, len(sessions))
	for _, s := range sessions {
		messages, err := listKiloMessages(db, s.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, kiloSessionBundle{
			Session:  s,
			Messages: messages,
		})
	}
	return out, nil
}

func listKiloSessions(db *sql.DB, sinceMillis int64) ([]kiloSession, error) {
	rows, err := db.Query(`SELECT
		id, project_id, parent_id, slug, directory, path, title, version,
		share_url, revert, permission, workspace_id, agent, model,
		time_created, time_updated, time_compacting, time_archived
		FROM session
		WHERE COALESCE(time_updated, time_created, 0) > ?
		ORDER BY time_created, id`, sinceMillis)
	if err != nil {
		return nil, fmt.Errorf("kilo: query sessions: %w", err)
	}
	defer rows.Close()

	var out []kiloSession
	for rows.Next() {
		var s kiloSession
		var parentID, path, shareURL, revert, permission, workspaceID, agent, model sql.NullString
		var timeCompacting, timeArchived sql.NullInt64
		if err := rows.Scan(
			&s.ID, &s.ProjectID, &parentID, &s.Slug, &s.Directory, &path, &s.Title, &s.Version,
			&shareURL, &revert, &permission, &workspaceID, &agent, &model,
			&s.TimeCreated, &s.TimeUpdated, &timeCompacting, &timeArchived,
		); err != nil {
			return nil, fmt.Errorf("kilo: scan session: %w", err)
		}
		s.ParentID = nullText(parentID)
		s.Path = nullText(path)
		s.ShareURL = nullText(shareURL)
		s.Revert = nullJSON(revert)
		s.Permission = nullJSON(permission)
		s.WorkspaceID = nullText(workspaceID)
		s.Agent = nullText(agent)
		s.Model = nullJSON(model)
		s.TimeCompacting = nullInt(timeCompacting)
		s.TimeArchived = nullInt(timeArchived)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("kilo: read sessions: %w", err)
	}
	return out, nil
}

func listKiloMessages(db *sql.DB, sessionID string) ([]kiloMessageBundle, error) {
	rows, err := db.Query(`SELECT id, session_id, time_created, time_updated, data
		FROM message
		WHERE session_id = ?
		ORDER BY time_created, id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("kilo: query messages for session %s: %w", sessionID, err)
	}
	defer rows.Close()

	var out []kiloMessageBundle
	for rows.Next() {
		var m kiloMessage
		var data string
		if err := rows.Scan(&m.ID, &m.SessionID, &m.TimeCreated, &m.TimeUpdated, &data); err != nil {
			return nil, fmt.Errorf("kilo: scan message: %w", err)
		}
		m.Data = json.RawMessage(data)
		role, err := kiloRawString(m.Data, "role")
		if err != nil {
			return nil, fmt.Errorf("kilo: parse message %s role: %w", m.ID, err)
		}
		m.Role = role
		parts, err := listKiloParts(db, m.SessionID, m.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, kiloMessageBundle{Message: m, Parts: parts})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("kilo: read messages: %w", err)
	}
	return out, nil
}

func listKiloParts(db *sql.DB, sessionID, messageID string) ([]kiloPart, error) {
	rows, err := db.Query(`SELECT id, message_id, session_id, time_created, time_updated, data
		FROM part
		WHERE session_id = ? AND message_id = ?
		ORDER BY id`, sessionID, messageID)
	if err != nil {
		return nil, fmt.Errorf("kilo: query parts for message %s: %w", messageID, err)
	}
	defer rows.Close()

	var out []kiloPart
	for rows.Next() {
		var p kiloPart
		var data string
		if err := rows.Scan(&p.ID, &p.MessageID, &p.SessionID, &p.TimeCreated, &p.TimeUpdated, &data); err != nil {
			return nil, fmt.Errorf("kilo: scan part: %w", err)
		}
		p.Data = json.RawMessage(data)
		typ, err := kiloRawString(p.Data, "type")
		if err != nil {
			return nil, fmt.Errorf("kilo: parse part %s type: %w", p.ID, err)
		}
		p.Type = typ
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("kilo: read parts: %w", err)
	}
	return out, nil
}

func kiloRawString(raw json.RawMessage, key string) (string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", err
	}
	var value string
	if err := json.Unmarshal(obj[key], &value); err != nil {
		return "", err
	}
	return value, nil
}

func nullText(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

func nullJSON(v sql.NullString) json.RawMessage {
	if !v.Valid || v.String == "" {
		return nil
	}
	return json.RawMessage(v.String)
}

func nullInt(v sql.NullInt64) int64 {
	if !v.Valid {
		return 0
	}
	return v.Int64
}
