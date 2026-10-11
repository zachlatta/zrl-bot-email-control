// Package mailbox gives agents full control of one Gmail mailbox. Reads and
// organization are unrestricted; every outbound path funnels through gate.go.
package mailbox

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"mime"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/zachlatta/zrl-bot-email-control/internal/recipients"
	"golang.org/x/sync/errgroup"
	"golang.org/x/text/encoding/htmlindex"
	"google.golang.org/api/gmail/v1"
)

const (
	defaultMaxBodyChars       = 100_000
	defaultMaxAttachmentBytes = 20 << 20
	defaultSearchResults      = 25
	maxSearchResults          = 100
	// fetchConcurrency bounds parallel Gmail reads when a listing has to
	// fetch every message, well under Gmail's per-user rate limit.
	fetchConcurrency = 10
)

// ErrNotConnected means the mailbox has not completed Google authorization.
var ErrNotConnected = errors.New("mailbox is not connected to Gmail yet; an admin must open /admin and connect it")

// Config wires a Service.
type Config struct {
	Address   string
	Allowlist *recipients.Allowlist
	// Gmail returns an authorized client for the mailbox.
	Gmail func(context.Context) (*gmail.Service, error)
}

// Service implements every mailbox operation exposed to agents.
type Service struct {
	cfg Config
}

func New(cfg Config) *Service {
	return &Service{cfg: cfg}
}

// Address is the bot mailbox address.
func (s *Service) Address() string { return s.cfg.Address }

// AllowedRecipients lists the outbound allowlist.
func (s *Service) AllowedRecipients() []string { return s.cfg.Allowlist.Addresses() }

func (s *Service) gmail(ctx context.Context) (*gmail.Service, error) {
	return s.cfg.Gmail(ctx)
}

// Profile describes the mailbox.
type Profile struct {
	EmailAddress      string   `json:"email_address"`
	MessagesTotal     int64    `json:"messages_total"`
	ThreadsTotal      int64    `json:"threads_total"`
	AllowedRecipients []string `json:"allowed_recipients" jsonschema:"The only addresses this server will ever send mail to."`
}

func (s *Service) Profile(ctx context.Context) (Profile, error) {
	service, err := s.gmail(ctx)
	if err != nil {
		return Profile{}, err
	}
	profile, err := service.Users.GetProfile("me").Context(ctx).Do()
	if err != nil {
		return Profile{}, err
	}
	return Profile{EmailAddress: profile.EmailAddress, MessagesTotal: profile.MessagesTotal, ThreadsTotal: profile.ThreadsTotal, AllowedRecipients: s.AllowedRecipients()}, nil
}

// SearchInput selects messages or threads with Gmail search syntax.
type SearchInput struct {
	Query            string   `json:"query,omitempty" jsonschema:"Gmail search syntax, e.g. 'in:inbox is:unread from:zach@hackclub.com has:attachment newer_than:7d'."`
	LabelIDs         []string `json:"label_ids,omitempty" jsonschema:"Only return results carrying all of these label IDs or names."`
	MaxResults       int64    `json:"max_results,omitempty" jsonschema:"Results per page, default 25, max 100."`
	PageToken        string   `json:"page_token,omitempty" jsonschema:"next_page_token from a previous call."`
	IncludeSpamTrash bool     `json:"include_spam_trash,omitempty"`
}

// MessageSummary is a compact message listing.
type MessageSummary struct {
	ID             string   `json:"id"`
	ThreadID       string   `json:"thread_id"`
	From           string   `json:"from"`
	To             string   `json:"to,omitempty"`
	Cc             string   `json:"cc,omitempty"`
	Subject        string   `json:"subject"`
	Date           string   `json:"date"`
	Snippet        string   `json:"snippet"`
	LabelIDs       []string `json:"label_ids"`
	HasAttachments bool     `json:"has_attachments"`
}

type SearchResult struct {
	Messages           []MessageSummary `json:"messages"`
	NextPageToken      string           `json:"next_page_token,omitempty"`
	ResultSizeEstimate int64            `json:"result_size_estimate"`
}

func clampResults(value int64) int64 {
	if value <= 0 {
		return defaultSearchResults
	}
	return min(value, maxSearchResults)
}

