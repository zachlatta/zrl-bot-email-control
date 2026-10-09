// Package gmailfake is an in-memory stand-in for the Gmail REST API used by
// tests. It speaks enough of the real wire protocol (JSON resources and
// multipart/related media uploads) that the official Go client works against
// it unmodified.
package gmailfake

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// Message is a stored message.
type Message struct {
	ID       string
	ThreadID string
	LabelIDs []string
	Raw      []byte
}

// Server is the fake. All fields are guarded by mu; use the accessor methods.
type Server struct {
	Address string

	mu       sync.Mutex
	server   *httptest.Server
	next     int
	messages map[string]*Message
	order    []string
	labels   map[string]*gmail.Label
	drafts   map[string]string // draft ID -> message ID
	filters  map[string]*gmail.Filter
	sent     [][]byte
	requests []string
	renders  int
}

// New starts a fake for address and registers cleanup on t.
func New(t testing.TB, address string) *Server {
	s := &Server{
		Address:  address,
		messages: map[string]*Message{},
		labels:   map[string]*gmail.Label{},
		drafts:   map[string]string{},
		filters:  map[string]*gmail.Filter{},
	}
	for _, id := range []string{"INBOX", "SENT", "DRAFT", "UNREAD", "STARRED", "IMPORTANT", "TRASH", "SPAM"} {
		s.labels[id] = &gmail.Label{Id: id, Name: id, Type: "system"}
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.server.Close)
	return s
}

// Service returns an official Gmail client pointed at the fake.
func (s *Server) Service(t testing.TB) *gmail.Service {
	service, err := gmail.NewService(t.Context(), option.WithEndpoint(s.server.URL+"/"), option.WithHTTPClient(s.server.Client()))
	if err != nil {
		t.Fatalf("gmail client: %v", err)
	}
	return service
}

// AddMessage stores a raw message (e.g. an inbound email) and returns its ID.
func (s *Server) AddMessage(raw string, labels ...string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.newID("m")
	s.messages[id] = &Message{ID: id, ThreadID: "t" + id, LabelIDs: labels, Raw: []byte(strings.ReplaceAll(raw, "\n", "\r\n"))}
	s.order = append(s.order, id)
	return id
}

// AddMessageInThread stores a raw message in an existing thread.
func (s *Server) AddMessageInThread(threadID, raw string, labels ...string) string {
	id := s.AddMessage(raw, labels...)
	s.mu.Lock()
	s.messages[id].ThreadID = threadID
	s.mu.Unlock()
	return id
}

// Message returns a copy of a stored message.
func (s *Server) Message(id string) (Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	message, ok := s.messages[id]
	if !ok {
		return Message{}, false
	}
	copied := *message
	copied.LabelIDs = slices.Clone(message.LabelIDs)
	return copied, true
}

// Sent returns the raw bytes of every message delivered via send.
func (s *Server) Sent() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sent)
}

