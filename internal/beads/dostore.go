package beads

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const doStoreTimeout = 10 * time.Second

type DoStore struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func NewDoStore(baseURL, token string) *DoStore {
	return &DoStore{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		httpClient: &http.Client{
			Timeout: doStoreTimeout,
		},
	}
}

type doTxRecorder struct {
	ops []map[string]any
}

func (t *doTxRecorder) Update(id string, opts UpdateOpts) error {
	t.ops = append(t.ops, map[string]any{"kind": "update", "id": id, "opts": opts})
	return nil
}

func (t *doTxRecorder) SetMetadataBatch(id string, kvs map[string]string) error {
	t.ops = append(t.ops, map[string]any{"kind": "set_metadata_batch", "id": id, "kvs": kvs})
	return nil
}

func (t *doTxRecorder) Close(id string) error {
	t.ops = append(t.ops, map[string]any{"kind": "close", "id": id})
	return nil
}

func (s *DoStore) Create(b Bead) (Bead, error) {
	var created Bead
	err := s.doJSON("POST", "/beads", b, &created, false)
	return created, err
}

func (s *DoStore) Get(id string) (Bead, error) {
	var b Bead
	err := s.doJSON("GET", "/beads/"+url.PathEscape(id), nil, &b, true)
	return b, err
}

func (s *DoStore) Update(id string, opts UpdateOpts) error {
	return s.doJSON("PATCH", "/beads/"+url.PathEscape(id), map[string]any{"opts": opts}, nil, false)
}

func (s *DoStore) Close(id string) error {
	return s.doJSON("POST", "/beads/"+url.PathEscape(id)+"/close", map[string]any{}, nil, false)
}

func (s *DoStore) Reopen(id string) error {
	return s.doJSON("POST", "/beads/"+url.PathEscape(id)+"/reopen", map[string]any{}, nil, false)
}

func (s *DoStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	var resp struct {
		Closed int `json:"closed"`
	}
	err := s.doJSON("POST", "/beads/close-all", map[string]any{"ids": ids, "metadata": metadata}, &resp, false)
	return resp.Closed, err
}

func (s *DoStore) List(query ListQuery) ([]Bead, error) {
	b, _ := json.Marshal(query)
	path := "/beads?query=" + url.QueryEscape(string(b))
	var out []Bead
	err := s.doJSON("GET", path, nil, &out, true)
	return out, err
}

func (s *DoStore) ListOpen(status ...string) ([]Bead, error) {
	q := ListQuery{}
	if len(status) > 0 {
		q.Status = status[0]
	}
	return s.List(q)
}

func (s *DoStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	q := ListQuery{Status: "open"}
	if len(query) > 0 && query[0].Assignee != "" {
		q.Assignee = query[0].Assignee
	}
	return s.List(q)
}

func (s *DoStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	q := ListQuery{ParentID: parentID, IncludeClosed: HasOpt(opts, IncludeClosed)}
	return s.List(q)
}

func (s *DoStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	q := ListQuery{Label: label, Limit: limit, IncludeClosed: HasOpt(opts, IncludeClosed)}
	return s.List(q)
}

func (s *DoStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	q := ListQuery{Assignee: assignee, Status: status, Limit: limit}
	return s.List(q)
}

func (s *DoStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	q := ListQuery{Metadata: filters, Limit: limit, IncludeClosed: HasOpt(opts, IncludeClosed)}
	return s.List(q)
}

func (s *DoStore) SetMetadata(id, key, value string) error {
	return s.Update(id, UpdateOpts{Metadata: map[string]string{key: value}})
}

func (s *DoStore) SetMetadataBatch(id string, kvs map[string]string) error {
	return s.doJSON("POST", "/beads/"+url.PathEscape(id)+"/metadata", map[string]any{"kvs": kvs}, nil, false)
}

func (s *DoStore) Tx(commitMsg string, fn func(tx Tx) error) error {
	if fn == nil {
		return errors.New("beads tx: nil callback")
	}
	// Reads inside Tx are not batched; only callback writes are serialized and sent to /tx.
	recorder := &doTxRecorder{}
	if err := fn(recorder); err != nil {
		return err
	}
	return s.doJSON("POST", "/tx", map[string]any{"commit_msg": commitMsg, "ops": recorder.ops}, nil, false)
}

func (s *DoStore) Delete(id string) error {
	return s.doJSON("DELETE", "/beads/"+url.PathEscape(id), nil, nil, false)
}

func (s *DoStore) Ping() error {
	return s.doJSON("GET", "/ping", nil, nil, true)
}

func (s *DoStore) DepAdd(issueID, dependsOnID, depType string) error {
	return s.doJSON("POST", "/deps", map[string]any{"issue_id": issueID, "depends_on_id": dependsOnID, "dep_type": depType}, nil, false)
}

func (s *DoStore) DepRemove(issueID, dependsOnID string) error {
	return s.doJSON("DELETE", "/deps/"+url.PathEscape(issueID)+"/"+url.PathEscape(dependsOnID), nil, nil, false)
}

func (s *DoStore) DepList(id, direction string) ([]Dep, error) {
	if direction == "" {
		direction = "down"
	}
	var deps []Dep
	err := s.doJSON("GET", "/deps/"+url.PathEscape(id)+"?direction="+url.QueryEscape(direction), nil, &deps, true)
	return deps, err
}

func (s *DoStore) doJSON(method, path string, body any, out any, retryable bool) error {
	attempts := 1
	if retryable {
		attempts = 2
	}
	for i := 0; i < attempts; i++ {
		err := s.doJSONOnce(method, path, body, out)
		if err == nil {
			return nil
		}
		if i+1 >= attempts || !strings.Contains(err.Error(), "status 5") {
			return err
		}
	}
	return nil
}

func (s *DoStore) doJSONOnce(method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, s.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("do store: %s %s status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
