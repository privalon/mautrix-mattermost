// Copyright 2024-2026 Remi Philippe
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// senderFor builds an EventSender for the given Mattermost user ID. If the
// user has a double puppet UserLogin registered, SenderLogin is set so the
// bridgev2 framework uses that user's double puppet intent instead of a ghost.
func (m *MattermostClient) senderFor(mmUserID string) bridgev2.EventSender {
	sender := bridgev2.EventSender{
		Sender:   MakeUserID(mmUserID),
		IsFromMe: mmUserID == m.userID,
	}
	if loginID, ok := m.connector.DoublePuppetLoginID(mmUserID); ok {
		sender.SenderLogin = loginID
	}
	return sender
}

// handleEvent dispatches a Mattermost WebSocket event to the appropriate handler.
func (m *MattermostClient) handleEvent(evt *model.WebSocketEvent) {
	switch evt.EventType() {
	case model.WebsocketEventPosted:
		m.handlePosted(evt)
	case model.WebsocketEventPostEdited:
		m.handlePostEdited(evt)
	case model.WebsocketEventPostDeleted:
		m.handlePostDeleted(evt)
	case model.WebsocketEventReactionAdded:
		m.handleReactionAdded(evt)
	case model.WebsocketEventReactionRemoved:
		m.handleReactionRemoved(evt)
	case model.WebsocketEventTyping:
		m.handleTyping(evt)
	case model.WebsocketEventChannelViewed:
		m.handleChannelViewed(evt)
	default:
		m.log.Trace().Str("event_type", string(evt.EventType())).Msg("Unhandled event type")
	}
}

// parsePostedEvent extracts and validates a post from a WebSocket event,
// applying all echo prevention layers. Returns (nil, nil) to skip silently,
// (nil, err) to log an error, or (post, nil) to proceed.
func (m *MattermostClient) parsePostedEvent(evt *model.WebSocketEvent) (*model.Post, error) {
	postJSON, ok := evt.GetData()["post"].(string)
	if !ok {
		return nil, fmt.Errorf("posted event missing post data")
	}

	var post model.Post
	if err := json.Unmarshal([]byte(postJSON), &post); err != nil {
		return nil, fmt.Errorf("failed to unmarshal post: %w", err)
	}

	// Echo prevention: skip the echo of posts this bridge created from Matrix
	// (matched by pending_post_id). The user's own posts made from other
	// Mattermost clients are real new content and are bridged.
	if post.UserId == m.userID && m.consumePendingEcho(post.PendingPostId) {
		return nil, nil
	}

	// Echo prevention: skip non-default post types (system messages), except
	// custom-typed posts that carry files (e.g. a voice-message plugin's own
	// post type): those are real content and are transferred as-is.
	if !shouldBridgePostType(&post) {
		m.log.Debug().
			Str("post_id", post.Id).
			Str("post_type", post.Type).
			Msg("Skipping non-default post type")
		return nil, nil
	}

	// Echo prevention: skip posts from puppet bot users.
	if m.connector.IsPuppetUserID(post.UserId) {
		m.log.Debug().
			Str("post_id", post.Id).
			Str("user_id", post.UserId).
			Msg("Skipping puppet bot post (echo prevention)")
		return nil, nil
	}

	// Echo prevention: skip posts from usernames matching known bridge patterns.
	senderName, _ := evt.GetData()["sender_name"].(string)
	senderName = strings.TrimPrefix(senderName, "@")
	if senderName != "" && isBridgeUsername(senderName, m.connector.Config.BotPrefix) {
		m.log.Debug().
			Str("post_id", post.Id).
			Str("username", senderName).
			Msg("Skipping bridge username post (echo prevention)")
		return nil, nil
	}

	return &post, nil
}

// shouldBridgePostType reports whether a post of this type is user content.
// Default posts always are. Mattermost's own system messages ("system_*") never
// are. Any other, custom post type is bridged only when it carries files, so
// attachments created by plugins are not lost.
func shouldBridgePostType(post *model.Post) bool {
	if post.Type == "" || post.Type == model.PostTypeDefault {
		return true
	}
	if strings.HasPrefix(post.Type, "system_") {
		return false
	}
	return len(post.FileIds) > 0
}