// Requests returns "METHOD path" for every request received.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// DraftIDs returns the IDs of stored drafts.
func (s *Server) DraftIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id := range s.drafts {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// SetDraftRaw replaces a draft's content, simulating an edit made elsewhere.
func (s *Server) SetDraftRaw(draftID, raw string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages[s.drafts[draftID]].Raw = []byte(strings.ReplaceAll(raw, "\n", "\r\n"))
}

// Filters returns stored filters.
func (s *Server) Filters() []*gmail.Filter {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*gmail.Filter
	for _, filter := range s.filters {
		out = append(out, filter)
	}
	return out
}

func (s *Server) newID(prefix string) string {
	s.next++
	return fmt.Sprintf("%s%04d", prefix, s.next)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	path := r.URL.Path
	upload := strings.HasPrefix(path, "/upload/")
	path = strings.TrimPrefix(path, "/upload")
	const prefix = "/gmail/v1/users/me/"
	if !strings.HasPrefix(path, prefix) {
		http.Error(w, "unknown path", http.StatusNotFound)
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	route := r.Method + " " + parts[0]
	switch {
	case route == "GET profile":
		writeJSON(w, &gmail.Profile{EmailAddress: s.Address, MessagesTotal: int64(len(s.messages)), ThreadsTotal: int64(len(s.threads())), HistoryId: 1})
	case parts[0] == "messages":
		s.handleMessages(w, r, parts[1:], upload)
	case parts[0] == "threads":
		s.handleThreads(w, r, parts[1:])
	case parts[0] == "labels":
		s.handleLabels(w, r, parts[1:])
	case parts[0] == "drafts":
		s.handleDrafts(w, r, parts[1:], upload)
	case parts[0] == "settings" && len(parts) > 1 && parts[1] == "filters":
		s.handleFilters(w, r, parts[2:])
	default:
		http.Error(w, "unsupported fake route "+route, http.StatusNotImplemented)
	}
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request, rest []string, upload bool) {
	switch {
	case r.Method == http.MethodGet && len(rest) == 0:
		var list []*gmail.Message
		for _, id := range s.matching(r) {
			list = append(list, &gmail.Message{Id: id, ThreadId: s.messages[id].ThreadID})
		}
		writeJSON(w, &gmail.ListMessagesResponse{Messages: list, ResultSizeEstimate: int64(len(list))})
	case r.Method == http.MethodPost && len(rest) == 1 && rest[0] == "send":
		meta, raw, err := readUpload(r, upload)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id := s.newID("m")
		thread := meta.ThreadId
		if thread == "" {
			thread = "t" + id
		}
		s.messages[id] = &Message{ID: id, ThreadID: thread, LabelIDs: []string{"SENT"}, Raw: raw}
		s.order = append(s.order, id)
		s.sent = append(s.sent, raw)
		writeJSON(w, &gmail.Message{Id: id, ThreadId: thread, LabelIds: []string{"SENT"}})
	case r.Method == http.MethodPost && len(rest) == 1 && rest[0] == "batchModify":
		var request gmail.BatchModifyMessagesRequest
		if !readJSON(w, r, &request) {
			return
		}
		for _, id := range request.Ids {
			if message, ok := s.messages[id]; ok {
				message.LabelIDs = applyLabels(message.LabelIDs, request.AddLabelIds, request.RemoveLabelIds)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && len(rest) == 1 && rest[0] == "batchDelete":
		var request gmail.BatchDeleteMessagesRequest
		if !readJSON(w, r, &request) {
			return
		}
		for _, id := range request.Ids {
			s.deleteMessage(id)
		}
		w.WriteHeader(http.StatusNoContent)
	case len(rest) >= 1:
		message, ok := s.messages[rest[0]]
		if !ok {
			http.Error(w, `{"error":{"code":404,"message":"Requested entity was not found."}}`, http.StatusNotFound)
			return
		}
		switch {
		case r.Method == http.MethodGet && len(rest) == 1:
			writeJSON(w, s.render(message, r.URL.Query().Get("format")))
		case r.Method == http.MethodGet && len(rest) == 3 && rest[1] == "attachments":
			data, ok := attachmentData(message, rest[2])
			if !ok {
				http.Error(w, "no attachment", http.StatusNotFound)
				return
			}
			writeJSON(w, &gmail.MessagePartBody{AttachmentId: rest[2], Size: int64(len(data)), Data: base64.URLEncoding.EncodeToString(data)})
		case r.Method == http.MethodPost && len(rest) == 2 && rest[1] == "modify":
			var request gmail.ModifyMessageRequest
			if !readJSON(w, r, &request) {
				return
			}
			message.LabelIDs = applyLabels(message.LabelIDs, request.AddLabelIds, request.RemoveLabelIds)
			writeJSON(w, s.render(message, "minimal"))
		case r.Method == http.MethodPost && len(rest) == 2 && rest[1] == "trash":
			message.LabelIDs = applyLabels(message.LabelIDs, []string{"TRASH"}, []string{"INBOX"})
			writeJSON(w, s.render(message, "minimal"))
		case r.Method == http.MethodPost && len(rest) == 2 && rest[1] == "untrash":
			message.LabelIDs = applyLabels(message.LabelIDs, nil, []string{"TRASH"})
			writeJSON(w, s.render(message, "minimal"))
		case r.Method == http.MethodDelete && len(rest) == 1:
			s.deleteMessage(message.ID)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unsupported message route", http.StatusNotImplemented)
		}
	default:
		http.Error(w, "unsupported message route", http.StatusNotImplemented)
	}
}

func (s *Server) deleteMessage(id string) {
	delete(s.messages, id)
	s.order = slices.DeleteFunc(s.order, func(v string) bool { return v == id })
}

func (s *Server) matching(r *http.Request) []string {
	query := strings.ToLower(r.URL.Query().Get("q"))
	labels := r.URL.Query()["labelIds"]
	var ids []string
	for i := len(s.order) - 1; i >= 0; i-- {
		message := s.messages[s.order[i]]
		if slices.Contains(message.LabelIDs, "DRAFT") && !slices.Contains(labels, "DRAFT") {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(string(message.Raw)), query) {
			continue
		}
		skip := false
		for _, label := range labels {
			if !slices.Contains(message.LabelIDs, label) {
				skip = true
			}
		}
		if !skip {
			ids = append(ids, message.ID)
		}
	}
	return ids
}

func (s *Server) threads() map[string][]string {
	threads := map[string][]string{}
	for _, id := range s.order {
		message := s.messages[id]
		threads[message.ThreadID] = append(threads[message.ThreadID], id)
	}
	return threads
}

func (s *Server) handleThreads(w http.ResponseWriter, r *http.Request, rest []string) {
	threads := s.threads()
	if r.Method == http.MethodGet && len(rest) == 0 {
		seen := map[string]bool{}
		var list []*gmail.Thread
		for _, id := range s.matching(r) {
			thread := s.messages[id].ThreadID
			if !seen[thread] {
				seen[thread] = true
				list = append(list, &gmail.Thread{Id: thread})
			}
		}
		writeJSON(w, &gmail.ListThreadsResponse{Threads: list, ResultSizeEstimate: int64(len(list))})
		return
	}
	ids, ok := threads[rest[0]]
	if !ok {
		http.Error(w, "no thread", http.StatusNotFound)
		return
	}
	switch {
	case r.Method == http.MethodGet && len(rest) == 1:
		thread := &gmail.Thread{Id: rest[0]}
		for _, id := range ids {
			thread.Messages = append(thread.Messages, s.render(s.messages[id], r.URL.Query().Get("format")))
		}
		writeJSON(w, thread)
	case r.Method == http.MethodPost && len(rest) == 2:
		var add, remove []string
		switch rest[1] {
		case "modify":
			var request gmail.ModifyThreadRequest
			if !readJSON(w, r, &request) {
				return
			}
			add, remove = request.AddLabelIds, request.RemoveLabelIds
		case "trash":
			add, remove = []string{"TRASH"}, []string{"INBOX"}
		case "untrash":
			remove = []string{"TRASH"}
		}
		for _, id := range ids {
			s.messages[id].LabelIDs = applyLabels(s.messages[id].LabelIDs, add, remove)
		}
		writeJSON(w, &gmail.Thread{Id: rest[0]})
	default:
		http.Error(w, "unsupported thread route", http.StatusNotImplemented)
	}
}

func (s *Server) handleLabels(w http.ResponseWriter, r *http.Request, rest []string) {
	switch {
	case r.Method == http.MethodGet && len(rest) == 0:
		var labels []*gmail.Label
		for _, label := range s.labels {
			labels = append(labels, label)
		}
		slices.SortFunc(labels, func(a, b *gmail.Label) int { return strings.Compare(a.Id, b.Id) })
		writeJSON(w, &gmail.ListLabelsResponse{Labels: labels})
	case r.Method == http.MethodPost && len(rest) == 0:
		var label gmail.Label
		if !readJSON(w, r, &label) {
			return
		}
		for _, existing := range s.labels {
			if strings.EqualFold(existing.Name, label.Name) {
				http.Error(w, `{"error":{"code":409,"message":"Label name exists or conflicts"}}`, http.StatusConflict)
				return
			}
		}
		label.Id = s.newID("Label_")
		label.Type = "user"
		s.labels[label.Id] = &label
		writeJSON(w, &label)
	case len(rest) == 1:
		label, ok := s.labels[rest[0]]
		if !ok {
			http.Error(w, "no label", http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, label)
		case http.MethodPatch, http.MethodPut:
			var update gmail.Label
			if !readJSON(w, r, &update) {
				return
			}
			if update.Name != "" {
				label.Name = update.Name
			}
			if update.LabelListVisibility != "" {
				label.LabelListVisibility = update.LabelListVisibility
			}
			if update.MessageListVisibility != "" {
				label.MessageListVisibility = update.MessageListVisibility
			}
			if update.Color != nil {
				label.Color = update.Color
			}
			writeJSON(w, label)
		case http.MethodDelete:
			delete(s.labels, rest[0])
			for _, message := range s.messages {
				message.LabelIDs = applyLabels(message.LabelIDs, nil, []string{rest[0]})
			}
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		http.Error(w, "unsupported label route", http.StatusNotImplemented)
	}
}

func (s *Server) handleDrafts(w http.ResponseWriter, r *http.Request, rest []string, upload bool) {
	switch {
	case r.Method == http.MethodGet && len(rest) == 0:
		var drafts []*gmail.Draft
		for _, id := range s.DraftIDsLocked() {
			drafts = append(drafts, &gmail.Draft{Id: id, Message: &gmail.Message{Id: s.drafts[id], ThreadId: s.messages[s.drafts[id]].ThreadID}})
		}
		writeJSON(w, &gmail.ListDraftsResponse{Drafts: drafts, ResultSizeEstimate: int64(len(drafts))})
	case r.Method == http.MethodPost && len(rest) == 0:
		meta, raw, err := readDraftUpload(r, upload)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		messageID := s.newID("m")
		thread := ""
		if meta.Message != nil {
			thread = meta.Message.ThreadId
		}
		if thread == "" {
			thread = "t" + messageID
		}
		s.messages[messageID] = &Message{ID: messageID, ThreadID: thread, LabelIDs: []string{"DRAFT"}, Raw: raw}
		s.order = append(s.order, messageID)
		draftID := s.newID("r")
		s.drafts[draftID] = messageID
		writeJSON(w, &gmail.Draft{Id: draftID, Message: &gmail.Message{Id: messageID, ThreadId: thread}})
	case len(rest) >= 1 && rest[0] == "send":
		// The server under test must never call drafts.send: it re-sends a
		// gated copy through messages.send instead.
		http.Error(w, "drafts.send is forbidden in this fake", http.StatusForbidden)
	case len(rest) == 1:
		messageID, ok := s.drafts[rest[0]]
		if !ok {
			http.Error(w, "no draft", http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, &gmail.Draft{Id: rest[0], Message: s.render(s.messages[messageID], r.URL.Query().Get("format"))})
		case http.MethodPut:
			_, raw, err := readDraftUpload(r, upload)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			s.messages[messageID].Raw = raw
			writeJSON(w, &gmail.Draft{Id: rest[0], Message: &gmail.Message{Id: messageID, ThreadId: s.messages[messageID].ThreadID}})
		case http.MethodDelete:
			delete(s.drafts, rest[0])
			s.deleteMessage(messageID)
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		http.Error(w, "unsupported draft route", http.StatusNotImplemented)
	}
}

// DraftIDsLocked is DraftIDs for callers already holding mu.
func (s *Server) DraftIDsLocked() []string {
	var ids []string
	for id := range s.drafts {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (s *Server) handleFilters(w http.ResponseWriter, r *http.Request, rest []string) {
	switch {
	case r.Method == http.MethodGet && len(rest) == 0:
		var filters []*gmail.Filter
		for _, filter := range s.filters {
			filters = append(filters, filter)
		}
		writeJSON(w, &gmail.ListFiltersResponse{Filter: filters})
	case r.Method == http.MethodPost && len(rest) == 0:
		var filter gmail.Filter
		if !readJSON(w, r, &filter) {
			return
		}
		filter.Id = s.newID("f")
		s.filters[filter.Id] = &filter
		writeJSON(w, &filter)
	case r.Method == http.MethodDelete && len(rest) == 1:
		delete(s.filters, rest[0])
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unsupported filter route", http.StatusNotImplemented)
	}
}

func applyLabels(current, add, remove []string) []string {
	out := slices.DeleteFunc(slices.Clone(current), func(v string) bool { return slices.Contains(remove, v) })
	for _, label := range add {
		if !slices.Contains(out, label) {
			out = append(out, label)
		}
	}
	return out
}

// render converts a stored raw message into Gmail's resource representation.
func (s *Server) render(message *Message, format string) *gmail.Message {
	out := &gmail.Message{Id: message.ID, ThreadId: message.ThreadID, LabelIds: slices.Clone(message.LabelIDs), SizeEstimate: int64(len(message.Raw))}
	switch format {
	case "minimal":
		return out
	case "raw":
		out.Raw = base64.URLEncoding.EncodeToString(message.Raw)
		return out
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(message.Raw))
	if err != nil {
		return out
	}
	body, _ := io.ReadAll(parsed.Body)
	s.renders++
	counter := s.renders
	out.Payload = buildPart(message.ID, "", map[string][]string(parsed.Header), body, &counter)
	if format == "metadata" {
		stripBodies(out.Payload)
	}
	if text := firstText(out.Payload); text != "" {
		out.Snippet = text
		if len(out.Snippet) > 100 {
			out.Snippet = out.Snippet[:100]
		}
	}
	return out
}

func stripBodies(part *gmail.MessagePart) {
	part.Body = &gmail.MessagePartBody{Size: part.Body.Size}
	if len(part.Parts) > 0 {
		part.Parts = nil
	}
}

func firstText(part *gmail.MessagePart) string {
	if part.MimeType == "text/plain" && part.Body != nil && part.Body.Data != "" {
		data, _ := base64.URLEncoding.DecodeString(part.Body.Data)
		return string(data)
	}
	for _, child := range part.Parts {
		if text := firstText(child); text != "" {
			return text
		}
	}
	return ""
}

func buildPart(messageID, partID string, header map[string][]string, body []byte, counter *int) *gmail.MessagePart {
	part := &gmail.MessagePart{PartId: partID}
	var names []string
	for key := range header {
		names = append(names, key)
	}
	slices.Sort(names)
	for _, key := range names {
		for _, value := range header[key] {
			part.Headers = append(part.Headers, &gmail.MessagePartHeader{Name: key, Value: value})
		}
	}
	contentType := firstValue(header, "Content-Type")
	if contentType == "" {
		contentType = "text/plain"
	}
	mediaType, params, _ := mime.ParseMediaType(contentType)
	part.MimeType = mediaType
	if strings.HasPrefix(mediaType, "multipart/") {
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		index := 0
		for {
			child, err := reader.NextRawPart()
			if err != nil {
				break
			}
			data, _ := io.ReadAll(child)
			childID := fmt.Sprint(index)
			if partID != "" {
				childID = partID + "." + childID
			}
			part.Parts = append(part.Parts, buildPart(messageID, childID, map[string][]string(child.Header), data, counter))
			index++
		}
		part.Body = &gmail.MessagePartBody{}
		return part
	}
	decoded := decodeTransfer(firstValue(header, "Content-Transfer-Encoding"), body)
	_, dispositionParams, _ := mime.ParseMediaType(firstValue(header, "Content-Disposition"))
	filename := dispositionParams["filename"]
	if filename == "" {
		filename = params["name"]
	}
	part.Filename = filename
	if filename != "" {
		// Real Gmail returns a different attachment ID on every fetch; only
		// the prefix is stable here.
		part.Body = &gmail.MessagePartBody{AttachmentId: fmt.Sprintf("att-%s-%s-%d", messageID, partIDOrRoot(partID), *counter), Size: int64(len(decoded))}
		return part
	}
	part.Body = &gmail.MessagePartBody{Size: int64(len(decoded)), Data: base64.URLEncoding.EncodeToString(decoded)}
	return part
}

func partIDOrRoot(partID string) string {
	if partID == "" {
		return "root"
	}
	return partID
}

func attachmentData(message *Message, attachmentID string) ([]byte, bool) {
	parsed, err := mail.ReadMessage(bytes.NewReader(message.Raw))
	if err != nil {
		return nil, false
	}
	body, _ := io.ReadAll(parsed.Body)
	return findAttachment(message.ID, "", map[string][]string(parsed.Header), body, attachmentID)
}

func findAttachment(messageID, partID string, header map[string][]string, body []byte, want string) ([]byte, bool) {
	mediaType, params, _ := mime.ParseMediaType(firstValue(header, "Content-Type"))
	if strings.HasPrefix(mediaType, "multipart/") {
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		index := 0
		for {
			child, err := reader.NextRawPart()
			if err != nil {
				return nil, false
			}
			data, _ := io.ReadAll(child)
			childID := fmt.Sprint(index)
			if partID != "" {
				childID = partID + "." + childID
			}
			if found, ok := findAttachment(messageID, childID, map[string][]string(child.Header), data, want); ok {
				return found, true
			}
			index++
		}
	}
	if strings.HasPrefix(want, "att-"+messageID+"-"+partIDOrRoot(partID)+"-") {
		return decodeTransfer(firstValue(header, "Content-Transfer-Encoding"), body), true
	}
	return nil, false
}

func decodeTransfer(encoding string, body []byte) []byte {
	switch strings.ToLower(encoding) {
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(body)), ""))
		if err == nil {
			return decoded
		}
	case "quoted-printable":
		decoded, err := io.ReadAll(newQPReader(body))
		if err == nil {
			return decoded
		}
	}
	return body
}

func firstValue(header map[string][]string, key string) string {
	for name, values := range header {
		if strings.EqualFold(name, key) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func readUpload(r *http.Request, upload bool) (*gmail.Message, []byte, error) {
	if !upload {
		var message gmail.Message
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			return nil, nil, err
		}
		raw, err := base64.URLEncoding.DecodeString(message.Raw)
		return &message, raw, err
	}
	metaJSON, raw, err := readMultipartRelated(r)
	if err != nil {
		return nil, nil, err
	}
	var meta gmail.Message
	if len(metaJSON) > 0 {
		if err := json.Unmarshal(metaJSON, &meta); err != nil {
			return nil, nil, err
		}
	}
	return &meta, raw, nil
}

func readDraftUpload(r *http.Request, upload bool) (*gmail.Draft, []byte, error) {
	if !upload {
		var draft gmail.Draft
		if err := json.NewDecoder(r.Body).Decode(&draft); err != nil {
			return nil, nil, err
		}
		if draft.Message == nil {
			return nil, nil, fmt.Errorf("draft has no message")
		}
		raw, err := base64.URLEncoding.DecodeString(draft.Message.Raw)
		return &draft, raw, err
	}
	metaJSON, raw, err := readMultipartRelated(r)
	if err != nil {
		return nil, nil, err
	}
	var meta gmail.Draft
	if len(metaJSON) > 0 {
		if err := json.Unmarshal(metaJSON, &meta); err != nil {
			return nil, nil, err
		}
	}
	return &meta, raw, nil
}

func readMultipartRelated(r *http.Request) ([]byte, []byte, error) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, nil, err
	}
	if mediaType != "multipart/related" {
		raw, err := io.ReadAll(r.Body)
		return nil, raw, err
	}
	reader := multipart.NewReader(r.Body, params["boundary"])
	var chunks [][]byte
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		data, _ := io.ReadAll(part)
		chunks = append(chunks, data)
	}
	if len(chunks) != 2 {
		return nil, nil, fmt.Errorf("expected metadata and media parts, got %d", len(chunks))
	}
	return chunks[0], chunks[1], nil
}

func readJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	if err := json.NewDecoder(r.Body).Decode(value); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

// URL is the fake's base URL.
func (s *Server) URL() string { return s.server.URL }