func (s *Service) Search(ctx context.Context, input SearchInput) (SearchResult, error) {
	service, err := s.gmail(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	labelIDs, err := s.resolveLabels(ctx, service, input.LabelIDs)
	if err != nil {
		return SearchResult{}, err
	}
	call := service.Users.Messages.List("me").Q(input.Query).MaxResults(clampResults(input.MaxResults)).IncludeSpamTrash(input.IncludeSpamTrash)
	if len(labelIDs) > 0 {
		call = call.LabelIds(labelIDs...)
	}
	if input.PageToken != "" {
		call = call.PageToken(input.PageToken)
	}
	response, err := call.Context(ctx).Do()
	if err != nil {
		return SearchResult{}, err
	}
	result := SearchResult{NextPageToken: response.NextPageToken, ResultSizeEstimate: response.ResultSizeEstimate, Messages: make([]MessageSummary, len(response.Messages))}
	err = forEachConcurrently(ctx, len(response.Messages), func(ctx context.Context, i int) error {
		id := response.Messages[i].Id
		message, err := service.Users.Messages.Get("me", id).Format("full").Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("get message %s: %w", id, err)
		}
		result.Messages[i] = summarize(message)
		return nil
	})
	if err != nil {
		return SearchResult{}, err
	}
	return result, nil
}

// forEachConcurrently calls fetch for 0..n-1 with bounded parallelism and
// returns the first error. Callers write results by index, so order is kept.
func forEachConcurrently(ctx context.Context, n int, fetch func(context.Context, int) error) error {
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(fetchConcurrency)
	for i := range n {
		group.Go(func() error { return fetch(ctx, i) })
	}
	return group.Wait()
}

// ThreadSummary is a compact thread listing.
type ThreadSummary struct {
	ID           string   `json:"id"`
	Subject      string   `json:"subject"`
	Snippet      string   `json:"snippet"`
	MessageCount int      `json:"message_count"`
	Participants []string `json:"participants"`
	LastDate     string   `json:"last_date"`
	LabelIDs     []string `json:"label_ids"`
}

type SearchThreadsResult struct {
	Threads            []ThreadSummary `json:"threads"`
	NextPageToken      string          `json:"next_page_token,omitempty"`
	ResultSizeEstimate int64           `json:"result_size_estimate"`
}

func (s *Service) SearchThreads(ctx context.Context, input SearchInput) (SearchThreadsResult, error) {
	service, err := s.gmail(ctx)
	if err != nil {
		return SearchThreadsResult{}, err
	}
	labelIDs, err := s.resolveLabels(ctx, service, input.LabelIDs)
	if err != nil {
		return SearchThreadsResult{}, err
	}
	call := service.Users.Threads.List("me").Q(input.Query).MaxResults(clampResults(input.MaxResults)).IncludeSpamTrash(input.IncludeSpamTrash)
	if len(labelIDs) > 0 {
		call = call.LabelIds(labelIDs...)
	}
	if input.PageToken != "" {
		call = call.PageToken(input.PageToken)
	}
	response, err := call.Context(ctx).Do()
	if err != nil {
		return SearchThreadsResult{}, err
	}
	result := SearchThreadsResult{NextPageToken: response.NextPageToken, ResultSizeEstimate: response.ResultSizeEstimate, Threads: make([]ThreadSummary, len(response.Threads))}
	err = forEachConcurrently(ctx, len(response.Threads), func(ctx context.Context, i int) error {
		id := response.Threads[i].Id
		thread, err := service.Users.Threads.Get("me", id).Format("metadata").MetadataHeaders("From", "To", "Cc", "Subject", "Date").Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("get thread %s: %w", id, err)
		}
		summary := ThreadSummary{ID: thread.Id, MessageCount: len(thread.Messages), Participants: []string{}, LabelIDs: []string{}}
		for j, message := range thread.Messages {
			headers := headerMap(message.Payload)
			if j == 0 {
				summary.Subject = decodeHeader(headers["subject"])
			}
			summary.Snippet = html.UnescapeString(message.Snippet)
			summary.LastDate = headers["date"]
			from := decodeHeader(headers["from"])
			if from != "" && !slices.Contains(summary.Participants, from) {
				summary.Participants = append(summary.Participants, from)
			}
			for _, label := range message.LabelIds {
				if !slices.Contains(summary.LabelIDs, label) {
					summary.LabelIDs = append(summary.LabelIDs, label)
				}
			}
		}
		result.Threads[i] = summary
		return nil
	})
	if err != nil {
		return SearchThreadsResult{}, err
	}
	return result, nil
}

// GetOptions controls how much of a message is returned.
type GetOptions struct {
	IncludeHTML    bool `json:"include_html,omitempty" jsonschema:"Also return the HTML body."`
	IncludeHeaders bool `json:"include_headers,omitempty" jsonschema:"Also return every raw header."`
	MaxBodyChars   int  `json:"max_body_chars,omitempty" jsonschema:"Truncate each body to this many characters (default 100000)."`
}

// AttachmentInfo describes one attachment of a message.
type AttachmentInfo struct {
	AttachmentID string `json:"attachment_id"`
	PartID       string `json:"part_id"`
	Filename     string `json:"filename"`
	MimeType     string `json:"mime_type"`
	Size         int64  `json:"size"`
}