// parsePostEditedEvent extracts and validates an edited post from a WebSocket event,
// applying echo prevention. Returns (nil, nil) to skip, (nil, err) for errors,
// or (post, nil) to proceed.
func (m *MattermostClient) parsePostEditedEvent(evt *model.WebSocketEvent) (*model.Post, error) {
	postJSON, ok := evt.GetData()["post"].(string)
	if !ok {
		return nil, nil
	}

	var post model.Post
	if err := json.Unmarshal([]byte(postJSON), &post); err != nil {
		return nil, fmt.Errorf("failed to unmarshal edited post: %w", err)
	}

	if post.UserId == m.userID {
		return nil, nil
	}

	// Echo prevention: skip edits from puppet bot users.
	if m.connector.IsPuppetUserID(post.UserId) {
		m.log.Debug().
			Str("post_id", post.Id).
			Str("user_id", post.UserId).
			Msg("Skipping puppet bot edit (echo prevention)")
		return nil, nil
	}

	// Echo prevention: skip edits from usernames matching known bridge patterns.
	senderName, _ := evt.GetData()["sender_name"].(string)
	senderName = strings.TrimPrefix(senderName, "@")
	if senderName != "" && isBridgeUsername(senderName, m.connector.Config.BotPrefix) {
		m.log.Debug().
			Str("post_id", post.Id).
			Str("username", senderName).
			Msg("Skipping bridge username edit (echo prevention)")
		return nil, nil
	}

	return &post, nil
}

// parsePostDeletedEvent extracts and validates a deleted post from a WebSocket event,
// applying echo prevention. Returns (nil, nil) to skip, (nil, err) for errors,
// or (post, nil) to proceed.
func (m *MattermostClient) parsePostDeletedEvent(evt *model.WebSocketEvent) (*model.Post, error) {
	postJSON, ok := evt.GetData()["post"].(string)
	if !ok {
		return nil, nil
	}

	var post model.Post
	if err := json.Unmarshal([]byte(postJSON), &post); err != nil {
		return nil, fmt.Errorf("failed to unmarshal deleted post: %w", err)
	}

	if post.UserId == m.userID {
		return nil, nil
	}

	// Echo prevention: skip deletes from puppet bot users.
	if m.connector.IsPuppetUserID(post.UserId) {
		m.log.Debug().
			Str("post_id", post.Id).
			Str("user_id", post.UserId).
			Msg("Skipping puppet bot delete (echo prevention)")
		return nil, nil
	}

	// Echo prevention: skip deletes from usernames matching known bridge patterns.
	senderName, _ := evt.GetData()["sender_name"].(string)
	senderName = strings.TrimPrefix(senderName, "@")
	if senderName != "" && isBridgeUsername(senderName, m.connector.Config.BotPrefix) {
		m.log.Debug().
			Str("post_id", post.Id).
			Str("username", senderName).
			Msg("Skipping bridge username delete (echo prevention)")
		return nil, nil
	}

	return &post, nil
}

// parseReactionEvent extracts and validates a reaction from a WebSocket event.
// Returns (nil, nil) to skip, (nil, err) for errors, or (reaction, nil) to proceed.
func (m *MattermostClient) parseReactionEvent(evt *model.WebSocketEvent) (*model.Reaction, error) {
	reactionJSON, ok := evt.GetData()["reaction"].(string)
	if !ok {
		return nil, nil
	}

	var reaction model.Reaction
	if err := json.Unmarshal([]byte(reactionJSON), &reaction); err != nil {
		return nil, fmt.Errorf("failed to unmarshal reaction: %w", err)
	}

	// Echo prevention: skip own reactions.
	if reaction.UserId == m.userID {
		return nil, nil
	}

	// Echo prevention: skip reactions from puppet bot users.
	if m.connector.IsPuppetUserID(reaction.UserId) {
		m.log.Debug().
			Str("post_id", reaction.PostId).
			Str("user_id", reaction.UserId).
			Str("emoji", reaction.EmojiName).
			Msg("Skipping puppet bot reaction (echo prevention)")
		return nil, nil
	}

	// Echo prevention: skip reactions from usernames matching known bridge patterns.
	senderName, _ := evt.GetData()["sender_name"].(string)
	senderName = strings.TrimPrefix(senderName, "@")
	if senderName != "" && isBridgeUsername(senderName, m.connector.Config.BotPrefix) {
		m.log.Debug().
			Str("post_id", reaction.PostId).
			Str("username", senderName).
			Str("emoji", reaction.EmojiName).
			Msg("Skipping bridge username reaction (echo prevention)")
		return nil, nil
	}

	return &reaction, nil
}

