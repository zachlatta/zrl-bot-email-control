// Package mcpserver exposes the mailbox as MCP tools.
package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zachlatta/zrl-bot-email-control/internal/mailbox"
)

const maxInlineImageBytes = 5 << 20

type Empty struct{}

type OK struct {
	OK bool `json:"ok"`
}

type MessageIDsInput struct {
	MessageIDs []string `json:"message_ids" jsonschema:"Gmail message IDs."`
}

type ModifyMessagesInput struct {
	MessageIDs   []string `json:"message_ids" jsonschema:"Gmail message IDs."`
	AddLabels    []string `json:"add_labels,omitempty" jsonschema:"Label IDs or names to add, e.g. STARRED, IMPORTANT, INBOX, or a user label name."`
	RemoveLabels []string `json:"remove_labels,omitempty" jsonschema:"Label IDs or names to remove, e.g. INBOX (archive) or UNREAD (mark read)."`
}

type ModifyThreadInput struct {
	ThreadID     string   `json:"thread_id" jsonschema:"Gmail thread ID."`
	AddLabels    []string `json:"add_labels,omitempty" jsonschema:"Label IDs or names to add, e.g. STARRED or a user label name."`
	RemoveLabels []string `json:"remove_labels,omitempty" jsonschema:"Label IDs or names to remove, e.g. [\"INBOX\"] to archive the thread or [\"UNREAD\"] to mark it read."`
}

type ThreadIDInput struct {
	ThreadID string `json:"thread_id" jsonschema:"Gmail thread ID."`
}

type GetMessageInput struct {
	MessageID string `json:"message_id" jsonschema:"Gmail message ID."`
	mailbox.GetOptions
}

type GetThreadInput struct {
	ThreadID string `json:"thread_id" jsonschema:"Gmail thread ID."`
	mailbox.ThreadOptions
}

type ReplyToMessageInput struct {
	mailbox.ReplyInput
	ArchiveThread bool `json:"archive_thread,omitempty" jsonschema:"After the reply is sent, archive the whole thread (remove INBOX). Nothing is archived if the send fails."`
}

type DraftIDInput struct {
	DraftID string `json:"draft_id"`
}

type UpdateDraftInput struct {
	DraftID string `json:"draft_id"`
	mailbox.Outgoing
}

type ListDraftsInput struct {
	MaxResults int64 `json:"max_results,omitempty"`
}

type LabelIDInput struct {
	LabelID string `json:"label_id" jsonschema:"Label ID or name."`
}

type UpdateLabelInput struct {
	LabelID string `json:"label_id" jsonschema:"Label ID or current name."`
	mailbox.LabelInput
}

type FilterIDInput struct {
	FilterID string `json:"filter_id"`
}

type LabelsOutput struct {
	Labels []mailbox.Label `json:"labels"`
}

type FiltersOutput struct {
	Filters []mailbox.Filter `json:"filters"`
}

type DraftsOutput struct {
	Drafts []mailbox.DraftSummary `json:"drafts"`
}

