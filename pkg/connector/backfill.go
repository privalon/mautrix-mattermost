// Copyright 2024-2026 Remi Philippe
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// Compile-time assertion that MattermostClient implements BackfillingNetworkAPI.
var _ bridgev2.BackfillingNetworkAPI = (*MattermostClient)(nil)

// FetchMessages implements bridgev2.BackfillingNetworkAPI.
func (m *MattermostClient) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	channelID := ParsePortalID(params.Portal.ID)

	// Replies are part of the channel's own post list (each carries ReplyTo),
	// so there is nothing separate to fetch for a thread. bridgev2 only asks
	// for one when a backfilled message sets ShouldBackfillThread; answering
	// such a request with the channel's posts again would re-send them.
	if params.ThreadRoot != "" {
		return &bridgev2.FetchMessagesResponse{Forward: params.Forward}, nil
	}

	maxCount := m.connector.Config.BackfillMaxCount
	if maxCount <= 0 {
		maxCount = 100
	}
	if params.Count > 0 {
		maxCount = params.Count
	}

	perPage := maxCount
	if perPage > 200 {
		perPage = 200
	}

	var postList *model.PostList
	var err error

	if params.Forward && params.AnchorMessage != nil {
		anchorPostID := ParseMessageID(params.AnchorMessage.ID)
		postList, _, err = m.client.GetPostsAfter(ctx, channelID, anchorPostID, 0, perPage, "", false, false)
	} else if params.AnchorMessage != nil {
		anchorPostID := ParseMessageID(params.AnchorMessage.ID)
		postList, _, err = m.client.GetPostsBefore(ctx, channelID, anchorPostID, 0, perPage, "", false, false)
	} else {
		postList, _, err = m.client.GetPostsForChannel(ctx, channelID, 0, perPage, "", false, false)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to fetch posts for backfill: %w", err)
	}

	// Sort chronologically (oldest first).
	posts := postList.ToSlice()
	sort.Slice(posts, func(i, j int) bool {
		return posts[i].CreateAt < posts[j].CreateAt
	})

	if len(posts) > maxCount {
		posts = posts[:maxCount]
	}

	var messages []*bridgev2.BackfillMessage
	for _, post := range posts {
		// Skip system messages.
		if post.Type != "" && post.Type != model.PostTypeDefault {
			continue
		}

		converted := m.convertPostToMatrix(post)

		msg := &bridgev2.BackfillMessage{
			ConvertedMessage: converted,
			Sender: bridgev2.EventSender{
				Sender:   MakeUserID(post.UserId),
				IsFromMe: post.UserId == m.userID,
			},
			ID:        MakeMessageID(post.Id),
			Timestamp: time.UnixMilli(post.CreateAt),
		}

		// ShouldBackfillThread is deliberately never set: replies already
		// arrive in this same list (with ReplyTo), and on a homeserver
		// without batch send bridgev2 would run a thread backfill per reply
		// that replays the channel, duplicating up to a page of messages
		// per reply in Matrix.

		messages = append(messages, msg)
	}

	hasMore := len(postList.Order) >= perPage

	resp := &bridgev2.FetchMessagesResponse{
		Messages: messages,
		HasMore:  hasMore,
		Forward:  params.Forward,
	}

	// Use PrevPostId as cursor for backward pagination.
	if !params.Forward && postList.PrevPostId != "" {
		resp.Cursor = networkid.PaginationCursor(postList.PrevPostId)
	}

	return resp, nil
}