// Header is one raw header.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// MessageDetail is a fully decoded message.
type MessageDetail struct {
	MessageSummary
	Bcc             string           `json:"bcc,omitempty"`
	ReplyTo         string           `json:"reply_to,omitempty"`
	MessageIDHeader string           `json:"message_id_header,omitempty"`
	InReplyTo       string           `json:"in_reply_to,omitempty"`
	References      string           `json:"references,omitempty"`
	Text            string           `json:"text"`
	HTML            string           `json:"html,omitempty"`
	Truncated       bool             `json:"truncated,omitempty"`
	QuotedTrimmed   bool             `json:"quoted_trimmed,omitempty" jsonschema:"Quoted earlier messages were cut from the end of text; pass include_quoted to keep them."`
	Attachments     []AttachmentInfo `json:"attachments"`
	Headers         []Header         `json:"headers,omitempty"`
}

func (s *Service) GetMessage(ctx context.Context, id string, options GetOptions) (MessageDetail, error) {
	service, err := s.gmail(ctx)
	if err != nil {
		return MessageDetail{}, err
	}
	message, err := service.Users.Messages.Get("me", id).Format("full").Context(ctx).Do()
	if err != nil {
		return MessageDetail{}, err
	}
	return detail(message, options), nil
}

// ThreadOptions controls get_thread output.
type ThreadOptions struct {
	GetOptions
	IncludeQuoted bool `json:"include_quoted,omitempty" jsonschema:"Keep the quoted earlier messages ('> ...' lines and the 'On ... wrote:' line) at the end of each reply's text. By default they are cut, because the thread already contains those messages. The first message is never cut."`
}

// ThreadDetail is every message of a thread, oldest first.
type ThreadDetail struct {
	ID       string          `json:"id"`
	Messages []MessageDetail `json:"messages"`
}

func (s *Service) GetThread(ctx context.Context, id string, options ThreadOptions) (ThreadDetail, error) {
	service, err := s.gmail(ctx)
	if err != nil {
		return ThreadDetail{}, err
	}
	thread, err := service.Users.Threads.Get("me", id).Format("full").Context(ctx).Do()
	if err != nil {
		return ThreadDetail{}, err
	}
	out := ThreadDetail{ID: thread.Id, Messages: []MessageDetail{}}
	for i, message := range thread.Messages {
		message := detail(message, options.GetOptions)
		// The first message may quote mail that is not in this thread (Zach
		// copying the bot into an existing conversation), so it is kept whole.
		if i > 0 && !options.IncludeQuoted {
			message.Text, message.QuotedTrimmed = trimQuoted(message.Text)
		}
		out.Messages = append(out.Messages, message)
	}
	return out, nil
}

// attributionLine matches the "On <date> <someone> wrote:" line that
// introduces a quote, which mail clients often wrap onto two lines.
var attributionLine = regexp.MustCompile(`(?is)^on\s.{1,300}\swrote:$`)

// trimQuoted removes a bottom-posted quote: the trailing run of "> " lines
// and the attribution line just above it. Inline replies (new text between
// quoted lines) and messages that are entirely quoted are left unchanged.
func trimQuoted(text string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	start := end
	for start > 0 && (strings.HasPrefix(lines[start-1], ">") || strings.TrimSpace(lines[start-1]) == "") {
		start--
	}
	if start == end || !slices.ContainsFunc(lines[start:end], func(line string) bool { return strings.HasPrefix(line, ">") }) {
		return text, false
	}
	// Drop the attribution, which may span up to two lines.
	for span := 1; span <= 2 && start-span >= 0; span++ {
		if attributionLine.MatchString(strings.TrimSpace(strings.Join(lines[start-span:start], " "))) {
			start -= span
			break
		}
	}
	kept := strings.TrimRight(strings.Join(lines[:start], "\n"), " \t\n")
	if strings.TrimSpace(kept) == "" {
		return text, false
	}
	return kept, true
}

// AttachmentData is attachment content.
type AttachmentData struct {
	Filename   string `json:"filename"`
	MimeType   string `json:"mime_type"`
	Size       int64  `json:"size" jsonschema:"Size of the whole attachment in bytes."`
	Offset     int64  `json:"offset" jsonschema:"Byte offset of data_base64 within the attachment."`
	Length     int64  `json:"length" jsonschema:"Number of bytes in data_base64."`
	NextOffset int64  `json:"next_offset,omitempty" jsonschema:"Set when more bytes remain: call again with this offset."`
	DataBase64 string `json:"data_base64" jsonschema:"Standard base64 of the bytes from offset to offset+length."`
	Text       string `json:"text,omitempty" jsonschema:"Decoded text for text/* attachments (whole file only)."`
}