// parseTypingEvent extracts typing event data. Returns ("", "", false) to skip.
func (m *MattermostClient) parseTypingEvent(evt *model.WebSocketEvent) (userID, channelID string, ok bool) {
	uid, uidOk := evt.GetData()["user_id"].(string)
	if !uidOk || uid == m.userID {
		return "", "", false
	}
	return uid, evt.GetBroadcast().ChannelId, true
}

// parseChannelViewedEvent extracts channel viewed data. Returns ("", false) to skip.
func (m *MattermostClient) parseChannelViewedEvent(evt *model.WebSocketEvent) (channelID string, ok bool) {
	chID, chOk := evt.GetData()["channel_id"].(string)
	if !chOk {
		return "", false
	}
	return chID, true
}

func (m *MattermostClient) handlePosted(evt *model.WebSocketEvent) {
	post, err := m.parsePostedEvent(evt)
	if err != nil {
		m.log.Warn().Err(err).Msg("Failed to parse posted event")
		return
	}
	if post == nil {
		return
	}

	m.log.Debug().
		Str("post_id", post.Id).
		Str("channel_id", post.ChannelId).
		Str("user_id", post.UserId).
		Msg("Received new message")

	ts := time.UnixMilli(post.CreateAt)

	m.eventSender.QueueRemoteEvent(m.userLogin, &simplevent.Message[*model.Post]{
		EventMeta: simplevent.EventMeta{
			Type: bridgev2.RemoteEventMessage,
			LogContext: func(c zerolog.Context) zerolog.Context {
				return c.Str("post_id", post.Id).Str("channel_id", post.ChannelId)
			},
			PortalKey:    makePortalKey(post.ChannelId),
			Sender:       m.senderFor(post.UserId),
			Timestamp:    ts,
			CreatePortal: true,
		},
		ID:   MakeMessageID(post.Id),
		Data: post,
		ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, data *model.Post) (*bridgev2.ConvertedMessage, error) {
			return m.convertPostToMatrixMedia(ctx, data, matrixUploader(intent, portal.MXID)), nil
		},
	})
}

func (m *MattermostClient) handlePostEdited(evt *model.WebSocketEvent) {
	post, err := m.parsePostEditedEvent(evt)
	if err != nil {
		m.log.Error().Err(err).Msg("Failed to parse post edited event")
		return
	}
	if post == nil {
		return
	}

	ts := time.UnixMilli(post.EditAt)

	m.eventSender.QueueRemoteEvent(m.userLogin, &simplevent.Message[*model.Post]{
		EventMeta: simplevent.EventMeta{
			Type: bridgev2.RemoteEventEdit,
			LogContext: func(c zerolog.Context) zerolog.Context {
				return c.Str("post_id", post.Id).Str("channel_id", post.ChannelId)
			},
			PortalKey: makePortalKey(post.ChannelId),
			Sender:    m.senderFor(post.UserId),
			Timestamp: ts,
		},
		TargetMessage: MakeMessageID(post.Id),
		Data:          post,
		ConvertEditFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, existing []*database.Message, data *model.Post) (*bridgev2.ConvertedEdit, error) {
			return m.convertEditToMatrix(data, existing), nil
		},
	})
}

func (m *MattermostClient) handlePostDeleted(evt *model.WebSocketEvent) {
	post, err := m.parsePostDeletedEvent(evt)
	if err != nil {
		m.log.Error().Err(err).Msg("Failed to parse post deleted event")
		return
	}
	if post == nil {
		return
	}

	ts := time.UnixMilli(post.DeleteAt)

	m.eventSender.QueueRemoteEvent(m.userLogin, &simplevent.MessageRemove{
		EventMeta: simplevent.EventMeta{
			Type: bridgev2.RemoteEventMessageRemove,
			LogContext: func(c zerolog.Context) zerolog.Context {
				return c.Str("post_id", post.Id).Str("channel_id", post.ChannelId)
			},
			PortalKey: makePortalKey(post.ChannelId),
			Sender:    m.senderFor(post.UserId),
			Timestamp: ts,
		},
		TargetMessage: MakeMessageID(post.Id),
	})
}

