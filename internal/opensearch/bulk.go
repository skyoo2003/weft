// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
)

// The three actions _bulk understands, and the one it names rather than ignores.
//
// `update` is refused per request instead of implemented: a partial update means
// reading the stored body, merging, and re-indexing the result, and the merge rule
// — does a null delete the field, does an array replace or append — is a decision
// this server has no reason to make before somebody needs it. A 400 naming the
// action is what a client can act on; indexing the fragment as though it were a
// whole document would replace the rest of it without saying so.
const (
	opIndex  = "index"
	opCreate = "create"
	opDelete = "delete"
	opUpdate = "update"
)

// bulkAction is one action line of an NDJSON batch, resolved against its index.
type bulkAction struct {
	op   string
	id   string
	doc  engine.Document
	body json.RawMessage

	// created is filled in by apply and read back when the item is rendered.
	created bool
}

// apply performs one action. It runs on the index's writer goroutine — see
// Index.Apply — so the Resolve and the write beside it cannot be interleaved.
func (a *bulkAction) apply(x *Index) error {
	if a.op == opDelete {
		if !x.deleteLocked(a.id) {
			return &apiError{status: http.StatusNotFound, kind: "document_missing_exception",
				reason: fmt.Sprintf("no document with id %q in %q", a.id, x.Name())}
		}
		return nil
	}
	if a.op == opCreate {
		if _, exists := x.Engine().Resolve(a.id); exists {
			return &apiError{status: http.StatusConflict, kind: "version_conflict_engine_exception",
				reason: fmt.Sprintf("a document with id %q already exists in %q; use index to replace it", a.id, x.Name())}
		}
	}
	created, err := x.putLocked(a.doc, a.body)
	a.created = created
	return err
}

// bulkItem is one action's answer, in the shape a client reads it.
//
// Keyed by the verb — items[i] is {"index": {...}} — which is OpenSearch's shape
// and is what lets a client tell which action produced which result without
// counting positions.
type bulkItem struct {
	Index   string     `json:"_index"`
	ID      string     `json:"_id"`
	Version int        `json:"_version,omitempty"`
	Result  string     `json:"result,omitempty"`
	Shards  *shardInfo `json:"_shards,omitempty"`
	Status  int        `json:"status"`
	Error   *bulkError `json:"error,omitempty"`
}

type bulkError struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

// bulkMeta is what an action line carries.
type bulkMeta struct {
	Index string `json:"_index"`
	ID    string `json:"_id"`
}

// bulk answers POST /_bulk and POST /{index}/_bulk.
//
// The whole body is parsed before anything is written. A batch is a unit at parse
// time because an action line without its source line is not a partial batch — it
// is a batch whose remaining pairs are all off by one, and applying the prefix
// would write documents under the wrong ids. Parse errors therefore fail the
// request; per-*item* errors — a missing index, a body that will not map, a create
// over an id that exists — are reported per item, which is the thing a client's
// bulk helper retries from.
func (s *Server) bulk(w http.ResponseWriter, r *http.Request) error {
	start := time.Now()

	body, err := readAll(r)
	if err != nil {
		return err
	}
	commit, apiErr := parseRefresh(r)
	if apiErr != nil {
		return apiErr
	}
	actions, targets, apiErr := parseBulk(body, r.PathValue("index"))
	if apiErr != nil {
		return apiErr
	}
	if len(actions) == 0 {
		return badRequest(kindIllegalArgument,
			"the bulk body holds no action: it is newline-delimited JSON, one action line and — except for "+
				"delete — one source line after it")
	}

	items := make([]map[string]bulkItem, len(actions))
	// Every item is resolved and mapped before any of them is written, so a batch
	// naming a missing index reports that against the item rather than against the
	// request, and a body that will not map never reaches the writer.
	byIndex := map[string][]int{}
	for i, a := range actions {
		name := targets[i]
		x, ok := s.reg.Get(name)
		if !ok {
			items[i] = failedItem(a, name, http.StatusNotFound, "index_not_found_exception",
				fmt.Sprintf("no such index [%s]", name))
			continue
		}
		if a.op != opDelete {
			d, err := documentFrom(a.id, a.body, x.Mapping())
			if err != nil {
				items[i] = failedItem(a, name, http.StatusBadRequest, "mapper_parsing_exception", err.Error())
				continue
			}
			a.doc = d
		}
		byIndex[name] = append(byIndex[name], i)
	}

	for name, positions := range byIndex {
		x, ok := s.reg.Get(name)
		if !ok {
			// Dropped between the resolve above and here. Reported per item, for
			// the reason a missing index is: the rest of the batch still landed.
			for _, i := range positions {
				items[i] = failedItem(actions[i], name, http.StatusNotFound, "index_not_found_exception",
					fmt.Sprintf("index [%s] was deleted while this batch was running", name))
			}
			continue
		}
		batch := make([]*bulkAction, 0, len(positions))
		for _, i := range positions {
			batch = append(batch, actions[i])
		}
		// One hold of the writer for this index's whole share of the batch, which
		// is what D5 gave the writer goroutine to make possible.
		for j, err := range x.Apply(batch) {
			items[positions[j]] = itemFor(batch[j], name, err)
		}
		if commit {
			if err := x.Commit(r.Context()); err != nil {
				return fmt.Errorf("commit %q after a bulk request: %w", name, err)
			}
		}
	}

	failed := false
	for _, item := range items {
		for _, v := range item {
			failed = failed || v.Error != nil
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"took": time.Since(start).Milliseconds(),
		// Reported rather than assumed. A client's bulk helper branches on this
		// before it looks at a single item.
		"errors": failed,
		"items":  items,
	})
	return nil
}