// Complete reports whether the data is the whole attachment.
func (a AttachmentData) Complete() bool { return a.Offset == 0 && a.Length == a.Size }

// AttachmentRequest selects an attachment, or a byte range of one.
type AttachmentRequest struct {
	MessageID    string `json:"message_id"`
	AttachmentID string `json:"attachment_id" jsonschema:"attachment_id from get_message."`
	Offset       int64  `json:"offset,omitempty" jsonschema:"First byte to return (default 0). Use with length to download a large file in pieces."`
	Length       int64  `json:"length,omitempty" jsonschema:"Bytes to return from offset (default: to the end). Through a proxy such as PDW, use 5242880 (5 MiB) pieces for files over about 10 MiB, then join them."`
	MaxBytes     int64  `json:"max_bytes,omitempty" jsonschema:"Refuse to return more than this many bytes in one call (default 20 MiB)."`
}

func (s *Service) GetAttachment(ctx context.Context, request AttachmentRequest) (AttachmentData, error) {
	maxBytes := request.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxAttachmentBytes
	}
	if request.Offset < 0 || request.Length < 0 {
		return AttachmentData{}, errors.New("offset and length must not be negative")
	}
	service, err := s.gmail(ctx)
	if err != nil {
		return AttachmentData{}, err
	}
	info, data, err := s.fetchAttachment(ctx, service, request.MessageID, request.AttachmentID, 0)
	if err != nil {
		return AttachmentData{}, err
	}
	size := int64(len(data))
	if request.Offset >= size && !(request.Offset == 0 && size == 0) {
		return AttachmentData{}, fmt.Errorf("offset %d is past the end of attachment %q (%d bytes)", request.Offset, info.Filename, size)
	}
	end := size
	if request.Length > 0 {
		end = min(request.Offset+request.Length, size)
	}
	if end-request.Offset > maxBytes {
		return AttachmentData{}, fmt.Errorf("attachment %q is %d bytes from offset %d, over max_bytes %d; download it in pieces with offset and length (for example length 5242880)", info.Filename, end-request.Offset, request.Offset, maxBytes)
	}
	chunk := data[request.Offset:end]
	out := AttachmentData{Filename: info.Filename, MimeType: info.MimeType, Size: size, Offset: request.Offset, Length: int64(len(chunk)), DataBase64: base64.StdEncoding.EncodeToString(chunk)}
	if end < size {
		out.NextOffset = end
	}
	if out.Complete() && (strings.HasPrefix(info.MimeType, "text/") || info.MimeType == "application/json") {
		if utf8.Valid(data) {
			out.Text = string(data)
		}
	}
	return out, nil
}

func (s *Service) fetchAttachment(ctx context.Context, service *gmail.Service, messageID, attachmentID string, maxBytes int64) (AttachmentInfo, []byte, error) {
	message, err := service.Users.Messages.Get("me", messageID).Format("full").Context(ctx).Do()
	if err != nil {
		return AttachmentInfo{}, nil, err
	}
	// Gmail issues a different attachment ID on every fetch, so the stable
	// handle is the MIME part ID ("part:<id>"). A raw Gmail attachment ID from
	// any earlier fetch is still accepted and passed straight through.
	var part *gmail.MessagePart
	walkParts(message.Payload, func(p *gmail.MessagePart) {
		if part == nil && p.Body != nil && ("part:"+p.PartId == attachmentID || p.Body.AttachmentId == attachmentID) {
			part = p
		}
	})
	gmailID := ""
	if part == nil {
		if strings.HasPrefix(attachmentID, "part:") {
			return AttachmentInfo{}, nil, fmt.Errorf("attachment %s not found on message %s", attachmentID, messageID)
		}
		part = &gmail.MessagePart{Filename: "attachment", MimeType: "application/octet-stream", Body: &gmail.MessagePartBody{AttachmentId: attachmentID}}
	}
	gmailID = part.Body.AttachmentId
	info := attachmentInfo(part)
	if maxBytes > 0 && info.Size > maxBytes {
		return AttachmentInfo{}, nil, fmt.Errorf("attachment %q is %d bytes, over max_bytes %d", info.Filename, info.Size, maxBytes)
	}
	encoded := part.Body.Data
	if gmailID != "" {
		body, err := service.Users.Messages.Attachments.Get("me", messageID, gmailID).Context(ctx).Do()
		if err != nil {
			return AttachmentInfo{}, nil, err
		}
		encoded = body.Data
	}
	data, err := decodeBase64URL(encoded)
	if err != nil {
		return AttachmentInfo{}, nil, fmt.Errorf("decode attachment: %w", err)
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return AttachmentInfo{}, nil, fmt.Errorf("attachment %q is %d bytes, over max_bytes %d", info.Filename, len(data), maxBytes)
	}
	return info, data, nil
}