func (m *MattermostClient) handleReactionAdded(evt *model.WebSocketEvent) {
	reaction, err := m.parseReactionEvent(evt)
	if err != nil {
		m.log.Error().Err(err).Msg("Failed to parse reaction added event")
		return
	}
	if reaction == nil {
		return
	}

	ts := time.UnixMilli(reaction.CreateAt)
	emoji := reactionToEmoji(reaction.EmojiName)

	m.eventSender.QueueRemoteEvent(m.userLogin, &simplevent.Reaction{
		EventMeta: simplevent.EventMeta{
			Type: bridgev2.RemoteEventReaction,
			LogContext: func(c zerolog.Context) zerolog.Context {
				return c.Str("post_id", reaction.PostId).Str("emoji", reaction.EmojiName)
			},
			PortalKey: makePortalKey(evt.GetBroadcast().ChannelId),
			Sender:    m.senderFor(reaction.UserId),
			Timestamp: ts,
		},
		TargetMessage: MakeMessageID(reaction.PostId),
		EmojiID:       MakeEmojiID(reaction.EmojiName),
		Emoji:         emoji,
	})
}

func (m *MattermostClient) handleReactionRemoved(evt *model.WebSocketEvent) {
	reaction, err := m.parseReactionEvent(evt)
	if err != nil {
		m.log.Error().Err(err).Msg("Failed to parse reaction removed event")
		return
	}
	if reaction == nil {
		return
	}

	m.eventSender.QueueRemoteEvent(m.userLogin, &simplevent.Reaction{
		EventMeta: simplevent.EventMeta{
			Type: bridgev2.RemoteEventReactionRemove,
			LogContext: func(c zerolog.Context) zerolog.Context {
				return c.Str("post_id", reaction.PostId).Str("emoji", reaction.EmojiName)
			},
			PortalKey: makePortalKey(evt.GetBroadcast().ChannelId),
			Sender:    m.senderFor(reaction.UserId),
		},
		TargetMessage: MakeMessageID(reaction.PostId),
		EmojiID:       MakeEmojiID(reaction.EmojiName),
	})
}

func (m *MattermostClient) handleTyping(evt *model.WebSocketEvent) {
	userID, channelID, ok := m.parseTypingEvent(evt)
	if !ok {
		return
	}

	timeout := m.connector.Config.TypingTimeout
	if timeout <= 0 {
		timeout = 5
	}

	m.eventSender.QueueRemoteEvent(m.userLogin, &simplevent.Typing{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventTyping,
			PortalKey: makePortalKey(channelID),
			Sender:    m.senderFor(userID),
		},
		Timeout: time.Duration(timeout) * time.Second,
	})
}

func (m *MattermostClient) handleChannelViewed(evt *model.WebSocketEvent) {
	channelID, ok := m.parseChannelViewedEvent(evt)
	if !ok {
		return
	}

	m.eventSender.QueueRemoteEvent(m.userLogin, &simplevent.Receipt{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventReadReceipt,
			PortalKey: makePortalKey(channelID),
			Sender: bridgev2.EventSender{
				IsFromMe: true,
				Sender:   MakeUserID(m.userID),
			},
		},
	})
}

// convertPostToMatrix converts a Mattermost post to a bridgev2.ConvertedMessage.
// It does not transfer attachment contents; use convertPostToMatrixMedia for that.
func (m *MattermostClient) convertPostToMatrix(post *model.Post) *bridgev2.ConvertedMessage {
	return m.convertPostToMatrixMedia(context.Background(), post, nil)
}

// matrixMediaUploader uploads attachment bytes to the Matrix media repo.
type matrixMediaUploader func(ctx context.Context, data []byte, fileName, mimeType string) (id.ContentURIString, *event.EncryptedFileInfo, error)

// matrixUploader builds an uploader that stores media through intent for roomID.
func matrixUploader(intent bridgev2.MatrixAPI, roomID id.RoomID) matrixMediaUploader {
	if intent == nil {
		return nil
	}
	return func(ctx context.Context, data []byte, fileName, mimeType string) (id.ContentURIString, *event.EncryptedFileInfo, error) {
		return intent.UploadMedia(ctx, roomID, data, fileName, mimeType)
	}
}

