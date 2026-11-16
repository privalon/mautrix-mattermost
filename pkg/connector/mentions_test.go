// Copyright 2024-2026 Remi Philippe
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"context"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestRewriteMatrixMentions(t *testing.T) {
	t.Parallel()
	lookup := func(mxid id.UserID) (string, bool) {
		switch mxid {
		case "@mattermost_abc:example.com":
			return "alice", true
		case "@bob:example.com":
			return "bob.smith", true
		}
		return "", false
	}
	tests := []struct{ name, in, want string }{
		{"ghost pill", `hi <a href="https://matrix.to/#/@mattermost_abc:example.com">Alice A</a>!`, `hi @alice!`},
		{"encoded mxid", `<a href="https://matrix.to/#/%40bob%3Aexample.com">Bob</a>`, `@bob.smith`},
		{"via params", `<a href="https://matrix.to/#/@bob:example.com?via=example.com">Bob</a>`, `@bob.smith`},
		{"unknown user falls back to text", `<a href="https://matrix.to/#/@nobody:example.com">No <b>Body</b></a>`, `No Body`},
		{"room link untouched", `<a href="https://matrix.to/#/#room:example.com">room</a>`, `<a href="https://matrix.to/#/#room:example.com">room</a>`},
		{"normal link untouched", `<a href="https://example.com/@alice">x</a>`, `<a href="https://example.com/@alice">x</a>`},
		{"two pills", `<a href="https://matrix.to/#/@bob:example.com">B</a> and <a href="https://matrix.to/#/@mattermost_abc:example.com">A</a>`, `@bob.smith and @alice`},
		{"no pills", `plain <em>text</em>`, `plain <em>text</em>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := rewriteMatrixMentions(tt.in, lookup); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLinkMentions(t *testing.T) {
	t.Parallel()
	lookup := func(name string) (id.UserID, bool) {
		switch name {
		case "alice":
			return "@alice:example.com", true
		case "bob.smith":
			return "@bob:example.com", true
		}
		return "", false
	}
	tests := []struct {
		name      string
		in        string
		want      string
		wantUsers []id.UserID
		wantRoom  bool
	}{
		{"simple", "hi @alice", `hi <a href="https://matrix.to/#/@alice:example.com">@alice</a>`, []id.UserID{"@alice:example.com"}, false},
		{"sentence punctuation", "thanks @alice.", `thanks <a href="https://matrix.to/#/@alice:example.com">@alice</a>.`, []id.UserID{"@alice:example.com"}, false},
		{"dotted username", "@bob.smith ok", `<a href="https://matrix.to/#/@bob:example.com">@bob.smith</a> ok`, []id.UserID{"@bob:example.com"}, false},
		{"case insensitive", "@Alice", `<a href="https://matrix.to/#/@alice:example.com">@Alice</a>`, []id.UserID{"@alice:example.com"}, false},
		{"duplicate counted once", "@alice @alice", `<a href="https://matrix.to/#/@alice:example.com">@alice</a> <a href="https://matrix.to/#/@alice:example.com">@alice</a>`, []id.UserID{"@alice:example.com"}, false},
		{"unknown left alone", "@ghosty hi", "@ghosty hi", nil, false},
		{"email untouched", "mail me at x@alice.com", "mail me at x@alice.com", nil, false},
		{"url untouched", "see https://example.com/@alice", "see https://example.com/@alice", nil, false},
		{"inline code untouched", "run <code>@alice</code> now", "run <code>@alice</code> now", nil, false},
		{"code block untouched", "<pre><code>@alice</code></pre> @bob.smith", `<pre><code>@alice</code></pre> <a href="https://matrix.to/#/@bob:example.com">@bob.smith</a>`, []id.UserID{"@bob:example.com"}, false},
		{"channel broadcast", "@channel heads up", "@channel heads up", nil, true},
		{"here and all", "@here @all", "@here @all", nil, true},
		{"lone at", "@ alice", "@ alice", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, users, room := linkMentions(tt.in, lookup)
			if got != tt.want {
				t.Errorf("body: got %q, want %q", got, tt.want)
			}
			if len(users) != len(tt.wantUsers) {
				t.Fatalf("users: got %v, want %v", users, tt.wantUsers)
			}
			for i := range users {
				if users[i] != tt.wantUsers[i] {
					t.Errorf("users[%d]: got %v, want %v", i, users[i], tt.wantUsers[i])
				}
			}
			if room != tt.wantRoom {
				t.Errorf("room: got %v, want %v", room, tt.wantRoom)
			}
		})
	}
}

// mentionTestClient returns a client whose fake Mattermost knows alice and a
// puppet bot, with the bot registered as a Matrix puppet.
func mentionTestClient(t *testing.T) (*MattermostClient, *fakeMM) {
	t.Helper()
	fake := newFakeMM()
	t.Cleanup(fake.Close)
	fake.Users["alice-id"] = &model.User{Id: "alice-id", Username: "alice"}
	fake.Users["bot-id"] = &model.User{Id: "bot-id", Username: "agent-bot"}

	mc := newFullTestClient(fake.Server.URL)
	mc.connector.Puppets["@agent:example.com"] = &PuppetClient{
		MXID: "@agent:example.com", UserID: "bot-id", Username: "agent-bot",
	}
	return mc, fake
}

func TestConvertPostToMatrix_MentionBecomesPillAndMentions(t *testing.T) {
	t.Parallel()
	mc, _ := mentionTestClient(t)
	mc.connector.Puppets["@alice:example.com"] = &PuppetClient{MXID: "@alice:example.com", UserID: "alice-id", Username: "alice"}

	msg := mc.convertPostToMatrix(&model.Post{Id: "p1", Message: "hello @alice and @agent-bot, also @nobody"})
	c := msg.Parts[0].Content

	if c.Format != event.FormatHTML {
		t.Fatalf("expected HTML, got %q", c.Format)
	}
	for _, want := range []string{
		`<a href="https://matrix.to/#/@alice:example.com">@alice</a>`,
		`<a href="https://matrix.to/#/@agent:example.com">@agent-bot</a>`,
		`@nobody`,
	} {
		if !strings.Contains(c.FormattedBody, want) {
			t.Errorf("formatted body missing %q: %s", want, c.FormattedBody)
		}
	}
	if strings.Contains(c.FormattedBody, `href="https://matrix.to/#/@nobody`) {
		t.Error("unknown user must not become a pill")
	}
	if c.Mentions == nil || !c.Mentions.Has("@alice:example.com") || !c.Mentions.Has("@agent:example.com") || len(c.Mentions.UserIDs) != 2 {
		t.Errorf("m.mentions: %+v", c.Mentions)
	}
	if !strings.Contains(c.Body, "@alice") {
		t.Errorf("plain body should keep the @handle, got %q", c.Body)
	}
}

func TestConvertPostToMatrix_MentionKeepsFormatting(t *testing.T) {
	t.Parallel()
	mc, _ := mentionTestClient(t)
	mc.connector.Puppets["@alice:example.com"] = &PuppetClient{MXID: "@alice:example.com", UserID: "alice-id", Username: "alice"}

	c := mc.convertPostToMatrix(&model.Post{Id: "p1", Message: "**urgent** @alice `@alice`"}).Parts[0].Content
	if !strings.Contains(c.FormattedBody, "<strong>urgent</strong>") ||
		!strings.Contains(c.FormattedBody, "<code>@alice</code>") ||
		strings.Count(c.FormattedBody, "matrix.to") != 1 {
		t.Errorf("unexpected body: %s", c.FormattedBody)
	}
}

func TestConvertPostToMatrix_RoomMention(t *testing.T) {
	t.Parallel()
	mc, fake := mentionTestClient(t)
	c := mc.convertPostToMatrix(&model.Post{Id: "p1", Message: "@channel standup"}).Parts[0].Content
	if c.Mentions == nil || !c.Mentions.Room {
		t.Errorf("expected room mention, got %+v", c.Mentions)
	}
	if fake.UsernameLookups.Load() != 0 {
		t.Error("broadcast mentions must not hit the user API")
	}
}

func TestConvertPostToMatrix_NoAtSignNoLookup(t *testing.T) {
	t.Parallel()
	mc, fake := mentionTestClient(t)
	c := mc.convertPostToMatrix(&model.Post{Id: "p1", Message: "just text"}).Parts[0].Content
	if c.Mentions != nil || fake.UsernameLookups.Load() != 0 {
		t.Errorf("unexpected mention handling: %+v lookups=%d", c.Mentions, fake.UsernameLookups.Load())
	}
}

func TestMentionLookupsAreCached(t *testing.T) {
	t.Parallel()
	mc, fake := mentionTestClient(t)
	mc.connector.Puppets["@alice:example.com"] = &PuppetClient{MXID: "@alice:example.com", UserID: "alice-id", Username: "alice"}
	for i := 0; i < 3; i++ {
		mc.convertPostToMatrix(&model.Post{Id: "p", Message: "@alice and @nobody"})
	}
	if got := fake.UsernameLookups.Load(); got != 2 {
		t.Errorf("expected one lookup per distinct name (2), got %d", got)
	}
}

func TestMatrixContentForMattermost_PillBecomesNativeHandle(t *testing.T) {
	t.Parallel()
	mc, _ := mentionTestClient(t)
	content := &event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          "Agent Bot: please look",
		Format:        event.FormatHTML,
		FormattedBody: `<a href="https://matrix.to/#/@agent:example.com">Agent Bot</a>: please look`,
	}
	got := matrixfmtParse(mc.matrixContentForMattermost(context.Background(), content))
	if got != "@agent-bot: please look" {
		t.Errorf("got %q", got)
	}
	if strings.Contains(got, "matrix.to") || strings.Contains(got, "example.com") {
		t.Errorf("Matrix handle leaked into Mattermost text: %q", got)
	}
	if content.FormattedBody == "" || !strings.Contains(content.FormattedBody, "matrix.to") {
		t.Error("original content must not be modified")
	}
}