// ModifyMessages adds and removes labels (IDs or names) on messages.
func (s *Service) ModifyMessages(ctx context.Context, ids, add, remove []string) error {
	if len(ids) == 0 {
		return errors.New("message_ids is required")
	}
	service, err := s.gmail(ctx)
	if err != nil {
		return err
	}
	addIDs, err := s.resolveLabels(ctx, service, add)
	if err != nil {
		return err
	}
	removeIDs, err := s.resolveLabels(ctx, service, remove)
	if err != nil {
		return err
	}
	for chunk := range slices.Chunk(ids, 1000) {
		if err := service.Users.Messages.BatchModify("me", &gmail.BatchModifyMessagesRequest{Ids: chunk, AddLabelIds: addIDs, RemoveLabelIds: removeIDs}).Context(ctx).Do(); err != nil {
			return err
		}
	}
	return nil
}

// ModifyThread adds and removes labels on every message in a thread.
func (s *Service) ModifyThread(ctx context.Context, id string, add, remove []string) error {
	service, err := s.gmail(ctx)
	if err != nil {
		return err
	}
	addIDs, err := s.resolveLabels(ctx, service, add)
	if err != nil {
		return err
	}
	removeIDs, err := s.resolveLabels(ctx, service, remove)
	if err != nil {
		return err
	}
	_, err = service.Users.Threads.Modify("me", id, &gmail.ModifyThreadRequest{AddLabelIds: addIDs, RemoveLabelIds: removeIDs}).Context(ctx).Do()
	return err
}

// Archive removes messages from the inbox.
func (s *Service) Archive(ctx context.Context, ids []string) error {
	return s.ModifyMessages(ctx, ids, nil, []string{"INBOX"})
}

// Trash moves messages to the trash.
func (s *Service) Trash(ctx context.Context, ids []string) error {
	return s.eachMessage(ctx, ids, func(service *gmail.Service, id string) error {
		_, err := service.Users.Messages.Trash("me", id).Context(ctx).Do()
		return err
	})
}

// Untrash restores messages from the trash.
func (s *Service) Untrash(ctx context.Context, ids []string) error {
	return s.eachMessage(ctx, ids, func(service *gmail.Service, id string) error {
		_, err := service.Users.Messages.Untrash("me", id).Context(ctx).Do()
		return err
	})
}

// TrashThread / UntrashThread act on a whole thread.
func (s *Service) TrashThread(ctx context.Context, id string, trash bool) error {
	service, err := s.gmail(ctx)
	if err != nil {
		return err
	}
	if trash {
		_, err = service.Users.Threads.Trash("me", id).Context(ctx).Do()
	} else {
		_, err = service.Users.Threads.Untrash("me", id).Context(ctx).Do()
	}
	return err
}

