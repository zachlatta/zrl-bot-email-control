package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zachlatta/zrl-bot-email-control/internal/gmailfake"
	"github.com/zachlatta/zrl-bot-email-control/internal/mailbox"
	"github.com/zachlatta/zrl-bot-email-control/internal/recipients"
	"google.golang.org/api/gmail/v1"
)

const bot = "zrl-bot@hackclub.com"

func connect(t *testing.T) (*mcp.ClientSession, *gmailfake.Server) {
	t.Helper()
	fake := gmailfake.New(t, bot)
	allow, _ := recipients.ParseAllowlist("zach@hackclub.com,zach@zachlatta.com")
	service := mailbox.New(mailbox.Config{Address: bot, Allowlist: allow, Gmail: func(context.Context) (*gmail.Service, error) { return fake.Service(t), nil }})
	server := New(service, "test")
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = server.Run(ctx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, fake
}

func call(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	return result
}

func text(result *mcp.CallToolResult) string {
	var parts []string
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func structured(t *testing.T, result *mcp.CallToolResult, into any) {
	t.Helper()
	if result.IsError {
		t.Fatalf("tool error: %s", text(result))
	}
	data, _ := json.Marshal(result.StructuredContent)
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("decode structured content: %v (%s)", err, data)
	}
}

func TestToolCatalog(t *testing.T) {
	session, _ := connect(t)
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		if tool.Description == "" {
			t.Errorf("tool %s has no description", tool.Name)
		}
	}
	for _, want := range []string{
		"get_mailbox_info", "search_messages", "search_threads", "get_message", "get_thread", "get_attachment",
		"send_email", "reply_to_message", "forward_message",
		"create_draft", "create_reply_draft", "update_draft", "list_drafts", "get_draft", "send_draft", "delete_draft",
		"modify_message_labels", "modify_thread_labels", "archive_messages", "mark_read", "mark_unread",
		"trash_messages", "untrash_messages", "trash_thread", "untrash_thread", "delete_messages_permanently",
		"list_labels", "create_label", "update_label", "delete_label",
		"list_filters", "create_filter", "delete_filter",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("missing tool %s (have %v)", want, names)
		}
	}
	session.InitializeResult()
	if info := session.InitializeResult().ServerInfo; info.Name != "zrl-bot-email-control" {
		t.Errorf("server info = %#v", info)
	}
	if instructions := session.InitializeResult().Instructions; !strings.Contains(instructions, "zach@hackclub.com") || !strings.Contains(instructions, bot) {
		t.Errorf("instructions should name the mailbox and allowlist: %q", instructions)
	}
}

func TestSendToolEnforcesAllowlist(t *testing.T) {
	session, fake := connect(t)
	blocked := call(t, session, "send_email", map[string]any{"to": []string{"someone@example.com"}, "subject": "x", "text": "x"})
	if !blocked.IsError || !strings.Contains(text(blocked), "recipient not allowed") {
		t.Fatalf("send to stranger: error=%v %q", blocked.IsError, text(blocked))
	}
	blocked = call(t, session, "send_email", map[string]any{"to": []string{"zach@hackclub.com"}, "bcc": []string{"spy@example.com"}, "text": "x"})
	if !blocked.IsError || len(fake.Sent()) != 0 {
		t.Fatalf("bcc to stranger not blocked: %q", text(blocked))
	}
	var sent mailbox.SendResult
	structured(t, call(t, session, "send_email", map[string]any{"to": []string{"zach@hackclub.com"}, "subject": "hello", "text": "hi"}), &sent)
	if sent.ID == "" || len(fake.Sent()) != 1 {
		t.Fatalf("send = %#v, delivered %d", sent, len(fake.Sent()))
	}
}

