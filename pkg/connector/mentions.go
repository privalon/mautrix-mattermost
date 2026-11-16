// Copyright 2024-2026 Remi Philippe
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"context"
	"html"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// User mentions are translated in both directions:
//   - Matrix -> Mattermost: a Matrix mention pill (link to matrix.to/#/@mxid)
//     becomes the user's native Mattermost "@username".
//   - Mattermost -> Matrix: "@username" becomes a Matrix pill plus m.mentions,
//     so the mentioned Matrix user is actually notified.

var (
	// matrixPillRe matches a user mention pill as sent by Matrix clients.
	matrixPillRe = regexp.MustCompile(`(?is)<a\s+[^>]*?href="https://matrix\.to/#/([^"]+)"[^>]*>(.*?)</a>`)
	htmlTagRe    = regexp.MustCompile(`<[^>]+>`)
	// codeSegmentRe matches code spans/blocks, which must never get pills.
	codeSegmentRe = regexp.MustCompile(`(?is)<pre>.*?</pre>|<code[^>]*>.*?</code>`)
	// mmMentionRe matches "@username" at a word boundary. Mattermost usernames
	// are lowercase letters, digits, '.', '-' and '_'.
	mmMentionRe = regexp.MustCompile(`(^|[^\w@/.:+-])@([A-Za-z0-9][A-Za-z0-9._-]{0,63})`)
)

// mmRoomMentions are Mattermost's broadcast mentions.
var mmRoomMentions = map[string]bool{"all": true, "channel": true, "here": true}

const (
	mentionCacheTTL    = 10 * time.Minute
	mentionMissCacheTL = 1 * time.Minute
)

type mentionCacheEntry struct {
	value string
	found bool
	at    time.Time
}

// mentionCache caches user lookups (including misses, briefly). The zero
// value is ready to use.
type mentionCache struct {
	mu sync.Mutex
	m  map[string]mentionCacheEntry
}

func (c *mentionCache) get(key string) (value string, found, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, exists := c.m[key]
	if !exists {
		return "", false, false
	}
	ttl := mentionCacheTTL
	if !e.found {
		ttl = mentionMissCacheTL
	}
	if time.Since(e.at) > ttl {
		delete(c.m, key)
		return "", false, false
	}
	return e.value, e.found, true
}

func (c *mentionCache) put(key, value string, found bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]mentionCacheEntry)
	}
	if len(c.m) > 5000 {
		c.m = make(map[string]mentionCacheEntry)
	}
	c.m[key] = mentionCacheEntry{value: value, found: found, at: time.Now()}
}

// ---------------------------------------------------------------------------
// Matrix -> Mattermost
// ---------------------------------------------------------------------------

// rewriteMatrixMentions replaces Matrix user pills in an HTML body with the
// user's Mattermost "@username" (lookup returns it), or with the pill's plain
// text when the user has no Mattermost identity. Non-user links are untouched.
func rewriteMatrixMentions(formatted string, lookup func(mxid id.UserID) (username string, ok bool)) string {
	return matrixPillRe.ReplaceAllStringFunc(formatted, func(match string) string {
		parts := matrixPillRe.FindStringSubmatch(match)
		target := parts[1]
		if dec, err := url.PathUnescape(target); err == nil {
			target = dec
		}
		if i := strings.IndexByte(target, '?'); i >= 0 {
			target = target[:i]
		}
		if !strings.HasPrefix(target, "@") {
			return match // room/event links stay links
		}
		if username, ok := lookup(id.UserID(target)); ok && username != "" {
			return "@" + username
		}
		return html.EscapeString(html.UnescapeString(strings.TrimSpace(htmlTagRe.ReplaceAllString(parts[2], ""))))
	})
}

// matrixContentForMattermost returns content with Matrix mention pills
// rewritten to native Mattermost handles, ready for matrixfmt.Parse.
func (m *MattermostClient) matrixContentForMattermost(ctx context.Context, content *event.MessageEventContent) *event.MessageEventContent {
	if content == nil || content.Format != event.FormatHTML || content.FormattedBody == "" ||
		!matrixPillRe.MatchString(content.FormattedBody) {
		return content
	}
	cp := *content
	cp.FormattedBody = rewriteMatrixMentions(content.FormattedBody, func(mxid id.UserID) (string, bool) {
		return m.mattermostUsernameForMXID(ctx, mxid)
	})
	return &cp
}

// mattermostUsernameForMXID resolves a Matrix user ID to a Mattermost username:
// a configured puppet bot, a bridge ghost, or a real Matrix user with a login.
func (m *MattermostClient) mattermostUsernameForMXID(ctx context.Context, mxid id.UserID) (string, bool) {
	if m.connector == nil {
		return "", false
	}
	m.connector.puppetMu.RLock()
	puppet, isPuppet := m.connector.Puppets[mxid]
	m.connector.puppetMu.RUnlock()
	if isPuppet && puppet.Username != "" {
		return puppet.Username, true
	}

	mmUserID := ""
	if br := m.connector.Bridge; br != nil {
		if br.Matrix != nil {
			if uid, ok := br.Matrix.ParseGhostMXID(mxid); ok {
				mmUserID = ParseUserID(uid)
			}
		}
		if mmUserID == "" && br.DB != nil {
			if user, err := br.GetExistingUserByMXID(ctx, mxid); err == nil && user != nil {
				if login := user.GetDefaultLogin(); login != nil {
					mmUserID = ParseUserLoginID(login.ID)
				}
			}
		}
	}
	if mmUserID == "" {
		return "", false
	}
	return m.mattermostUsernameByID(ctx, mmUserID)
}