// DeletePermanently deletes messages immediately, bypassing trash.
func (s *Service) DeletePermanently(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return errors.New("message_ids is required")
	}
	service, err := s.gmail(ctx)
	if err != nil {
		return err
	}
	for chunk := range slices.Chunk(ids, 1000) {
		if err := service.Users.Messages.BatchDelete("me", &gmail.BatchDeleteMessagesRequest{Ids: chunk}).Context(ctx).Do(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) eachMessage(ctx context.Context, ids []string, apply func(*gmail.Service, string) error) error {
	if len(ids) == 0 {
		return errors.New("message_ids is required")
	}
	service, err := s.gmail(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := apply(service, id); err != nil {
			return fmt.Errorf("message %s: %w", id, err)
		}
	}
	return nil
}

// Label is a Gmail label.
type Label struct {
	ID                    string `json:"id"`
	Name                  string `json:"name"`
	Type                  string `json:"type"`
	MessagesTotal         int64  `json:"messages_total,omitempty"`
	MessagesUnread        int64  `json:"messages_unread,omitempty"`
	ThreadsTotal          int64  `json:"threads_total,omitempty"`
	ThreadsUnread         int64  `json:"threads_unread,omitempty"`
	LabelListVisibility   string `json:"label_list_visibility,omitempty"`
	MessageListVisibility string `json:"message_list_visibility,omitempty"`
	BackgroundColor       string `json:"background_color,omitempty"`
	TextColor             string `json:"text_color,omitempty"`
}

// LabelInput creates or updates a label. Empty fields are left unchanged.
type LabelInput struct {
	Name                  string `json:"name,omitempty" jsonschema:"Label name; use '/' for nesting, e.g. 'Projects/Alpha'."`
	LabelListVisibility   string `json:"label_list_visibility,omitempty" jsonschema:"labelShow, labelShowIfUnread, or labelHide."`
	MessageListVisibility string `json:"message_list_visibility,omitempty" jsonschema:"show or hide."`
	BackgroundColor       string `json:"background_color,omitempty" jsonschema:"Hex color from Gmail's palette, e.g. #16a766."`
	TextColor             string `json:"text_color,omitempty" jsonschema:"Hex color from Gmail's palette, e.g. #ffffff."`
}

func (input LabelInput) toGmail() *gmail.Label {
	label := &gmail.Label{Name: input.Name, LabelListVisibility: input.LabelListVisibility, MessageListVisibility: input.MessageListVisibility}
	if input.BackgroundColor != "" || input.TextColor != "" {
		label.Color = &gmail.LabelColor{BackgroundColor: input.BackgroundColor, TextColor: input.TextColor}
	}
	return label
}

func fromGmailLabel(label *gmail.Label) Label {
	out := Label{ID: label.Id, Name: label.Name, Type: label.Type, MessagesTotal: label.MessagesTotal, MessagesUnread: label.MessagesUnread, ThreadsTotal: label.ThreadsTotal, ThreadsUnread: label.ThreadsUnread, LabelListVisibility: label.LabelListVisibility, MessageListVisibility: label.MessageListVisibility}
	if label.Color != nil {
		out.BackgroundColor, out.TextColor = label.Color.BackgroundColor, label.Color.TextColor
	}
	return out
}

func (s *Service) ListLabels(ctx context.Context) ([]Label, error) {
	service, err := s.gmail(ctx)
	if err != nil {
		return nil, err
	}
	response, err := service.Users.Labels.List("me").Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	labels := make([]Label, 0, len(response.Labels))
	for _, label := range response.Labels {
		labels = append(labels, fromGmailLabel(label))
	}
	slices.SortFunc(labels, func(a, b Label) int { return strings.Compare(a.Name, b.Name) })
	return labels, nil
}

func (s *Service) GetLabel(ctx context.Context, id string) (Label, error) {
	service, err := s.gmail(ctx)
	if err != nil {
		return Label{}, err
	}
	ids, err := s.resolveLabels(ctx, service, []string{id})
	if err != nil {
		return Label{}, err
	}
	label, err := service.Users.Labels.Get("me", ids[0]).Context(ctx).Do()
	if err != nil {
		return Label{}, err
	}
	return fromGmailLabel(label), nil
}

func (s *Service) CreateLabel(ctx context.Context, input LabelInput) (Label, error) {
	if strings.TrimSpace(input.Name) == "" {
		return Label{}, errors.New("label name is required")
	}
	service, err := s.gmail(ctx)
	if err != nil {
		return Label{}, err
	}
	label, err := service.Users.Labels.Create("me", input.toGmail()).Context(ctx).Do()
	if err != nil {
		return Label{}, err
	}
	return fromGmailLabel(label), nil
}

func (s *Service) UpdateLabel(ctx context.Context, id string, input LabelInput) (Label, error) {
	service, err := s.gmail(ctx)
	if err != nil {
		return Label{}, err
	}
	ids, err := s.resolveLabels(ctx, service, []string{id})
	if err != nil {
		return Label{}, err
	}
	label, err := service.Users.Labels.Patch("me", ids[0], input.toGmail()).Context(ctx).Do()
	if err != nil {
		return Label{}, err
	}
	return fromGmailLabel(label), nil
}

func (s *Service) DeleteLabel(ctx context.Context, id string) error {
	service, err := s.gmail(ctx)
	if err != nil {
		return err
	}
	ids, err := s.resolveLabels(ctx, service, []string{id})
	if err != nil {
		return err
	}
	return service.Users.Labels.Delete("me", ids[0]).Context(ctx).Do()
}

// resolveLabels maps label IDs or (case-insensitive) names to IDs.
func (s *Service) resolveLabels(ctx context.Context, service *gmail.Service, values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	response, err := service.Users.Labels.List("me").Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("list labels: %w", err)
	}
	var ids []string
	for _, value := range values {
		var match string
		for _, label := range response.Labels {
			if label.Id == value {
				match = label.Id
				break
			}
		}
		if match == "" {
			for _, label := range response.Labels {
				if strings.EqualFold(label.Name, value) {
					match = label.Id
					break
				}
			}
		}
		if match == "" {
			return nil, fmt.Errorf("unknown label %q; create it first or call list_labels", value)
		}
		if !slices.Contains(ids, match) {
			ids = append(ids, match)
		}
	}
	return ids, nil
}

