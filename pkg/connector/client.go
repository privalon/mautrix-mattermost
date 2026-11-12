// Copyright 2024-2026 Remi Philippe
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
)

// remoteEventSender is an interface for queuing remote events. This allows
// tests to inject a mock instead of requiring a full bridgev2.Bridge.
type remoteEventSender interface {
	QueueRemoteEvent(login *bridgev2.UserLogin, evt bridgev2.RemoteEvent)
}

// bridgeEventSender is the production implementation that delegates to the bridge.
type bridgeEventSender struct {
	bridge *bridgev2.Bridge
}

func (b *bridgeEventSender) QueueRemoteEvent(login *bridgev2.UserLogin, evt bridgev2.RemoteEvent) {
	b.bridge.QueueRemoteEvent(login, evt)
}

// MattermostClient represents a single authenticated Mattermost user connection.
type MattermostClient struct {
	connector   *MattermostConnector
	userLogin   *bridgev2.UserLogin
	eventSender remoteEventSender

	client    *model.Client4
	wsClient  *model.WebSocketClient
	userID    string
	teamID    string
	serverURL string

	stopOnce sync.Once
	stopChan chan struct{}
	log      zerolog.Logger

	// sentPending holds the pending_post_id of posts this bridge created from
	// Matrix, so the WebSocket echo of exactly those posts is skipped while the
	// user's own posts from other Mattermost clients are still bridged.
	sentPending sync.Map // pending post id (string) -> time.Time

	// wsReconnecting guards against two reconnect loops running at once.
	wsReconnecting atomic.Bool
	// wsConnect and wsBackoffBase exist so tests can drive the reconnect loop.
	wsConnect     func() error
	wsBackoffBase time.Duration
}

const (
	// memberPageSize is how many channel members are fetched per channel. A
	// channel with at least this many members is treated as having a partial
	// member list (see channelMembersToChatMembers).
	memberPageSize = 200

	// pendingEchoTTL is how long a Matrix-originated post is remembered for
	// echo suppression.
	pendingEchoTTL = 5 * time.Minute

	wsBackoffBaseDefault = 5 * time.Second
	wsBackoffMax         = 5 * time.Minute
)

var (
	_ bridgev2.NetworkAPI                    = (*MattermostClient)(nil)
	_ bridgev2.EditHandlingNetworkAPI        = (*MattermostClient)(nil)
	_ bridgev2.ReactionHandlingNetworkAPI    = (*MattermostClient)(nil)
	_ bridgev2.RedactionHandlingNetworkAPI   = (*MattermostClient)(nil)
	_ bridgev2.ReadReceiptHandlingNetworkAPI = (*MattermostClient)(nil)
	_ bridgev2.TypingHandlingNetworkAPI      = (*MattermostClient)(nil)
)

// NewMattermostClient creates a new client from an existing user login.
func NewMattermostClient(login *bridgev2.UserLogin, connector *MattermostConnector) *MattermostClient {
	log := login.Log.With().Str("component", "mm_client").Logger()
	mc := &MattermostClient{
		connector:   connector,
		userLogin:   login,
		eventSender: &bridgeEventSender{bridge: connector.Bridge},
		stopChan:    make(chan struct{}),
		log:         log,
	}
	meta := login.Metadata.(*UserLoginMetadata)
	if meta == nil {
		return mc
	}
	// Always restore the MM user ID so IsThisUser() works for all login
	// types, including lightweight double-puppet-only logins.
	mc.userID = meta.UserID
	mc.teamID = meta.TeamID
	mc.serverURL = normalizeServerURL(meta.ServerURL)
	if meta.Token != "" && !meta.DoublePuppetOnly {
		mc.client = model.NewAPIv4Client(mc.serverURL)
		mc.client.SetToken(meta.Token)
	}
	return mc
}