func TestMatrixContentForMattermost_UnresolvableUserKeepsDisplayName(t *testing.T) {
	t.Parallel()
	mc, _ := mentionTestClient(t)
	content := &event.MessageEventContent{
		MsgType:       event.MsgText,
		Format:        event.FormatHTML,
		FormattedBody: `<a href="https://matrix.to/#/@stranger:other.org">Some Stranger</a> hi`,
	}
	got := matrixfmtParse(mc.matrixContentForMattermost(context.Background(), content))
	if got != "Some Stranger hi" {
		t.Errorf("got %q", got)
	}
}

func TestMatrixContentForMattermost_NilAndPlain(t *testing.T) {
	t.Parallel()
	mc, _ := mentionTestClient(t)
	if mc.matrixContentForMattermost(context.Background(), nil) != nil {
		t.Error("nil content must stay nil")
	}
	plain := &event.MessageEventContent{MsgType: event.MsgText, Body: "hi"}
	if mc.matrixContentForMattermost(context.Background(), plain) != plain {
		t.Error("plain content must be returned unchanged")
	}
}

func FuzzLinkMentions(f *testing.F) {
	f.Add("hi @alice, <code>@bob</code> @channel x@y.z")
	f.Add("@@@a @.- @a.b_c-")
	f.Fuzz(func(t *testing.T, in string) {
		out, users, _ := linkMentions(in, func(name string) (id.UserID, bool) {
			return id.UserID("@" + name + ":example.com"), name != ""
		})
		if len(users) == 0 && out != in {
			t.Fatalf("body changed without any mention: %q -> %q", in, out)
		}
	})
}

func FuzzRewriteMatrixMentions(f *testing.F) {
	f.Add(`<a href="https://matrix.to/#/@a:b">x</a>`)
	f.Add(`<a href="https://matrix.to/#/%40a%3Ab?via=c">`)
	f.Fuzz(func(t *testing.T, in string) {
		_ = rewriteMatrixMentions(in, func(id.UserID) (string, bool) { return "u", true })
	})
}