// FilterInput describes a Gmail filter.
type FilterInput struct {
	From           string   `json:"from,omitempty"`
	To             string   `json:"to,omitempty"`
	Subject        string   `json:"subject,omitempty"`
	Query          string   `json:"query,omitempty" jsonschema:"Gmail search query the message must match."`
	NegatedQuery   string   `json:"negated_query,omitempty" jsonschema:"Gmail search query the message must not match."`
	HasAttachment  bool     `json:"has_attachment,omitempty"`
	ExcludeChats   bool     `json:"exclude_chats,omitempty"`
	Size           int64    `json:"size,omitempty" jsonschema:"Size in bytes compared with size_comparison."`
	SizeComparison string   `json:"size_comparison,omitempty" jsonschema:"larger or smaller."`
	AddLabels      []string `json:"add_labels,omitempty" jsonschema:"Label IDs or names to add (e.g. STARRED, or a user label)."`
	RemoveLabels   []string `json:"remove_labels,omitempty" jsonschema:"Label IDs or names to remove (INBOX to skip the inbox, UNREAD to mark read)."`
	Forward        string   `json:"forward,omitempty" jsonschema:"Auto-forward address. Must be on the allowlist and already verified in Gmail."`
}

// Filter is a stored Gmail filter.
type Filter struct {
	ID       string        `json:"id"`
	Criteria FilterInput   `json:"criteria"`
	Action   *FilterAction `json:"action,omitempty"`
}

type FilterAction struct {
	AddLabelIDs    []string `json:"add_label_ids,omitempty"`
	RemoveLabelIDs []string `json:"remove_label_ids,omitempty"`
	Forward        string   `json:"forward,omitempty"`
}

func fromGmailFilter(filter *gmail.Filter) Filter {
	out := Filter{ID: filter.Id}
	if c := filter.Criteria; c != nil {
		out.Criteria = FilterInput{From: c.From, To: c.To, Subject: c.Subject, Query: c.Query, NegatedQuery: c.NegatedQuery, HasAttachment: c.HasAttachment, ExcludeChats: c.ExcludeChats, Size: c.Size, SizeComparison: c.SizeComparison}
	}
	if a := filter.Action; a != nil {
		out.Action = &FilterAction{AddLabelIDs: a.AddLabelIds, RemoveLabelIDs: a.RemoveLabelIds, Forward: a.Forward}
	}
	return out
}

func (s *Service) ListFilters(ctx context.Context) ([]Filter, error) {
	service, err := s.gmail(ctx)
	if err != nil {
		return nil, err
	}
	response, err := service.Users.Settings.Filters.List("me").Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	filters := []Filter{}
	for _, filter := range response.Filter {
		filters = append(filters, fromGmailFilter(filter))
	}
	return filters, nil
}

func (s *Service) CreateFilter(ctx context.Context, input FilterInput) (Filter, error) {
	if err := s.gateFilterForward(input.Forward); err != nil {
		return Filter{}, err
	}
	service, err := s.gmail(ctx)
	if err != nil {
		return Filter{}, err
	}
	addIDs, err := s.resolveLabels(ctx, service, input.AddLabels)
	if err != nil {
		return Filter{}, err
	}
	removeIDs, err := s.resolveLabels(ctx, service, input.RemoveLabels)
	if err != nil {
		return Filter{}, err
	}
	filter, err := service.Users.Settings.Filters.Create("me", &gmail.Filter{
		Criteria: &gmail.FilterCriteria{From: input.From, To: input.To, Subject: input.Subject, Query: input.Query, NegatedQuery: input.NegatedQuery, HasAttachment: input.HasAttachment, ExcludeChats: input.ExcludeChats, Size: input.Size, SizeComparison: input.SizeComparison},
		Action:   &gmail.FilterAction{AddLabelIds: addIDs, RemoveLabelIds: removeIDs, Forward: input.Forward},
	}).Context(ctx).Do()
	if err != nil {
		return Filter{}, err
	}
	return fromGmailFilter(filter), nil
}

func (s *Service) DeleteFilter(ctx context.Context, id string) error {
	service, err := s.gmail(ctx)
	if err != nil {
		return err
	}
	return service.Users.Settings.Filters.Delete("me", id).Context(ctx).Do()
}

// --- decoding helpers ---

func headerMap(part *gmail.MessagePart) map[string]string {
	headers := map[string]string{}
	if part == nil {
		return headers
	}
	for _, header := range part.Headers {
		key := strings.ToLower(header.Name)
		if _, exists := headers[key]; !exists {
			headers[key] = header.Value
		}
	}
	return headers
}

func decodeHeader(value string) string {
	decoded, err := new(mime.WordDecoder).DecodeHeader(value)
	if err != nil {
		return value
	}
	return decoded
}

func summarize(message *gmail.Message) MessageSummary {
	headers := headerMap(message.Payload)
	summary := MessageSummary{
		ID: message.Id, ThreadID: message.ThreadId,
		From: decodeHeader(headers["from"]), To: decodeHeader(headers["to"]), Cc: decodeHeader(headers["cc"]),
		Subject: decodeHeader(headers["subject"]), Date: headers["date"],
		Snippet: html.UnescapeString(message.Snippet), LabelIDs: message.LabelIds,
	}
	if summary.LabelIDs == nil {
		summary.LabelIDs = []string{}
	}
	walkParts(message.Payload, func(p *gmail.MessagePart) {
		if isAttachment(p) {
			summary.HasAttachments = true
		}
	})
	return summary
}