func (m *MattermostClient) mattermostUsernameByID(ctx context.Context, userID string) (string, bool) {
	key := "id:" + userID
	if v, found, ok := m.mentions.get(key); ok {
		return v, found
	}
	if m.client == nil {
		return "", false
	}
	user, _, err := m.client.GetUser(ctx, userID, "")
	if err != nil || user == nil || user.Username == "" {
		m.mentions.put(key, "", false)
		return "", false
	}
	m.mentions.put(key, user.Username, true)
	return user.Username, true
}

// ---------------------------------------------------------------------------
// Mattermost -> Matrix
// ---------------------------------------------------------------------------

// linkMentions turns "@username" in an HTML body (outside code) into Matrix
// pills. lookup maps a lowercase Mattermost username to a Matrix user ID.
// It returns the new body and the mentioned users; room is true for
// @all/@channel/@here.
func linkMentions(formatted string, lookup func(username string) (id.UserID, bool)) (out string, users []id.UserID, room bool) {
	var sb strings.Builder
	last := 0
	process := func(seg string) string {
		return mmMentionRe.ReplaceAllStringFunc(seg, func(match string) string {
			parts := mmMentionRe.FindStringSubmatch(match)
			prefix, name := parts[1], parts[2]
			// Trailing punctuation belongs to the sentence, not the name.
			trimmed := strings.TrimRight(name, ".-_")
			tail := name[len(trimmed):]
			if trimmed == "" {
				return match
			}
			lower := strings.ToLower(trimmed)
			if mmRoomMentions[lower] {
				room = true
				return match
			}
			mxid, ok := lookup(lower)
			if !ok {
				return match
			}
			users = appendUnique(users, mxid)
			return prefix + `<a href="https://matrix.to/#/` + url.PathEscape(string(mxid)) + `">@` + trimmed + `</a>` + tail
		})
	}
	for _, loc := range codeSegmentRe.FindAllStringIndex(formatted, -1) {
		sb.WriteString(process(formatted[last:loc[0]]))
		sb.WriteString(formatted[loc[0]:loc[1]]) // code: untouched
		last = loc[1]
	}
	sb.WriteString(process(formatted[last:]))
	return sb.String(), users, room
}

func appendUnique(list []id.UserID, v id.UserID) []id.UserID {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// hasMMMention is a cheap pre-check so messages without "@" never touch the API.
func hasMMMention(text string) bool {
	return strings.Contains(text, "@") && mmMentionRe.MatchString(text)
}

// textContentWithMentions converts Mattermost markdown to Matrix content and
// links @mentions to Matrix users (pill + m.mentions).
func (m *MattermostClient) textContentWithMentions(ctx context.Context, text string) *event.MessageEventContent {
	parsed := mattermostfmtParse(text)
	content := &event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          parsed.Body,
		Format:        parsed.Format,
		FormattedBody: parsed.FormattedBody,
	}
	if !hasMMMention(text) {
		return content
	}

	formatted := parsed.FormattedBody
	if formatted == "" {
		formatted = strings.ReplaceAll(html.EscapeString(parsed.Body), "\n", "<br/>")
	}
	linked, users, room := linkMentions(formatted, func(username string) (id.UserID, bool) {
		return m.matrixUserForMattermostUsername(ctx, username)
	})
	if len(users) == 0 && !room {
		return content
	}
	if len(users) > 0 {
		content.Format = event.FormatHTML
		content.FormattedBody = linked
	}
	content.Mentions = &event.Mentions{UserIDs: users, Room: room}
	return content
}

// matrixUserForMattermostUsername resolves a Mattermost username to the Matrix
// user that represents it: the user's own account, a double-puppeted real
// Matrix user, a puppet bot, or the bridge ghost.
func (m *MattermostClient) matrixUserForMattermostUsername(ctx context.Context, username string) (id.UserID, bool) {
	key := "name:" + username
	if v, found, ok := m.mentions.get(key); ok {
		return id.UserID(v), found
	}
	if m.client == nil {
		return "", false
	}
	user, _, err := m.client.GetUserByUsername(ctx, username, "")
	if err != nil || user == nil || user.Id == "" {
		m.mentions.put(key, "", false)
		return "", false
	}
	mxid := m.matrixUserForMattermostID(user.Id)
	if mxid == "" {
		m.mentions.put(key, "", false)
		return "", false
	}
	m.mentions.put(key, string(mxid), true)
	return mxid, true
}

func (m *MattermostClient) matrixUserForMattermostID(mmUserID string) id.UserID {
	if m.connector == nil {
		return ""
	}
	if mmUserID == m.userID && m.userLogin != nil && m.userLogin.UserMXID != "" {
		return m.userLogin.UserMXID
	}
	m.connector.puppetMu.RLock()
	for mxid, p := range m.connector.Puppets {
		if p.UserID == mmUserID {
			m.connector.puppetMu.RUnlock()
			return mxid
		}
	}
	m.connector.puppetMu.RUnlock()

	br := m.connector.Bridge
	if br == nil {
		return ""
	}
	if loginID, ok := m.connector.DoublePuppetLoginID(mmUserID); ok {
		if login := br.GetCachedUserLoginByID(loginID); login != nil && login.UserMXID != "" {
			return login.UserMXID
		}
	}
	if br.Matrix != nil {
		if intent := br.Matrix.GhostIntent(MakeUserID(mmUserID)); intent != nil {
			return intent.GetMXID()
		}
	}
	return ""
}