// Connect implements bridgev2.NetworkAPI. It does not return an error;
// connection errors are reported via BridgeState.
func (m *MattermostClient) Connect(ctx context.Context) {
	// Double-puppet-only logins have no MM client and don't need a connection.
	// Re-register in the dpLogins map so the mapping survives restarts.
	meta, _ := m.userLogin.Metadata.(*UserLoginMetadata)
	if meta != nil && meta.DoublePuppetOnly {
		m.connector.dpLoginsMu.Lock()
		m.connector.dpLogins[meta.UserID] = m.userLogin.ID
		m.connector.dpLoginsMu.Unlock()
		m.log.Info().
			Str("mm_user_id", meta.UserID).
			Str("matrix_mxid", string(m.userLogin.UserMXID)).
			Msg("Restored double-puppet-only login")
		return
	}

	if m.client == nil {
		m.log.Warn().Msg("Client not initialized, login first")
		m.userLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Error:      "mm-not-logged-in",
			Message:    "Not logged in to Mattermost",
		})
		return
	}

	m.log.Info().Str("server_url", m.serverURL).Msg("Connecting to Mattermost")

	me, _, err := m.client.GetMe(ctx, "")
	if err != nil {
		m.log.Error().Err(err).Msg("Failed to verify Mattermost session")
		m.userLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Error:      "mm-token-invalid",
			Message:    "Mattermost authentication token is invalid",
		})
		return
	}
	m.userID = me.Id
	m.log.Info().Str("user_id", me.Id).Str("username", me.Username).Msg("Authenticated")

	if m.teamID == "" {
		teams, _, err := m.client.GetTeamsForUser(ctx, m.userID, "")
		if err != nil {
			m.log.Error().Err(err).Msg("Failed to get teams")
			m.userLogin.BridgeState.Send(status.BridgeState{
				StateEvent: status.StateUnknownError,
				Error:      "mm-teams-failed",
				Message:    "Failed to get teams",
			})
			return
		}
		if len(teams) > 0 {
			m.teamID = teams[0].Id
		}
	}

	if err := m.connectWebSocket(); err != nil {
		m.log.Error().Err(err).Msg("WebSocket connection failed")
		m.userLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateTransientDisconnect,
			Error:      "mm-ws-failed",
			Message:    "WebSocket connection failed",
		})
		// Keep trying instead of staying disconnected until the next restart;
		// the channel sync runs as soon as a connection succeeds.
		go m.reconnectWebSocket(true)
		return
	}

	m.userLogin.BridgeState.Send(status.BridgeState{
		StateEvent: status.StateConnected,
	})

	// Sync existing channels to create portal rooms in Matrix.
	go m.syncChannels(ctx)
}

func (m *MattermostClient) connectWebSocket() error {
	if m.wsConnect != nil {
		return m.wsConnect()
	}
	wsURL := httpToWS(normalizeServerURL(m.serverURL))
	var err error
	m.wsClient, err = model.NewWebSocketClient4(wsURL, m.client.AuthToken)
	if err != nil {
		return fmt.Errorf("failed to create websocket client: %w", err)
	}
	m.wsClient.Listen()

	go m.listenWebSocket()

	m.log.Info().Str("ws_url", wsURL).Msg("WebSocket connected")
	return nil
}

// httpToWS converts an HTTP(S) URL to a WS(S) URL.
func httpToWS(url string) string {
	if strings.HasPrefix(url, "https://") {
		return "wss://" + strings.TrimPrefix(url, "https://")
	}
	if strings.HasPrefix(url, "http://") {
		return "ws://" + strings.TrimPrefix(url, "http://")
	}
	return url
}

func (m *MattermostClient) listenWebSocket() {
	for {
		select {
		case <-m.stopChan:
			return
		case event, ok := <-m.wsClient.EventChannel:
			if !ok {
				m.log.Warn().Msg("WebSocket event channel closed, reconnecting")
				m.handleWebSocketDisconnect()
				return
			}
			if event == nil {
				continue
			}
			m.handleEvent(event)
		}
	}
}

// sendBridgeState reports a bridge state, tolerating a client without a user
// login (unit tests, lightweight logins).
func (m *MattermostClient) sendBridgeState(state status.BridgeState) {
	if m.userLogin == nil || m.userLogin.BridgeState == nil {
		return
	}
	m.userLogin.BridgeState.Send(state)
}

func (m *MattermostClient) handleWebSocketDisconnect() {
	m.sendBridgeState(status.BridgeState{
		StateEvent: status.StateTransientDisconnect,
		Error:      "mm-ws-disconnected",
		Message:    "WebSocket disconnected, reconnecting",
	})
	// Posts made while disconnected are not replayed by the server, so resync
	// the channels afterwards (backfill only fetches what is newer than the
	// last bridged message).
	m.reconnectWebSocket(true)
}