func detail(message *gmail.Message, options GetOptions) MessageDetail {
	headers := headerMap(message.Payload)
	out := MessageDetail{
		MessageSummary: summarize(message),
		Bcc:            decodeHeader(headers["bcc"]), ReplyTo: decodeHeader(headers["reply-to"]),
		MessageIDHeader: headers["message-id"], InReplyTo: headers["in-reply-to"], References: headers["references"],
		Attachments: []AttachmentInfo{},
	}
	var text, htmlBody string
	walkParts(message.Payload, func(p *gmail.MessagePart) {
		if isAttachment(p) {
			out.Attachments = append(out.Attachments, attachmentInfo(p))
			return
		}
		if p.Body == nil || p.Body.Data == "" {
			return
		}
		switch p.MimeType {
		case "text/plain":
			if text == "" {
				text = decodePartText(p)
			}
		case "text/html":
			if htmlBody == "" {
				htmlBody = decodePartText(p)
			}
		}
	})
	if text == "" && htmlBody != "" {
		text = htmlToText(htmlBody)
	}
	limit := options.MaxBodyChars
	if limit <= 0 {
		limit = defaultMaxBodyChars
	}
	var truncated bool
	out.Text, truncated = truncate(text, limit)
	out.Truncated = truncated
	if options.IncludeHTML {
		out.HTML, truncated = truncate(htmlBody, limit)
		out.Truncated = out.Truncated || truncated
	}
	if options.IncludeHeaders && message.Payload != nil {
		for _, header := range message.Payload.Headers {
			out.Headers = append(out.Headers, Header{Name: header.Name, Value: header.Value})
		}
	}
	return out
}

func truncate(value string, limit int) (string, bool) {
	if utf8.RuneCountInString(value) <= limit {
		return value, false
	}
	runes := []rune(value)
	return string(runes[:limit]), true
}

func isAttachment(part *gmail.MessagePart) bool {
	if part == nil || part.Body == nil {
		return false
	}
	return part.Filename != "" && (part.Body.AttachmentId != "" || part.Body.Data != "")
}

func attachmentInfo(part *gmail.MessagePart) AttachmentInfo {
	id := "part:" + part.PartId
	if part.PartId == "" {
		id = part.Body.AttachmentId
	}
	mimeType := part.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return AttachmentInfo{AttachmentID: id, PartID: part.PartId, Filename: part.Filename, MimeType: mimeType, Size: part.Body.Size}
}

func walkParts(part *gmail.MessagePart, visit func(*gmail.MessagePart)) {
	if part == nil {
		return
	}
	visit(part)
	for _, child := range part.Parts {
		walkParts(child, visit)
	}
}

func decodeBase64URL(value string) ([]byte, error) {
	trimmed := strings.TrimRight(value, "=")
	if data, err := base64.RawURLEncoding.DecodeString(trimmed); err == nil {
		return data, nil
	}
	return base64.RawStdEncoding.DecodeString(trimmed)
}

func decodePartText(part *gmail.MessagePart) string {
	data, err := decodeBase64URL(part.Body.Data)
	if err != nil {
		return ""
	}
	charset := ""
	for _, header := range part.Headers {
		if strings.EqualFold(header.Name, "Content-Type") {
			if _, params, err := mime.ParseMediaType(header.Value); err == nil {
				charset = strings.ToLower(params["charset"])
			}
		}
	}
	if charset != "" && charset != "utf-8" && charset != "us-ascii" {
		if encoding, err := htmlindex.Get(charset); err == nil {
			if decoded, err := encoding.NewDecoder().Bytes(data); err == nil {
				return string(decoded)
			}
		}
	}
	return strings.ToValidUTF8(string(data), "�")
}

var (
	htmlDropBlocks = regexp.MustCompile(`(?is)<(script|style|head)[^>]*>.*?</(script|style|head)>`)
	htmlBreaks     = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/li|/tr|/h[1-6])[^>]*>`)
	htmlTags       = regexp.MustCompile(`(?s)<[^>]*>`)
	blankLines     = regexp.MustCompile(`\n{3,}`)
)

func htmlToText(value string) string {
	value = htmlDropBlocks.ReplaceAllString(value, "")
	value = htmlBreaks.ReplaceAllString(value, "\n")
	value = htmlTags.ReplaceAllString(value, "")
	value = html.UnescapeString(value)
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t\r")
	}
	return strings.TrimSpace(blankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}