// New builds the MCP server for service.
func New(service *mailbox.Service, version string) *mcp.Server {
	allowed := strings.Join(service.AllowedRecipients(), ", ")
	if allowed == "" {
		allowed = "(none — sending is disabled)"
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "zrl-bot-email-control", Title: "zrl-bot email", Version: version}, &mcp.ServerOptions{
		Instructions: fmt.Sprintf("Full control of the Gmail mailbox %s: search, read, download attachments, label, archive, trash, delete, filter, draft, send, reply and forward. "+
			"Outgoing mail is hard-limited by the server: every To, Cc and Bcc address must be one of: %s. "+
			"Anything else is rejected before it reaches Gmail; do not try to work around it. "+
			"Message and thread IDs come from search_messages / search_threads. Label arguments accept IDs (INBOX, UNREAD, STARRED, ...) or label names.",
			service.Address(), allowed),
	})
	closed := false
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closed}
	mutating := &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: &closed}
	destructive := &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: &closed}
	outbound := &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(true)}

	add(server, "get_mailbox_info", "Mailbox address, message/thread totals, and the allowlisted recipients this server can send to.", readOnly,
		func(ctx context.Context, _ Empty) (mailbox.Profile, error) { return service.Profile(ctx) })
	add(server, "search_messages", "Search messages with Gmail query syntax (same as the Gmail search box). Returns summaries with IDs, sender, subject, date, snippet and labels. Use query 'in:inbox' for the inbox.", readOnly,
		service.Search)
	add(server, "search_threads", "Search conversations with Gmail query syntax. Returns one summary per thread.", readOnly,
		service.SearchThreads)
	add(server, "get_message", "Read one message: headers, decoded text (and optionally HTML) body, and attachment list.", readOnly,
		func(ctx context.Context, input GetMessageInput) (mailbox.MessageDetail, error) {
			return service.GetMessage(ctx, input.MessageID, input.GetOptions)
		})
	add(server, "get_thread", "Read every message in a conversation, oldest first. Each reply's quoted copy of earlier messages is cut from its text (quoted_trimmed is set); pass include_quoted to keep it. Pass latest (e.g. 3) to get only the newest messages of a long thread. The last message is the one to answer with reply_to_message.", readOnly,
		func(ctx context.Context, input GetThreadInput) (mailbox.ThreadDetail, error) {
			return service.GetThread(ctx, input.ThreadID, input.ThreadOptions)
		})
	mcp.AddTool(server, &mcp.Tool{Name: "get_attachment", Description: "Download an attachment. Returns base64 data (and decoded text for text files); whole images are also returned as image content. For large files, pass offset and length to download pieces and follow next_offset until it is absent.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, input mailbox.AttachmentRequest) (*mcp.CallToolResult, mailbox.AttachmentData, error) {
			data, err := service.GetAttachment(ctx, input)
			if err != nil {
				return nil, mailbox.AttachmentData{}, err
			}
			if !strings.HasPrefix(data.MimeType, "image/") || !data.Complete() || data.Size > maxInlineImageBytes {
				return nil, data, nil
			}
			raw, err := base64.StdEncoding.DecodeString(data.DataBase64)
			if err != nil {
				return nil, data, nil
			}
			summary, _ := json.Marshal(data)
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: string(summary)},
				&mcp.ImageContent{Data: raw, MIMEType: data.MimeType},
			}}, data, nil
		})

	add(server, "send_email", "Send a new email (text and/or HTML, with attachments as base64 or references to existing attachments). Every To/Cc/Bcc address must be allowlisted. To answer a message, use reply_to_message instead: it addresses whoever wrote. With thread_id, the email is sent as a reply to the thread's latest message (subject defaults to \"Re: <thread subject>\"), but still only to the to/cc/bcc you give.", outbound,
		service.Send)
	add(server, "reply_to_message", "Reply in-thread to a message. Recipients default to the sender, or the Reply-To address (plus original To/Cc with reply_all); when the message was sent by this mailbox, they default to its original To. Pass to/cc to override. The original is quoted unless omit_quote is set. Text and/or HTML body, attachments as in send_email. Pass archive_thread to archive the thread once the reply is sent. The result lists the delivered to/cc/bcc/subject.", outbound,
		func(ctx context.Context, input ReplyToMessageInput) (mailbox.SendResult, error) {
			input.ReplyInput.ArchiveThread = input.ArchiveThread
			return service.Reply(ctx, input.ReplyInput)
		})
	add(server, "forward_message", "Forward a message, including its attachments by default, with an optional note.", outbound,
		service.Forward)

	add(server, "create_draft", "Save a new draft without sending it. Recipients must be allowlisted.", mutating,
		service.CreateDraft)
	add(server, "create_reply_draft", "Save an in-thread reply as a draft without sending it.", mutating,
		service.CreateReplyDraft)
	add(server, "update_draft", "Replace a draft's recipients, subject, body and attachments.", mutating,
		func(ctx context.Context, input UpdateDraftInput) (mailbox.DraftSummary, error) {
			return service.UpdateDraft(ctx, input.DraftID, input.Outgoing)
		})
	add(server, "list_drafts", "List drafts.", readOnly,
		func(ctx context.Context, input ListDraftsInput) (DraftsOutput, error) {
			drafts, err := service.ListDrafts(ctx, input.MaxResults)
			return DraftsOutput{Drafts: drafts}, err
		})
	add(server, "get_draft", "Read a draft.", readOnly,
		func(ctx context.Context, input DraftIDInput) (mailbox.Draft, error) {
			return service.GetDraft(ctx, input.DraftID)
		})
	add(server, "send_draft", "Send a draft. Its current recipients are re-checked against the allowlist first.", outbound,
		func(ctx context.Context, input DraftIDInput) (mailbox.SendResult, error) {
			return service.SendDraft(ctx, input.DraftID)
		})
	add(server, "delete_draft", "Permanently delete a draft.", destructive,
		func(ctx context.Context, input DraftIDInput) (OK, error) {
			return ok(service.DeleteDraft(ctx, input.DraftID))
		})

	add(server, "modify_message_labels", "Add and/or remove labels on messages (IDs or names). Remove INBOX to archive, UNREAD to mark read; add STARRED to star.", mutating,
		func(ctx context.Context, input ModifyMessagesInput) (OK, error) {
			return ok(service.ModifyMessages(ctx, input.MessageIDs, input.AddLabels, input.RemoveLabels))
		})
	add(server, "modify_thread_labels", "Add and/or remove labels on every message in a thread.", mutating,
		func(ctx context.Context, input ModifyThreadInput) (OK, error) {
			return ok(service.ModifyThread(ctx, input.ThreadID, input.AddLabels, input.RemoveLabels))
		})
	add(server, "archive_messages", "Archive messages (remove them from the inbox).", mutating,
		func(ctx context.Context, input MessageIDsInput) (OK, error) {
			return ok(service.Archive(ctx, input.MessageIDs))
		})
	add(server, "mark_read", "Mark messages as read.", mutating,
		func(ctx context.Context, input MessageIDsInput) (OK, error) {
			return ok(service.ModifyMessages(ctx, input.MessageIDs, nil, []string{"UNREAD"}))
		})
	add(server, "mark_unread", "Mark messages as unread.", mutating,
		func(ctx context.Context, input MessageIDsInput) (OK, error) {
			return ok(service.ModifyMessages(ctx, input.MessageIDs, []string{"UNREAD"}, nil))
		})
	add(server, "trash_messages", "Move messages to the trash (recoverable for 30 days).", destructive,
		func(ctx context.Context, input MessageIDsInput) (OK, error) {
			return ok(service.Trash(ctx, input.MessageIDs))
		})
	add(server, "untrash_messages", "Restore messages from the trash.", mutating,
		func(ctx context.Context, input MessageIDsInput) (OK, error) {
			return ok(service.Untrash(ctx, input.MessageIDs))
		})
	add(server, "trash_thread", "Move a whole conversation to the trash.", destructive,
		func(ctx context.Context, input ThreadIDInput) (OK, error) {
			return ok(service.TrashThread(ctx, input.ThreadID, true))
		})
	add(server, "untrash_thread", "Restore a whole conversation from the trash.", mutating,
		func(ctx context.Context, input ThreadIDInput) (OK, error) {
			return ok(service.TrashThread(ctx, input.ThreadID, false))
		})
	add(server, "delete_messages_permanently", "Permanently delete messages immediately, bypassing the trash. Irreversible.", destructive,
		func(ctx context.Context, input MessageIDsInput) (OK, error) {
			return ok(service.DeletePermanently(ctx, input.MessageIDs))
		})

	add(server, "list_labels", "List all labels with IDs and counts.", readOnly,
		func(ctx context.Context, _ Empty) (LabelsOutput, error) {
			labels, err := service.ListLabels(ctx)
			return LabelsOutput{Labels: labels}, err
		})
	add(server, "create_label", "Create a label. Use '/' in the name to nest, e.g. 'Receipts/2026'.", mutating,
		service.CreateLabel)
	add(server, "update_label", "Rename or restyle a label.", mutating,
		func(ctx context.Context, input UpdateLabelInput) (mailbox.Label, error) {
			return service.UpdateLabel(ctx, input.LabelID, input.LabelInput)
		})
	add(server, "delete_label", "Delete a user label (messages keep existing, just lose the label).", destructive,
		func(ctx context.Context, input LabelIDInput) (OK, error) {
			return ok(service.DeleteLabel(ctx, input.LabelID))
		})

	add(server, "list_filters", "List Gmail filters.", readOnly,
		func(ctx context.Context, _ Empty) (FiltersOutput, error) {
			filters, err := service.ListFilters(ctx)
			return FiltersOutput{Filters: filters}, err
		})
	add(server, "create_filter", "Create a Gmail filter that labels, archives, or marks matching incoming mail. Auto-forwarding is only allowed to allowlisted, Gmail-verified addresses.", mutating,
		service.CreateFilter)
	add(server, "delete_filter", "Delete a Gmail filter.", destructive,
		func(ctx context.Context, input FilterIDInput) (OK, error) {
			return ok(service.DeleteFilter(ctx, input.FilterID))
		})
	return server
}

func add[In, Out any](server *mcp.Server, name, description string, annotations *mcp.ToolAnnotations, handler func(context.Context, In) (Out, error)) {
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: description, Annotations: annotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input In) (*mcp.CallToolResult, Out, error) {
			out, err := handler(ctx, input)
			return nil, out, err
		})
}

func ok(err error) (OK, error) {
	if err != nil {
		return OK{}, err
	}
	return OK{OK: true}, nil
}

func ptr[T any](value T) *T { return &value }