// reconnectWebSocket keeps trying to re-establish the WebSocket with
// exponential backoff until it succeeds or the client is stopped. A single
// failed attempt used to leave the bridge permanently deaf.
func (m *MattermostClient) reconnectWebSocket(resync bool) {
	if !m.wsReconnecting.CompareAndSwap(false, true) {
		return
	}
	defer m.wsReconnecting.Store(false)

	base := m.wsBackoffBase
	if base <= 0 {
		base = wsBackoffBaseDefault
	}
	delay := base
	for attempt := 1; ; attempt++ {
		select {
		case <-m.stopChan:
			return
		case <-time.After(delay):
		}
		err := m.connectWebSocket()
		if err == nil {
			m.log.Info().Int("attempt", attempt).Msg("WebSocket reconnected")
			m.sendBridgeState(status.BridgeState{StateEvent: status.StateConnected})
			if resync {
				go m.syncChannels(context.Background())
			}
			return
		}
		m.log.Warn().Err(err).Int("attempt", attempt).Dur("retry_in", delay*2).Msg("Failed to reconnect WebSocket")
		m.sendBridgeState(status.BridgeState{
			StateEvent: status.StateTransientDisconnect,
			Error:      "mm-ws-reconnect-failed",
			Message:    "Failed to reconnect WebSocket, retrying",
		})
		delay *= 2
		if delay > wsBackoffMax {
			delay = wsBackoffMax
		}
	}
}

// syncChannels fetches all Mattermost channels the user is a member of
// (including DMs and group DMs) and queues ChatResync events so the bridge
// creates portal rooms in Matrix.
func (m *MattermostClient) syncChannels(ctx context.Context) {
	channelMap := make(map[string]*model.Channel)

	// Fetch team channels if we have a team ID.
	if m.teamID != "" {
		channels, _, err := m.client.GetChannelsForTeamForUser(ctx, m.teamID, m.userID, false, "")
		if err != nil {
			m.log.Error().Err(err).Msg("Failed to fetch team channels for sync")
		} else {
			for _, ch := range channels {
				channelMap[ch.Id] = ch
			}
		}
	}

	// Fetch all channels including DMs/group DMs (cross-team).
	allChannels, _, err := m.client.GetChannelsForUserWithLastDeleteAt(ctx, m.userID, 0)
	if err != nil {
		m.log.Error().Err(err).Msg("Failed to fetch all user channels for sync")
		if len(channelMap) == 0 {
			return
		}
	} else {
		for _, ch := range allChannels {
			channelMap[ch.Id] = ch
		}
	}

	m.log.Info().Int("count", len(channelMap)).Msg("Syncing channels")

	for _, ch := range channelMap {
		m.log.Debug().
			Str("channel_id", ch.Id).
			Str("channel_name", ch.Name).
			Str("channel_type", string(ch.Type)).
			Msg("Syncing channel")

		members, _, err := m.client.GetChannelMembers(ctx, ch.Id, 0, memberPageSize, "")
		if err != nil {
			m.log.Warn().Err(err).Str("channel_id", ch.Id).Msg("Failed to get channel members")
			continue
		}

		chatInfo := m.channelToChatInfo(ch, members)

		var checkBackfill func(ctx context.Context, latestMessage *database.Message) (bool, error)
		var latestMessageTS time.Time
		if m.connector.Config.BackfillEnabled && ch.LastPostAt > 0 {
			lastPostAt := ch.LastPostAt
			latestMessageTS = time.UnixMilli(lastPostAt)
			checkBackfill = func(_ context.Context, latestMessage *database.Message) (bool, error) {
				if latestMessage == nil {
					return true, nil
				}
				return latestMessage.Timestamp.Before(time.UnixMilli(lastPostAt)), nil
			}
		}

		m.eventSender.QueueRemoteEvent(m.userLogin, &simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventChatResync,
				PortalKey: makePortalKey(ch.Id),
				LogContext: func(c zerolog.Context) zerolog.Context {
					return c.Str("channel_id", ch.Id).Str("channel_name", ch.Name)
				},
				CreatePortal: true,
			},
			ChatInfo:               chatInfo,
			LatestMessageTS:        latestMessageTS,
			CheckNeedsBackfillFunc: checkBackfill,
		})
	}

	m.log.Info().Msg("Channel sync complete")
}

// Disconnect closes the WebSocket connection and stops the client's event loop.
func (m *MattermostClient) Disconnect() {
	m.stopOnce.Do(func() {
		close(m.stopChan)
	})
	if m.wsClient != nil {
		m.wsClient.Close()
		m.wsClient = nil
	}
}

// IsLoggedIn reports whether the client holds a valid authentication token.
func (m *MattermostClient) IsLoggedIn() bool {
	return m.client != nil && m.client.AuthToken != ""
}

func (m *MattermostClient) LogoutRemote(ctx context.Context) {
	if m.client != nil {
		_, _ = m.client.Logout(ctx)
	}
	m.Disconnect()
}