func TestReadArchiveAndAttachmentTools(t *testing.T) {
	session, fake := connect(t)
	png := []byte("\x89PNG\r\n\x1a\nfake")
	id := fake.AddMessage("From: zach@hackclub.com\nTo: zrl-bot@hackclub.com\nSubject: pic\nContent-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: text/plain\n\nlook\n--b\nContent-Type: image/png\nContent-Disposition: attachment; filename=\"a.png\"\nContent-Transfer-Encoding: base64\n\n"+base64.StdEncoding.EncodeToString(png)+"\n--b--\n", "INBOX", "UNREAD")

	var search mailbox.SearchResult
	structured(t, call(t, session, "search_messages", map[string]any{"query": "pic"}), &search)
	if len(search.Messages) != 1 || search.Messages[0].ID != id {
		t.Fatalf("search = %#v", search)
	}
	var detail mailbox.MessageDetail
	structured(t, call(t, session, "get_message", map[string]any{"message_id": id}), &detail)
	if len(detail.Attachments) != 1 {
		t.Fatalf("detail = %#v", detail)
	}
	attachment := call(t, session, "get_attachment", map[string]any{"message_id": id, "attachment_id": detail.Attachments[0].AttachmentID})
	if attachment.IsError {
		t.Fatal(text(attachment))
	}
	var sawImage bool
	for _, content := range attachment.Content {
		if image, ok := content.(*mcp.ImageContent); ok && image.MIMEType == "image/png" && string(image.Data) == string(png) {
			sawImage = true
		}
	}
	if !sawImage {
		t.Errorf("image attachment not returned as image content: %#v", attachment.Content)
	}

	if result := call(t, session, "mark_read", map[string]any{"message_ids": []string{id}}); result.IsError {
		t.Fatal(text(result))
	}
	if result := call(t, session, "archive_messages", map[string]any{"message_ids": []string{id}}); result.IsError {
		t.Fatal(text(result))
	}
	message, _ := fake.Message(id)
	if slices.Contains(message.LabelIDs, "INBOX") || slices.Contains(message.LabelIDs, "UNREAD") {
		t.Fatalf("labels after archive+read = %v", message.LabelIDs)
	}
	if result := call(t, session, "forward_message", map[string]any{"message_id": id, "to": []string{"zach@zachlatta.com"}}); result.IsError {
		t.Fatal(text(result))
	}
	if raw := string(fake.Sent()[0]); !strings.Contains(raw, "filename=a.png") {
		t.Fatalf("forward lost attachment:\n%s", raw)
	}
}

func TestChunkedAttachmentAndThreadTools(t *testing.T) {
	session, fake := connect(t)
	png := []byte("\x89PNG\r\n\x1a\nfake image bytes")
	id := fake.AddMessage("From: zach@hackclub.com\nTo: zrl-bot@hackclub.com\nSubject: pic\nMessage-ID: <p1@x>\nContent-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: text/plain\n\nlook\n--b\nContent-Type: image/png\nContent-Disposition: attachment; filename=\"a.png\"\nContent-Transfer-Encoding: base64\n\n"+base64.StdEncoding.EncodeToString(png)+"\n--b--\n", "INBOX")
	message, _ := fake.Message(id)
	fake.AddMessageInThread(message.ThreadID, "From: zach@hackclub.com\nTo: zrl-bot@hackclub.com\nSubject: Re: pic\nContent-Type: text/plain\n\nWell?\n\nOn Sun, Oct 11, 2026 at 9:00 AM Zach <zach@hackclub.com> wrote:\n> look\n", "INBOX")

	var chunk mailbox.AttachmentData
	result := call(t, session, "get_attachment", map[string]any{"message_id": id, "attachment_id": "part:1", "offset": 4, "length": 4})
	structured(t, result, &chunk)
	data, _ := base64.StdEncoding.DecodeString(chunk.DataBase64)
	if string(data) != string(png[4:8]) || chunk.Size != int64(len(png)) || chunk.NextOffset != 8 {
		t.Fatalf("chunk = %+v (%q)", chunk, data)
	}
	for _, content := range result.Content {
		if _, ok := content.(*mcp.ImageContent); ok {
			t.Fatal("a partial image was returned as image content")
		}
	}

	var thread mailbox.ThreadDetail
	structured(t, call(t, session, "get_thread", map[string]any{"thread_id": message.ThreadID}), &thread)
	if got := strings.TrimSpace(thread.Messages[1].Text); got != "Well?" {
		t.Fatalf("get_thread kept the quote: %q", got)
	}
	structured(t, call(t, session, "get_thread", map[string]any{"thread_id": message.ThreadID, "include_quoted": true}), &thread)
	if !strings.Contains(thread.Messages[1].Text, "> look") {
		t.Fatalf("include_quoted dropped the quote: %q", thread.Messages[1].Text)
	}
}

func TestLabelArgumentsAreDocumented(t *testing.T) {
	session, _ := connect(t)
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "modify_thread_labels" {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"thread_id", "add_labels", "remove_labels"} {
			if schema.Properties[name].Description == "" {
				t.Errorf("modify_thread_labels.%s has no description: %s", name, raw)
			}
		}
		return
	}
	t.Fatal("modify_thread_labels not found")
}