// parseBulk cuts the NDJSON body into actions.
//
// targets is one index name per action, parallel to actions, because the name may
// come from the path or from the action line and the item has to echo back
// whichever it was.
func parseBulk(body []byte, defaultIndex string) (actions []*bulkAction, targets []string, err *apiError) {
	lines := bytes.Split(body, []byte("\n"))
	for i := 0; i < len(lines); i++ {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var envelope map[string]json.RawMessage
		if e := json.Unmarshal(line, &envelope); e != nil {
			return nil, nil, badRequest(kindIllegalArgument,
				"the action on line %d did not decode as JSON: %v", i+1, e)
		}
		if len(envelope) != 1 {
			return nil, nil, badRequest(kindIllegalArgument,
				"the action on line %d names %d verbs and an action names exactly one", i+1, len(envelope))
		}

		for op, raw := range envelope {
			switch op {
			case opIndex, opCreate, opDelete:
			case opUpdate:
				return nil, nil, badRequest(kindIllegalArgument,
					"update is not supported (line %d): a partial update means reading the stored body and "+
						"merging into it, and the merge rule — whether null deletes a field, whether an "+
						"array replaces or appends — is not one this server should invent. Send the whole "+
						"document with index", i+1)
			default:
				return nil, nil, badRequest(kindIllegalArgument,
					"%q on line %d is not a bulk action; this server speaks %s, %s and %s",
					op, i+1, opIndex, opCreate, opDelete)
			}

			var meta bulkMeta
			if e := json.Unmarshal(raw, &meta); e != nil {
				return nil, nil, badRequest(kindIllegalArgument,
					"the %s action on line %d did not decode: %v", op, i+1, e)
			}
			name := meta.Index
			if name == "" {
				name = defaultIndex
			}
			if name == "" {
				return nil, nil, badRequest(kindIllegalArgument,
					"the %s action on line %d names no index and the request path names none either", op, i+1)
			}
			if meta.ID == "" {
				return nil, nil, badRequest(kindIllegalArgument,
					"the %s action on line %d has no _id: weft keys documents by the id you give them and "+
						"generates none", op, i+1)
			}

			a := &bulkAction{op: op, id: meta.ID}
			if op != opDelete {
				// The source line. Its absence is a parse failure and not an empty
				// document: every pair after it would be read off by one.
				i++
				if i >= len(lines) || len(bytes.TrimSpace(lines[i])) == 0 {
					return nil, nil, badRequest(kindIllegalArgument,
						"the %s action on line %d has no source line after it, so every action after it "+
							"would be read against the wrong document", op, i)
				}
				a.body = json.RawMessage(bytes.Clone(bytes.TrimSpace(lines[i])))
			}
			actions = append(actions, a)
			targets = append(targets, name)
		}
	}
	return actions, targets, nil
}

// itemFor renders one applied action's result.
func itemFor(a *bulkAction, index string, err error) map[string]bulkItem {
	if err != nil {
		status, kind := http.StatusInternalServerError, "internal_server_error"
		var api *apiError
		if errors.As(err, &api) {
			status, kind = api.status, api.kind
		}
		return failedItem(a, index, status, kind, err.Error())
	}
	result, status := resultUpdated, http.StatusOK
	switch {
	case a.op == opDelete:
		result = resultDeleted
	case a.created:
		result, status = resultCreated, http.StatusCreated
	}
	shards := oneShard
	return map[string]bulkItem{a.op: {
		Index: index, ID: a.id, Version: 1, Result: result, Shards: &shards, Status: status,
	}}
}

func failedItem(a *bulkAction, index string, status int, kind, reason string) map[string]bulkItem {
	return map[string]bulkItem{a.op: {
		Index:  index,
		ID:     a.id,
		Status: status,
		Error:  &bulkError{Type: kind, Reason: reason},
	}}
}