// IsThisUser reports whether the given network user ID matches this client's Mattermost user.
func (m *MattermostClient) IsThisUser(_ context.Context, userID networkid.UserID) bool {
	return string(userID) == m.userID
}

func (m *MattermostClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	if m.client == nil {
		return nil, bridgev2.ErrNotLoggedIn
	}
	channelID := ParsePortalID(portal.ID)
	channel, _, err := m.client.GetChannel(ctx, channelID, "")
	if err != nil {
		return nil, fmt.Errorf("failed to get channel info: %w", err)
	}

	members, _, err := m.client.GetChannelMembers(ctx, channelID, 0, memberPageSize, "")
	if err != nil {
		return nil, fmt.Errorf("failed to get channel members: %w", err)
	}

	return m.channelToChatInfo(channel, members), nil
}

func (m *MattermostClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	if m.client == nil {
		return nil, bridgev2.ErrNotLoggedIn
	}
	mmUserID := ParseUserID(ghost.ID)
	user, _, err := m.client.GetUser(ctx, mmUserID, "")
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %w", err)
	}
	return m.mmUserToUserInfo(user), nil
}

func (m *MattermostClient) GetCapabilities(_ context.Context, _ *bridgev2.Portal) *event.RoomFeatures {
	return &event.RoomFeatures{
		Formatting: event.FormattingFeatureMap{
			event.FmtBold:          event.CapLevelFullySupported,
			event.FmtItalic:        event.CapLevelFullySupported,
			event.FmtStrikethrough: event.CapLevelFullySupported,
			event.FmtInlineCode:    event.CapLevelFullySupported,
			event.FmtCodeBlock:     event.CapLevelFullySupported,
			event.FmtBlockquote:    event.CapLevelFullySupported,
			event.FmtInlineLink:    event.CapLevelFullySupported,
			event.FmtUserLink:      event.CapLevelFullySupported,
			event.FmtUnorderedList: event.CapLevelFullySupported,
			event.FmtOrderedList:   event.CapLevelFullySupported,
			event.FmtHeaders:       event.CapLevelFullySupported,
		},
		File: event.FileFeatureMap{
			event.MsgImage: {
				MimeTypes: map[string]event.CapabilitySupportLevel{
					"image/*": event.CapLevelFullySupported,
				},
				MaxSize: 100 * 1024 * 1024,
				Caption: event.CapLevelFullySupported,
			},
			event.MsgVideo: {
				MimeTypes: map[string]event.CapabilitySupportLevel{
					"video/*": event.CapLevelFullySupported,
				},
				MaxSize: 100 * 1024 * 1024,
				Caption: event.CapLevelFullySupported,
			},
			event.MsgAudio: {
				MimeTypes: map[string]event.CapabilitySupportLevel{
					"audio/*": event.CapLevelFullySupported,
				},
				MaxSize: 100 * 1024 * 1024,
			},
			event.MsgFile: {
				MimeTypes: map[string]event.CapabilitySupportLevel{
					"*/*": event.CapLevelFullySupported,
				},
				MaxSize: 100 * 1024 * 1024,
			},
		},
		MaxTextLength:       16383,
		Reply:               event.CapLevelFullySupported,
		Edit:                event.CapLevelFullySupported,
		Delete:              event.CapLevelFullySupported,
		Reaction:            event.CapLevelFullySupported,
		ReadReceipts:        true,
		TypingNotifications: true,
	}
}

// rememberPendingPost records the pending_post_id of a post this bridge is about
// to create from Matrix. It must be called before the post is created: the
// WebSocket echo can arrive before the REST call returns.
func (m *MattermostClient) rememberPendingPost(pendingID string) {
	if pendingID == "" {
		return
	}
	now := time.Now()
	m.sentPending.Range(func(k, v any) bool {
		if t, ok := v.(time.Time); ok && now.Sub(t) > pendingEchoTTL {
			m.sentPending.Delete(k)
		}
		return true
	})
	m.sentPending.Store(pendingID, now)
}

// consumePendingEcho reports whether pendingID belongs to a post created by this
// bridge (and forgets it), i.e. whether the WebSocket event is our own echo.
func (m *MattermostClient) consumePendingEcho(pendingID string) bool {
	if pendingID == "" {
		return false
	}
	v, ok := m.sentPending.LoadAndDelete(pendingID)
	if !ok {
		return false
	}
	t, _ := v.(time.Time)
	return time.Since(t) <= pendingEchoTTL
}