// convertPostToMatrixMedia converts a post and, when upload is non-nil,
// transfers its attachments to Matrix.
func (m *MattermostClient) convertPostToMatrixMedia(ctx context.Context, post *model.Post, upload matrixMediaUploader) *bridgev2.ConvertedMessage {
	var parts []*bridgev2.ConvertedMessagePart

	// A voice-plugin post: the "🎤 Voice message (m:ss)" text is redundant next
	// to the Matrix voice event, so drop it.
	voice := parseMMVoice(post)
	skipText := voice != nil && len(post.FileIds) > 0 && isVoicePlaceholderText(post.Message)

	if post.Message != "" && !skipText {
		parts = append(parts, &bridgev2.ConvertedMessagePart{
			ID:      MakeMessagePartID(0),
			Type:    event.EventMessage,
			Content: m.textContentWithMentions(ctx, post.Message),
		})
	}

	for i, fileID := range post.FileIds {
		filePart := m.convertFileToMatrixMedia(ctx, fileID, i+1, upload)
		if filePart != nil {
			if voice != nil && filePart.Content.MsgType == event.MsgAudio {
				applyMMVoice(filePart.Content, voice)
				voice = nil // only the first audio file is the voice message
			}
			parts = append(parts, filePart)
		}
	}

	msg := &bridgev2.ConvertedMessage{
		Parts: parts,
	}

	if post.RootId != "" {
		replyTo := MakeMessageID(post.RootId)
		msg.ReplyTo = &networkid.MessageOptionalPartID{MessageID: replyTo}
	}

	return msg
}

// convertEditToMatrix converts an edited Mattermost post to a bridgev2.ConvertedEdit.
func (m *MattermostClient) convertEditToMatrix(post *model.Post, existing []*database.Message) *bridgev2.ConvertedEdit {
	var editParts []*bridgev2.ConvertedEditPart
	var targetPart *database.Message
	if len(existing) > 0 {
		targetPart = existing[0]
	}

	editParts = append(editParts, &bridgev2.ConvertedEditPart{
		Part:    targetPart,
		Type:    event.EventMessage,
		Content: m.textContentWithMentions(context.Background(), post.Message),
	})

	return &bridgev2.ConvertedEdit{
		ModifiedParts: editParts,
	}
}

// convertFileToMatrix converts a Mattermost file attachment to a Matrix message
// part without transferring its contents (metadata only).
func (m *MattermostClient) convertFileToMatrix(fileID string, partIndex int) *bridgev2.ConvertedMessagePart {
	return m.convertFileToMatrixMedia(context.Background(), fileID, partIndex, nil)
}

// convertFileToMatrixMedia converts a Mattermost file attachment to a Matrix
// message part. With a non-nil upload func the file is downloaded from
// Mattermost and uploaded to Matrix so the event carries a real media URL;
// if the transfer fails a text notice part is returned instead of a broken
// media event.
func (m *MattermostClient) convertFileToMatrixMedia(ctx context.Context, fileID string, partIndex int, upload matrixMediaUploader) *bridgev2.ConvertedMessagePart {
	fileInfo, _, err := m.client.GetFileInfo(ctx, fileID)
	if err != nil {
		m.log.Error().Err(err).Str("file_id", fileID).Msg("Failed to get file info")
		if upload != nil {
			return fileFailurePart(partIndex, fileID)
		}
		return nil
	}

	msgType := event.MsgFile
	mimeType := fileInfo.MimeType
	if mimeType == "" || mimeType == "application/octet-stream" {
		// Unknown type: guess from the extension, else transfer as a plain file.
		if guessed := mime.TypeByExtension(strings.ToLower(filepath.Ext(fileInfo.Name))); guessed != "" {
			mimeType = guessed
		}
	}
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		msgType = event.MsgImage
	case strings.HasPrefix(mimeType, "video/"):
		msgType = event.MsgVideo
	case strings.HasPrefix(mimeType, "audio/"):
		msgType = event.MsgAudio
	}

	content := &event.MessageEventContent{
		MsgType: msgType,
		Body:    fileInfo.Name,
		Info: &event.FileInfo{
			MimeType: mimeType,
			Size:     int(fileInfo.Size),
		},
	}
	if msgType == event.MsgImage {
		content.Info.Width = fileInfo.Width
		content.Info.Height = fileInfo.Height
	}
	if msgType == event.MsgAudio && isVoiceMessage(fileInfo.Name, mimeType) {
		// Render as a voice message (playable bubble) in Matrix clients.
		content.MSC3245Voice = &event.MSC3245Voice{}
	}

	if upload != nil {
		data, _, err := m.client.GetFile(ctx, fileID)
		if err != nil {
			m.log.Error().Err(err).Str("file_id", fileID).Msg("Failed to download file from Mattermost")
			return fileFailurePart(partIndex, fileInfo.Name)
		}
		url, encFile, err := upload(ctx, data, fileInfo.Name, mimeType)
		if err != nil {
			m.log.Error().Err(err).Str("file_id", fileID).Msg("Failed to upload file to Matrix")
			return fileFailurePart(partIndex, fileInfo.Name)
		}
		content.Info.Size = len(data)
		if encFile != nil {
			content.File = encFile
		} else {
			content.URL = url
		}
	}

	return &bridgev2.ConvertedMessagePart{
		ID:      MakeMessagePartID(partIndex),
		Type:    event.EventMessage,
		Content: content,
		Extra: map[string]any{
			"fi.mau.mattermost.file_id": fileID,
		},
	}
}

func fileFailurePart(partIndex int, name string) *bridgev2.ConvertedMessagePart {
	return &bridgev2.ConvertedMessagePart{
		ID:   MakeMessagePartID(partIndex),
		Type: event.EventMessage,
		Content: &event.MessageEventContent{
			MsgType: event.MsgNotice,
			Body:    fmt.Sprintf("[Attachment %q could not be bridged from Mattermost]", name),
		},
	}
}

// isVoiceMessage guesses whether a Mattermost audio attachment is a recorded
// voice message rather than a music/audio file.
func isVoiceMessage(name, mimeType string) bool {
	lower := strings.ToLower(name)
	if strings.Contains(lower, "voice") || strings.Contains(lower, "recording") {
		return true
	}
	switch strings.ToLower(strings.SplitN(mimeType, ";", 2)[0]) {
	case "audio/ogg", "audio/opus", "audio/webm":
		return true
	}
	return false
}

// isBridgeUsername returns true if the username belongs to a known bridge
// infrastructure bot that should never be relayed. It checks against
// hardcoded bridge usernames and an optional configurable prefix.
func isBridgeUsername(username, botPrefix string) bool {
	switch {
	case username == "mattermost-bridge":
		return true
	case strings.HasPrefix(username, "mattermost_"):
		// Ghost users created by the bridge (username_template: mattermost_{{.}})
		return true
	case botPrefix != "" && strings.HasPrefix(username, botPrefix):
		return true
	default:
		return false
	}
}

// reactionToEmoji converts a Mattermost emoji name to a Unicode emoji.
func reactionToEmoji(name string) string {
	emojiMap := map[string]string{
		"+1":               "\U0001f44d",
		"-1":               "\U0001f44e",
		"heart":            "\u2764\ufe0f",
		"smile":            "\U0001f604",
		"laughing":         "\U0001f606",
		"thumbsup":         "\U0001f44d",
		"thumbsdown":       "\U0001f44e",
		"wave":             "\U0001f44b",
		"clap":             "\U0001f44f",
		"fire":             "\U0001f525",
		"100":              "\U0001f4af",
		"tada":             "\U0001f389",
		"eyes":             "\U0001f440",
		"thinking":         "\U0001f914",
		"white_check_mark": "\u2705",
		"x":                "\u274c",
		"warning":          "\u26a0\ufe0f",
		"rocket":           "\U0001f680",
		"star":             "\u2b50",
		"pray":             "\U0001f64f",
	}

	if emoji, ok := emojiMap[name]; ok {
		return emoji
	}
	// Any other Mattermost system emoji: build the Unicode string from the
	// server's own table. Custom (server-specific) emoji are not in it and stay
	// as :name: text.
	if emoji, ok := mattermostEmojiNameToUnicode(name); ok {
		return emoji
	}
	return fmt.Sprintf(":%s:", name)
}

// mattermostEmojiNameToUnicode converts a Mattermost system emoji name to its
// Unicode string ("2764-fe0f" -> U+2764 U+FE0F).
func mattermostEmojiNameToUnicode(name string) (string, bool) {
	id, ok := model.GetSystemEmojiId(name)
	if !ok || id == "" {
		return "", false
	}
	var b strings.Builder
	for _, part := range strings.Split(id, "-") {
		cp, err := strconv.ParseInt(part, 16, 32)
		if err != nil || cp <= 0 {
			return "", false
		}
		b.WriteRune(rune(cp))
	}
	return b.String(), true
}
